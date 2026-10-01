// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// typingFromTransaction reads the typing event out of an appservice transaction the way the
// appservice does, with the user IDs sorted the way the portal sorts them.
func typingFromTransaction(t *testing.T, txn string) *event.TypingEventContent {
	t.Helper()
	var parsed appservice.Transaction
	require.NoError(t, json.Unmarshal([]byte(txn), &parsed))
	require.Len(t, parsed.EphemeralEvents, 1)
	evt := parsed.EphemeralEvents[0]
	evt.Type.Class = event.EphemeralEventType
	require.NoError(t, evt.Content.ParseRaw(evt.Type))
	content := evt.Content.AsTyping()
	slices.Sort(content.UserIDs)
	return content
}

func TestTypingChanges(t *testing.T) {
	const ada, grace = id.UserID("@ada:example.com"), id.UserID("@grace:example.com")
	type kinds = map[id.UserID]event.TypingKind

	t.Run("the kind comes out of the transaction's typing event", func(t *testing.T) {
		content := typingFromTransaction(t, `{"events": [], "ephemeral": [{
			"type": "m.typing",
			"room_id": "!room:example.com",
			"content": {
				"user_ids": ["@grace:example.com", "@ada:example.com"],
				"im.mxg.typing.kinds": {"@ada:example.com": "recording_voice", "@gone:example.com": "uploading_photo"}
			}
		}]}`)
		stopped, started, now := typingChanges(nil, nil, content)
		assert.Empty(t, stopped)
		assert.Equal(t, []id.UserID{ada, grace}, started)
		assert.Equal(t, kinds{ada: event.TypingKindRecordingVoice}, now, "only typing users, and not those typing text")
		assert.Equal(t, TypingTypeRecordingMedia, TypingTypeOfKind(typingKindOf(now, ada)))
		assert.Equal(t, TypingTypeText, TypingTypeOfKind(typingKindOf(now, grace)))
	})
	t.Run("a homeserver without kinds is plain typing", func(t *testing.T) {
		content := typingFromTransaction(t, `{"ephemeral": [{
			"type": "m.typing", "room_id": "!room:example.com", "content": {"user_ids": ["@ada:example.com"]}
		}]}`)
		stopped, started, now := typingChanges(nil, nil, content)
		assert.Empty(t, stopped)
		assert.Equal(t, []id.UserID{ada}, started)
		assert.Empty(t, now)
		assert.Equal(t, event.TypingKindText, typingKindOf(now, ada))
	})
	t.Run("going from typing to recording is sent to the network again", func(t *testing.T) {
		content := &event.TypingEventContent{
			UserIDs: []id.UserID{ada, grace},
			Kinds:   event.TypingKinds{ada: event.TypingKindRecordingVoice},
		}
		stopped, started, now := typingChanges([]id.UserID{ada, grace}, nil, content)
		assert.Empty(t, stopped)
		assert.Equal(t, []id.UserID{ada}, started, "grace kept typing text")
		assert.Equal(t, kinds{ada: event.TypingKindRecordingVoice}, now)
	})
	t.Run("and so is going back to typing text", func(t *testing.T) {
		content := &event.TypingEventContent{UserIDs: []id.UserID{ada, grace}}
		stopped, started, now := typingChanges([]id.UserID{ada, grace}, kinds{ada: event.TypingKindRecordingVoice}, content)
		assert.Empty(t, stopped)
		assert.Equal(t, []id.UserID{ada}, started)
		assert.Empty(t, now)
	})
	t.Run("the same kind again changes nothing", func(t *testing.T) {
		content := &event.TypingEventContent{
			UserIDs: []id.UserID{ada},
			Kinds:   event.TypingKinds{ada: event.TypingKindUploadingPhoto},
		}
		stopped, started, now := typingChanges([]id.UserID{ada}, kinds{ada: event.TypingKindUploadingPhoto}, content)
		assert.Empty(t, stopped)
		assert.Empty(t, started)
		assert.Equal(t, kinds{ada: event.TypingKindUploadingPhoto}, now)
	})
	t.Run("stopping forgets the kind", func(t *testing.T) {
		content := &event.TypingEventContent{UserIDs: []id.UserID{grace}}
		stopped, started, now := typingChanges([]id.UserID{ada}, kinds{ada: event.TypingKindRecordingVoice}, content)
		assert.Equal(t, []id.UserID{ada}, stopped)
		assert.Equal(t, []id.UserID{grace}, started)
		assert.Empty(t, now)
	})
}

func TestTypingTypeOfKind(t *testing.T) {
	for kind, want := range map[event.TypingKind]TypingType{
		event.TypingKindText:            TypingTypeText,
		event.TypingKindRecordingVoice:  TypingTypeRecordingMedia,
		event.TypingKindRecordingVideo:  TypingTypeRecordingMedia,
		event.TypingKindUploadingPhoto:  TypingTypeUploadingMedia,
		event.TypingKindUploadingVideo:  TypingTypeUploadingMedia,
		event.TypingKindUploadingFile:   TypingTypeUploadingMedia,
		event.TypingKindUploadingVoice:  TypingTypeUploadingMedia,
		event.TypingKindChoosingSticker: TypingTypeText,
		"playing_a_game":                TypingTypeText,
		"":                              TypingTypeText,
	} {
		assert.Equal(t, want, TypingTypeOfKind(kind), kind)
	}
	// A typing type's own kind maps back to it, so a connector that only knows the three types gets
	// from Matrix what it would have sent to Matrix.
	for _, typingType := range []TypingType{TypingTypeText, TypingTypeRecordingMedia, TypingTypeUploadingMedia} {
		assert.Equal(t, typingType, TypingTypeOfKind(typingType.Kind()))
	}
}

type remoteTyping struct {
	RemoteEvent
	typingType TypingType
	kind       event.TypingKind
}

func (rt *remoteTyping) GetTimeout() time.Duration       { return 5 * time.Second }
func (rt *remoteTyping) GetTypingType() TypingType       { return rt.typingType }
func (rt *remoteTyping) GetTypingKind() event.TypingKind { return rt.kind }

type remoteTypingWithoutType struct {
	RemoteEvent
}

func (rt *remoteTypingWithoutType) GetTimeout() time.Duration { return 5 * time.Second }

func TestRemoteTypingKind(t *testing.T) {
	assert.Equal(t, event.TypingKindText, remoteTypingKind(&remoteTypingWithoutType{}))
	assert.Equal(t, event.TypingKindRecordingVoice, remoteTypingKind(&remoteTyping{typingType: TypingTypeRecordingMedia}),
		"a connector that gives no kind is asked for its typing type")
	assert.Equal(t, event.TypingKindUploadingPhoto,
		remoteTypingKind(&remoteTyping{typingType: TypingTypeUploadingMedia, kind: event.TypingKindUploadingPhoto}))
	assert.Equal(t, event.TypingKindChoosingSticker, remoteTypingKind(&remoteTyping{kind: event.TypingKindChoosingSticker}))
	assert.Equal(t, event.TypingKindUploadingFile,
		remoteTypingKind(&remoteTyping{typingType: TypingTypeUploadingMedia, kind: "uploading_hologram"}),
		"a kind outside the vocabulary falls back to the typing type")
}
