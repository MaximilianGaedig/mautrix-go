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

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// UserBlockingNetworkAPI is an optional interface that network connectors can implement to block people.
// Matrix's block is the user's ignore list: ignoring a ghost blocks the person it stands for, and
// un-ignoring it unblocks them.
type UserBlockingNetworkAPI interface {
	NetworkAPI
	HandleMatrixBlock(ctx context.Context, ghost *Ghost, blocked bool) error
}

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

// HandleIgnoredUserList blocks or unblocks, on each of the sender's logins that can, the people whose ghosts
// the sender started or stopped ignoring.
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
		ghost, err := br.GetGhostByID(ctx, ghostID)
		if err != nil || ghost == nil {
			return
		}
		for _, login := range user.GetUserLogins() {
			api, ok := login.Client.(UserBlockingNetworkAPI)
			if !ok {
				continue
			}
			if err := api.HandleMatrixBlock(ctx, ghost, blocked); err != nil {
				log.Err(err).Str("ghost_id", string(ghostID)).Bool("blocked", blocked).
					Str("login_id", string(login.ID)).Msg("Failed to change block")
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
