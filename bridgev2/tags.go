// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"maunium.net/go/mautrix/event"
)

// TagChange says what a room tag change did to one tag: added (true), removed (false), or nothing (nil).
// Connectors that bridge a chat state as a tag (pinned chats as m.favourite, archived chats as a configured
// tag) act only on the tag that changed, so that unrelated tags never unpin or unarchive a chat.
func TagChange(msg *MatrixRoomTag, tag event.RoomTag) *bool {
	if tag == "" {
		return nil
	}
	var has, had bool
	if msg.Content != nil {
		_, has = msg.Content.Tags[tag]
	}
	if msg.PrevContent != nil {
		_, had = msg.PrevContent.Tags[tag]
	}
	if has == had {
		return nil
	}
	return &has
}
