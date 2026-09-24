// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

// BridgeLoginEventType is the state event, in the user's management room (one per login, the login ID as
// state key), in which the bridge says whether it is connected to the network as that account, so a client
// can show it (and what to do when it isn't) without reading the bridge's chat notices.
var BridgeLoginEventType = event.Type{Type: "im.mxg.bridge_login", Class: event.StateEventType}

// BridgeLoginContent is the content of the [BridgeLoginEventType] state event.
type BridgeLoginContent struct {
	// The bridge state: CONNECTED, CONNECTING, TRANSIENT_DISCONNECT, BAD_CREDENTIALS, UNKNOWN_ERROR, …
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
	// Who the login is on the network.
	RemoteID   string `json:"remote_id,omitempty"`
	RemoteName string `json:"remote_name,omitempty"`
	// The network, and what to send in the management room to log in again.
	Network       string `json:"network"`
	CommandPrefix string `json:"command_prefix"`
	UpdatedTS     int64  `json:"updated_ts"`
	// Where the bridge's provisioning API is, so a client can ask this bridge things the homeserver cannot
	// answer - above all "who on this network is called X?", for people who have never been bridged and so
	// have no ghost in the user directory. Only sent when a client can actually authenticate against it with
	// its own Matrix access token (provisioning.allow_matrix_auth); the shared secret is never published.
	ProvisioningURL string `json:"provisioning_url,omitempty"`
	// What that API can do for this login, so a client asks only for what exists.
	Can *BridgeLoginCapabilities `json:"can,omitempty"`
}

// BridgeLoginCapabilities says which optional provisioning endpoints this login's network supports.
type BridgeLoginCapabilities struct {
	// The network can be searched for users by name (POST /v3/search_users).
	SearchUsers bool `json:"search_users,omitempty"`
	// An identifier can be turned into a chat (POST /v3/create_dm/{identifier}).
	CreateDM bool `json:"create_dm,omitempty"`
}

// ProvisioningReachable is implemented by Matrix connectors whose provisioning API a client can reach and
// authenticate against on its own. Optional: without it, or with an empty URL, nothing is published and a
// client simply has one fewer thing it can ask the bridge.
type ProvisioningReachable interface {
	ProvisioningPublicURL() string
}

// provisioningReach is where to reach this bridge's provisioning API and what it can do for this login.
func (br *Bridge) provisioningReach(login *UserLogin) (string, *BridgeLoginCapabilities) {
	reachable, ok := br.Matrix.(ProvisioningReachable)
	if !ok {
		return "", nil
	}
	url := reachable.ProvisioningPublicURL()
	if url == "" {
		return "", nil
	}
	can := &BridgeLoginCapabilities{}
	_, can.SearchUsers = login.Client.(UserSearchingNetworkAPI)
	_, can.CreateDM = login.Client.(IdentifierResolvingNetworkAPI)
	return url, can
}

// sendFinalLoginState records a login's last state before its queue is torn down. Send() only queues,
// and deleting a login closes that queue straight after, so the state most worth having - that the
// bridge is logged out - is the one most likely to be dropped.
func (bsq *BridgeStateQueue) sendFinalLoginState(ctx context.Context, state status.BridgeState) {
	if bsq == nil {
		return
	}
	bsq.publishLoginState(ctx, state.Fill(bsq.login))
}

// publishLoginState records the login's connection state in the user's management room.
func (bsq *BridgeStateQueue) publishLoginState(ctx context.Context, state status.BridgeState) {
	login := bsq.login
	room, err := login.User.GetManagementRoom(ctx)
	if err != nil || room == "" {
		return
	}
	content := &BridgeLoginContent{
		State:         string(state.StateEvent),
		Error:         string(state.Error),
		Message:       state.Message,
		RemoteID:      string(state.RemoteID),
		RemoteName:    state.RemoteName,
		Network:       bsq.bridge.Network.GetName().DisplayName,
		CommandPrefix: bsq.bridge.Config.CommandPrefix,
		UpdatedTS:     time.Now().UnixMilli(),
	}
	if content.RemoteID == "" {
		content.RemoteID = string(login.ID)
	}
	if content.RemoteName == "" {
		content.RemoteName = login.RemoteName
	}
	content.ProvisioningURL, content.Can = bsq.bridge.provisioningReach(login)
	_, err = bsq.bridge.Bot.SendState(ctx, room, BridgeLoginEventType, string(login.ID), &event.Content{Parsed: content}, time.Time{})
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to publish the login's connection state")
	}
}
