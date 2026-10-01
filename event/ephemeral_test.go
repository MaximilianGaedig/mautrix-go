// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package event_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func parseTyping(t *testing.T, content string) *event.TypingEventContent {
	t.Helper()
	evt := &event.Event{Type: event.EphemeralEventTyping, Content: event.Content{VeryRaw: json.RawMessage(content)}}
	require.NoError(t, evt.Content.ParseRaw(evt.Type))
	return evt.Content.AsTyping()
}

func TestTypingKinds(t *testing.T) {
	const ada, grace, linus = id.UserID("@ada:example.com"), id.UserID("@grace:example.com"), id.UserID("@linus:example.com")

	t.Run("a typing event says what each user is doing", func(t *testing.T) {
		content := parseTyping(t, `{
			"user_ids": ["@ada:example.com", "@grace:example.com"],
			"im.mxg.typing.kinds": {"@ada:example.com": "recording_voice"}
		}`)
		assert.Equal(t, []id.UserID{ada, grace}, content.UserIDs)
		assert.Equal(t, event.TypingKindRecordingVoice, content.KindOf(ada))
		assert.Equal(t, event.TypingKindText, content.KindOf(grace), "not listed is typing text")
	})
	t.Run("an event from a homeserver without kinds is all text", func(t *testing.T) {
		content := parseTyping(t, `{"user_ids": ["@ada:example.com"]}`)
		assert.Equal(t, []id.UserID{ada}, content.UserIDs)
		assert.Equal(t, event.TypingKindText, content.KindOf(ada))
	})
	t.Run("a kind nobody knows is typing text", func(t *testing.T) {
		content := parseTyping(t, `{
			"user_ids": ["@ada:example.com", "@grace:example.com", "@linus:example.com"],
			"im.mxg.typing.kinds": {
				"@ada:example.com": "playing_a_game",
				"@grace:example.com": 7,
				"@linus:example.com": "uploading_photo"
			}
		}`)
		assert.Equal(t, event.TypingKindText, content.KindOf(ada))
		assert.Equal(t, event.TypingKindText, content.KindOf(grace))
		assert.Equal(t, event.TypingKindUploadingPhoto, content.KindOf(linus), "the entries that make sense are kept")
	})
	t.Run("malformed kinds do not cost the typing notification", func(t *testing.T) {
		for _, kinds := range []string{`"recording_voice"`, `["@ada:example.com"]`, `null`, `3`} {
			content := parseTyping(t, `{"user_ids": ["@ada:example.com"], "im.mxg.typing.kinds": `+kinds+`}`)
			assert.Equal(t, []id.UserID{ada}, content.UserIDs, kinds)
			assert.Equal(t, event.TypingKindText, content.KindOf(ada), kinds)
		}
	})
	t.Run("plain typing is written as it always was", func(t *testing.T) {
		data, err := json.Marshal(&event.TypingEventContent{UserIDs: []id.UserID{ada}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"user_ids": ["@ada:example.com"]}`, string(data))
	})
	t.Run("every kind is known, and only those", func(t *testing.T) {
		for _, kind := range []event.TypingKind{
			event.TypingKindText, event.TypingKindRecordingVoice, event.TypingKindRecordingVideo,
			event.TypingKindUploadingPhoto, event.TypingKindUploadingVideo, event.TypingKindUploadingFile,
			event.TypingKindUploadingVoice, event.TypingKindChoosingSticker,
		} {
			assert.Equal(t, kind, kind.Known())
		}
		assert.Equal(t, event.TypingKindText, event.TypingKind("").Known())
		assert.Equal(t, event.TypingKindText, event.TypingKind("Recording_Voice").Known())
	})
}
