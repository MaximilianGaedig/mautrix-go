// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	globalName   = "Alice Example"
	globalAvatar = id.ContentURIString("mxc://example.org/global")
	roomAvatar   = id.ContentURIString("mxc://example.org/room")
)

var testGlobal = roomProfile{Name: globalName, Avatar: globalAvatar}

func joined(name string, avatar id.ContentURIString) *event.MemberEventContent {
	return &event.MemberEventContent{Membership: event.MembershipJoin, Displayname: name, AvatarURL: avatar}
}

func nick(name string) *ChatMember {
	return &ChatMember{Nickname: &name}
}

// ghostNick is a nickname for the test ghost, as a network connector reports it.
func ghostNick(name string) *ChatMember {
	member := nick(name)
	member.Sender = testGhostID
	return member
}

func TestMemberProfileUpdate(t *testing.T) {
	t.Run("a nickname is set", func(t *testing.T) {
		assert.Equal(t, joined("Ali", globalAvatar), memberProfileUpdate(joined(globalName, globalAvatar), nick("Ali"), testGlobal, true))
	})
	t.Run("a nickname is changed", func(t *testing.T) {
		assert.Equal(t, joined("Lissy", globalAvatar), memberProfileUpdate(joined("Ali", globalAvatar), nick("Lissy"), testGlobal, true))
	})
	t.Run("a cleared nickname goes back to the global name", func(t *testing.T) {
		assert.Equal(t, joined(globalName, globalAvatar), memberProfileUpdate(joined("Ali", globalAvatar), nick(""), testGlobal, true))
	})
	t.Run("the same nickname sends nothing", func(t *testing.T) {
		assert.Nil(t, memberProfileUpdate(joined("Ali", globalAvatar), nick("Ali"), testGlobal, true))
	})
	t.Run("no nickname and the global name sends nothing", func(t *testing.T) {
		assert.Nil(t, memberProfileUpdate(joined(globalName, globalAvatar), nick(""), testGlobal, true))
	})
	t.Run("a network that says nothing leaves the event alone", func(t *testing.T) {
		assert.Nil(t, memberProfileUpdate(joined("Something else", ""), &ChatMember{}, testGlobal, true))
	})
	t.Run("a nickname equal to the global name is the global name", func(t *testing.T) {
		assert.Nil(t, memberProfileUpdate(joined(globalName, globalAvatar), nick(globalName), testGlobal, true))
		assert.Equal(t, joined(globalName, globalAvatar), memberProfileUpdate(joined("Ali", globalAvatar), nick(globalName), testGlobal, true))
	})
	t.Run("a real Matrix user is never renamed", func(t *testing.T) {
		assert.Nil(t, memberProfileUpdate(joined("Me", ""), nick("Ali"), testGlobal, false))
	})
	t.Run("a cleared nickname with no global name known keeps the name", func(t *testing.T) {
		assert.Nil(t, memberProfileUpdate(joined("Ali", ""), nick(""), roomProfile{}, true))
	})
	t.Run("a ghost that is not joined is left to the membership change", func(t *testing.T) {
		assert.Nil(t, memberProfileUpdate(&event.MemberEventContent{Membership: event.MembershipInvite}, nick("Ali"), testGlobal, true))
		assert.Nil(t, memberProfileUpdate(nil, nick("Ali"), testGlobal, true))
	})
	t.Run("a ghost that is leaving is not renamed first", func(t *testing.T) {
		leaving := nick("Ali")
		leaving.Membership = event.MembershipLeave
		assert.Nil(t, memberProfileUpdate(joined(globalName, globalAvatar), leaving, testGlobal, true))
	})
	t.Run("a room avatar is set, changed and cleared without touching the name", func(t *testing.T) {
		room, none := roomAvatar, id.ContentURIString("")
		assert.Equal(t, joined("Ali", roomAvatar), memberProfileUpdate(joined("Ali", globalAvatar), &ChatMember{RoomAvatar: &room}, testGlobal, true))
		assert.Nil(t, memberProfileUpdate(joined("Ali", roomAvatar), &ChatMember{RoomAvatar: &room}, testGlobal, true))
		assert.Equal(t, joined("Ali", globalAvatar), memberProfileUpdate(joined("Ali", roomAvatar), &ChatMember{RoomAvatar: &none}, testGlobal, true))
	})
	t.Run("only the name and avatar of the old event are carried over", func(t *testing.T) {
		current := joined(globalName, globalAvatar)
		current.JoinAuthorisedViaUsersServer = "@admin:example.org"
		current.Reason = "old reason"
		assert.Equal(t, joined("Ali", globalAvatar), memberProfileUpdate(current, nick("Ali"), testGlobal, true))
	})
}

func TestApplyRoomProfileOnJoin(t *testing.T) {
	t.Run("the nickname is in the event the ghost joins with, along with the global avatar", func(t *testing.T) {
		// The intent fills in the global profile only when both fields are empty, so the avatar
		// would be lost if it were not filled in next to the nickname.
		content := &event.MemberEventContent{Membership: event.MembershipJoin}
		assert.True(t, nick("Ali").applyRoomProfile(content, testGlobal))
		assert.Equal(t, joined("Ali", globalAvatar), content)
	})
	t.Run("a stale name from an old membership is replaced when there is no nickname", func(t *testing.T) {
		content := joined("Ali", globalAvatar)
		assert.True(t, nick("").applyRoomProfile(content, testGlobal))
		assert.Equal(t, joined(globalName, globalAvatar), content)
	})
	t.Run("a network that says nothing leaves the event empty for the intent to fill", func(t *testing.T) {
		content := &event.MemberEventContent{Membership: event.MembershipJoin}
		assert.False(t, (&ChatMember{}).applyRoomProfile(content, testGlobal))
		assert.Equal(t, &event.MemberEventContent{Membership: event.MembershipJoin}, content)
	})
}

func TestRoomProfileOverride(t *testing.T) {
	room, none := roomAvatar, id.ContentURIString("")
	assert.Equal(t, roomProfile{Name: "Ali"}, nick("Ali").override(roomProfile{}, testGlobal))
	assert.Equal(t, roomProfile{}, nick("").override(roomProfile{Name: "Ali"}, testGlobal))
	assert.Equal(t, roomProfile{}, nick(globalName).override(roomProfile{Name: "Ali"}, testGlobal), "the global name is not an override")
	assert.Equal(t, roomProfile{Name: "Ali", Avatar: roomAvatar}, (&ChatMember{RoomAvatar: &room}).override(roomProfile{Name: "Ali"}, testGlobal), "a field the network did not mention is kept")
	assert.Equal(t, roomProfile{Name: "Ali"}, (&ChatMember{RoomAvatar: &none}).override(roomProfile{Name: "Ali", Avatar: roomAvatar}, testGlobal))
}

func TestIsOwnGhostEvent(t *testing.T) {
	ghost, user := id.UserID("@meta_1:example.org"), id.UserID("@user:example.org")
	assert.True(t, isOwnGhostEvent(ghost, ghost, ghost))
	assert.False(t, isOwnGhostEvent(user, user, ghost), "a double puppet joins as the real user")
	assert.False(t, isOwnGhostEvent(user, ghost, ghost), "a ghost inviting a real user")
	assert.False(t, isOwnGhostEvent("", "", ""))
}

type sentMember struct {
	RoomID  id.RoomID
	Content event.MemberEventContent
}

// fakeGhostIntent records the member events a ghost sends.
type fakeGhostIntent struct {
	MatrixAPI
	mxid id.UserID
	sent []sentMember
}

func (f *fakeGhostIntent) GetMXID() id.UserID { return f.mxid }

func (f *fakeGhostIntent) SendState(_ context.Context, roomID id.RoomID, eventType event.Type, stateKey string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	if eventType != event.StateMember || stateKey != f.mxid.String() {
		panic("unexpected state event")
	}
	f.sent = append(f.sent, sentMember{roomID, *content.Parsed.(*event.MemberEventContent)})
	return &mautrix.RespSendEvent{}, nil
}

// fakeHomeserver answers what the ghost's member event is in each room.
type fakeHomeserver struct {
	MatrixConnector
	members map[id.RoomID]*event.MemberEventContent
}

func (f *fakeHomeserver) GetStateEvent(_ context.Context, roomID id.RoomID, _ event.Type, _ string) (*event.Event, error) {
	member, ok := f.members[roomID]
	if !ok {
		return nil, mautrix.MNotFound
	}
	return &event.Event{Content: event.Content{Parsed: member}}, nil
}

func (f *fakeHomeserver) ParseGhostMXID(userID id.UserID) (networkid.UserID, bool) {
	if userID == testGhostMXID {
		return testGhostID, true
	}
	return "", false
}

const (
	testGhostID   = networkid.UserID("1")
	testGhostMXID = id.UserID("@meta_1:example.org")
	nickRoom      = id.RoomID("!nick:example.org")
	leftRoom      = id.RoomID("!left:example.org")
)

func newRoomProfileTestGhost(t *testing.T, db *dbutil.Database) (*Ghost, *fakeGhostIntent, *fakeHomeserver) {
	hs := &fakeHomeserver{members: map[id.RoomID]*event.MemberEventContent{}}
	intent := &fakeGhostIntent{mxid: testGhostMXID}
	br := &Bridge{
		DB:         &database.Database{KV: &database.KVQuery{BridgeID: "test", Database: db}},
		Matrix:     hs,
		ghostsByID: map[networkid.UserID]*Ghost{},
	}
	ghost := &Ghost{
		Ghost:  &database.Ghost{ID: testGhostID, Name: globalName, AvatarMXC: globalAvatar},
		Bridge: br,
		Intent: intent,
	}
	br.ghostsByID[testGhostID] = ghost
	return ghost, intent, hs
}

func newKVTestDB(t *testing.T) *dbutil.Database {
	rawDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	// Every query has to see the one in-memory database.
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = rawDB.Close() })
	db, err := dbutil.NewWithDB(rawDB, "sqlite3")
	require.NoError(t, err)
	_, err = db.Exec(context.Background(), `CREATE TABLE kv_store (bridge_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (bridge_id, key))`)
	require.NoError(t, err)
	return db
}

func TestRoomProfileSurvivesGlobalProfileChange(t *testing.T) {
	ctx := context.Background()
	db := newKVTestDB(t)
	ghost, intent, hs := newRoomProfileTestGhost(t, db)
	portal := &Portal{Portal: &database.Portal{MXID: nickRoom}, Bridge: ghost.Bridge}

	// A chat sync finds the ghost joined under its global name, with a nickname on the network.
	update := portal.roomProfileUpdate(ctx, testGhostMXID, joined(globalName, globalAvatar), ghostNick("Ali"), intent)
	require.Equal(t, joined("Ali", globalAvatar), update)
	hs.members[nickRoom] = update

	t.Run("a homeserver that left the nickname alone gets no event", func(t *testing.T) {
		ghost.Name = "Alice Renamed"
		ghost.reapplyRoomProfiles(ctx)
		assert.Empty(t, intent.sent)
	})
	t.Run("a homeserver that overwrote the nickname gets it back", func(t *testing.T) {
		hs.members[nickRoom] = joined("Alice Renamed", globalAvatar)
		ghost.reapplyRoomProfiles(ctx)
		assert.Equal(t, []sentMember{{nickRoom, *joined("Ali", globalAvatar)}}, intent.sent)
		intent.sent = nil
	})
	t.Run("the nickname is still known after a restart", func(t *testing.T) {
		restarted, restartedIntent, restartedHS := newRoomProfileTestGhost(t, db)
		restarted.Name = "Alice Again"
		restartedHS.members[nickRoom] = joined("Alice Again", globalAvatar)
		restarted.reapplyRoomProfiles(ctx)
		assert.Equal(t, []sentMember{{nickRoom, *joined("Ali", globalAvatar)}}, restartedIntent.sent)
	})
	t.Run("a room the ghost has left is forgotten instead of joined again", func(t *testing.T) {
		ghost.updateRoomProfile(ctx, leftRoom, func(roomProfile) roomProfile { return roomProfile{Name: "Gone"} })
		hs.members[leftRoom] = &event.MemberEventContent{Membership: event.MembershipLeave}
		hs.members[nickRoom] = joined("Ali", globalAvatar)
		ghost.reapplyRoomProfiles(ctx)
		assert.Empty(t, intent.sent)
		assert.Equal(t, map[id.RoomID]roomProfile{nickRoom: {Name: "Ali"}}, ghost.rememberedRoomProfiles(ctx))
	})
	t.Run("a cleared nickname is forgotten", func(t *testing.T) {
		update := portal.roomProfileUpdate(ctx, testGhostMXID, joined("Ali", globalAvatar), ghostNick(""), intent)
		assert.Equal(t, joined("Alice Renamed", globalAvatar), update)
		assert.Empty(t, ghost.rememberedRoomProfiles(ctx))
		var rows int
		require.NoError(t, db.QueryRow(ctx, `SELECT COUNT(*) FROM kv_store`).Scan(&rows))
		assert.Zero(t, rows, "the last nickname takes the row with it")
		restarted, _, _ := newRoomProfileTestGhost(t, db)
		assert.Empty(t, restarted.rememberedRoomProfiles(ctx))
	})
}

func TestRoomProfileOnlyForOwnGhostEvents(t *testing.T) {
	ctx := context.Background()
	ghost, intent, _ := newRoomProfileTestGhost(t, newKVTestDB(t))
	portal := &Portal{Portal: &database.Portal{MXID: nickRoom}, Bridge: ghost.Bridge}
	member := ghostNick("Ali")
	userMXID := id.UserID("@user:example.org")
	doublePuppet := &fakeGhostIntent{mxid: userMXID}

	t.Run("a double puppet keeps its own name", func(t *testing.T) {
		assert.Nil(t, portal.roomProfileUpdate(ctx, userMXID, joined("Me", ""), member, doublePuppet))
		content := &event.MemberEventContent{Membership: event.MembershipJoin}
		joining := *member
		joining.Membership = event.MembershipJoin
		portal.applyRoomProfile(ctx, userMXID, content, &joining, doublePuppet)
		assert.Equal(t, &event.MemberEventContent{Membership: event.MembershipJoin}, content)
	})
	t.Run("a real user without a double puppet keeps its own name", func(t *testing.T) {
		assert.Nil(t, portal.roomProfileUpdate(ctx, userMXID, joined("Me", ""), member, nil))
	})
	t.Run("nothing was remembered for them", func(t *testing.T) {
		assert.Empty(t, ghost.rememberedRoomProfiles(ctx))
	})
	t.Run("the ghost itself gets the nickname when it joins", func(t *testing.T) {
		content := &event.MemberEventContent{Membership: event.MembershipJoin}
		joining := *member
		joining.Membership = event.MembershipJoin
		portal.applyRoomProfile(ctx, testGhostMXID, content, &joining, intent)
		assert.Equal(t, joined("Ali", globalAvatar), content)
		assert.Equal(t, map[id.RoomID]roomProfile{nickRoom: {Name: "Ali"}}, ghost.rememberedRoomProfiles(ctx))
	})
	t.Run("leaving forgets the nickname", func(t *testing.T) {
		leaving := ChatMember{Membership: event.MembershipLeave}
		portal.applyRoomProfile(ctx, testGhostMXID, &event.MemberEventContent{Membership: event.MembershipLeave}, &leaving, intent)
		assert.Empty(t, ghost.rememberedRoomProfiles(ctx))
	})
}
