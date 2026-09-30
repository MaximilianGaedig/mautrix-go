// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package calllog bridges a network call as one timeline message that follows the call: it appears when the
// call starts ("Incoming voice call") and is edited as the call goes on ("Missed voice call", "Voice call,
// 3:12"), the way chat apps show calls. Connectors report what happened to a call; the package keeps track
// of each call and turns the reports into remote events.
package calllog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
)

// State is where a call is.
type State int

const (
	Ringing  State = iota // offered, not answered yet
	Ongoing               // answered
	Missed                // rang out or was cancelled by the caller before anyone answered
	Declined              // rejected by the callee
	Ended                 // answered, then hung up
)

// Call is what the log knows about one call.
type Call struct {
	Portal   networkid.PortalKey
	Caller   bridgev2.EventSender
	Video    bool
	Group    bool
	Outgoing bool // placed by the logged-in user
	State    State

	Started  time.Time
	Answered time.Time
	Finished time.Time
}

// Duration is how long the call was connected, or zero when it never was.
func (c *Call) Duration() time.Duration {
	if c.Answered.IsZero() || c.Finished.Before(c.Answered) {
		return 0
	}
	return c.Finished.Sub(c.Answered).Truncate(time.Second)
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// Text is the line the timeline shows for the call.
func (c *Call) Text() string {
	kind := "voice call"
	if c.Video {
		kind = "video call"
	}
	if c.Group {
		kind = "group " + kind
	}
	switch c.State {
	case Ringing:
		if c.Outgoing {
			return "Outgoing " + kind
		}
		return "Incoming " + kind
	case Ongoing:
		return capitalize(kind) + " in progress"
	case Missed:
		if c.Outgoing {
			return "Cancelled " + kind
		}
		return "Missed " + kind
	case Declined:
		return "Declined " + kind
	default:
		if d := c.Duration(); d > 0 {
			return fmt.Sprintf("%s, %s", capitalize(kind), formatDuration(d))
		}
		return capitalize(kind) + " ended"
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

func (c *Call) content() *event.MessageEventContent {
	callType := event.BeeperActionMessageCallTypeVoice
	if c.Video {
		callType = event.BeeperActionMessageCallTypeVideo
	}
	return &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    c.Text(),
		BeeperActionMessage: &event.BeeperActionMessage{
			Type:     event.BeeperActionMessageCall,
			CallType: callType,
		},
	}
}

// How long a call's state is kept after it finished, for late or repeated reports.
const keepFinished = 10 * time.Minute

// Log keeps the calls of one login. Each report returns the remote event to queue, or nil when the report
// changes nothing on screen.
type Log struct {
	lock  sync.Mutex
	calls map[string]*Call
}

func New() *Log {
	return &Log{calls: make(map[string]*Call)}
}

// MessageID is the message a call is shown as.
func MessageID(callID string) networkid.MessageID {
	return networkid.MessageID("call:" + callID)
}

// Start records a new call and returns the message for it. A call already known (a repeated offer) returns nil.
func (l *Log) Start(callID string, call Call) bridgev2.RemoteEvent {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.expire(call.Started)
	if _, ok := l.calls[callID]; ok {
		return nil
	}
	call.State = Ringing
	stored := call
	l.calls[callID] = &stored
	snapshot := stored
	return &simplevent.Message[*Call]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    call.Portal,
			Sender:       call.Caller,
			CreatePortal: true,
			Timestamp:    call.Started,
		},
		ID:   MessageID(callID),
		Data: &snapshot,
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, c *Call) (*bridgev2.ConvertedMessage, error) {
			return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
				Type:    event.EventMessage,
				Content: c.content(),
			}}}, nil
		},
	}
}

// Answer marks the call connected.
func (l *Log) Answer(callID string, at time.Time) bridgev2.RemoteEvent {
	return l.update(callID, at, func(c *Call) {
		if c.State == Ringing {
			c.State = Ongoing
			c.Answered = at
		}
	})
}

// Decline marks the call rejected by the callee.
func (l *Log) Decline(callID string, at time.Time) bridgev2.RemoteEvent {
	return l.update(callID, at, func(c *Call) {
		if c.State == Ringing {
			c.State = Declined
			c.Finished = at
		}
	})
}

// End marks the call over: missed if nobody answered, ended otherwise.
func (l *Log) End(callID string, at time.Time) bridgev2.RemoteEvent {
	return l.update(callID, at, func(c *Call) {
		switch c.State {
		case Ringing:
			c.State = Missed
			c.Finished = at
		case Ongoing:
			c.State = Ended
			c.Finished = at
		}
	})
}

func (l *Log) update(callID string, at time.Time, change func(*Call)) bridgev2.RemoteEvent {
	l.lock.Lock()
	defer l.lock.Unlock()
	call, ok := l.calls[callID]
	if !ok {
		// Started before the bridge was running: nothing on screen to update.
		return nil
	}
	before := call.Text()
	change(call)
	if call.Text() == before {
		return nil
	}
	snapshot := *call
	return &simplevent.Message[*Call]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: call.Portal,
			Sender:    call.Caller,
			Timestamp: at,
		},
		ID:            MessageID(callID),
		TargetMessage: MessageID(callID),
		Data:          &snapshot,
		ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, c *Call) (*bridgev2.ConvertedEdit, error) {
			part := (&bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: c.content()}).ToEditPart(existing[0])
			// The call line changing isn't someone editing a message.
			part.TopLevelExtra = map[string]any{"com.beeper.dont_render_edited": true}
			return &bridgev2.ConvertedEdit{ModifiedParts: []*bridgev2.ConvertedEditPart{part}}, nil
		},
	}
}

func (l *Log) expire(now time.Time) {
	for id, call := range l.calls {
		if !call.Finished.IsZero() && now.Sub(call.Finished) > keepFinished {
			delete(l.calls, id)
		} else if call.Finished.IsZero() && now.Sub(call.Started) > 24*time.Hour {
			delete(l.calls, id)
		}
	}
}
