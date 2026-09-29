// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package callbridge

import (
	"strings"
	"testing"

	"maunium.net/go/mautrix/id"
)

func TestFirstLiveKitSkipsWhatItCannotUse(t *testing.T) {
	// A homeserver may advertise several foci, including types this cannot talk to and entries with
	// no URL at all. Picking the first of the list rather than the first usable one is the easy bug.
	got := FirstLiveKit([]RTCTransport{
		{Type: "jitsi", LivekitServiceURL: "https://jitsi.example"},
		{Type: "livekit", LivekitServiceURL: ""},
		{Type: "livekit", LivekitServiceURL: "https://lk.example"},
	})
	if got == nil {
		t.Fatal("no focus picked from a list containing a usable one")
	}
	if got.LivekitServiceURL != "https://lk.example" {
		t.Errorf("picked %q", got.LivekitServiceURL)
	}
	if FirstLiveKit(nil) != nil {
		t.Error("an empty list produced a focus")
	}
}

func TestStateKeyIsTakenOutOfTheUsersNamespace(t *testing.T) {
	/*
	 * The leading underscore is load-bearing.
	 *
	 * A state key beginning with a user ID is reserved for that user, so without the prefix the
	 * bridge could not write one on a ghost's behalf in a room that enforces MSC3757 owned state
	 * keys - the homeserver would reject it.
	 */
	got := RTCStateKey(id.UserID("@discord_123:example.com"), "DISCORDBRIDGE")
	if !strings.HasPrefix(got, "_") {
		t.Errorf("state key %q does not start with the underscore that takes it out of the owned namespace", got)
	}
	if !strings.Contains(got, "DISCORDBRIDGE") {
		t.Errorf("state key %q does not identify the device, so two devices would share one membership", got)
	}
	// Two devices of the same user must not collide, or one would clear the other's membership.
	other := RTCStateKey(id.UserID("@discord_123:example.com"), "OTHER")
	if got == other {
		t.Error("two devices produced the same state key")
	}
}

func TestMemberContentSaysWhereToFindTheCall(t *testing.T) {
	focus := &RTCTransport{Type: "livekit", LivekitServiceURL: "https://lk.example"}
	room := id.RoomID("!room:example.com")
	content := RTCMemberContent(focus, room, "DISCORDBRIDGE", "@discord_123:example.com", false, 0)

	if content["application"] != "m.call" {
		t.Errorf("application is %v, want m.call", content["application"])
	}
	if content["m.call.intent"] != "audio" {
		t.Errorf("intent is %v, want audio", content["m.call.intent"])
	}
	// The preferred focus is how other participants know which LiveKit room this membership is in;
	// an alias that is not the room ID puts the bridge in a different call from everyone else.
	foci, ok := content["foci_preferred"].([]any)
	if !ok || len(foci) != 1 {
		t.Fatalf("foci_preferred is %v", content["foci_preferred"])
	}
	entry := foci[0].(map[string]any)
	if entry["livekit_alias"] != room {
		t.Errorf("livekit_alias is %v, want the room ID %v", entry["livekit_alias"], room)
	}
	if entry["livekit_service_url"] != "https://lk.example" {
		t.Errorf("livekit_service_url is %v", entry["livekit_service_url"])
	}
	// created_ts is omitted rather than sent as zero: a zero timestamp is read as an actual time.
	if _, present := content["created_ts"]; present {
		t.Error("created_ts was sent even though there is none yet")
	}
	if withTS := RTCMemberContent(focus, room, "D", "@u:example.com", true, 1234); withTS["created_ts"] != int64(1234) {
		t.Errorf("created_ts is %v, want 1234", withTS["created_ts"])
	}
	if withTS := RTCMemberContent(focus, room, "D", "@u:example.com", true, 1234); withTS["m.call.intent"] != "video" {
		t.Error("a video membership did not say so")
	}
}

func TestLeavingIsAnEmptyMembership(t *testing.T) {
	/*
	 * A participant leaves by clearing the content, not by sending anything that says "left".
	 *
	 * So empty content has to be read as a departure. Treating it as a malformed join would leave
	 * the bridge relaying to somebody who is no longer in the call.
	 */
	if got := ParseRTCMembership(map[string]any{}); got.Joined {
		t.Error("empty content was read as a join")
	}
	if got := ParseRTCMembership(nil); got.Joined {
		t.Error("nil content was read as a join")
	}
}

func TestMembershipIsReadBack(t *testing.T) {
	focus := &RTCTransport{Type: "livekit", LivekitServiceURL: "https://lk.example"}
	// What we send has to be what we can read: these two are each other's inverse, and a change to
	// one that is not made to the other would be invisible until two bridges talked to each other.
	sent := RTCMemberContent(focus, id.RoomID("!r:example.com"), "DEV", "@u:example.com", true, 0)
	got := ParseRTCMembership(sent)
	if !got.Joined {
		t.Fatal("our own membership did not read back as a join")
	}
	if got.DeviceID != "DEV" {
		t.Errorf("device is %q, want DEV", got.DeviceID)
	}
	if !got.Video {
		t.Error("a video membership read back as audio")
	}
}

func TestSomeoneElsesApplicationIsNotOurCall(t *testing.T) {
	// The same state event type carries other applications; treating one as a call member would put
	// the bridge into something that is not a call.
	got := ParseRTCMembership(map[string]any{"application": "m.something", "device_id": "DEV"})
	if got.Joined {
		t.Error("a non-call application was read as a call membership")
	}
}
