// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
)

// MarkAllDMs records every one-to-one portal in the user's m.direct, the account data Matrix clients
// read to tell a chat with one person from a room.
//
// A portal is marked when it is created, but only if double puppeting was already set up then: the
// bridge writes the user's own account data as the user. A chat bridged before that stays unmarked
// forever, so every client but this one files it under Rooms, shows it by its members ("<bot> and 2
// others") and offers it group behaviour. This brings those up to date once the bridge has a double
// puppet, and is harmless for the ones already marked.
func (br *Bridge) MarkAllDMs(ctx context.Context) {
	portals, err := br.GetAllPortalsWithMXID(ctx)
	if err != nil {
		br.Log.Err(err).Msg("Failed to list portals to mark as DMs")
		return
	}
	var marked int
	for _, portal := range portals {
		if br.IsStopping() || ctx.Err() != nil {
			return
		}
		if portal.RoomType != database.RoomTypeDM || portal.OtherUserID == "" {
			continue
		}
		login := portal.Bridge.GetCachedUserLoginByID(portal.Receiver)
		if login == nil {
			continue
		}
		dp, ok := login.User.DoublePuppet(ctx).(MarkAsDMMatrixAPI)
		if !ok {
			continue
		}
		ghost, err := br.GetGhostByID(ctx, portal.OtherUserID)
		if err != nil {
			zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to get the chat's other user to mark it as a DM")
			continue
		}
		if err := dp.MarkAsDM(ctx, portal.MXID, ghost.Intent.GetMXID()); err != nil {
			zerolog.Ctx(ctx).Debug().Err(err).Stringer("room_id", portal.MXID).Msg("Failed to mark the chat as a DM")
			continue
		}
		marked++
		time.Sleep(50 * time.Millisecond)
	}
	br.Log.Info().Int("marked", marked).Msg("Marked one-to-one chats as DMs")
}
