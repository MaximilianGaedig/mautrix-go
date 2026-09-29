// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"encoding/json"
	"testing"
)

func settings() *BridgeSettingsContent {
	return &BridgeSettingsContent{
		Source: BridgeSettingsSource{ID: "telegram", Name: "Telegram"},
		Settings: []BridgeSettingControl{
			{Key: SettingKeyBackfill, Label: "Import older messages", Type: SettingTypeAction},
			{Key: SettingKeyRelay, Label: "Relay", Type: SettingTypeBoolean, Value: false},
			{Key: "media_quality", Label: "Media quality", Type: SettingTypeEnum, Value: "original", Options: []BridgeSettingOption{
				{Value: "original", Label: "Original"},
				{Value: "compressed", Label: "Compressed"},
			}},
			{Key: "history_days", Label: "Days to import", Type: SettingTypeNumber, Value: float64(30)},
			{Key: "nickname", Label: "Nickname", Type: SettingTypeText, Value: "me"},
		},
	}
}

func set(key string, value any) *BridgeSettingsSetContent {
	return &BridgeSettingsSetContent{MsgType: BridgeSettingsSetMsgType, Key: key, Value: value}
}

func TestValidSetAcceptsWhatWasOffered(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *BridgeSettingsSetContent
	}{
		{"an action with no value", set(SettingKeyBackfill, nil)},
		{"a boolean", set(SettingKeyRelay, true)},
		{"an offered enum option", set("media_quality", "compressed")},
		{"a number", set("history_days", float64(7))},
		{"text", set("nickname", "somebody")},
	} {
		if _, ok := settings().ValidSet(tc.req); !ok {
			t.Errorf("%s was refused", tc.name)
		}
	}
}

func TestValidSetRefusesWhatWasNot(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *BridgeSettingsSetContent
	}{
		// A control that was never declared was never offered. Acting on it would mean acting on
		// something no client was told it could ask for.
		{"a key that does not exist", set("wipe_everything", true)},
		// An enum's options are the whole list; anything else is not a choice the bridge published.
		{"an enum value that is not an option", set("media_quality", "potato")},
		// The type is part of the offer: a client sending the wrong one is confused, and guessing
		// what it meant is how a boolean ends up set from the string "false".
		{"a string for a boolean", set(SettingKeyRelay, "true")},
		{"a boolean for a number", set("history_days", true)},
		{"a number for text", set("nickname", float64(3))},
		{"a value on an action", set(SettingKeyBackfill, true)},
	} {
		if _, ok := settings().ValidSet(tc.req); ok {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

func TestValidSetRefusesADisabledControl(t *testing.T) {
	// Disabled is a withdrawn offer, not a hint. A bridge that acted on it would start a history
	// import for a login it has just said is not connected.
	content := settings()
	content.Control(SettingKeyBackfill).DisabledReason = "Not connected to Telegram"
	control, ok := content.ValidSet(set(SettingKeyBackfill, nil))
	if ok {
		t.Error("a disabled control was accepted")
	}
	// Still returned, so a bridge can say which control it refused and why.
	if control == nil || control.DisabledReason == "" {
		t.Error("the refused control was not reported back")
	}
}

func TestSetRequestSurvivesJSON(t *testing.T) {
	// The value arrives as JSON, so the types ValidSet checks are JSON's: a number is a float64
	// whatever the client meant by it. Round-tripping is what proves the checks match reality.
	raw, err := json.Marshal(set("history_days", 7))
	if err != nil {
		t.Fatal(err)
	}
	var back BridgeSettingsSetContent
	if err = json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings().ValidSet(&back); !ok {
		t.Errorf("a number that went through JSON was refused: %#v", back.Value)
	}
}

func TestSettingsContentSerialisesTheWayTheProposalSays(t *testing.T) {
	raw, err := json.Marshal(&BridgeSettingsContent{
		Source:   BridgeSettingsSource{ID: "telegram", Name: "Telegram"},
		Settings: []BridgeSettingControl{{Key: SettingKeyBackfill, Label: "Import older messages", Type: SettingTypeAction}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"source":{"id":"telegram","name":"Telegram"},` +
		`"settings":[{"key":"backfill","label":"Import older messages","type":"action","value":null}]}`
	if string(raw) != want {
		t.Errorf("serialised as\n%s\nwant\n%s", raw, want)
	}
}

func TestControlLookup(t *testing.T) {
	if settings().Control("nope") != nil {
		t.Error("a control that does not exist was found")
	}
	if got := settings().Control(SettingKeyRelay); got == nil || got.Type != SettingTypeBoolean {
		t.Errorf("relay looked up as %+v", got)
	}
}
