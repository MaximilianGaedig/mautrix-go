// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

type spaceTestMatrix struct{ MatrixConnector }

func (spaceTestMatrix) ServerName() string { return "example.org" }

// A room whose parent is already right but which never made it into the parent space (rooms carried over from
// a legacy bridge database have in_space=false) is added when the network repeats the same parent.
func TestUpdateParentAddsRoomMissingFromItsSpace(t *testing.T) {
	bot := &fakeSettingsBot{}
	br := &Bridge{Bot: bot, Matrix: spaceTestMatrix{}, Config: &bridgeconfig.BridgeConfig{}}
	parent := &Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "guild"}, MXID: "!space:example.org"}, Bridge: br}
	portal := &Portal{
		Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "channel"}, MXID: "!room:example.org", ParentKey: networkid.PortalKey{ID: "guild"}},
		Bridge: br,
		Parent: parent,
	}

	changed := portal.updateParent(context.Background(), "guild", nil)

	assert.True(t, changed)
	assert.True(t, portal.InSpace)
	var child bool
	for _, state := range bot.states {
		if state.room == "!space:example.org" && state.evtType == event.StateSpaceChild.Type && state.stateKey == "!room:example.org" {
			child = true
		}
	}
	assert.True(t, child, "the parent space got no m.space.child for the room")

	bot.states = nil
	assert.False(t, portal.updateParent(context.Background(), "guild", nil), "a room already in its space is left alone")
	assert.Empty(t, bot.states)
}
