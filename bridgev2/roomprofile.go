// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Some networks let a person be called something else in one chat only (a Messenger nickname, a
// Discord server profile). On Matrix that lives in the ghost's member event in that room, which is
// free to differ from the ghost's global profile. This file decides what that member event should
// say, and remembers the answer, because the member event alone does not survive: a homeserver
// rewrites it in every room when the global profile changes.

// roomProfile is a name and avatar. It is used both for a ghost's global profile and for what a
// ghost is called in one room, where an empty field means "the same as globally".
type roomProfile struct {
	Name   string              `json:"name,omitempty"`
	Avatar id.ContentURIString `json:"avatar,omitempty"`
}

func (member *ChatMember) hasRoomProfile() bool {
	return member.Nickname != nil || member.RoomAvatar != nil
}

// isOwnGhostEvent reports whether a member event is a ghost speaking about itself. Only those get
// a room profile: the same chat member can also stand for a real Matrix user (a double puppet, or
// the user themselves), whose name in a room is theirs to choose.
func isOwnGhostEvent(target, sender, ghost id.UserID) bool {
	return target != "" && target == ghost && sender == ghost
}

// applyRoomProfile writes the name and avatar the network wants for this room into a member event
// and reports whether that changed anything.
//
// A nil field of the member means the network said nothing, so that part is left alone. A pointer
// to an empty string means there is no override (any more), so that part goes back to the global
// value.
func (member *ChatMember) applyRoomProfile(content *event.MemberEventContent, global roomProfile) bool {
	if !member.hasRoomProfile() {
		return false
	}
	old := roomProfile{Name: content.Displayname, Avatar: content.AvatarURL}
	if member.Nickname != nil {
		content.Displayname = cmp.Or(*member.Nickname, global.Name, content.Displayname)
	}
	if member.RoomAvatar != nil {
		content.AvatarURL = cmp.Or(*member.RoomAvatar, global.Avatar, content.AvatarURL)
	}
	// The intent fills in the global profile only for a member event that has neither field, so
	// once one of them is set here the other one has to be filled in here too.
	content.Displayname = cmp.Or(content.Displayname, global.Name)
	content.AvatarURL = cmp.Or(content.AvatarURL, global.Avatar)
	return old != roomProfile{Name: content.Displayname, Avatar: content.AvatarURL}
}

// memberProfileUpdate returns the member event to send so that a ghost which is and stays joined
// is called what the network wants in this room, or nil when nothing has to be sent.
func memberProfileUpdate(current *event.MemberEventContent, member *ChatMember, global roomProfile, ownGhost bool) *event.MemberEventContent {
	if !ownGhost || current == nil || current.Membership != event.MembershipJoin {
		return nil
	} else if member.Membership != "" && member.Membership != event.MembershipJoin {
		return nil
	}
	// A new event rather than a copy of the current one: fields like the server that authorised a
	// restricted join belong to the event that joined and would not pass again.
	content := &event.MemberEventContent{
		Membership:  event.MembershipJoin,
		Displayname: current.Displayname,
		AvatarURL:   current.AvatarURL,
	}
	if !member.applyRoomProfile(content, global) {
		return nil
	}
	return content
}

// override merges what the network says now into what was remembered for the room. A value equal
// to the global one is no override: there is nothing to keep through a global change.
func (member *ChatMember) override(remembered, global roomProfile) roomProfile {
	if member.Nickname != nil {
		remembered.Name = *member.Nickname
	}
	if member.RoomAvatar != nil {
		remembered.Avatar = *member.RoomAvatar
	}
	if remembered.Name == global.Name {
		remembered.Name = ""
	}
	if remembered.Avatar == global.Avatar {
		remembered.Avatar = ""
	}
	return remembered
}

// roomProfileMemory is what a ghost is called in the rooms where that differs from its global
// profile. It is kept in the bridge's key-value store, one row per ghost that has any, so the
// bridge still knows it after a restart without waiting for every chat to be synced again.
type roomProfileMemory struct {
	lock   sync.Mutex
	loaded bool
	rooms  map[id.RoomID]roomProfile
}

const roomProfileKeyPrefix = "room_profiles:"

func (ghost *Ghost) roomProfileKey() database.Key {
	return database.Key(roomProfileKeyPrefix + string(ghost.ID))
}

func (ghost *Ghost) globalProfile() roomProfile {
	return roomProfile{Name: ghost.Name, Avatar: ghost.AvatarMXC}
}

// lockedLoadRoomProfiles reads the remembered room profiles once per ghost.
func (ghost *Ghost) lockedLoadRoomProfiles(ctx context.Context) {
	mem := &ghost.roomProfiles
	if mem.loaded {
		return
	}
	mem.loaded = true
	mem.rooms = make(map[id.RoomID]roomProfile)
	stored := ghost.Bridge.DB.KV.Get(ctx, ghost.roomProfileKey())
	if stored == "" {
		return
	}
	if err := json.Unmarshal([]byte(stored), &mem.rooms); err != nil {
		zerolog.Ctx(ctx).Err(err).Str("ghost_id", string(ghost.ID)).Msg("Failed to parse remembered room profiles")
	}
}

// rememberedRoomProfiles returns a copy of the remembered room profiles.
func (ghost *Ghost) rememberedRoomProfiles(ctx context.Context) map[id.RoomID]roomProfile {
	ghost.roomProfiles.lock.Lock()
	defer ghost.roomProfiles.lock.Unlock()
	ghost.lockedLoadRoomProfiles(ctx)
	return maps.Clone(ghost.roomProfiles.rooms)
}

// updateRoomProfile changes what is remembered for one room. Returning the zero value forgets it.
func (ghost *Ghost) updateRoomProfile(ctx context.Context, roomID id.RoomID, update func(remembered roomProfile) roomProfile) {
	mem := &ghost.roomProfiles
	mem.lock.Lock()
	defer mem.lock.Unlock()
	ghost.lockedLoadRoomProfiles(ctx)
	old := mem.rooms[roomID]
	updated := update(old)
	if updated == old {
		return
	}
	if updated == (roomProfile{}) {
		delete(mem.rooms, roomID)
	} else {
		mem.rooms[roomID] = updated
	}
	if len(mem.rooms) == 0 {
		ghost.Bridge.DB.KV.Delete(ctx, ghost.roomProfileKey())
	} else if data, err := json.Marshal(mem.rooms); err != nil {
		zerolog.Ctx(ctx).Err(err).Str("ghost_id", string(ghost.ID)).Msg("Failed to serialize room profiles to remember")
	} else {
		ghost.Bridge.DB.KV.Set(ctx, ghost.roomProfileKey(), string(data))
	}
}

func (ghost *Ghost) forgetRoomProfile(ctx context.Context, roomID id.RoomID) {
	ghost.updateRoomProfile(ctx, roomID, func(roomProfile) roomProfile { return roomProfile{} })
}

// expectsRoomProfile reports whether the ghost's member event in the room is meant to differ from
// its global profile, in which case the difference is not a profile that drifted.
func (ghost *Ghost) expectsRoomProfile(ctx context.Context, roomID id.RoomID, member *ChatMember) bool {
	// Only networks that speak about room profiles can have remembered one, and the others should
	// not pay a database read per ghost to find that out.
	if !member.hasRoomProfile() {
		return false
	}
	return ghost.rememberedRoomProfiles(ctx)[roomID] != roomProfile{}
}

// currentMember returns the ghost's member event in a room as the homeserver has it now. The
// bridge's own copy can lag behind right after a global profile change, which is exactly when
// this is asked.
func (ghost *Ghost) currentMember(ctx context.Context, roomID id.RoomID) (*event.MemberEventContent, error) {
	mx, ok := ghost.Bridge.Matrix.(MatrixConnectorWithArbitraryRoomState)
	if !ok {
		return ghost.Bridge.Matrix.GetMemberInfo(ctx, roomID, ghost.Intent.GetMXID())
	}
	evt, err := mx.GetStateEvent(ctx, roomID, event.StateMember, ghost.Intent.GetMXID().String())
	if errors.Is(err, mautrix.MNotFound) || (err == nil && evt == nil) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return evt.Content.AsMember(), nil
}

// reapplyRoomProfiles puts the remembered names and avatars back after the ghost's global profile
// was pushed. A homeserver that was not asked to (or cannot) leave differing rooms alone has just
// overwritten them with the global values.
func (ghost *Ghost) reapplyRoomProfiles(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	global := ghost.globalProfile()
	for roomID, remembered := range ghost.rememberedRoomProfiles(ctx) {
		current, err := ghost.currentMember(ctx, roomID)
		if err != nil {
			log.Err(err).Stringer("room_id", roomID).Msg("Failed to get member event to put the room profile back")
			continue
		} else if current == nil || current.Membership != event.MembershipJoin {
			// Sending the event would join the ghost again, so a room it has left is dropped
			// instead.
			ghost.forgetRoomProfile(ctx, roomID)
			continue
		}
		wanted := &ChatMember{Nickname: &remembered.Name, RoomAvatar: &remembered.Avatar}
		content := memberProfileUpdate(current, wanted, global, true)
		if content == nil {
			continue
		}
		_, err = ghost.Intent.SendState(ctx, roomID, event.StateMember, ghost.Intent.GetMXID().String(), &event.Content{Parsed: content}, time.Now())
		if err != nil {
			log.Err(err).Stringer("room_id", roomID).Msg("Failed to put the room profile back after a global profile change")
		} else {
			log.Debug().Stringer("room_id", roomID).Msg("Put the room profile back after a global profile change")
		}
	}
}

// roomProfileGhost returns the ghost whose member event may carry the room profile of the given
// chat member, or nil when the event is not a ghost's own or the network said nothing about it.
func (portal *Portal) roomProfileGhost(ctx context.Context, target id.UserID, member *ChatMember, intent MatrixAPI) *Ghost {
	if intent == nil || member.Sender == "" || !member.hasRoomProfile() {
		return nil
	}
	ghost, err := portal.Bridge.GetGhostByID(ctx, member.Sender)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("ghost_id", string(member.Sender)).Msg("Failed to get ghost to apply room profile")
		return nil
	} else if !isOwnGhostEvent(target, intent.GetMXID(), ghost.Intent.GetMXID()) {
		return nil
	}
	return ghost
}

// applyRoomProfile sets the room profile in the member event with which a ghost joins the room.
func (portal *Portal) applyRoomProfile(ctx context.Context, target id.UserID, content *event.MemberEventContent, member *ChatMember, intent MatrixAPI) {
	if member.Membership != event.MembershipJoin {
		// Whatever was remembered belongs to the membership that is ending. If the ghost comes
		// back, the network says again what it is called.
		if ghost, _ := portal.Bridge.GetGhostByMXID(ctx, target); ghost != nil {
			ghost.forgetRoomProfile(ctx, portal.MXID)
		}
		return
	}
	ghost := portal.roomProfileGhost(ctx, target, member, intent)
	if ghost == nil {
		return
	}
	global := ghost.globalProfile()
	ghost.updateRoomProfile(ctx, portal.MXID, func(remembered roomProfile) roomProfile {
		return member.override(remembered, global)
	})
	member.applyRoomProfile(content, global)
}

// roomProfileUpdate returns the member event to send for a ghost whose membership is not changing
// but whose room profile is, or nil.
func (portal *Portal) roomProfileUpdate(ctx context.Context, target id.UserID, current *event.MemberEventContent, member *ChatMember, intent MatrixAPI) *event.MemberEventContent {
	if current == nil || current.Membership != event.MembershipJoin {
		return nil
	}
	ghost := portal.roomProfileGhost(ctx, target, member, intent)
	if ghost == nil {
		return nil
	}
	global := ghost.globalProfile()
	ghost.updateRoomProfile(ctx, portal.MXID, func(remembered roomProfile) roomProfile {
		return member.override(remembered, global)
	})
	return memberProfileUpdate(current, member, global, true)
}
