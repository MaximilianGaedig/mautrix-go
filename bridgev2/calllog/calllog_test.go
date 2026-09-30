// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package calllog

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

var t0 = time.Unix(1_700_000_000, 0)

func text(t *testing.T, evt bridgev2.RemoteEvent) string {
	t.Helper()
	require.NotNil(t, evt)
	return evt.(*simplevent.Message[*Call]).Data.Text()
}

func TestAnsweredCall(t *testing.T) {
	l := New()
	start := l.Start("a", Call{Started: t0})
	assert.Equal(t, bridgev2.RemoteEventMessage, start.GetType())
	assert.Equal(t, "Incoming voice call", text(t, start))
	assert.Equal(t, "Voice call in progress", text(t, l.Answer("a", t0.Add(5*time.Second))))
	end := l.End("a", t0.Add(5*time.Second+192*time.Second))
	assert.Equal(t, bridgev2.RemoteEventEdit, end.GetType())
	assert.Equal(t, MessageID("a"), end.(*simplevent.Message[*Call]).TargetMessage)
	assert.Equal(t, "Voice call, 3:12", text(t, end))
	assert.Nil(t, l.End("a", t0.Add(time.Hour)), "a repeated end changes nothing")
}

func TestUnansweredCalls(t *testing.T) {
	l := New()
	l.Start("missed", Call{Started: t0, Video: true})
	assert.Equal(t, "Missed video call", text(t, l.End("missed", t0.Add(30*time.Second))))

	l.Start("declined", Call{Started: t0})
	assert.Equal(t, "Declined voice call", text(t, l.Decline("declined", t0.Add(time.Second))))
	assert.Nil(t, l.End("declined", t0.Add(2*time.Second)), "the end after a decline keeps it declined")

	l.Start("mine", Call{Started: t0, Outgoing: true, Group: true})
	assert.Equal(t, "Cancelled group voice call", text(t, l.End("mine", t0.Add(time.Second))))
}

func TestUnknownAndRepeatedCalls(t *testing.T) {
	l := New()
	assert.Nil(t, l.End("never-seen", t0), "a call from before the bridge started has no message to edit")
	require.NotNil(t, l.Start("a", Call{Started: t0}))
	assert.Nil(t, l.Start("a", Call{Started: t0}), "a repeated offer isn't a second call")
}

func TestLongCall(t *testing.T) {
	c := Call{State: Ended, Answered: t0, Finished: t0.Add(2*time.Hour + 3*time.Minute + 4*time.Second)}
	assert.Equal(t, "Voice call, 2:03:04", c.Text())
}
