// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"errors"
	"fmt"
	"math"

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

/*
 * BridgeSettingsEventType is the state event in which a bridge lists the controls it offers.
 *
 * It is published in two places, and the state key says whose controls they are. In the user's
 * management room it is one event per login, keyed by login ID like [BridgeLoginEventType] beside
 * it, and describes the account. In a portal room it is keyed by the bridge bot's user ID - the
 * declaring party - and describes that chat.
 */
var BridgeSettingsEventType = event.Type{Type: "im.mxg.settings", Class: event.StateEventType}

// BridgeSettingsSetMsgType is the msgtype of the message a client sends to change one.
const BridgeSettingsSetMsgType = "im.mxg.settings.set"

// BridgeSettingControlType is how a control is drawn and what its value means.
type BridgeSettingControlType string

const (
	// SettingTypeBoolean is on or off; Value is a bool.
	SettingTypeBoolean BridgeSettingControlType = "boolean"
	// SettingTypeEnum is one of Options; Value is the chosen option's Value.
	SettingTypeEnum BridgeSettingControlType = "enum"
	// SettingTypeNumber is a whole number between Min and Max. Never a fraction: event content
	// may not hold floats, and a server answers one with an error rather than rounding it.
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

// BridgeSettingScope says what a set of controls is about.
type BridgeSettingScope string

const (
	// SettingScopeLogin is the account as a whole; published in the user's management room.
	SettingScopeLogin BridgeSettingScope = "login"
	// SettingScopeRoom is one chat; published in that chat's portal room.
	SettingScopeRoom BridgeSettingScope = "room"
)

// BridgeSettingControl is one thing a client may offer to change.
type BridgeSettingControl struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Hint is a sentence saying what the control does, for a client to show under the label. The
	// same word as an info field's, so one renderer reads both.
	Hint string                   `json:"hint,omitempty"`
	Type BridgeSettingControlType `json:"type"`
	// Value is the current value, or null for an action. Typed per Type, so a client renders it
	// without knowing what the key means.
	Value any `json:"value"`
	// Options is required for an enum and meaningless otherwise.
	Options []BridgeSettingOption `json:"options,omitempty"`
	// Min and Max bound a number, both inclusive. Either may be absent, which leaves that side open.
	Min *int64 `json:"min,omitempty"`
	Max *int64 `json:"max,omitempty"`
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
	Source BridgeSettingsSource `json:"source"`
	// Scope says whether these are the account's controls or this chat's. The room it is found in
	// says the same thing, but a client collecting every declaration it has synced should not have
	// to know which rooms are management rooms to tell the two apart.
	Scope    BridgeSettingScope     `json:"scope,omitempty"`
	Settings []BridgeSettingControl `json:"settings"`
}

// BridgeSettingsSetContent is the content of a [BridgeSettingsSetMsgType] message.
type BridgeSettingsSetContent struct {
	MsgType string `json:"msgtype"`
	Key     string `json:"key"`
	// Value is the wanted value, or null for an action.
	Value any `json:"value"`
	// Target is the state key of the declaration the control was read from: the bridge bot in a
	// portal room, so a room bridged by more than one party can be told apart, and the login ID in a
	// management room, so a user with two accounts on one network can say which. Empty means
	// "whoever is listening", which is right whenever there is only one.
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
	// SettingKeyBackfill is whether this chat's older history is imported. A room control.
	SettingKeyBackfill = "backfill"
	// SettingKeyRelay carries other Matrix users' messages through a login, for people in the
	// room who have not logged in themselves. A room control: relaying is set per chat.
	SettingKeyRelay = "relay"
	// SettingKeyBackfillAll asks for the rest of every chat's history. A login control, and an
	// action: it has no state of its own to show.
	SettingKeyBackfillAll = "backfill_all"
)

// Why a request was refused. The code travels with the notice, so a client can put the refusal
// next to the control it belongs to instead of reading it out of a sentence.
const (
	SettingRefusedUnknown   = "unknown_setting"
	SettingRefusedDisabled  = "disabled"
	SettingRefusedInvalid   = "invalid_value"
	SettingRefusedForbidden = "forbidden"
	SettingRefusedFailed    = "failed"
)

// BridgeSettingRefusal is a set request turned down, in words for the user who made it.
type BridgeSettingRefusal struct {
	Code    string
	Message string
}

func (r *BridgeSettingRefusal) Error() string {
	return r.Message
}

// RefuseSetting is what a control's Apply returns to turn a request down in its own words.
func RefuseSetting(code, format string, args ...any) error {
	return &BridgeSettingRefusal{Code: code, Message: fmt.Sprintf(format, args...)}
}

// The largest whole number every JSON implementation carries exactly. A client written in
// JavaScript holds numbers as doubles, so anything beyond this would arrive as a different number.
const maxSafeInteger = 1<<53 - 1

// wholeNumber reads a number that may have come through JSON, where every number is a float64, and
// refuses one that is not whole: "7.5 days" is not a value any control here can hold.
func wholeNumber(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > maxSafeInteger {
			return 0, false
		}
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}

// checkValue reports whether value is one this control can hold, and returns it in the one Go type
// its control type uses (bool, string, int64, or nil for an action).
func (control *BridgeSettingControl) checkValue(value any) (any, error) {
	switch control.Type {
	case SettingTypeAction:
		// An action has no value; one being sent means the client is confused about the control.
		if value != nil {
			return nil, errors.New("an action takes no value")
		}
		return nil, nil
	case SettingTypeBoolean:
		// The type is part of the offer. Guessing what a client meant by the wrong one is how a
		// boolean ends up switched on by the string "false".
		v, ok := value.(bool)
		if !ok {
			return nil, errors.New("the value must be true or false")
		}
		return v, nil
	case SettingTypeEnum:
		v, ok := value.(string)
		if !ok {
			return nil, errors.New("the value must be one of the options")
		}
		for _, option := range control.Options {
			if option.Value == v {
				return v, nil
			}
		}
		return nil, fmt.Errorf("%q is not one of the options", v)
	case SettingTypeNumber:
		v, ok := wholeNumber(value)
		if !ok {
			return nil, errors.New("the value must be a whole number")
		}
		if control.Min != nil && v < *control.Min {
			return nil, fmt.Errorf("the value must be at least %d", *control.Min)
		}
		if control.Max != nil && v > *control.Max {
			return nil, fmt.Errorf("the value must be at most %d", *control.Max)
		}
		return v, nil
	case SettingTypeText:
		v, ok := value.(string)
		if !ok {
			return nil, errors.New("the value must be text")
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unknown control type %q", control.Type)
	}
}

/*
 * CheckSet reports whether a set request is one this content could have asked for, and returns the
 * wanted value in the Go type its control uses.
 *
 * Checked against the declared controls rather than against a list of keys, because the declaration
 * is the contract: a control that is absent was never offered, and one that is disabled was offered
 * and then withdrawn - both are refusals, and a bridge that acts on either would be acting on
 * something no client was told it could ask for.
 *
 * The control is returned even on a refusal, so a bridge can say which one it turned down and why.
 */
func (c *BridgeSettingsContent) CheckSet(req *BridgeSettingsSetContent) (*BridgeSettingControl, any, error) {
	control := c.Control(req.Key)
	if control == nil {
		return nil, nil, RefuseSetting(SettingRefusedUnknown, "There is no setting called %q here.", req.Key)
	}
	if control.DisabledReason != "" {
		return control, nil, RefuseSetting(SettingRefusedDisabled, "%s cannot be changed right now: %s", control.Label, control.DisabledReason)
	}
	value, err := control.checkValue(req.Value)
	if err != nil {
		return control, nil, RefuseSetting(SettingRefusedInvalid, "%s was not changed: %v.", control.Label, err)
	}
	return control, value, nil
}

// ValidSet is [BridgeSettingsContent.CheckSet] for a caller that only needs yes or no.
func (c *BridgeSettingsContent) ValidSet(req *BridgeSettingsSetContent) (*BridgeSettingControl, bool) {
	control, _, err := c.CheckSet(req)
	return control, err == nil
}
