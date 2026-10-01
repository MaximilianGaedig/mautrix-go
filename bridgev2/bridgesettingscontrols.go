// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"fmt"
	"slices"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
)

/*
 * The controls every bridge has, whatever its network.
 *
 * None of them keeps anything of its own. Each one reads and writes the thing the bridge already
 * acts on - the chat's backfill task, the portal's relay - so a control and the command that does
 * the same job cannot disagree: there is only one switch, and both are a hand on it.
 */

func init() {
	RegisterBridgeSetting(&BridgeSetting{
		Key:   SettingKeyBackfill,
		Scope: SettingScopeRoom,
		Type:  SettingTypeBoolean,
		Label: "Import older messages",
		Hint:  "Off leaves this chat's older history on the other network and takes it out of the import queue. On imports all of it.",
		State: roomBackfillState,
		Apply: applyRoomBackfill,
	})
	RegisterBridgeSetting(&BridgeSetting{
		Key:   SettingKeyRelay,
		Scope: SettingScopeRoom,
		Type:  SettingTypeBoolean,
		Label: "Relay other people's messages",
		Hint:  "Lets people in this room who have not logged in write to the chat: their messages are sent through a logged-in account, with their name in front.",
		State: roomRelayState,
		Apply: applyRoomRelay,
	})
	RegisterBridgeSetting(&BridgeSetting{
		Key:   SettingKeyBackfillAll,
		Scope: SettingScopeLogin,
		Type:  SettingTypeAction,
		Label: "Import the rest of every chat",
		Hint:  "Carries on with every chat whose import stopped before the beginning. Chats you chose not to import are left alone.",
		State: loginBackfillAllState,
		Apply: applyLoginBackfillAll,
	})
}

// backfillQueueRuns is whether anything acts on a chat's backfill task. Where nothing does, a
// control that edits the task would be a switch wired to nothing.
func (br *Bridge) backfillQueueRuns() bool {
	return br.Config.Backfill.Enabled && br.Config.Backfill.Queue.Enabled && br.Matrix.GetCapabilities().BatchSending
}

// backfillTaskImports is whether the queue is importing this chat's history or has finished doing
// so: the two states of a task that mean "all of it", against skipped and stopped short.
func backfillTaskImports(task *database.BackfillTask) bool {
	return task != nil && task.UserLoginID != "" && task.BatchCount != -2 && (task.IsDone || !task.QueueDone)
}

// loginOwnedBy is the user's own login in this chat, or nil if they have none in it.
func (portal *Portal) loginOwnedBy(ctx context.Context, user *User) (*UserLogin, error) {
	if portal.Receiver != "" {
		login := portal.Bridge.GetCachedUserLoginByID(portal.Receiver)
		if login != nil && login.UserMXID == user.MXID {
			return login, nil
		}
		return nil, nil
	}
	userPortals, err := portal.Bridge.DB.UserPortal.GetAllForUserInPortal(ctx, user.MXID, portal.PortalKey)
	if err != nil {
		return nil, err
	}
	for _, up := range userPortals {
		if login := portal.Bridge.GetCachedUserLoginByID(up.LoginID); login != nil {
			return login, nil
		}
	}
	return nil, nil
}

func roomBackfillState(ctx context.Context, br *Bridge, target BridgeSettingTarget) (*BridgeSettingState, error) {
	if !br.backfillQueueRuns() {
		return nil, nil
	}
	task, err := br.DB.BackfillTask.GetNextForPortal(ctx, target.Portal.PortalKey, true)
	if err != nil {
		return nil, err
	}
	if task == nil || task.UserLoginID == "" {
		// A chat that was never queued can still be asked for, as long as some login in it can
		// fetch history at all. If none can, there is nothing to switch on.
		logins, err := br.GetUserLoginsInPortal(ctx, target.Portal.PortalKey)
		if err != nil {
			return nil, err
		}
		canFetch := slices.ContainsFunc(logins, func(login *UserLogin) bool {
			_, ok := login.Client.(BackfillingNetworkAPI)
			return ok
		})
		if !canFetch {
			return nil, nil
		}
	}
	return &BridgeSettingState{Value: backfillTaskImports(task)}, nil
}

/*
 * applyRoomBackfill is the `backfill` and `backfill skip` commands as a switch.
 *
 * Off marks the chat's task skipped, which the queue passes by; on asks for the whole history and
 * makes it due now. Both act on the task's own login where it has one, since a task belongs to the
 * login it was created through and no other can advance it.
 */
func applyRoomBackfill(ctx context.Context, br *Bridge, target BridgeSettingTarget, by *User, value any) error {
	portal := target.Portal
	own, err := portal.loginOwnedBy(ctx, by)
	if err != nil {
		return err
	}
	// What gets imported into a chat is decided by the people whose account it is imported from.
	// Somebody merely invited to the room should not be able to pull years of another person's
	// history into it, or stop it arriving.
	if own == nil && !by.Permissions.Admin {
		return RefuseSetting(SettingRefusedForbidden, "Only someone whose own %s account is in this chat can change what is imported into it.", br.Network.GetName().DisplayName)
	}
	login := own
	task, err := br.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, true)
	if err != nil {
		return err
	}
	if task != nil && task.UserLoginID != "" {
		if taskLogin, _ := br.GetExistingUserLoginByID(ctx, task.UserLoginID); taskLogin != nil {
			login = taskLogin
		}
	}
	if login == nil {
		return RefuseSetting(SettingRefusedFailed, "No account in this chat can import its history.")
	}
	if value.(bool) {
		err = portal.RequestFullBackfill(ctx, login.ID)
	} else {
		if err = br.DB.BackfillTask.EnsureExists(ctx, portal.PortalKey, login.ID); err == nil {
			err = br.DB.BackfillTask.Skip(ctx, portal.PortalKey, login.ID)
		}
	}
	if err != nil {
		return err
	}
	// The import status says the same thing in more words, and is what the rest of the client reads.
	go portal.PublishBackfillStatus(br.BackgroundCtx, login, true)
	return nil
}

func roomRelayState(ctx context.Context, br *Bridge, target BridgeSettingTarget) (*BridgeSettingState, error) {
	if !br.Config.Relay.Enabled {
		return nil, nil
	}
	relay := target.Portal.Relay
	if relay == nil {
		return &BridgeSettingState{Value: false}, nil
	}
	return &BridgeSettingState{
		Value: true,
		// Whose account it goes through is the thing people in the room want to know, and it is
		// already announced there by the command that sets it.
		Hint: fmt.Sprintf("Messages from people in this room who have not logged in are sent through %s, with their name in front.", relay.RemoteName),
	}, nil
}

// The pretend state event whose power level stands for "may manage the relay here", as in the
// set-relay command: a room can raise or lower it like any other.
var settingRelayPowerEvent = event.Type{Type: "fi.mau.bridge.set_relay", Class: event.StateEventType}

// userCanManageRelay is the set-relay command's own rule, so the switch and the command let the
// same people through.
func (portal *Portal) userCanManageRelay(ctx context.Context, user *User) bool {
	if !user.Permissions.ManageRelay {
		return false
	}
	if user.Permissions.Admin || (portal.Relay != nil && portal.Relay.UserMXID == user.MXID) {
		return true
	}
	levels, err := portal.Bridge.Matrix.GetPowerLevels(ctx, portal.MXID)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to check room power levels")
		return false
	}
	return levels.GetUserLevel(user.MXID) >= levels.GetEventLevel(settingRelayPowerEvent)
}

/*
 * relayFor picks the login that `set-relay` with no argument would pick for this user.
 *
 * A switch has no room for the command's optional login ID, so it is the command's default: the
 * user's own account, unless the bridge is configured to prefer or insist on its default relays.
 */
func (portal *Portal) relayFor(ctx context.Context, user *User) (*UserLogin, error) {
	br := portal.Bridge
	cfg := &br.Config.Relay
	onlyDefaultRelays := !user.Permissions.Admin && cfg.AdminOnly
	if portal.Receiver != "" {
		relay := br.GetCachedUserLoginByID(portal.Receiver)
		if relay == nil {
			return nil, RefuseSetting(SettingRefusedFailed, "The account this chat belongs to is not logged in.")
		} else if slices.Contains(cfg.DefaultRelays, relay.ID) {
			return relay, nil
		} else if relay.UserMXID != user.MXID && !user.Permissions.Admin {
			return nil, RefuseSetting(SettingRefusedForbidden, "Only bridge admins can set another user's login as the relay.")
		} else if onlyDefaultRelays {
			return nil, RefuseSetting(SettingRefusedForbidden, "You're not allowed to use yourself as relay.")
		}
		return relay, nil
	}
	relay, err := portal.loginOwnedBy(ctx, user)
	if err != nil {
		return nil, err
	}
	if relay == nil {
		relay = user.GetDefaultLogin()
	}
	isLoggedIn := relay != nil
	preferDefaultRelay := cfg.PreferDefault && len(cfg.DefaultRelays) > 0
	if !onlyDefaultRelays && !preferDefaultRelay && relay != nil {
		return relay, nil
	}
	if len(cfg.DefaultRelays) == 0 {
		if isLoggedIn {
			return nil, RefuseSetting(SettingRefusedForbidden, "You're not allowed to use yourself as relay and there are no default relay users configured.")
		}
		return nil, RefuseSetting(SettingRefusedForbidden, "You're not logged in and there are no default relay users configured.")
	}
	logins, err := br.GetUserLoginsInPortal(ctx, portal.PortalKey)
	if err != nil {
		return nil, err
	}
	for _, loginID := range cfg.DefaultRelays {
		for _, login := range logins {
			if login.ID == loginID {
				return login, nil
			}
		}
	}
	if isLoggedIn && onlyDefaultRelays {
		return nil, RefuseSetting(SettingRefusedForbidden, "You're not allowed to use yourself as relay and none of the default relay users are in the chat.")
	} else if isLoggedIn {
		return nil, RefuseSetting(SettingRefusedFailed, "None of the default relay users are in the chat.")
	}
	return nil, RefuseSetting(SettingRefusedForbidden, "You're not logged in and none of the default relay users are in the chat.")
}

// applyRoomRelay is the `set-relay` and `unset-relay` commands as a switch.
func applyRoomRelay(ctx context.Context, br *Bridge, target BridgeSettingTarget, by *User, value any) error {
	portal := target.Portal
	if !portal.userCanManageRelay(ctx, by) {
		return RefuseSetting(SettingRefusedForbidden, "You don't have permission to manage the relay in this room.")
	}
	on := value.(bool)
	// Already so. Switching on a relay that is set must not quietly move it to whoever asked.
	if on == (portal.Relay != nil) {
		return nil
	}
	if !on {
		return portal.SetRelay(ctx, nil)
	}
	relay, err := portal.relayFor(ctx, by)
	if err != nil {
		return err
	}
	return portal.SetRelay(ctx, relay)
}

func loginBackfillAllState(ctx context.Context, br *Bridge, target BridgeSettingTarget) (*BridgeSettingState, error) {
	if _, ok := target.Login.Client.(BackfillingNetworkAPI); !ok || !br.backfillQueueRuns() {
		return nil, nil
	}
	state := &BridgeSettingState{}
	if !target.LoggedIn {
		state.DisabledReason = "Logged out of " + br.Network.GetName().DisplayName
	}
	return state, nil
}

/*
 * applyLoginBackfillAll asks for the rest of every chat this login has that stopped short.
 *
 * Narrower than the admin's `backfill-all` on purpose. That one reopens every task of every user;
 * this is one user's button, so it touches only their login's chats, leaves alone the ones they
 * chose to skip, and does not make the network be asked again about chats it already said it has
 * nothing older for.
 */
func applyLoginBackfillAll(ctx context.Context, br *Bridge, target BridgeSettingTarget, by *User, _ any) error {
	login := target.Login
	if login.UserMXID != by.MXID {
		return RefuseSetting(SettingRefusedForbidden, "That account is not yours.")
	}
	userPortals, err := br.DB.UserPortal.GetAllForLogin(ctx, login.UserLogin)
	if err != nil {
		return err
	}
	var asked []*Portal
	for _, up := range userPortals {
		portal, err := br.GetExistingPortalByKey(ctx, up.Portal)
		if err != nil || portal == nil || portal.MXID == "" {
			continue
		}
		task, err := br.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, true)
		if err != nil {
			return err
		}
		if task != nil && task.UserLoginID != "" {
			stoppedShort := task.QueueDone && !task.IsDone && task.BatchCount != -2
			if !stoppedShort || task.UserLoginID != login.ID {
				continue
			}
		}
		if err = portal.RequestFullBackfill(ctx, login.ID); err != nil {
			return err
		}
		asked = append(asked, portal)
	}
	zerolog.Ctx(ctx).Info().Int("chats", len(asked)).Msg("Asked for the rest of the login's chats")
	go func() {
		ctx := login.Log.WithContext(br.BackgroundCtx)
		for _, portal := range asked {
			portal.PublishBackfillStatus(ctx, login, false)
		}
	}()
	return nil
}
