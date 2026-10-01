// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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

// albumTestClient is a network client that only sends single messages. Every response is
// pending, which is the one way a message finishes without the bridge database.
type albumTestClient struct {
	NetworkAPI
	lock  sync.Mutex
	calls [][]id.EventID
	fail  map[id.EventID]error
	sent  chan struct{}
}

func (c *albumTestClient) record(msgs ...*MatrixMessage) {
	ids := make([]id.EventID, len(msgs))
	for i, msg := range msgs {
		ids[i] = msg.Event.ID
	}
	c.lock.Lock()
	c.calls = append(c.calls, ids)
	c.lock.Unlock()
	if c.sent != nil {
		c.sent <- struct{}{}
	}
}

func (c *albumTestClient) getCalls() [][]id.EventID {
	c.lock.Lock()
	defer c.lock.Unlock()
	return append([][]id.EventID(nil), c.calls...)
}

func (c *albumTestClient) HandleMatrixMessage(ctx context.Context, msg *MatrixMessage) (*MatrixMessageResponse, error) {
	c.record(msg)
	if err := c.fail[msg.Event.ID]; err != nil {
		return nil, err
	}
	return &MatrixMessageResponse{Pending: true}, nil
}

// albumTestAlbumClient can also send albums.
type albumTestAlbumClient struct {
	albumTestClient
	albumErr   error
	dropResult bool
}

func (c *albumTestAlbumClient) HandleMatrixAlbum(ctx context.Context, msgs []*MatrixMessage) ([]MatrixAlbumPartResult, error) {
	c.record(msgs...)
	if c.albumErr != nil {
		return nil, c.albumErr
	}
	results := make([]MatrixAlbumPartResult, len(msgs))
	for i, msg := range msgs {
		if err := c.fail[msg.Event.ID]; err != nil {
			results[i].Err = err
		} else {
			results[i].Response = &MatrixMessageResponse{Pending: true}
		}
	}
	if c.dropResult {
		results = results[1:]
	}
	return results, nil
}

// albumTestMatrix records the delivery statuses the portal sends.
type albumTestMatrix struct {
	MatrixConnector
	lock     sync.Mutex
	statuses map[id.EventID]*MessageStatus
}

func (m *albumTestMatrix) SendMessageStatus(ctx context.Context, status *MessageStatus, evt *MessageStatusEventInfo) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.statuses[evt.SourceEventID] = status
}

func (m *albumTestMatrix) getStatus(evtID id.EventID) *MessageStatus {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.statuses[evtID]
}

func newAlbumTestPortal() (*Portal, *albumTestMatrix) {
	matrix := &albumTestMatrix{statuses: make(map[id.EventID]*MessageStatus)}
	bridge := &Bridge{
		Matrix:        matrix,
		Config:        &bridgeconfig.BridgeConfig{},
		BackgroundCtx: context.Background(),
	}
	portal := &Portal{
		Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "chat"}, MXID: "!room:example.org"},
		Bridge: bridge,
	}
	portal.backgroundCtx = context.Background()
	return portal, matrix
}

func albumTestLogin(client NetworkAPI) *UserLogin {
	return &UserLogin{UserLogin: &database.UserLogin{ID: "login"}, Client: client}
}

// albumTestMessage builds what handleMatrixMessage has in hand right before sending. The content
// goes through JSON so that the marker's numbers have the types they have in a real event.
func albumTestMessage(t *testing.T, portal *Portal, evtID id.EventID, msgType event.MessageType, marker any) *MatrixMessage {
	t.Helper()
	raw := map[string]any{"msgtype": msgType, "body": "file.jpg"}
	if marker != nil {
		raw[AlbumFieldKey] = marker
	}
	data, err := json.Marshal(raw)
	require.NoError(t, err)
	evt := &event.Event{
		ID:     evtID,
		Type:   event.EventMessage,
		Sender: "@user:example.org",
		RoomID: portal.MXID,
	}
	require.NoError(t, json.Unmarshal(data, &evt.Content))
	require.NoError(t, evt.Content.ParseRaw(evt.Type))
	return &MatrixMessage{MatrixEventBase: MatrixEventBase[*event.MessageEventContent]{
		Event:   evt,
		Content: evt.Content.AsMessage(),
		Portal:  portal,
	}}
}

func albumPart(albumID string, index, count int) AlbumMarker {
	return AlbumMarker{ID: albumID, Index: index, Count: count}
}

func TestParseAlbumMarker(t *testing.T) {
	parse := func(jsonMarker string) (AlbumMarker, bool) {
		var raw map[string]any
		require.NoError(t, json.Unmarshal([]byte(`{"fi.mau.album":`+jsonMarker+`}`), &raw))
		return ParseAlbumMarker(raw)
	}
	marker, ok := parse(`{"id":"abc","index":2,"count":5}`)
	assert.True(t, ok)
	assert.Equal(t, AlbumMarker{ID: "abc", Index: 2, Count: 5}, marker)

	for name, bad := range map[string]string{
		"no id":              `{"index":0,"count":2}`,
		"no count":           `{"id":"abc","index":0}`,
		"a fractional index": `{"id":"abc","index":0.5,"count":2}`,
		"a string index":     `{"id":"abc","index":"0","count":2}`,
		"not an object":      `"abc"`,
	} {
		_, ok = parse(bad)
		assert.False(t, ok, name)
	}
	_, ok = ParseAlbumMarker(map[string]any{"body": "no marker"})
	assert.False(t, ok)
}

func TestHoldAlbumPart_UnmarkedMessageIsNotHeld(t *testing.T) {
	portal, _ := newAlbumTestPortal()
	client := &albumTestAlbumClient{}
	login := albumTestLogin(client)

	// An album is waiting for its second part.
	first := albumTestMessage(t, portal, "$photo0", event.MsgImage, albumPart("album", 0, 2))
	res, held := portal.holdAlbumPart(context.Background(), login, first, nil)
	require.True(t, held)
	assert.Equal(t, EventHandlingResultQueued, res)

	// Anything without a marker goes on to be sent right away, the album doesn't delay it.
	for _, msgType := range []event.MessageType{event.MsgText, event.MsgImage} {
		msg := albumTestMessage(t, portal, "$other", msgType, nil)
		_, held = portal.holdAlbumPart(context.Background(), login, msg, nil)
		assert.False(t, held, "unmarked %s", msgType)
	}
	assert.Empty(t, client.getCalls(), "the hook itself sends nothing for a message it doesn't hold")
}

func TestHoldAlbumPart_NetworkWithoutAlbumsHoldsNothing(t *testing.T) {
	portal, _ := newAlbumTestPortal()
	client := &albumTestClient{}
	login := albumTestLogin(client)
	for i := range 2 {
		msg := albumTestMessage(t, portal, id.EventID(fmt.Sprintf("$photo%d", i)), event.MsgImage, albumPart("album", i, 2))
		_, held := portal.holdAlbumPart(context.Background(), login, msg, nil)
		assert.False(t, held)
	}
	assert.Empty(t, client.getCalls())
	assert.Nil(t, portal.albums.collector, "a network without albums never even gets a collector")
}

func TestHoldAlbumPart_OnlyHoldsWhatCanBeInAnAlbum(t *testing.T) {
	portal, _ := newAlbumTestPortal()
	login := albumTestLogin(&albumTestAlbumClient{})
	ctx := context.Background()

	text := albumTestMessage(t, portal, "$text", event.MsgText, albumPart("a", 0, 2))
	_, held := portal.holdAlbumPart(ctx, login, text, nil)
	assert.False(t, held, "a text is not held")

	relayed := albumTestMessage(t, portal, "$relayed", event.MsgImage, albumPart("b", 0, 2))
	relayed.OrigSender = &OrigSender{UserID: "@other:example.org"}
	_, held = portal.holdAlbumPart(ctx, login, relayed, nil)
	assert.False(t, held, "a relayed message is not held")

	sticker := albumTestMessage(t, portal, "$sticker", event.MsgImage, albumPart("c", 0, 2))
	sticker.Event.Type = event.EventSticker
	_, held = portal.holdAlbumPart(ctx, login, sticker, nil)
	assert.False(t, held, "a sticker is not held")

	single := albumTestMessage(t, portal, "$single", event.MsgImage, albumPart("d", 0, 1))
	_, held = portal.holdAlbumPart(ctx, login, single, nil)
	assert.False(t, held, "an album of one is an ordinary message")

	noCount := albumTestMessage(t, portal, "$nocount", event.MsgImage, map[string]any{"id": "e", "index": 0})
	_, held = portal.holdAlbumPart(ctx, login, noCount, nil)
	assert.False(t, held, "an album of unknown size can't be collected")

	_, pending := portal.albums.getCollector().NextDeadline()
	assert.False(t, pending, "none of them is waiting in the collector")
}

func TestHoldAlbumPart_SendsCompleteAlbumInIndexOrder(t *testing.T) {
	portal, matrix := newAlbumTestPortal()
	client := &albumTestAlbumClient{}
	login := albumTestLogin(client)
	ctx := context.Background()

	// The parts arrive out of order. Nothing is sent until the last one is there.
	for _, index := range []int{1, 0} {
		msg := albumTestMessage(t, portal, id.EventID(fmt.Sprintf("$photo%d", index)), event.MsgImage, albumPart("album", index, 3))
		res, held := portal.holdAlbumPart(ctx, login, msg, nil)
		require.True(t, held)
		assert.Equal(t, EventHandlingResultQueued, res)
		assert.Empty(t, client.getCalls())
	}
	last := albumTestMessage(t, portal, "$photo2", event.MsgVideo, albumPart("album", 2, 3))
	res, held := portal.holdAlbumPart(ctx, login, last, nil)
	require.True(t, held)
	assert.True(t, res.Success)
	assert.Equal(t, [][]id.EventID{{"$photo0", "$photo1", "$photo2"}}, client.getCalls(), "one album call with every part")
	assert.Empty(t, matrix.statuses, "no part failed")
	assert.Nil(t, portal.albums.flush, "nothing is left to flush")
}

func TestHoldAlbumPart_SendersDoNotMix(t *testing.T) {
	portal, _ := newAlbumTestPortal()
	client := &albumTestAlbumClient{}
	login := albumTestLogin(client)
	ctx := context.Background()

	mine := albumTestMessage(t, portal, "$mine0", event.MsgImage, albumPart("album", 0, 2))
	theirs := albumTestMessage(t, portal, "$theirs1", event.MsgImage, albumPart("album", 1, 2))
	theirs.Event.Sender = "@someone:example.org"
	portal.holdAlbumPart(ctx, login, mine, nil)
	portal.holdAlbumPart(ctx, login, theirs, nil)
	assert.Empty(t, client.getCalls(), "the same album ID from another sender is another album")

	mine = albumTestMessage(t, portal, "$mine1", event.MsgImage, albumPart("album", 1, 2))
	portal.holdAlbumPart(ctx, login, mine, nil)
	assert.Equal(t, [][]id.EventID{{"$mine0", "$mine1"}}, client.getCalls())
}

func TestHoldAlbumPart_EachPartHasItsOwnStatus(t *testing.T) {
	portal, matrix := newAlbumTestPortal()
	failure := errors.New("too large")
	client := &albumTestAlbumClient{albumTestClient: albumTestClient{fail: map[id.EventID]error{"$photo0": failure}}}
	login := albumTestLogin(client)
	ctx := context.Background()

	portal.holdAlbumPart(ctx, login, albumTestMessage(t, portal, "$photo0", event.MsgImage, albumPart("album", 0, 2)), nil)
	res, held := portal.holdAlbumPart(ctx, login, albumTestMessage(t, portal, "$photo1", event.MsgImage, albumPart("album", 1, 2)), nil)
	require.True(t, held)
	assert.True(t, res.Success, "the part that completed the album was sent")
	require.NotNil(t, matrix.getStatus("$photo0"), "the held part that failed gets its own error status")
	assert.ErrorIs(t, matrix.getStatus("$photo0").InternalError, failure)
	assert.Nil(t, matrix.getStatus("$photo1"))
}

func TestHoldAlbumPart_FailureOfTheLastPartIsReportedOnce(t *testing.T) {
	portal, matrix := newAlbumTestPortal()
	failure := errors.New("album refused")
	client := &albumTestAlbumClient{albumErr: failure}
	login := albumTestLogin(client)
	ctx := context.Background()

	portal.holdAlbumPart(ctx, login, albumTestMessage(t, portal, "$photo0", event.MsgImage, albumPart("album", 0, 2)), nil)
	res, held := portal.holdAlbumPart(ctx, login, albumTestMessage(t, portal, "$photo1", event.MsgImage, albumPart("album", 1, 2)), nil)
	require.True(t, held)
	assert.False(t, res.Success)
	assert.ErrorIs(t, res.Error, failure)
	assert.False(t, res.SendMSS, "the status was already sent, the event queue must not send it again")
	for _, evtID := range []id.EventID{"$photo0", "$photo1"} {
		require.NotNil(t, matrix.getStatus(evtID), evtID)
		assert.ErrorIs(t, matrix.getStatus(evtID).InternalError, failure)
	}
}

func TestHoldAlbumPart_TimeoutSendsWhatArrived(t *testing.T) {
	origTimeout := AlbumIdleTimeout
	AlbumIdleTimeout = 40 * time.Millisecond
	defer func() { AlbumIdleTimeout = origTimeout }()

	portal, _ := newAlbumTestPortal()
	client := &albumTestAlbumClient{albumTestClient: albumTestClient{sent: make(chan struct{}, 4)}}
	login := albumTestLogin(client)
	ctx := context.Background()

	// Two of three parts arrive, the third never does.
	for i := range 2 {
		msg := albumTestMessage(t, portal, id.EventID(fmt.Sprintf("$photo%d", i)), event.MsgImage, albumPart("album", i, 3))
		_, held := portal.holdAlbumPart(ctx, login, msg, nil)
		require.True(t, held)
	}
	assert.Empty(t, client.getCalls())
	select {
	case <-client.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the incomplete album was never sent")
	}
	assert.Equal(t, [][]id.EventID{{"$photo0", "$photo1"}}, client.getCalls(), "the two parts still go out as one album")

	// A single part that times out is an ordinary message.
	lone := albumTestMessage(t, portal, "$lone", event.MsgImage, albumPart("other", 0, 2))
	portal.holdAlbumPart(ctx, login, lone, nil)
	select {
	case <-client.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the lone part was never sent")
	}
	assert.Equal(t, []id.EventID{"$lone"}, client.getCalls()[1])
}

func TestDeliverAlbum(t *testing.T) {
	portal, _ := newAlbumTestPortal()
	msgs := []*MatrixMessage{
		albumTestMessage(t, portal, "$photo0", event.MsgImage, nil),
		albumTestMessage(t, portal, "$photo1", event.MsgImage, nil),
	}
	ctx := context.Background()

	t.Run("a batch of one is an ordinary message", func(t *testing.T) {
		client := &albumTestAlbumClient{}
		outcomes := deliverAlbum(ctx, client, msgs[:1])
		require.Len(t, outcomes, 1)
		assert.NoError(t, outcomes[0].Err)
		assert.NotNil(t, outcomes[0].Response)
		assert.Equal(t, [][]id.EventID{{"$photo0"}}, client.getCalls())
	})
	t.Run("a failed album fails every part", func(t *testing.T) {
		failure := errors.New("nope")
		outcomes := deliverAlbum(ctx, &albumTestAlbumClient{albumErr: failure}, msgs)
		require.Len(t, outcomes, 2)
		for _, outcome := range outcomes {
			assert.ErrorIs(t, outcome.Err, failure)
		}
	})
	t.Run("a part can fail on its own", func(t *testing.T) {
		failure := errors.New("nope")
		client := &albumTestAlbumClient{albumTestClient: albumTestClient{fail: map[id.EventID]error{"$photo1": failure}}}
		outcomes := deliverAlbum(ctx, client, msgs)
		assert.NoError(t, outcomes[0].Err)
		assert.NotNil(t, outcomes[0].Response)
		assert.ErrorIs(t, outcomes[1].Err, failure)
	})
	t.Run("a result that doesn't cover every part fails every part", func(t *testing.T) {
		outcomes := deliverAlbum(ctx, &albumTestAlbumClient{dropResult: true}, msgs)
		require.Len(t, outcomes, 2)
		for _, outcome := range outcomes {
			assert.ErrorIs(t, outcome.Err, ErrAlbumPartNotHandled)
		}
	})
}
