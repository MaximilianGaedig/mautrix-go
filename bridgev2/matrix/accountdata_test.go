// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package matrix

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/event"
)

func accountDataEvent(t *testing.T, raw string) *event.Event {
	t.Helper()
	var evt event.Event
	require.NoError(t, json.Unmarshal([]byte(raw), &evt))
	return &evt
}

func TestIsOwnAccountDataEcho(t *testing.T) {
	const dp = "mautrix-test"
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"marked unread by the bridge", `{"type":"m.marked_unread","sender":"@u:x","room_id":"!r:x",
			"content":{"unread":true,"fi.mau.double_puppet_source":"mautrix-test"}}`, true},
		{"marked unread by the user", `{"type":"m.marked_unread","sender":"@u:x","room_id":"!r:x",
			"content":{"unread":true}}`, false},
		{"favourite added by the bridge", `{"type":"m.tag","sender":"@u:x","room_id":"!r:x",
			"content":{"tags":{"m.favourite":{"fi.mau.double_puppet_source":"mautrix-test"}}},
			"unsigned":{"prev_content":{"tags":{}}}}`, true},
		{"favourite added by the user", `{"type":"m.tag","sender":"@u:x","room_id":"!r:x",
			"content":{"tags":{"m.favourite":{}}},"unsigned":{"prev_content":{"tags":{}}}}`, false},
		{"user adds a tag next to one the bridge set", `{"type":"m.tag","sender":"@u:x","room_id":"!r:x",
			"content":{"tags":{"m.favourite":{"fi.mau.double_puppet_source":"mautrix-test"},"m.lowpriority":{}}},
			"unsigned":{"prev_content":{"tags":{"m.favourite":{"fi.mau.double_puppet_source":"mautrix-test"}}}}}`, false},
		{"user removes a tag", `{"type":"m.tag","sender":"@u:x","room_id":"!r:x",
			"content":{"tags":{}},"unsigned":{"prev_content":{"tags":{"m.favourite":{}}}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isOwnAccountDataEcho(accountDataEvent(t, tc.raw), dp))
		})
	}
	assert.False(t, isOwnAccountDataEcho(accountDataEvent(t, `{"type":"m.marked_unread","content":{"unread":true}}`), ""),
		"without a double puppet value nothing is an echo")
}
