// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"
)

// typingRequests is a homeserver that only records the typing requests it gets.
type typingRequests struct {
	lock sync.Mutex
	got  []mautrix.ReqTyping
}

func (tr *typingRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/typing/") {
		var req mautrix.ReqTyping
		_ = json.NewDecoder(r.Body).Decode(&req)
		tr.lock.Lock()
		tr.got = append(tr.got, req)
		tr.lock.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

func (tr *typingRequests) take() []mautrix.ReqTyping {
	tr.lock.Lock()
	defer tr.lock.Unlock()
	got := tr.got
	tr.got = nil
	return got
}

// A contact recording a voice message or uploading a file is busy writing to us just as one typing
// text is. Matrix's typing notification has no kind to say which, so all of them show as typing:
// the other kinds used to be dropped and showed as nothing at all.
func TestMarkTypingOfEveryKind(t *testing.T) {
	hs := &typingRequests{}
	server := httptest.NewServer(hs)
	defer server.Close()
	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     &appservice.Registration{AppToken: "token", SenderLocalpart: "bot"},
		HomeserverDomain: "example.com",
		HomeserverURL:    server.URL,
	})
	require.NoError(t, err)
	ghost := &ASIntent{Matrix: as.Intent(id.NewUserID("ghost", "example.com"))}
	ctx := context.Background()
	const roomID = id.RoomID("!room:example.com")

	for _, tc := range []struct {
		name string
		kind bridgev2.TypingType
	}{
		{"text", bridgev2.TypingTypeText},
		{"recording media", bridgev2.TypingTypeRecordingMedia},
		{"uploading media", bridgev2.TypingTypeUploadingMedia},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, ghost.MarkTyping(ctx, roomID, tc.kind, 5*time.Second))
			assert.Equal(t, []mautrix.ReqTyping{{Typing: true, Timeout: 5000}}, hs.take(), "started")
			require.NoError(t, ghost.MarkTyping(ctx, roomID, tc.kind, 0))
			assert.Equal(t, []mautrix.ReqTyping{{Typing: false}}, hs.take(), "stopped")
		})
	}

	// The user's own typing on the network still isn't sent as theirs on Matrix, whatever its kind.
	puppet := &ASIntent{Matrix: as.Intent(id.NewUserID("user", "example.com"))}
	puppet.Matrix.IsCustomPuppet = true
	require.NoError(t, puppet.MarkTyping(ctx, roomID, bridgev2.TypingTypeRecordingMedia, 5*time.Second))
	assert.Empty(t, hs.take(), "double puppeted typing")
}
