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

func ignoreList(users ...id.UserID) *event.IgnoredUserListEventContent {
	list := &event.IgnoredUserListEventContent{IgnoredUsers: map[id.UserID]event.IgnoredUser{}}
	for _, u := range users {
		list.IgnoredUsers[u] = event.IgnoredUser{}
	}
	return list
}

func TestIgnoreListChanges(t *testing.T) {
	ignored, unignored := ignoreListChanges(ignoreList("@a:x", "@c:x"), ignoreList("@a:x", "@b:x"))
	assert.Equal(t, []id.UserID{"@c:x"}, ignored)
	assert.Equal(t, []id.UserID{"@b:x"}, unignored)

	ignored, unignored = ignoreListChanges(ignoreList("@a:x"), nil)
	assert.Equal(t, []id.UserID{"@a:x"}, ignored, "the first list: everyone on it is newly ignored")
	assert.Empty(t, unignored)
}

func TestWithIgnored(t *testing.T) {
	list := ignoreList("@a:x")
	out, changed := withIgnored(list, "@b:x", true)
	assert.True(t, changed)
	assert.Contains(t, out.IgnoredUsers, id.UserID("@b:x"))
	assert.NotContains(t, list.IgnoredUsers, id.UserID("@b:x"), "the original list is left alone")

	_, changed = withIgnored(list, "@a:x", true)
	assert.False(t, changed, "already ignored")

	out, changed = withIgnored(list, "@a:x", false)
	assert.True(t, changed)
	assert.Empty(t, out.IgnoredUsers)

	out, changed = withIgnored(nil, "@a:x", true)
	assert.True(t, changed)
	assert.Contains(t, out.IgnoredUsers, id.UserID("@a:x"))
}
