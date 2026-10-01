// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package matrix

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var _ bridgev2.TypingKindMatrixAPI = (*ASIntent)(nil)

// A network that tells a photo upload from a file upload gets to say so on Matrix, beyond what the
// three typing types can.
func TestMarkTypingKind(t *testing.T) {
	hs, as := typingGhost(t)
	ghost := &ASIntent{Matrix: as.Intent(id.NewUserID("ghost", "example.com"))}
	ctx := context.Background()
	const roomID = id.RoomID("!room:example.com")

	for _, kind := range []event.TypingKind{
		event.TypingKindRecordingVoice, event.TypingKindRecordingVideo,
		event.TypingKindUploadingPhoto, event.TypingKindUploadingVideo, event.TypingKindUploadingFile,
		event.TypingKindUploadingVoice, event.TypingKindChoosingSticker,
	} {
		require.NoError(t, ghost.MarkTypingKind(ctx, roomID, kind, 5*time.Second))
		assert.Equal(t, []typingRequest{{Typing: true, Timeout: 5000, Kind: string(kind), HasKind: true}}, hs.take(), kind)
	}

	require.NoError(t, ghost.MarkTypingKind(ctx, roomID, event.TypingKindText, 5*time.Second))
	assert.Equal(t, []typingRequest{{Typing: true, Timeout: 5000}}, hs.take(), "text is the default and isn't written")

	require.NoError(t, ghost.MarkTypingKind(ctx, roomID, "playing_a_game", 5*time.Second))
	assert.Equal(t, []typingRequest{{Typing: true, Timeout: 5000}}, hs.take(), "an unknown kind is plain typing")

	require.NoError(t, ghost.MarkTypingKind(ctx, roomID, event.TypingKindRecordingVoice, 0))
	assert.Equal(t, []typingRequest{{Typing: false}}, hs.take(), "stopped")
}
