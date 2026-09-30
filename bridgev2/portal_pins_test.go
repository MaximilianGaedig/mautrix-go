// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestPinChanges(t *testing.T) {
	a, b, c := id.EventID("$a"), id.EventID("$b"), id.EventID("$c")
	t.Run("a new pin", func(t *testing.T) {
		assert.Equal(t, []pinChange{{b, true}}, pinChanges([]id.EventID{a}, []id.EventID{a, b}))
	})
	t.Run("an unpin", func(t *testing.T) {
		assert.Equal(t, []pinChange{{a, false}}, pinChanges([]id.EventID{a, b}, []id.EventID{b}))
	})
	t.Run("pins before unpins", func(t *testing.T) {
		assert.Equal(t, []pinChange{{c, true}, {a, false}}, pinChanges([]id.EventID{a, b}, []id.EventID{b, c}))
	})
	t.Run("the first pins in a room without any", func(t *testing.T) {
		assert.Equal(t, []pinChange{{a, true}, {b, true}}, pinChanges(nil, []id.EventID{a, b}))
	})
	t.Run("reordering changes nothing", func(t *testing.T) {
		assert.Empty(t, pinChanges([]id.EventID{a, b}, []id.EventID{b, a}))
	})
}

func TestWithPinChange(t *testing.T) {
	a, b, c := id.EventID("$a"), id.EventID("$b"), id.EventID("$c")
	t.Run("pins to the end", func(t *testing.T) {
		assert.Equal(t, []id.EventID{a, b, c}, withPinChange([]id.EventID{a, b}, c, true))
	})
	t.Run("re-pinning moves it to the end instead of adding it twice", func(t *testing.T) {
		assert.Equal(t, []id.EventID{b, a}, withPinChange([]id.EventID{a, b}, a, true))
	})
	t.Run("unpins", func(t *testing.T) {
		assert.Equal(t, []id.EventID{a, c}, withPinChange([]id.EventID{a, b, c}, b, false))
	})
	t.Run("unpinning something not pinned changes nothing", func(t *testing.T) {
		assert.Equal(t, []id.EventID{a}, withPinChange([]id.EventID{a}, b, false))
	})
	t.Run("does not change the list it was given", func(t *testing.T) {
		original := []id.EventID{a, b}
		withPinChange(original, a, false)
		assert.Equal(t, []id.EventID{a, b}, original)
	})
}

func TestLowerEventLevelFor(t *testing.T) {
	ghost := id.UserID("@ghost:example.org")
	pl := &event.PowerLevelsEventContent{Users: map[id.UserID]int{"@bot:example.org": 100}}
	// Unlisted state events need state_default (50): a ghost at 0 can't send them until it's lowered.
	assert.True(t, lowerEventLevelFor(pl, event.StateUnstableBeaconInfo, ghost))
	assert.Equal(t, 0, pl.GetEventLevel(event.StateUnstableBeaconInfo))
	assert.False(t, lowerEventLevelFor(pl, event.StateUnstableBeaconInfo, ghost), "already sendable")
	// Other state events keep their level.
	assert.Equal(t, 50, pl.GetEventLevel(event.StateRoomName))
}
