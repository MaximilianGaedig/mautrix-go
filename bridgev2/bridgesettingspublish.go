// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

/*
 * Publishing the controls, and acting on a request to change one.
 *
 * Both ends of the same loop: the bridge writes state, a client renders it and sends a request, the
 * bridge applies it and writes the state again. The rewritten state is the acknowledgement, so the
 * one thing that must never happen is the state being rewritten when nothing changed - a client
 * waiting on state would take that for an answer, and every copy stays in the room for good.
 */

type settingsSentKey struct {
	room     id.RoomID
	stateKey string
}

// settingsSentCache remembers what was last published where, so that an unchanged declaration is
// not sent again.
type settingsSentCache struct {
	lock sync.Mutex
	sent map[settingsSentKey][]byte
}

// BridgeSettingsErrorKey is where a refusal notice says, for a client rather than a person, which
// control was refused and why.
const BridgeSettingsErrorKey = "im.mxg.settings.error"

// BridgeSettingsErrorContent is the content under [BridgeSettingsErrorKey].
type BridgeSettingsErrorContent struct {
	Key  string `json:"key"`
	Code string `json:"code"`
}

/*
 * publishSettings writes a declaration into a room unless that is what the room already says, and
 * reports whether it wrote.
 *
 * State events are replicated to every client and kept forever, so sending one that says what the
 * last one said costs every member of the room a timeline event for nothing. The first time a
 * declaration is published in a run there is nothing remembered to compare with, so the room is
 * asked: a restart then costs a read per room, not a write.
 */
func (br *Bridge) publishSettings(ctx context.Context, room id.RoomID, stateKey string, content *BridgeSettingsContent) (bool, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return false, err
	}
	key := settingsSentKey{room, stateKey}
	cache := &br.settingsSent
	// Held across the send, so that two publishes of the same declaration cannot both find it
	// missing and both send it.
	cache.lock.Lock()
	defer cache.lock.Unlock()
	if cache.sent == nil {
		cache.sent = make(map[settingsSentKey][]byte)
	}
	previous, known := cache.sent[key]
	if !known {
		previous, known = br.publishedSettings(ctx, room, stateKey)
	}
	if known && bytes.Equal(previous, raw) {
		cache.sent[key] = raw
		return false, nil
	}
	// Nothing was ever declared here and there is nothing to declare: an empty list would only
	// withdraw controls nobody has seen.
	if known && previous == nil && len(content.Settings) == 0 {
		cache.sent[key] = nil
		return false, nil
	}
	_, err = br.Bot.SendState(ctx, room, BridgeSettingsEventType, stateKey, &event.Content{Parsed: content}, time.Time{})
	if err != nil {
		return false, err
	}
	cache.sent[key] = raw
	return true, nil
}

// publishedSettings reads the declaration a room holds, in the form this bridge would write it.
// known is false when the room could not be asked, in which case the caller has to send to be sure;
// a nil declaration that is known means the room has none.
func (br *Bridge) publishedSettings(ctx context.Context, room id.RoomID, stateKey string) (raw []byte, known bool) {
	api, ok := br.Matrix.(MatrixConnectorWithArbitraryRoomState)
	if !ok {
		return nil, false
	}
	evt, err := api.GetStateEvent(ctx, room, BridgeSettingsEventType, stateKey)
	if errors.Is(err, mautrix.MNotFound) || (err == nil && evt == nil) {
		return nil, true
	} else if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to read the room's declared settings")
		return nil, false
	}
	// Through the same struct and back, so the comparison is between two things this code wrote
	// the same way rather than between its own bytes and the homeserver's.
	var previous BridgeSettingsContent
	if err = json.Unmarshal(evt.Content.VeryRaw, &previous); err != nil {
		return nil, false
	}
	if previous.Settings == nil {
		previous.Settings = []BridgeSettingControl{}
	}
	raw, err = json.Marshal(&previous)
	return raw, err == nil
}

/*
 * PublishSettings records what this chat will let its members change, in the chat's own room.
 *
 * Keyed by the bridge bot, the party doing the declaring. Nothing in a chat's declaration depends
 * on whether a login happens to be connected: that changes for every chat at once, and a reason
 * that came and went with the connection would rewrite the state of every room on every reconnect.
 * A request that cannot be met right now is answered with a notice instead.
 */
func (portal *Portal) PublishSettings(ctx context.Context) {
	if portal.MXID == "" || portal.Bridge.IsStopping() {
		return
	}
	br := portal.Bridge
	content := BridgeSettings.Content(ctx, br, SettingScopeRoom, BridgeSettingTarget{Portal: portal})
	if _, err := br.publishSettings(ctx, portal.MXID, br.Bot.GetMXID().String(), content); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Stringer("room_id", portal.MXID).Msg("Failed to publish the chat's settings")
	}
}

/*
 * loginStillLoggedIn is whether a login in this state has a session to act with, now or shortly.
 *
 * Deliberately not "is it connected". A login is briefly not connected every time the bridge
 * starts and every time the network hiccups, and a control that was withdrawn and offered again
 * each time would write two state events per reconnect into the management room for a reason no
 * user ever gets to read. Only the states that end with the user having to log in again withdraw
 * anything; what was asked for in between waits for the connection, as the import queue does.
 */
func loginStillLoggedIn(state status.BridgeStateEvent) bool {
	return state != status.StateBadCredentials && state != status.StateLoggedOut
}

// publishLoginSettings records what this login will let the user change, in their management room.
func (br *Bridge) publishLoginSettings(ctx context.Context, room id.RoomID, login *UserLogin, loggedIn bool) {
	content := BridgeSettings.Content(ctx, br, SettingScopeLogin, BridgeSettingTarget{Login: login, LoggedIn: loggedIn})
	if _, err := br.publishSettings(ctx, room, string(login.ID), content); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to publish the login's settings")
	}
}

// settingsSetRequest reads a set request out of a message, or returns nil for any other message.
func settingsSetRequest(evt *event.Event) *BridgeSettingsSetContent {
	if evt.Type != event.EventMessage || evt.Content.AsMessage().MsgType != BridgeSettingsSetMsgType {
		return nil
	}
	raw := evt.Content.VeryRaw
	if len(raw) == 0 {
		raw, _ = json.Marshal(evt.Content.Raw)
	}
	var req BridgeSettingsSetContent
	if err := json.Unmarshal(raw, &req); err != nil {
		// Still a request, just not one that names anything: it is answered as an unknown setting
		// rather than passed on to the network as a message.
		return &BridgeSettingsSetContent{MsgType: BridgeSettingsSetMsgType}
	}
	return &req
}

/*
 * handleSettingsSet takes a set request out of the stream of Matrix events, and reports whether the
 * event was one.
 *
 * It has to be taken before anything else looks at the event. In a management room every message is
 * a command, so the request would be answered with "unknown command"; in a portal it is a message,
 * so it would be sent to the other network as one.
 */
func (br *Bridge) handleSettingsSet(ctx context.Context, evt *event.Event, sender *User) bool {
	req := settingsSetRequest(evt)
	if req == nil {
		return false
	}
	go br.applySettingsSet(ctx, evt, sender, req)
	return true
}

// applySettingsSet checks a request against what was declared, applies it, and answers: with the
// rewritten state when it worked, with a notice when it did not.
func (br *Bridge) applySettingsSet(ctx context.Context, evt *event.Event, sender *User, req *BridgeSettingsSetContent) {
	log := zerolog.Ctx(ctx).With().Str("action", "set bridge setting").Str("setting", req.Key).Logger()
	ctx = log.WithContext(ctx)
	scope, target, republish, err := br.settingsTarget(ctx, evt, sender, req)
	if err != nil {
		br.refuseSetting(ctx, evt, req, err)
		return
	} else if republish == nil {
		// Addressed to another bridge in the room. Not ours to answer.
		return
	}
	// The same permission that sending the bridge a command takes: a setting is a command with a
	// button on it, and must not be a way around not being allowed to send one.
	if !sender.Permissions.Commands {
		br.refuseSetting(ctx, evt, req, RefuseSetting(SettingRefusedForbidden, "You are not allowed to change this bridge's settings."))
		return
	}
	declared := BridgeSettings.Content(ctx, br, scope, target)
	_, value, err := declared.CheckSet(req)
	if err != nil {
		br.refuseSetting(ctx, evt, req, err)
		return
	}
	setting := BridgeSettings.Get(scope, req.Key)
	if err = setting.Apply(ctx, br, target, sender, value); err != nil {
		var refusal *BridgeSettingRefusal
		if !errors.As(err, &refusal) {
			log.Err(err).Msg("Failed to apply a setting")
		}
		br.refuseSetting(ctx, evt, req, err)
		return
	}
	log.Debug().Any("value", value).Msg("Applied a setting")
	republish(ctx)
}

/*
 * settingsTarget works out what a request is about from the room it was sent in, and how to
 * publish the result. A nil republish with no error means the request is for somebody else.
 *
 * The room decides the scope, not the request: a request cannot name a chat or an account other
 * than the one whose room it was sent in. That is what keeps one user from changing another's -
 * a management room is its owner's alone, and only its owner's logins are looked at there.
 */
func (br *Bridge) settingsTarget(ctx context.Context, evt *event.Event, sender *User, req *BridgeSettingsSetContent) (scope BridgeSettingScope, target BridgeSettingTarget, republish func(context.Context), err error) {
	bot := br.Bot.GetMXID().String()
	if evt.RoomID == sender.ManagementRoom {
		var login *UserLogin
		logins := sender.GetUserLogins()
		switch {
		case req.Target != "" && req.Target != bot:
			for _, candidate := range logins {
				if string(candidate.ID) == req.Target {
					login = candidate
				}
			}
			if login == nil {
				return "", target, nil, RefuseSetting(SettingRefusedForbidden, "You have no account %q on this bridge.", req.Target)
			}
		case len(logins) == 1:
			login = logins[0]
		case len(logins) == 0:
			return "", target, nil, RefuseSetting(SettingRefusedForbidden, "You are not logged in.")
		default:
			return "", target, nil, RefuseSetting(SettingRefusedInvalid, "You have more than one account here, and the request did not say which one it is for.")
		}
		// The state the declaration in the room was last written from, so the request is checked
		// against what the client was shown.
		loggedIn := loginStillLoggedIn(login.BridgeState.GetPrev().StateEvent)
		target = BridgeSettingTarget{Login: login, LoggedIn: loggedIn}
		room := evt.RoomID
		return SettingScopeLogin, target, func(ctx context.Context) {
			br.publishLoginSettings(ctx, room, login, loggedIn)
		}, nil
	}
	portal, err := br.GetPortalByMXID(ctx, evt.RoomID)
	if err != nil {
		return "", target, nil, err
	} else if portal == nil {
		return "", target, nil, RefuseSetting(SettingRefusedUnknown, "This room is not a bridged chat, so it has no settings.")
	}
	if req.Target != "" && req.Target != bot {
		return "", target, nil, nil
	}
	return SettingScopeRoom, BridgeSettingTarget{Portal: portal}, portal.PublishSettings, nil
}

/*
 * refuseSetting answers a request that was not carried out.
 *
 * An ordinary notice, as a reply to the request, so it reads sensibly in a client that knows
 * nothing about settings - and the control and a code alongside, so one that does can show the
 * refusal where the user pressed rather than somewhere in the timeline.
 */
func (br *Bridge) refuseSetting(ctx context.Context, evt *event.Event, req *BridgeSettingsSetContent, reason error) {
	var refusal *BridgeSettingRefusal
	if !errors.As(reason, &refusal) {
		// Not written for a user, and may say things about the bridge's insides that are none of the
		// room's business. It is in the log.
		refusal = &BridgeSettingRefusal{Code: SettingRefusedFailed, Message: "The setting could not be changed. The bridge's log says why."}
	}
	content := &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType:   event.MsgNotice,
			Body:      refusal.Message,
			RelatesTo: (&event.RelatesTo{}).SetReplyTo(evt.ID),
		},
		Raw: map[string]any{
			BridgeSettingsErrorKey: &BridgeSettingsErrorContent{Key: req.Key, Code: refusal.Code},
		},
	}
	if _, err := br.Bot.SendMessage(ctx, evt.RoomID, event.EventMessage, content, nil); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to say that a setting was refused")
	}
}
