// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package commands

import (
	"maunium.net/go/mautrix/bridgev2"
)

var CommandRecreatePortal = &FullHandler{
	Func: fnRecreatePortal,
	Name: "recreate-portal",
	Help: HelpMeta{
		Section:     HelpSectionAdmin,
		Description: "Replace the current portal room with a new one for the same chat, backfilled again",
	},
	RequiresAdmin:  true,
	RequiresPortal: true,
}

func fnRecreatePortal(ce *Event) {
	login, _, err := ce.Portal.FindPreferredLogin(ce.Ctx, ce.User, false)
	if err != nil {
		ce.Reply("Failed to find a login to recreate the chat with: %v", err)
		return
	} else if login == nil {
		ce.Reply("You have no login in this chat to recreate it with")
		return
	}
	// The reply goes to the old room, which is about to stop being the portal, so send it first.
	ce.Reply("Creating a new room for this chat; this one will point to it once it is ready.")
	ce.MessageStatus.DisableMSS = true
	newMXID, err := ce.Portal.Recreate(ce.Ctx, login)
	if err != nil {
		ce.Reply("Failed to recreate the chat: %v", err)
		return
	}
	ce.Log.Info().Stringer("new_room_id", newMXID).Msg("Recreated portal")
}

var CommandSyncChats = &FullHandler{
	Func: fnSyncChats,
	Name: "sync-chats",
	Help: HelpMeta{
		Section:     HelpSectionChats,
		Description: "Go over your whole chat list again, creating rooms for chats that do not have one",
	},
	RequiresLogin: true,
}

func fnSyncChats(ce *Event) {
	started := 0
	for _, login := range ce.User.GetUserLogins() {
		syncer, ok := login.Client.(bridgev2.ChatListSyncingNetworkAPI)
		if !ok {
			continue
		}
		started++
		go func(login *bridgev2.UserLogin) {
			log := login.Log.With().Str("action", "sync chat list").Logger()
			ctx := log.WithContext(ce.Bridge.BackgroundCtx)
			if err := syncer.SyncChatList(ctx); err != nil {
				log.Err(err).Msg("Chat list sync failed")
				ce.Reply("Syncing the chat list of %s failed: %v", login.RemoteName, err)
				return
			}
			ce.Reply("Synced the chat list of %s. Chats that were missing a room get one as they come in.", login.RemoteName)
		}(login)
	}
	if started == 0 {
		ce.Reply("This bridge cannot go over the chat list again")
		return
	}
	ce.Reply("Syncing the chat list of %d login(s)…", started)
}
