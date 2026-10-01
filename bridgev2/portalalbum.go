// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// AlbumFieldKey is the top-level content field that marks media events which belong together as
// one album. Every item is still its own m.room.message event, so a client that doesn't know
// about albums shows each item normally.
//
// Bridges set the field on the albums they bring in, and a client sets it on the files it sends
// together. If the network connector implements [AlbumHandlingNetworkAPI], the events of such
// an outgoing album are sent to the network as one album.
const AlbumFieldKey = "fi.mau.album"

// AlbumMarker is the value of the [AlbumFieldKey] field.
type AlbumMarker struct {
	// ID is an opaque identifier shared by the items of the album.
	ID string `json:"id"`
	// Index is the 0-based position of the item within the album.
	Index int `json:"index"`
	// Count is the total number of items. Bridges omit it when the network doesn't tell,
	// but an outgoing album can only be collected when it is there.
	Count int `json:"count,omitempty"`
}

// The limits for collecting an outgoing album. They are variables rather than configuration
// because there is one sensible trade-off, and tests need to shorten them.
var (
	// AlbumIdleTimeout is how long an incomplete album waits for its next part. The client
	// uploads each file before sending its event, so this has to cover the upload of one file.
	// It is also how long the other parts are delayed when one of them never arrives.
	AlbumIdleTimeout = 15 * time.Second
	// AlbumMaxHold is the longest a part of an album is held back in total.
	AlbumMaxHold = 2 * time.Minute
	// AlbumMaxHeld is how many albums a portal tracks at once.
	AlbumMaxHeld = 16
	// AlbumMaxParts is the largest album a portal collects. Parts of a larger one are sent
	// one by one, like before.
	AlbumMaxParts = 100
)

// ParseAlbumMarker reads the album marker of an event, if it has a usable one.
func ParseAlbumMarker(raw map[string]any) (marker AlbumMarker, ok bool) {
	field, ok := raw[AlbumFieldKey].(map[string]any)
	if !ok {
		return marker, false
	}
	marker.ID, _ = field["id"].(string)
	var indexOK, countOK bool
	marker.Index, indexOK = albumMarkerInt(field["index"])
	marker.Count, countOK = albumMarkerInt(field["count"])
	return marker, marker.ID != "" && indexOK && countOK
}

// albumMarkerInt accepts the forms a JSON integer takes after decoding. A number with a
// fraction is not an integer, and the marker is then not usable.
func albumMarkerInt(val any) (int, bool) {
	switch typed := val.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

// albumKey keeps the albums of different senders apart. The portal itself keeps rooms apart,
// as every portal has its own collector.
type albumKey struct {
	user  id.UserID
	login networkid.UserLoginID
	album string
}

// heldAlbumPart is a Matrix message whose handling stopped right before it would have been
// sent, together with what is needed to resume it.
type heldAlbumPart struct {
	ctx    context.Context
	sender *UserLogin
	msg    *MatrixMessage
	timer  *event.BeeperDisappearingTimer
}

type portalAlbums struct {
	lock      sync.Mutex
	collector *AlbumCollector[albumKey, *heldAlbumPart]
	flush     *time.Timer
}

// portalAlbumFlushEvent goes through the portal's event queue when held albums run out of time.
// Sending them from the queue, and not from the timer, keeps an album in sequence with the
// other events of the room, like the echo of the album itself coming back from the network.
type portalAlbumFlushEvent struct{}

func (*portalAlbumFlushEvent) isPortalEvent() {}

func (pa *portalAlbums) getCollector() *AlbumCollector[albumKey, *heldAlbumPart] {
	pa.lock.Lock()
	defer pa.lock.Unlock()
	if pa.collector == nil {
		pa.collector = NewAlbumCollector[albumKey, *heldAlbumPart](AlbumIdleTimeout, AlbumMaxHold, AlbumMaxHeld, AlbumMaxParts)
	}
	return pa.collector
}

// collectAlbumPart decides whether a Matrix message is held back as a part of an album.
//
// It is not held, and so sent exactly like before, unless the network connector can send albums
// and the event is a media message with a usable album marker. Relayed messages are not held
// either: the relay puts the sender's name into each caption, which an album would show once
// per item.
//
// When it is held, the batches that became ready are returned. This part is in the last of them
// if it completed its album.
func (portal *Portal) collectAlbumPart(ctx context.Context, sender *UserLogin, msg *MatrixMessage, timer *event.BeeperDisappearingTimer) (ready [][]*heldAlbumPart, held bool) {
	if _, canSendAlbums := sender.Client.(AlbumHandlingNetworkAPI); !canSendAlbums {
		return nil, false
	} else if msg.OrigSender != nil || msg.Event.Type != event.EventMessage || msg.Content == nil {
		return nil, false
	}
	switch msg.Content.MsgType {
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
	default:
		return nil, false
	}
	marker, ok := ParseAlbumMarker(msg.Event.Content.Raw)
	if !ok {
		return nil, false
	}
	key := albumKey{user: msg.Event.Sender, login: sender.ID, album: marker.ID}
	result, ready := portal.albums.getCollector().Add(key, marker.Index, marker.Count, &heldAlbumPart{
		ctx:    ctx,
		sender: sender,
		msg:    msg,
		timer:  timer,
	})
	if result != AlbumPartHeld {
		zerolog.Ctx(ctx).Warn().
			Any("album", marker).
			Bool("duplicate_index", result == AlbumPartDuplicate).
			Msg("Not collecting message with unusable album marker, sending it on its own")
		return nil, false
	}
	return ready, true
}

// holdAlbumPart is the hook in the handling of a Matrix message, right before the message would
// be sent to the network. If it reports true, the message was taken over and the caller is done.
//
// Ordering: an album takes the place of the last of its parts to arrive. That part sends the
// whole album from inside the portal's event queue, so whatever follows it stays behind the
// album. Everything else is never held and never waits for an album: a text that arrives while
// an album is still incomplete goes out first. That matches the room, where the text also sits
// before the album's last file, and it means a missing part can delay only its own album.
func (portal *Portal) holdAlbumPart(ctx context.Context, sender *UserLogin, msg *MatrixMessage, timer *event.BeeperDisappearingTimer) (EventHandlingResult, bool) {
	ready, held := portal.collectAlbumPart(ctx, sender, msg, timer)
	if !held {
		return EventHandlingResult{}, false
	}
	// Until its album is sent, the event is in the same state as one waiting in the queue.
	res := EventHandlingResultQueued
	for _, batch := range ready {
		for i, partRes := range portal.sendAlbum(batch) {
			if batch[i].msg == msg {
				res = partRes
			}
		}
	}
	portal.scheduleAlbumFlush()
	if len(ready) == 0 {
		zerolog.Ctx(ctx).Debug().Msg("Holding message until the rest of its album arrives")
	}
	return res, true
}

// scheduleAlbumFlush makes sure the portal looks at its held albums when the next one runs out
// of time.
func (portal *Portal) scheduleAlbumFlush() {
	collector := portal.albums.getCollector()
	portal.albums.lock.Lock()
	defer portal.albums.lock.Unlock()
	if portal.albums.flush != nil {
		portal.albums.flush.Stop()
		portal.albums.flush = nil
	}
	deadline, ok := collector.NextDeadline()
	if !ok {
		return
	}
	// The floor keeps a deadline that has just passed from turning into a busy loop.
	wait := max(deadline.Sub(collector.Now()), 10*time.Millisecond)
	portal.albums.flush = time.AfterFunc(wait, func() {
		portal.queueEvent(portal.Log.WithContext(portal.backgroundCtx), &portalAlbumFlushEvent{})
	})
}

// flushDueAlbums sends the albums that waited long enough with the parts they have.
func (portal *Portal) flushDueAlbums(ctx context.Context) {
	for _, batch := range portal.albums.getCollector().Due() {
		zerolog.Ctx(ctx).Debug().
			Int("parts", len(batch)).
			Stringer("first_event_id", batch[0].msg.Event.ID).
			Msg("Album didn't complete in time, sending the parts that arrived")
		portal.sendAlbum(batch)
	}
	portal.scheduleAlbumFlush()
}

// sendAlbum sends a batch to the network and finishes every part the way a message sent on its
// own is finished: each has its own database row and its own delivery status.
func (portal *Portal) sendAlbum(batch []*heldAlbumPart) []EventHandlingResult {
	results := make([]EventHandlingResult, len(batch))
	finished := 0
	defer func() {
		// The parts that were held aren't the event the queue is handling, so nothing else
		// would tell their sender that they failed.
		if v := recover(); v != nil {
			for _, part := range batch[finished:] {
				portal.sendErrorStatus(part.ctx, part.msg.Event, ErrPanicInEventHandler)
			}
			panic(v)
		}
	}()
	msgs := make([]*MatrixMessage, len(batch))
	for i, part := range batch {
		msgs[i] = part.msg
	}
	ctx := batch[0].ctx
	if len(batch) > 1 {
		ctx = zerolog.Ctx(ctx).With().Int("album_parts", len(batch)).Logger().WithContext(ctx)
	}
	outcomes := deliverAlbum(ctx, batch[0].sender.Client, msgs)
	for i, part := range batch {
		if err := outcomes[i].Err; err != nil {
			zerolog.Ctx(part.ctx).Err(err).Msg("Failed to handle Matrix message as part of an album")
			portal.sendErrorStatus(part.ctx, part.msg.Event, err)
			results[i] = EventHandlingResultFailed.WithError(err)
		} else {
			results[i] = portal.finishMatrixMessage(part.ctx, part.msg, outcomes[i].Response, part.timer)
		}
		finished = i + 1
	}
	return results
}

// ErrAlbumPartNotHandled is the failure of an album part that the network connector returned
// neither a response nor an error for.
var ErrAlbumPartNotHandled = errors.New("network connector didn't handle album part")

// deliverAlbum hands a batch to the network connector and returns exactly one outcome per part.
// A batch of one is an ordinary message.
func deliverAlbum(ctx context.Context, client NetworkAPI, msgs []*MatrixMessage) []MatrixAlbumPartResult {
	outcomes := make([]MatrixAlbumPartResult, len(msgs))
	if len(msgs) == 1 {
		outcomes[0].Response, outcomes[0].Err = client.HandleMatrixMessage(ctx, msgs[0])
		if outcomes[0].Response == nil && outcomes[0].Err == nil {
			outcomes[0].Err = ErrAlbumPartNotHandled
		}
		return outcomes
	}
	results, err := client.(AlbumHandlingNetworkAPI).HandleMatrixAlbum(ctx, msgs)
	if err == nil && len(results) != len(msgs) {
		// Without one result per part there is no telling which part a result belongs to.
		err = fmt.Errorf("%w: got %d results for %d parts", ErrAlbumPartNotHandled, len(results), len(msgs))
	}
	for i := range outcomes {
		if err != nil {
			outcomes[i].Err = err
		} else if outcomes[i] = results[i]; outcomes[i].Response == nil && outcomes[i].Err == nil {
			outcomes[i].Err = ErrAlbumPartNotHandled
		}
	}
	return outcomes
}
