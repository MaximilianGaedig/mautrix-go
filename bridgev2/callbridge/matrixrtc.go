// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package callbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

/*
 * Joining a Matrix room's own call, from the bridge's side.
 *
 * Everything here is Matrix, not network: finding the homeserver's LiveKit focus, getting a token
 * for it as some identity, and publishing the membership that makes that identity visible to the
 * other participants. A bridge needs all of it before RTCLeg has anywhere to connect to, and none
 * of it has anything to do with which network is on the other side - which is why it belongs beside
 * RTCLeg rather than in each connector that wants a group call.
 */

const (
	// RTCMemberExpiry bounds a membership that was never cleared, after a crash. Long calls renew
	// theirs before it runs out.
	RTCMemberExpiry = time.Hour
)

// CallMemberEventType is the call membership state event (MSC4143 / MSC3401).
var CallMemberEventType = event.Type{Type: "org.matrix.msc3401.call.member", Class: event.StateEventType}

// RTCTransport is a MatrixRTC focus (MSC4143). For LiveKit it is the lk-jwt-service that issues
// tokens.
type RTCTransport struct {
	Type              string `json:"type"`
	LivekitServiceURL string `json:"livekit_service_url"`
}

var rtcHTTP = &http.Client{Timeout: 10 * time.Second}

// RTCFocus caches the homeserver's focus, which does not change between calls.
//
// Failures are cached too, for a minute: discovery reaches the network twice, and a homeserver that
// advertises no focus would otherwise be asked again on every single call attempt.
type RTCFocus struct {
	lock      sync.Mutex
	transport *RTCTransport
	tried     time.Time
}

// Get returns the homeserver's LiveKit focus, discovering it the first time.
func (f *RTCFocus) Get(ctx context.Context, cli *mautrix.Client, serverName string) (*RTCTransport, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.transport != nil {
		return f.transport, nil
	}
	if time.Since(f.tried) < time.Minute {
		return nil, errors.New("no LiveKit focus (discovery failed recently)")
	}
	f.tried = time.Now()
	transport, err := DiscoverRTCTransport(ctx, cli, serverName)
	if err != nil {
		return nil, err
	}
	f.transport = transport
	return transport, nil
}

// DiscoverRTCTransport finds the homeserver's LiveKit focus.
//
// Two places are tried, in the order a client would: the MSC4143 rtc/transports endpoint, then
// .well-known's org.matrix.msc4143.rtc_foci, which is what Element X actually reads. A homeserver
// that has been set up for Element Call will answer one of them.
func DiscoverRTCTransport(ctx context.Context, cli *mautrix.Client, serverName string) (*RTCTransport, error) {
	var transports struct {
		RTCTransports []RTCTransport `json:"rtc_transports"`
	}
	url := cli.BuildClientURL("unstable", "org.matrix.msc4143", "rtc", "transports")
	if _, err := cli.MakeRequest(ctx, http.MethodGet, url, nil, &transports); err == nil {
		if t := FirstLiveKit(transports.RTCTransports); t != nil {
			return t, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+serverName+"/.well-known/matrix/client", nil)
	if err != nil {
		return nil, err
	}
	resp, err := rtcHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch well-known: %w", err)
	}
	defer resp.Body.Close()
	var wk struct {
		RTCFoci []RTCTransport `json:"org.matrix.msc4143.rtc_foci"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wk); err != nil {
		return nil, fmt.Errorf("parse well-known: %w", err)
	}
	if t := FirstLiveKit(wk.RTCFoci); t != nil {
		return t, nil
	}
	return nil, errors.New("the homeserver advertises no LiveKit focus")
}

// FirstLiveKit picks the first usable LiveKit focus out of a list.
func FirstLiveKit(ts []RTCTransport) *RTCTransport {
	for i := range ts {
		if ts[i].Type == "livekit" && ts[i].LivekitServiceURL != "" {
			return &ts[i]
		}
	}
	return nil
}

// LiveKitToken asks the focus's lk-jwt-service for a token to a room's call, as cli's user.
//
// The identity is proven with an OpenID token rather than an access token: lk-jwt-service is not a
// Matrix client and has no business holding one, and the OpenID token is scoped to proving who the
// user is and nothing else. The room that comes back is the same LiveKit room Element X joins for
// this Matrix room, which is what puts the bridge in the call rather than beside it.
func LiveKitToken(ctx context.Context, cli *mautrix.Client, focus *RTCTransport, roomID id.RoomID, deviceID string) (url, token string, err error) {
	oid, err := cli.RequestOpenIDToken(ctx)
	if err != nil {
		return "", "", fmt.Errorf("openid token: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"room":         roomID,
		"openid_token": oid,
		"device_id":    deviceID,
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(focus.LivekitServiceURL, "/")+"/sfu/get", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rtcHTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("lk-jwt-service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("lk-jwt-service: HTTP %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		URL string `json:"url"`
		JWT string `json:"jwt"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("parse lk-jwt-service response: %w", err)
	}
	if out.URL == "" || out.JWT == "" {
		return "", "", errors.New("lk-jwt-service returned no url/jwt")
	}
	return out.URL, out.JWT, nil
}

// RTCStateKey is the per-device membership state key.
//
// The leading underscore is what makes it acceptable in a room without MSC3757 owned state keys: a
// key starting with a user ID is reserved for that user, and prefixing it takes it out of that
// namespace so the bridge can write one on a ghost's behalf.
func RTCStateKey(userID id.UserID, deviceID string) string {
	return "_" + string(userID) + "_" + deviceID + "_m.call"
}

// RTCMemberContent is the call membership, in the shape Element Call sends in Compatibility mode,
// which is what Element X reads.
func RTCMemberContent(focus *RTCTransport, roomID id.RoomID, deviceID, userID string, video bool, createdTS int64) map[string]any {
	intent := "audio"
	if video {
		intent = "video"
	}
	content := map[string]any{
		"application":   "m.call",
		"call_id":       "",
		"scope":         "m.room",
		"device_id":     deviceID,
		"membershipID":  userID + ":" + deviceID,
		"expires":       RTCMemberExpiry.Milliseconds(),
		"m.call.intent": intent,
		"focus_active":  map[string]any{"type": "livekit", "focus_selection": "multi_sfu"},
		"foci_preferred": []any{map[string]any{
			"type":                "livekit",
			"livekit_service_url": focus.LivekitServiceURL,
			"livekit_alias":       roomID,
		}},
	}
	if createdTS != 0 {
		content["created_ts"] = createdTS
	}
	return content
}

// RTCMembership is what a bridge needs out of somebody else's call.member event.
type RTCMembership struct {
	// Joined is false for the empty content that means someone left.
	Joined   bool
	DeviceID string
	Video    bool
}

// ParseRTCMembership reads a call.member event.
//
// A membership is left by *clearing* the content rather than by sending anything that says "left",
// so an event with no application is a departure and not a malformed join.
func ParseRTCMembership(content map[string]any) RTCMembership {
	if len(content) == 0 {
		return RTCMembership{}
	}
	application, _ := content["application"].(string)
	if application != "m.call" {
		return RTCMembership{}
	}
	deviceID, _ := content["device_id"].(string)
	intent, _ := content["m.call.intent"].(string)
	return RTCMembership{Joined: true, DeviceID: deviceID, Video: intent == "video"}
}
