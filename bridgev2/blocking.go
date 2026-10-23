// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"fmt"
	"slices"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Matrix's block is the user's ignore list: ignoring a ghost blocks the person it stands for, and un-ignoring it
// unblocks them. Upstream's UserBlockingNetworkAPI (com.beeper.block_user, sent in a DM) does the blocking on the
// network; this file only adds the ignore list on top of it, in both directions:
//
//   - Matrix to network: HandleIgnoredUserList calls HandleMatrixBlockUser for each DM portal the user has with
//     a newly (un)ignored ghost, exactly as if com.beeper.block_user had been sent in that DM.
//   - Network to Matrix: when a connector reports ChatInfo.UserBlocked for a DM (upstream's way to say the person
//     was blocked or unblocked on the network), Portal.UpdateInfo mirrors it into the ignore list through the
//     double puppet (mirrorBlockToIgnoreList).

// GlobalAccountDataMatrixAPI is a MatrixAPI that can read and write the user's global account data, which the
// double puppet needs to mirror a block from the network into the ignore list.
type GlobalAccountDataMatrixAPI interface {
	MatrixAPI
	GetAccountData(ctx context.Context, eventType string, into any) error
	SetAccountData(ctx context.Context, eventType string, content any) error
}

// ignoreListChanges is who a change to an ignore list starts and stops ignoring.
func ignoreListChanges(now, prev *event.IgnoredUserListEventContent) (ignored, unignored []id.UserID) {
	var nowUsers, prevUsers map[id.UserID]event.IgnoredUser
	if now != nil {
		nowUsers = now.IgnoredUsers
	}
	if prev != nil {
		prevUsers = prev.IgnoredUsers
	}
	for userID := range nowUsers {
		if _, had := prevUsers[userID]; !had {
			ignored = append(ignored, userID)
		}
	}
	for userID := range prevUsers {
		if _, has := nowUsers[userID]; !has {
			unignored = append(unignored, userID)
		}
	}
	slices.Sort(ignored)
	slices.Sort(unignored)
	return
}

// HandleIgnoredUserList blocks or unblocks, through upstream's UserBlockingNetworkAPI, the people whose ghosts
// the sender started or stopped ignoring, in every DM portal the sender's logins have with them. A ghost the user
// has no DM with is left alone: upstream's block is addressed by DM portal.
func (br *Bridge) HandleIgnoredUserList(ctx context.Context, evt *event.Event) {
	log := zerolog.Ctx(ctx).With().Str("action", "handle ignored user list").Stringer("sender", evt.Sender).Logger()
	ctx = log.WithContext(ctx)
	user, err := br.GetExistingUserByMXID(ctx, evt.Sender)
	if err != nil || user == nil {
		return
	}
	now, _ := evt.Content.Parsed.(*event.IgnoredUserListEventContent)
	var prev *event.IgnoredUserListEventContent
	if evt.Unsigned.PrevContent != nil {
		_ = evt.Unsigned.PrevContent.ParseRaw(event.AccountDataIgnoredUserList)
		prev, _ = evt.Unsigned.PrevContent.Parsed.(*event.IgnoredUserListEventContent)
	}
	ignored, unignored := ignoreListChanges(now, prev)
	change := func(userID id.UserID, blocked bool) {
		ghostID, isGhost := br.Matrix.ParseGhostMXID(userID)
		if !isGhost {
			return
		}
		for _, login := range user.GetUserLogins() {
			api, ok := login.Client.(UserBlockingNetworkAPI)
			if !ok {
				continue
			}
			for _, portal := range br.dmPortalsOf(ctx, login, ghostID) {
				portal.blockFromIgnoreList(ctx, api, blocked)
			}
		}
	}
	for _, userID := range ignored {
		change(userID, true)
	}
	for _, userID := range unignored {
		change(userID, false)
	}
}

// dmPortalsOf is the DM portals the login has with the ghost: its own (split portals), or a shared one it is in.
func (br *Bridge) dmPortalsOf(ctx context.Context, login *UserLogin, ghostID networkid.UserID) (out []*Portal) {
	log := zerolog.Ctx(ctx).With().Str("ghost_id", string(ghostID)).Str("login_id", string(login.ID)).Logger()
	portals, err := br.GetDMPortalsWith(ctx, ghostID)
	if err != nil {
		log.Err(err).Msg("Failed to find DM portals to change block")
		return nil
	}
	for _, portal := range portals {
		if portal.Receiver == login.ID {
			out = append(out, portal)
		} else if portal.Receiver == "" {
			if up, err := br.DB.UserPortal.Get(ctx, login.UserLogin, portal.PortalKey); err == nil && up != nil {
				out = append(out, portal)
			}
		}
	}
	if len(out) == 0 {
		log.Debug().Msg("No DM portal with ignored ghost, not changing block")
	}
	return out
}

// blockFromIgnoreList is upstream's handleMatrixBlockUser for a block that came from the ignore list instead of a
// com.beeper.block_user event. A portal already in that state is skipped: that is our own ignore-list write
// (mirrorBlockToIgnoreList) coming back.
func (portal *Portal) blockFromIgnoreList(ctx context.Context, api UserBlockingNetworkAPI, blocked bool) {
	if portal.UserBlocked == blocked {
		return
	}
	log := zerolog.Ctx(ctx).With().Stringer("portal_key", portal.PortalKey).Bool("blocked", blocked).Logger()
	err := api.HandleMatrixBlockUser(ctx, &MatrixBlockUser{
		Content: &event.BeeperBlockUserEventContent{Block: blocked},
		Portal:  portal,
	})
	if err != nil {
		log.Err(err).Msg("Failed to change block from ignore list")
		return
	}
	portal.UserBlocked = blocked
	portal.UpdateBridgeInfo(ctx)
	if err = portal.Save(ctx); err != nil {
		log.Err(err).Msg("Failed to save portal after block from ignore list")
	}
}

// mirrorBlockToIgnoreList puts the DM's other user on the source's ignore list when the network says they are
// blocked, and takes them off it when they are unblocked.
func (portal *Portal) mirrorBlockToIgnoreList(ctx context.Context, source *UserLogin) {
	if source == nil || portal.RoomType != database.RoomTypeDM || portal.OtherUserID == "" {
		return
	}
	if err := source.SetGhostBlocked(ctx, portal.OtherUserID, portal.UserBlocked); err != nil {
		zerolog.Ctx(ctx).Err(err).Bool("blocked", portal.UserBlocked).Msg("Failed to mirror block into ignore list")
	}
}

// withIgnored is the ignore list with userID added (blocked) or removed.
func withIgnored(list *event.IgnoredUserListEventContent, userID id.UserID, blocked bool) (*event.IgnoredUserListEventContent, bool) {
	out := &event.IgnoredUserListEventContent{IgnoredUsers: make(map[id.UserID]event.IgnoredUser)}
	if list != nil {
		for k, v := range list.IgnoredUsers {
			out.IgnoredUsers[k] = v
		}
	}
	_, had := out.IgnoredUsers[userID]
	if blocked == had {
		return out, false
	}
	if blocked {
		out.IgnoredUsers[userID] = event.IgnoredUser{}
	} else {
		delete(out.IgnoredUsers, userID)
	}
	return out, true
}

// SetGhostBlocked mirrors a block (or unblock) made on the network into the user's ignore list, through
// their double puppet.
func (ul *UserLogin) SetGhostBlocked(ctx context.Context, ghostID networkid.UserID, blocked bool) error {
	dp, ok := ul.User.DoublePuppet(ctx).(GlobalAccountDataMatrixAPI)
	if !ok {
		return nil
	}
	var list event.IgnoredUserListEventContent
	if err := dp.GetAccountData(ctx, event.AccountDataIgnoredUserList.Type, &list); err != nil {
		// Nothing ignored yet: an empty list.
		list = event.IgnoredUserListEventContent{}
	}
	updated, changed := withIgnored(&list, ul.Bridge.Matrix.GhostIntent(ghostID).GetMXID(), blocked)
	if !changed {
		return nil
	}
	if err := dp.SetAccountData(ctx, event.AccountDataIgnoredUserList.Type, updated); err != nil {
		return fmt.Errorf("failed to update ignore list: %w", err)
	}
	return nil
}
