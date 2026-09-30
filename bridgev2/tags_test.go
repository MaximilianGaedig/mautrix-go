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
)

func tagMsg(now, prev *event.TagEventContent) *MatrixRoomTag {
	return &MatrixRoomTag{MatrixEventBase: MatrixEventBase[*event.TagEventContent]{Content: now}, PrevContent: prev}
}

func tags(names ...event.RoomTag) *event.TagEventContent {
	c := &event.TagEventContent{Tags: event.Tags{}}
	for _, n := range names {
		c.Tags[n] = event.TagMetadata{}
	}
	return c
}

func TestTagChange(t *testing.T) {
	archive := event.RoomTag("u.archive")
	added := TagChange(tagMsg(tags(event.RoomTagFavourite), tags()), event.RoomTagFavourite)
	if assert.NotNil(t, added) {
		assert.True(t, *added)
	}
	removed := TagChange(tagMsg(tags(), tags(event.RoomTagFavourite)), event.RoomTagFavourite)
	if assert.NotNil(t, removed) {
		assert.False(t, *removed)
	}
	assert.Nil(t, TagChange(tagMsg(tags(event.RoomTagFavourite, archive), tags(event.RoomTagFavourite)), event.RoomTagFavourite),
		"archiving doesn't touch the pin")
	assert.Nil(t, TagChange(tagMsg(tags(archive), tags()), ""), "an unconfigured tag is never bridged")
	first := TagChange(tagMsg(tags(archive), nil), archive)
	if assert.NotNil(t, first) {
		assert.True(t, *first, "no previous tags: everything is new")
	}
}
