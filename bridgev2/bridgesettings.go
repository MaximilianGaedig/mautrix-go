// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"maunium.net/go/mautrix/event"
)

/*
 * What a bridge will let you change, declared rather than documented.
 *
 * The companion to [BridgeLoginEventType]: that says how things are, this says what can be done
 * about them. Both are read by a client that knows nothing about any particular network - each
 * control carries its own label and type, so rendering is a loop over a list rather than a switch
 * over bridges, and adding a control costs no client code at all.
 *
 * Changing one is an ordinary message event, not a state event, for two reasons. The user may not
 * have the power to send state in the room; and a request is not a fact - the bridge decides
 * whether it happened. The updated state event *is* the acknowledgement, so a client renders state
 * and waits for state, with no request/response plumbing of its own.
 */

// BridgeSettingsEventType is the state event in which a bridge lists the controls it offers. Same
// state-key rule as [BridgeLoginEventType]: one per login, keyed by login ID.
var BridgeSettingsEventType = event.Type{Type: "im.mxg.settings", Class: event.StateEventType}

// BridgeSettingsSetMsgType is the msgtype of the message a client sends to change one.
const BridgeSettingsSetMsgType = "im.mxg.settings.set"

// BridgeSettingControlType is how a control is drawn and what its value means.
type BridgeSettingControlType string

const (
	// SettingTypeBoolean is on or off; Value is a bool.
	SettingTypeBoolean BridgeSettingControlType = "boolean"
	// SettingTypeEnum is one of Options; Value is the chosen option's Value.
	SettingTypeEnum   BridgeSettingControlType = "enum"
	SettingTypeNumber BridgeSettingControlType = "number"
	SettingTypeText   BridgeSettingControlType = "text"
	// SettingTypeAction is a button: it has no value, and setting it means "do this now".
	SettingTypeAction BridgeSettingControlType = "action"
)

// BridgeSettingOption is one choice of an enum control.
type BridgeSettingOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// BridgeSettingControl is one thing a client may offer to change.
type BridgeSettingControl struct {
	Key   string                   `json:"key"`
	Label string                   `json:"label"`
	Type  BridgeSettingControlType `json:"type"`
	// Value is the current value, or null for an action. Typed per Type, so a client renders it
	// without knowing what the key means.
	Value any `json:"value"`
	// Options is required for an enum and meaningless otherwise.
	Options []BridgeSettingOption `json:"options,omitempty"`
	/*
	 * DisabledReason, when set, is shown to the user and the control is inert.
	 *
	 * This is how a bridge says "you are logged out, so importing history cannot start" in its own
	 * words. Without it a client has to infer why a control would not work, which means every client
	 * inventing its own guesses about every network.
	 */
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// BridgeSettingsSource names the party offering the controls, for a client showing several at once.
type BridgeSettingsSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// BridgeSettingsContent is the content of the [BridgeSettingsEventType] state event.
type BridgeSettingsContent struct {
	Source   BridgeSettingsSource   `json:"source"`
	Settings []BridgeSettingControl `json:"settings"`
}

// BridgeSettingsSetContent is the content of a [BridgeSettingsSetMsgType] message.
type BridgeSettingsSetContent struct {
	MsgType string `json:"msgtype"`
	Key     string `json:"key"`
	// Value is the wanted value, or null for an action.
	Value any `json:"value"`
	// Target is the bridge bot the request is for, so a room bridged by more than one party can be
	// told apart. Empty means "whoever is listening", which is right in a management room.
	Target string `json:"target,omitempty"`
}

// Control returns the named control, or nil.
func (c *BridgeSettingsContent) Control(key string) *BridgeSettingControl {
	for i := range c.Settings {
		if c.Settings[i].Key == key {
			return &c.Settings[i]
		}
	}
	return nil
}

// Known control keys. Named here rather than per bridge so that a client can special-case the two
// that every network has, and so two bridges cannot spell the same control differently.
const (
	// SettingKeyBackfill starts importing older history. An action: it has no state to show.
	SettingKeyBackfill = "backfill"
	// SettingKeyRelay carries other Matrix users' messages through this login, for people in the
	// room who have not logged in themselves.
	SettingKeyRelay = "relay"
)

/*
 * ValidSet reports whether a set request is one this content could have asked for.
 *
 * Checked against the declared controls rather than against a list of keys, because the declaration
 * is the contract: a control that is absent was never offered, and one that is disabled was offered
 * and then withdrawn - both are refusals, and a bridge that acts on either would be acting on
 * something no client was told it could ask for.
 */
func (c *BridgeSettingsContent) ValidSet(req *BridgeSettingsSetContent) (*BridgeSettingControl, bool) {
	control := c.Control(req.Key)
	if control == nil || control.DisabledReason != "" {
		return control, false
	}
	switch control.Type {
	case SettingTypeAction:
		// An action has no value; one being sent means the client is confused about the control.
		return control, req.Value == nil
	case SettingTypeBoolean:
		_, ok := req.Value.(bool)
		return control, ok
	case SettingTypeEnum:
		value, ok := req.Value.(string)
		if !ok {
			return control, false
		}
		for _, option := range control.Options {
			if option.Value == value {
				return control, true
			}
		}
		return control, false
	case SettingTypeNumber:
		_, ok := req.Value.(float64) // JSON numbers
		return control, ok
	case SettingTypeText:
		_, ok := req.Value.(string)
		return control, ok
	default:
		return control, false
	}
}

/*
 * loginSettings is what this login will let the user change, worked out from what the network
 * actually supports and what the login can do right now.
 *
 * Derived rather than configured: a control that is offered but cannot work is worse than one that
 * is absent, because the user taps it and nothing happens. So backfill appears only for a network
 * that implements it, and is disabled with a reason while the login is not connected.
 */
func (br *Bridge) loginSettings(login *UserLogin, connected bool) *BridgeSettingsContent {
	content := &BridgeSettingsContent{
		Source: BridgeSettingsSource{
			ID:   br.Network.GetName().NetworkID,
			Name: br.Network.GetName().DisplayName,
		},
	}
	notConnected := ""
	if !connected {
		notConnected = "Not connected to " + br.Network.GetName().DisplayName
	}
	if _, ok := login.Client.(BackfillingNetworkAPI); ok {
		content.Settings = append(content.Settings, BridgeSettingControl{
			Key:            SettingKeyBackfill,
			Label:          "Import older messages",
			Type:           SettingTypeAction,
			Value:          nil,
			DisabledReason: notConnected,
		})
	}
	// Relay is the bridge's own doing rather than the network's, so it is offered whatever the
	// network implements - but only to a user allowed to manage it, since offering a control the
	// server will refuse is the same lie as offering one that cannot work.
	if login.User.Permissions.ManageRelay {
		content.Settings = append(content.Settings, BridgeSettingControl{
			Key:   SettingKeyRelay,
			Label: "Carry other people's messages through this account",
			Type:  SettingTypeBoolean,
			Value: false,
		})
	}
	return content
}
