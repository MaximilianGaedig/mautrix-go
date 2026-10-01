// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/rs/zerolog"
)

/*
 * The controls a bridge can offer, kept in one list.
 *
 * A control is defined once - its key, what kind of thing it is, how to read it and how to change
 * it - and everything else follows from the definition: what is published, what a request is
 * checked against, and what happens when one is accepted. Nothing can be published that cannot be
 * set, and nothing can be set that was not published, because both come from the same entry.
 *
 * A network connector adds its own with [RegisterBridgeSetting]; the client needs no change for it.
 */

// BridgeSettingTarget is what a control is being read or changed for.
type BridgeSettingTarget struct {
	// Login is the account, for a login-scoped control.
	Login *UserLogin
	// LoggedIn is whether that account still has a session on the network. See [loginStillLoggedIn].
	LoggedIn bool
	// Portal is the chat, for a room-scoped control.
	Portal *Portal
}

// BridgeSettingState is how a control stands right now.
type BridgeSettingState struct {
	// Value is a bool for a boolean, a string for an enum or text, a whole number for a number, and
	// nil for an action.
	Value any
	// Hint replaces the definition's hint when what the control does depends on how it stands.
	Hint string
	// DisabledReason, when set, withdraws the control and says why.
	DisabledReason string
}

// BridgeSetting defines one control.
type BridgeSetting struct {
	// Key names the control on the wire. It never changes once a client may have seen it.
	Key   string
	Scope BridgeSettingScope
	Type  BridgeSettingControlType
	Label string
	Hint  string
	// Options are the choices of an enum.
	Options []BridgeSettingOption
	// Min and Max bound a number, both inclusive; nil leaves that side open.
	Min, Max *int64

	/*
	 * State reads the control for a target, or returns nil when it is not on offer there.
	 *
	 * Not on offer and disabled are different answers. A control the network cannot do at all is
	 * left out, because showing it would promise something that will never work; one that only
	 * cannot work at the moment is shown with a reason, because it will.
	 */
	State func(ctx context.Context, br *Bridge, target BridgeSettingTarget) (*BridgeSettingState, error)
	// Apply changes the control on behalf of a user. The value has already been checked against
	// the definition; whether this user may change it is for Apply to decide, since only the
	// control knows what changing it touches. Return [RefuseSetting] to turn the request down.
	Apply func(ctx context.Context, br *Bridge, target BridgeSettingTarget, by *User, value any) error
}

// Keys end up in event content and in clients' code, so they are kept to what needs no escaping
// anywhere.
var settingKeyRegex = regexp.MustCompile(`^[a-z][a-z0-9_.]*$`)

// control is the definition as it goes on the wire, before the state is filled in.
func (s *BridgeSetting) control() BridgeSettingControl {
	return BridgeSettingControl{
		Key:     s.Key,
		Label:   s.Label,
		Hint:    s.Hint,
		Type:    s.Type,
		Options: s.Options,
		Min:     s.Min,
		Max:     s.Max,
	}
}

// validate reports what is wrong with a definition. Checked when it is registered, so a control
// that could never be rendered or set fails at startup rather than in front of a user.
func (s *BridgeSetting) validate() error {
	if !settingKeyRegex.MatchString(s.Key) {
		return fmt.Errorf("setting key %q must be lowercase letters, digits, _ and .", s.Key)
	}
	if s.Scope != SettingScopeLogin && s.Scope != SettingScopeRoom {
		return fmt.Errorf("setting %s: unknown scope %q", s.Key, s.Scope)
	}
	if s.Label == "" {
		return fmt.Errorf("setting %s: a control needs a label", s.Key)
	}
	if s.State == nil || s.Apply == nil {
		return fmt.Errorf("setting %s: a control needs both State and Apply", s.Key)
	}
	switch s.Type {
	case SettingTypeBoolean, SettingTypeText, SettingTypeAction:
	case SettingTypeEnum:
		// An enum with no options is a dropdown with nothing in it.
		if len(s.Options) == 0 {
			return fmt.Errorf("setting %s: an enum needs options", s.Key)
		}
		seen := make(map[string]struct{}, len(s.Options))
		for _, option := range s.Options {
			if option.Label == "" {
				return fmt.Errorf("setting %s: option %q needs a label", s.Key, option.Value)
			}
			if _, duplicate := seen[option.Value]; duplicate {
				return fmt.Errorf("setting %s: option %q is listed twice", s.Key, option.Value)
			}
			seen[option.Value] = struct{}{}
		}
	case SettingTypeNumber:
		if s.Min != nil && s.Max != nil && *s.Min > *s.Max {
			return fmt.Errorf("setting %s: min %d is above max %d", s.Key, *s.Min, *s.Max)
		}
		for _, bound := range []*int64{s.Min, s.Max} {
			if bound != nil && (*bound > maxSafeInteger || *bound < -maxSafeInteger) {
				return fmt.Errorf("setting %s: bound %d is too large for a client to hold exactly", s.Key, *bound)
			}
		}
	default:
		return fmt.Errorf("setting %s: unknown type %q", s.Key, s.Type)
	}
	if s.Type != SettingTypeEnum && len(s.Options) > 0 {
		return fmt.Errorf("setting %s: only an enum has options", s.Key)
	}
	if s.Type != SettingTypeNumber && (s.Min != nil || s.Max != nil) {
		return fmt.Errorf("setting %s: only a number has a min or max", s.Key)
	}
	return nil
}

// stateValue is the value a control's State returned, checked and put in its wire form.
func stateValue(control *BridgeSettingControl, value any) (any, error) {
	// A float is refused here even when it happens to be whole. On the way in, JSON leaves no
	// choice; on the way out a float is a mistake in the control, and the day it is not whole the
	// homeserver refuses the event and the control silently stops updating.
	switch value.(type) {
	case float32, float64:
		return nil, errors.New("a float cannot go in event content")
	case int8, int16, int32, uint8, uint16, uint32:
		return nil, errors.New("a number's value must be an int or int64")
	}
	return control.checkValue(value)
}

// BridgeSettingsRegistry is the list of controls, in the order they are shown.
type BridgeSettingsRegistry struct {
	lock     sync.RWMutex
	settings []*BridgeSetting
}

// Register adds a control. The order of registration is the order of display.
func (r *BridgeSettingsRegistry) Register(setting *BridgeSetting) error {
	if err := setting.validate(); err != nil {
		return err
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	for _, existing := range r.settings {
		// Unique across scopes, not just within one: a key that meant one thing for a chat and
		// another for an account would be the same control spelled the same way twice.
		if existing.Key == setting.Key {
			return fmt.Errorf("setting %s is already registered", setting.Key)
		}
	}
	r.settings = append(r.settings, setting)
	return nil
}

// Get returns the control with this key in this scope, or nil.
func (r *BridgeSettingsRegistry) Get(scope BridgeSettingScope, key string) *BridgeSetting {
	r.lock.RLock()
	defer r.lock.RUnlock()
	for _, setting := range r.settings {
		if setting.Key == key && setting.Scope == scope {
			return setting
		}
	}
	return nil
}

/*
 * Content is what to publish for a target: every control of the scope that is on offer there.
 *
 * A control whose state cannot be read, or reads as something its own definition does not allow, is
 * left out and logged rather than published wrong. A client believes what it is shown, so a toggle
 * showing the wrong side is worse than a missing one.
 */
func (r *BridgeSettingsRegistry) Content(ctx context.Context, br *Bridge, scope BridgeSettingScope, target BridgeSettingTarget) *BridgeSettingsContent {
	name := br.Network.GetName()
	content := &BridgeSettingsContent{
		Source: BridgeSettingsSource{ID: name.NetworkID, Name: name.DisplayName},
		Scope:  scope,
		// Never nil: an empty list is how a declaration with nothing left in it is withdrawn, and
		// "settings": null is not a list to a client looping over it.
		Settings: []BridgeSettingControl{},
	}
	r.lock.RLock()
	defer r.lock.RUnlock()
	for _, setting := range r.settings {
		if setting.Scope != scope {
			continue
		}
		state, err := setting.State(ctx, br, target)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Str("setting", setting.Key).Msg("Failed to read a setting; leaving it out")
			continue
		} else if state == nil {
			continue
		}
		control := setting.control()
		control.Value, err = stateValue(&control, state.Value)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Str("setting", setting.Key).Msg("A setting read as a value it cannot hold; leaving it out")
			continue
		}
		if state.Hint != "" {
			control.Hint = state.Hint
		}
		control.DisabledReason = state.DisabledReason
		content.Settings = append(content.Settings, control)
	}
	return content
}

// BridgeSettings is the list every bridge publishes from.
var BridgeSettings = &BridgeSettingsRegistry{}

// RegisterBridgeSetting adds a control to what the bridge offers. For a network connector's own
// controls; call it before the bridge starts. It panics on a definition that could never work,
// which is a mistake in the connector and not something to find out from a user.
func RegisterBridgeSetting(setting *BridgeSetting) {
	if err := BridgeSettings.Register(setting); err != nil {
		panic(err)
	}
}
