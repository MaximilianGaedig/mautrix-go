// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package mautrix

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/id"
)

func TestProfilePropagation(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	client.UserID = "@ghost:example.org"
	avatar := id.MustParseContentURI("mxc://example.org/abc")
	const base = "/_matrix/client/v3/profile/@ghost:example.org/"

	t.Run("nothing is added unless asked", func(t *testing.T) {
		queries = nil
		require.NoError(t, client.SetDisplayName(context.Background(), "Ghost"))
		require.NoError(t, client.SetAvatarURL(context.Background(), avatar))
		assert.Equal(t, []string{base + "displayname?", base + "avatar_url?"}, queries)
	})
	t.Run("the mode asked for goes with the name and the avatar", func(t *testing.T) {
		queries = nil
		ctx := WithProfilePropagation(context.Background(), ProfilePropagationUnchanged)
		require.NoError(t, client.SetDisplayName(ctx, "Ghost"))
		require.NoError(t, client.SetAvatarURL(ctx, avatar))
		const query = "?computer.gingershaped.msc4466.propagate_to=unchanged"
		assert.Equal(t, []string{base + "displayname" + query, base + "avatar_url" + query}, queries)
	})
}
