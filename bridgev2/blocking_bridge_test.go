// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// The ignore list on top of upstream's UserBlockingNetworkAPI, against a real database: the question is which
// DM portal the network was asked to block in, and what the ignore list ends up holding.

type fakeBlockingMatrix struct {
	*fakeSettingsMatrix
}

func (fakeBlockingMatrix) ParseGhostMXID(userID id.UserID) (networkid.UserID, bool) {
	if !strings.HasPrefix(string(userID), "@tg_") {
		return "", false
	}
	return networkid.UserID(strings.TrimSuffix(strings.TrimPrefix(string(userID), "@tg_"), ":example.org")), true
}

func (fakeBlockingMatrix) GhostIntent(userID networkid.UserID) MatrixAPI {
	return fakeBlockGhost{mxid: id.UserID("@tg_" + string(userID) + ":example.org")}
}

type fakeBlockGhost struct {
	MatrixAPI
	mxid id.UserID
}

func (g fakeBlockGhost) GetMXID() id.UserID { return g.mxid }

type blockCall struct {
	portal networkid.PortalID
	block  bool
}

type fakeBlockingClient struct {
	NetworkAPI
	calls []blockCall
}

func (*fakeBlockingClient) IsLoggedIn() bool { return true }

func (c *fakeBlockingClient) HandleMatrixBlockUser(_ context.Context, msg *MatrixBlockUser) error {
	c.calls = append(c.calls, blockCall{msg.Portal.ID, msg.Content.Block})
	return nil
}

// fakeDoublePuppet is the user's own account data on the homeserver.
type fakeDoublePuppet struct {
	MatrixAPI
	data map[string]json.RawMessage
}

func (dp *fakeDoublePuppet) GetAccountData(_ context.Context, eventType string, into any) error {
	raw, ok := dp.data[eventType]
	if !ok {
		return assert.AnError
	}
	return json.Unmarshal(raw, into)
}

func (dp *fakeDoublePuppet) SetAccountData(_ context.Context, eventType string, content any) error {
	raw, err := json.Marshal(content)
	if err != nil {
		return err
	}
	dp.data[eventType] = raw
	return nil
}

func (dp *fakeDoublePuppet) ignored(t *testing.T) []id.UserID {
	var list event.IgnoredUserListEventContent
	if raw, ok := dp.data[event.AccountDataIgnoredUserList.Type]; ok {
		require.NoError(t, json.Unmarshal(raw, &list))
	}
	var out []id.UserID
	for userID := range list.IgnoredUsers {
		out = append(out, userID)
	}
	return out
}

func newBlockingHarness(t *testing.T) (*settingsHarness, *User, *UserLogin, *fakeBlockingClient, *fakeDoublePuppet) {
	h := newSettingsHarness(t)
	h.br.Matrix = fakeBlockingMatrix{h.matrix}
	user := h.user("@max:example.org", bridgeconfig.Permissions{})
	client := &fakeBlockingClient{}
	login := h.login(user, "max", client)
	dp := &fakeDoublePuppet{data: map[string]json.RawMessage{}}
	user.doublePuppetInitialized = true
	user.doublePuppetIntent = dp
	return h, user, login, client, dp
}

func (h *settingsHarness) dm(portalID networkid.PortalID, receiver *UserLogin, other networkid.UserID) *Portal {
	portal := h.portal(portalID, receiver)
	portal.RoomType = database.RoomTypeDM
	portal.OtherUserID = other
	require.NoError(h.t, portal.Save(h.ctx))
	return portal
}

func ignoreListEvent(sender id.UserID, now, prev *event.IgnoredUserListEventContent) *event.Event {
	evt := &event.Event{Sender: sender, Type: event.AccountDataIgnoredUserList}
	evt.Content.Parsed = now
	if prev != nil {
		raw, _ := json.Marshal(prev)
		evt.Unsigned.PrevContent = &event.Content{VeryRaw: raw}
	}
	return evt
}

func TestIgnoringAGhostBlocksThemInTheirDM(t *testing.T) {
	h, user, login, client, _ := newBlockingHarness(t)
	dm := h.dm("dm-alice", login, "alice")
	h.portal("group", login)
	other := h.user("@other:example.org", bridgeconfig.Permissions{})
	othersClient := &fakeBlockingClient{}
	h.dm("other-dm-alice", h.login(other, "other", othersClient), "alice")

	h.br.HandleIgnoredUserList(h.ctx, ignoreListEvent(user.MXID, ignoreList("@tg_alice:example.org", "@someone:elsewhere"), nil))
	assert.Equal(t, []blockCall{{"dm-alice", true}}, client.calls, "upstream's block, addressed to the DM portal")
	assert.Empty(t, othersClient.calls, "someone else's DM with the same person is theirs")
	assert.True(t, dm.UserBlocked)
	saved, err := h.br.DB.Portal.GetByKey(h.ctx, dm.PortalKey)
	require.NoError(t, err)
	assert.True(t, saved.UserBlocked, "saved like upstream's com.beeper.block_user handler does")

	h.br.HandleIgnoredUserList(h.ctx, ignoreListEvent(user.MXID, ignoreList(), ignoreList("@tg_alice:example.org")))
	assert.Equal(t, []blockCall{{"dm-alice", true}, {"dm-alice", false}}, client.calls, "un-ignoring unblocks")
	assert.False(t, dm.UserBlocked)
}

func TestIgnoringAGhostWithoutADMDoesNothing(t *testing.T) {
	h, user, _, client, _ := newBlockingHarness(t)
	h.br.HandleIgnoredUserList(h.ctx, ignoreListEvent(user.MXID, ignoreList("@tg_bob:example.org"), nil))
	assert.Empty(t, client.calls)
}

func TestNetworkBlockGoesOnTheIgnoreListAndItsEchoIsNotSentBack(t *testing.T) {
	h, user, login, client, dp := newBlockingHarness(t)
	dm := h.dm("dm-alice", login, "alice")

	// What UpdateInfo does when the connector reports ChatInfo.UserBlocked.
	dm.UserBlocked = true
	dm.mirrorBlockToIgnoreList(h.ctx, login)
	assert.Equal(t, []id.UserID{"@tg_alice:example.org"}, dp.ignored(t))

	// The homeserver hands our own write back as an ignore-list change: the person is already blocked.
	h.br.HandleIgnoredUserList(h.ctx, ignoreListEvent(user.MXID, ignoreList("@tg_alice:example.org"), nil))
	assert.Empty(t, client.calls, "no second block for our own echo")

	dm.UserBlocked = false
	dm.mirrorBlockToIgnoreList(h.ctx, login)
	assert.Empty(t, dp.ignored(t), "an unblock on the network takes them off the list")
}

func TestUpdateInfoMirrorsUserBlocked(t *testing.T) {
	h, _, login, _, dp := newBlockingHarness(t)
	dm := h.dm("dm-alice", login, "alice")
	dm.MXID = "" // no room: UpdateInfo touches nothing on Matrix but the ignore list
	blocked := true
	dm.UpdateInfo(h.ctx, &ChatInfo{UserBlocked: &blocked}, login, nil, time.Time{})
	assert.Equal(t, []id.UserID{"@tg_alice:example.org"}, dp.ignored(t))
}
