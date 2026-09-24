// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package matrix

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/provisionutil"
	"maunium.net/go/mautrix/id"
)

/*
 * Letting the homeserver search the network on a client's behalf.
 *
 * A bridged person is unknown to the homeserver until they have a ghost, which happens the first time
 * somebody talks to them. So the user directory - which is what every client's search uses - can only offer
 * the people the user has already spoken to, and a contact they have never messaged is missing from the one
 * place they would look for them.
 *
 * The network can be searched, and this bridge already does it (SearchUsers, per network). This is that
 * search offered to the homeserver, so the homeserver can fold the answers into /user_directory/search and
 * every client gets it - Element, Element X, anything - with no client-side support at all. Answering creates
 * the ghosts, so what the client receives is ordinary Matrix users it can open a chat with.
 *
 * Searched as the user who is searching, never as anybody else: the request names them, and only their own
 * logins are used.
 */

// userDirectorySearchPath is unstable and matched by tuwunel's appservice service. The spec's third-party
// lookup matches protocol-defined fields ("who is this exact handle?") rather than searching by name, so
// there is nothing spec'd to implement here yet; the prefix leaves room for there to be.
const userDirectorySearchPath = "POST /_matrix/app/unstable/im.mxg.user_directory_search"

type reqUserDirectorySearch struct {
	SearchTerm string    `json:"search_term"`
	Limit      int       `json:"limit"`
	UserID     id.UserID `json:"user_id"`
}

// respUserDirectorySearch is shaped like the client-server response so the homeserver can merge the two
// without translating anything.
type respUserDirectorySearch struct {
	Results []userDirectoryResult `json:"results"`
	Limited bool                  `json:"limited"`
}

type userDirectoryResult struct {
	UserID      id.UserID           `json:"user_id"`
	DisplayName string              `json:"display_name,omitempty"`
	AvatarURL   id.ContentURIString `json:"avatar_url,omitempty"`
	// The line the network uses to tell people with the same name apart: mutual friends, a location,
	// a username. A ghost's Matrix ID says nothing to the reader, so without this three people called
	// Max Müller are three identical rows and the reader has to guess.
	Context string `json:"im.mxg.context,omitempty"`
}

func (br *Connector) registerUserDirectorySearch() {
	br.AS.Router.HandleFunc(userDirectorySearchPath, br.PostUserDirectorySearch)
}

// PostUserDirectorySearch answers who on this network matches, for the user the homeserver names.
func (br *Connector) PostUserDirectorySearch(w http.ResponseWriter, r *http.Request) {
	if !br.AS.CheckServerToken(w, r) {
		return
	}
	var req reqUserDirectorySearch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mautrix.MNotJSON.WithMessage("Failed to decode request body").Write(w)
		return
	}
	if req.SearchTerm == "" || req.UserID == "" {
		mautrix.MInvalidParam.WithMessage("Missing search_term or user_id").Write(w)
		return
	}
	log := br.Log.With().
		Str("action", "user directory search").
		Stringer("searching_user", req.UserID).
		Logger()
	ctx := log.WithContext(r.Context())

	user, err := br.Bridge.GetExistingUserByMXID(ctx, req.UserID)
	if err != nil {
		log.Err(err).Msg("Failed to get the searching user")
		mautrix.MUnknown.WithMessage("Failed to get user").Write(w)
		return
	}
	// Somebody with no account on this network is not an error: this bridge simply has nothing to say.
	if user == nil {
		exhttp.WriteJSONResponse(w, http.StatusOK, &respUserDirectorySearch{Results: []userDirectoryResult{}})
		return
	}

	results := make([]userDirectoryResult, 0)
	for _, login := range user.GetUserLogins() {
		found, err := provisionutil.SearchUsers(ctx, login, req.SearchTerm)
		if err != nil {
			// A login that cannot be searched (logged out, or a network without search) is skipped, so the
			// user's other accounts still answer.
			zerolog.Ctx(ctx).Debug().Err(err).
				Str("login_id", string(login.ID)).
				Msg("Could not search this login's network")
			continue
		}
		for _, one := range found.Results {
			// Without a ghost there is no Matrix user to offer, and the search result would be a dead end.
			if one.MXID == "" {
				continue
			}
			results = append(results, userDirectoryResult{
				UserID:      one.MXID,
				DisplayName: one.Name,
				AvatarURL:   one.AvatarURL,
				Context:     one.Context,
			})
			if req.Limit > 0 && len(results) >= req.Limit {
				exhttp.WriteJSONResponse(w, http.StatusOK, &respUserDirectorySearch{
					Results: results,
					Limited: true,
				})
				return
			}
		}
	}
	exhttp.WriteJSONResponse(w, http.StatusOK, &respUserDirectorySearch{Results: results})
}
