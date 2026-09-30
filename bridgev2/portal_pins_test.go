// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
