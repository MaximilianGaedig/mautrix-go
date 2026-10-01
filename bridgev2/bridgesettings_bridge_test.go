// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

/*
 * A bridge with a real database and a pretend homeserver.
 *
 * The database is real because the two controls under test are switches on rows in it - a backfill
 * task, a portal's relay - and the question each test asks is whether the row the bridge acts on
 * changed, not whether a function was called. The homeserver is pretend because all it has to do is
 * remember what it was sent, which is the other half of every question here.
 */

type sentSettingsState struct {
	room     id.RoomID
	evtType  string
	stateKey string
	raw      []byte
}

type sentSettingsNotice struct {
	room id.RoomID
	raw  []byte
}

type fakeSettingsBot struct {
	MatrixAPI
	lock    sync.Mutex
	states  []sentSettingsState
	notices []sentSettingsNotice
}

func (bot *fakeSettingsBot) GetMXID() id.UserID { return "@bot:example.org" }

func (bot *fakeSettingsBot) SendState(_ context.Context, room id.RoomID, evtType event.Type, stateKey string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	bot.lock.Lock()
	defer bot.lock.Unlock()
	bot.states = append(bot.states, sentSettingsState{room, evtType.Type, stateKey, raw})
	return &mautrix.RespSendEvent{EventID: "$state"}, nil
}

func (bot *fakeSettingsBot) SendMessage(_ context.Context, room id.RoomID, _ event.Type, content *event.Content, _ *MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	bot.lock.Lock()
	defer bot.lock.Unlock()
	bot.notices = append(bot.notices, sentSettingsNotice{room, raw})
	return &mautrix.RespSendEvent{EventID: "$notice"}, nil
}

// settingsSent is every settings declaration written to a room, oldest first.
func (bot *fakeSettingsBot) settingsSent(room id.RoomID) (sent [][]byte) {
	bot.lock.Lock()
	defer bot.lock.Unlock()
	for _, state := range bot.states {
		if state.room == room && state.evtType == BridgeSettingsEventType.Type {
			sent = append(sent, state.raw)
		}
	}
	return sent
}

type fakeSettingsMatrix struct {
	MatrixConnector
	bot    *fakeSettingsBot
	levels *event.PowerLevelsEventContent
	reads  int
}

func (m *fakeSettingsMatrix) GetCapabilities() *MatrixCapabilities {
	return &MatrixCapabilities{BatchSending: true}
}

func (m *fakeSettingsMatrix) GetPowerLevels(context.Context, id.RoomID) (*event.PowerLevelsEventContent, error) {
	return m.levels, nil
}

func (m *fakeSettingsMatrix) NewUserIntent(context.Context, id.UserID, string) (MatrixAPI, string, error) {
	return nil, "", nil
}

// GetStateEvent answers from what the bot was told to write, the way a homeserver would.
func (m *fakeSettingsMatrix) GetStateEvent(_ context.Context, room id.RoomID, evtType event.Type, stateKey string) (*event.Event, error) {
	m.bot.lock.Lock()
	defer m.bot.lock.Unlock()
	if evtType == BridgeSettingsEventType {
		m.reads++
	}
	for i := len(m.bot.states) - 1; i >= 0; i-- {
		state := m.bot.states[i]
		if state.room == room && state.evtType == evtType.Type && state.stateKey == stateKey {
			return &event.Event{Type: evtType, RoomID: room, StateKey: &stateKey, Content: event.Content{VeryRaw: state.raw}}, nil
		}
	}
	return nil, mautrix.MNotFound
}

type fakeSettingsNetwork struct{ NetworkConnector }

func (fakeSettingsNetwork) GetName() BridgeName {
	return BridgeName{DisplayName: "Telegram", NetworkID: "telegram"}
}

// A login on a network that can fetch history, and one on a network that cannot.
type fakeBackfillingClient struct{ NetworkAPI }

func (fakeBackfillingClient) IsLoggedIn() bool { return true }
func (fakeBackfillingClient) FetchMessages(context.Context, FetchMessagesParams) (*FetchMessagesResponse, error) {
	return nil, nil
}

type fakePlainClient struct{ NetworkAPI }

func (fakePlainClient) IsLoggedIn() bool { return true }

type settingsHarness struct {
	t      *testing.T
	ctx    context.Context
	br     *Bridge
	bot    *fakeSettingsBot
	matrix *fakeSettingsMatrix
}

func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	ctx := context.Background()
	rawDB, err := sql.Open("sqlite3", ":memory:?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	// One connection, because every new connection to :memory: is a new, empty database.
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = rawDB.Close() })
	db, err := dbutil.NewWithDB(rawDB, "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	bot := &fakeSettingsBot{}
	matrix := &fakeSettingsMatrix{bot: bot, levels: &event.PowerLevelsEventContent{}}
	cfg := &bridgeconfig.BridgeConfig{CommandPrefix: "!tg"}
	cfg.Backfill.Enabled = true
	cfg.Backfill.Queue.Enabled = true
	cfg.Relay.Enabled = true
	br := &Bridge{
		ID:             "test",
		DB:             database.New("test", database.MetaTypes{}, db),
		Matrix:         matrix,
		Bot:            bot,
		Network:        fakeSettingsNetwork{},
		Config:         cfg,
		usersByMXID:    make(map[id.UserID]*User),
		userLoginsByID: make(map[networkid.UserLoginID]*UserLogin),
		portalsByKey:   make(map[networkid.PortalKey]*Portal),
		portalsByMXID:  make(map[id.RoomID]*Portal),
		BackgroundCtx:  ctx,
	}
	if err = br.DB.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	return &settingsHarness{t: t, ctx: ctx, br: br, bot: bot, matrix: matrix}
}

func (h *settingsHarness) user(mxid id.UserID, perms bridgeconfig.Permissions) *User {
	h.t.Helper()
	user := &User{
		User:        &database.User{BridgeID: h.br.ID, MXID: mxid, ManagementRoom: id.RoomID("!mgmt-" + mxid.Localpart() + ":example.org")},
		Bridge:      h.br,
		Permissions: perms,
		logins:      make(map[networkid.UserLoginID]*UserLogin),
	}
	if err := h.br.DB.User.Insert(h.ctx, user.User); err != nil {
		h.t.Fatal(err)
	}
	h.br.usersByMXID[mxid] = user
	return user
}

func (h *settingsHarness) login(user *User, loginID networkid.UserLoginID, client NetworkAPI) *UserLogin {
	h.t.Helper()
	login := &UserLogin{
		UserLogin: &database.UserLogin{BridgeID: h.br.ID, UserMXID: user.MXID, ID: loginID, RemoteName: "Max on Telegram"},
		Bridge:    h.br,
		User:      user,
		Client:    client,
	}
	if err := h.br.DB.UserLogin.Insert(h.ctx, login.UserLogin); err != nil {
		h.t.Fatal(err)
	}
	user.logins[loginID] = login
	h.br.userLoginsByID[loginID] = login
	return login
}

// loggedOut puts the login in the state a network leaves it in when it ends the session.
func (h *settingsHarness) loggedOut(login *UserLogin) {
	login.BridgeState = &BridgeStateQueue{prevSent: &status.BridgeState{StateEvent: status.StateBadCredentials}}
}

// portal makes a chat with a room, with these logins in it.
func (h *settingsHarness) portal(portalID networkid.PortalID, logins ...*UserLogin) *Portal {
	h.t.Helper()
	portal := &Portal{
		Portal: &database.Portal{
			BridgeID:  h.br.ID,
			PortalKey: networkid.PortalKey{ID: portalID},
			MXID:      id.RoomID("!" + string(portalID) + ":example.org"),
		},
		Bridge: h.br,
	}
	if err := h.br.DB.Portal.Insert(h.ctx, portal.Portal); err != nil {
		h.t.Fatal(err)
	}
	h.br.portalsByKey[portal.PortalKey] = portal
	h.br.portalsByMXID[portal.MXID] = portal
	for _, login := range logins {
		err := h.br.DB.UserPortal.Put(h.ctx, &database.UserPortal{
			BridgeID: h.br.ID, UserMXID: login.UserMXID, LoginID: login.ID, Portal: portal.PortalKey,
		})
		if err != nil {
			h.t.Fatal(err)
		}
	}
	return portal
}

func (h *settingsHarness) task(portal *Portal) *database.BackfillTask {
	h.t.Helper()
	task, err := h.br.DB.BackfillTask.GetNextForPortal(h.ctx, portal.PortalKey, true)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

// queued is whether the backfill queue would pick this chat up, which is the thing the control
// claims to decide. Read through the queue's own query rather than off the task's fields.
func (h *settingsHarness) queued(portal *Portal) bool {
	h.t.Helper()
	next, err := h.br.DB.BackfillTask.GetNextUnfinished(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return next != nil && next.PortalKey == portal.PortalKey
}

// set sends a set request the way a client would, and waits for the bridge to have answered it.
func (h *settingsHarness) set(room id.RoomID, sender *User, body string) {
	h.t.Helper()
	evt := &event.Event{Type: event.EventMessage, RoomID: room, Sender: sender.MXID, ID: "$request"}
	if err := json.Unmarshal([]byte(body), &evt.Content); err != nil {
		h.t.Fatal(err)
	}
	if err := evt.Content.ParseRaw(evt.Type); err != nil {
		h.t.Fatal(err)
	}
	req := settingsSetRequest(evt)
	if req == nil {
		h.t.Fatalf("not recognised as a set request: %s", body)
	}
	h.br.applySettingsSet(h.ctx, evt, sender, req)
}

// declared is the chat's controls as the room now holds them.
func (h *settingsHarness) declared(room id.RoomID) *BridgeSettingsContent {
	h.t.Helper()
	sent := h.bot.settingsSent(room)
	if len(sent) == 0 {
		return nil
	}
	var content BridgeSettingsContent
	if err := json.Unmarshal(sent[len(sent)-1], &content); err != nil {
		h.t.Fatal(err)
	}
	return &content
}

// refusal is the code of the last notice the room got, or "" if it got none.
func (h *settingsHarness) refusal(room id.RoomID) string {
	h.t.Helper()
	h.bot.lock.Lock()
	defer h.bot.lock.Unlock()
	for i := len(h.bot.notices) - 1; i >= 0; i-- {
		if h.bot.notices[i].room != room {
			continue
		}
		var notice struct {
			MsgType string                     `json:"msgtype"`
			Body    string                     `json:"body"`
			Error   BridgeSettingsErrorContent `json:"im.mxg.settings.error"`
			Relates struct {
				InReplyTo struct {
					EventID string `json:"event_id"`
				} `json:"m.in_reply_to"`
			} `json:"m.relates_to"`
		}
		if err := json.Unmarshal(h.bot.notices[i].raw, &notice); err != nil {
			h.t.Fatal(err)
		}
		if notice.MsgType != "m.notice" || notice.Body == "" {
			h.t.Errorf("a refusal should be a notice a person can read, got %s", h.bot.notices[i].raw)
		}
		if notice.Relates.InReplyTo.EventID != "$request" {
			h.t.Errorf("a refusal should answer the request it refuses, got %s", h.bot.notices[i].raw)
		}
		return notice.Error.Code
	}
	return ""
}

var userPerms = bridgeconfig.PermissionLevelUser

func TestRegistryRefusesDefinitionsThatCouldNeverWork(t *testing.T) {
	state := func(context.Context, *Bridge, BridgeSettingTarget) (*BridgeSettingState, error) { return nil, nil }
	apply := func(context.Context, *Bridge, BridgeSettingTarget, *User, any) error { return nil }
	good := func() *BridgeSetting {
		return &BridgeSetting{Key: "media_quality", Scope: SettingScopeRoom, Type: SettingTypeBoolean, Label: "Media quality", State: state, Apply: apply}
	}
	for _, tc := range []struct {
		name  string
		spoil func(*BridgeSetting)
	}{
		{"no key", func(s *BridgeSetting) { s.Key = "" }},
		{"a key that needs escaping", func(s *BridgeSetting) { s.Key = "Media Quality" }},
		{"no scope", func(s *BridgeSetting) { s.Scope = "" }},
		{"a scope nobody publishes", func(s *BridgeSetting) { s.Scope = "server" }},
		{"no label", func(s *BridgeSetting) { s.Label = "" }},
		{"a type no client can draw", func(s *BridgeSetting) { s.Type = "slider" }},
		// A float type is not in the set at all: event content cannot carry one.
		{"a float type", func(s *BridgeSetting) { s.Type = "float" }},
		{"an enum with nothing to choose", func(s *BridgeSetting) { s.Type = SettingTypeEnum }},
		{"an enum listing an option twice", func(s *BridgeSetting) {
			s.Type = SettingTypeEnum
			s.Options = []BridgeSettingOption{{Value: "a", Label: "A"}, {Value: "a", Label: "Also A"}}
		}},
		{"an enum option with no label", func(s *BridgeSetting) {
			s.Type = SettingTypeEnum
			s.Options = []BridgeSettingOption{{Value: "a"}}
		}},
		{"options on something that is not an enum", func(s *BridgeSetting) {
			s.Options = []BridgeSettingOption{{Value: "a", Label: "A"}}
		}},
		{"a range with nothing in it", func(s *BridgeSetting) {
			s.Type, s.Min, s.Max = SettingTypeNumber, new(int64(10)), new(int64(1))
		}},
		{"a bound a client cannot hold exactly", func(s *BridgeSetting) {
			s.Type, s.Max = SettingTypeNumber, new(int64(1<<60))
		}},
		{"a range on something that is not a number", func(s *BridgeSetting) { s.Min = new(int64(1)) }},
		{"nothing to read it with", func(s *BridgeSetting) { s.State = nil }},
		{"nothing to change it with", func(s *BridgeSetting) { s.Apply = nil }},
	} {
		setting := good()
		tc.spoil(setting)
		if err := (&BridgeSettingsRegistry{}).Register(setting); err == nil {
			t.Errorf("%s was registered", tc.name)
		}
	}
	registry := &BridgeSettingsRegistry{}
	if err := registry.Register(good()); err != nil {
		t.Fatalf("a sound definition was refused: %v", err)
	}
	// Twice under one key is one control spelled the same way by two parties, in any scope.
	again := good()
	again.Scope = SettingScopeLogin
	if err := registry.Register(again); err == nil {
		t.Error("a key was registered twice")
	}
	if registry.Get(SettingScopeRoom, "media_quality") == nil {
		t.Error("a registered control was not found")
	}
	// Found only in its own scope: a room's control cannot be set through a management room.
	if registry.Get(SettingScopeLogin, "media_quality") != nil {
		t.Error("a room control was found in the login scope")
	}
}

// everyTypeRegistry offers one control of each kind, with fixed state.
func everyTypeRegistry(t *testing.T, days any) *BridgeSettingsRegistry {
	t.Helper()
	apply := func(context.Context, *Bridge, BridgeSettingTarget, *User, any) error { return nil }
	fixed := func(value any) func(context.Context, *Bridge, BridgeSettingTarget) (*BridgeSettingState, error) {
		return func(context.Context, *Bridge, BridgeSettingTarget) (*BridgeSettingState, error) {
			return &BridgeSettingState{Value: value}, nil
		}
	}
	registry := &BridgeSettingsRegistry{}
	for _, setting := range []*BridgeSetting{
		{Key: "relay", Type: SettingTypeBoolean, Label: "Relay", Hint: "Carries other people's messages.", State: fixed(true)},
		{Key: "media_quality", Type: SettingTypeEnum, Label: "Media quality", State: fixed("original"), Options: []BridgeSettingOption{
			{Value: "original", Label: "Original"}, {Value: "compressed", Label: "Compressed"},
		}},
		{Key: "history_days", Type: SettingTypeNumber, Label: "Days to import", State: fixed(days), Min: new(int64(1)), Max: new(int64(365))},
		{Key: "resync", Type: SettingTypeAction, Label: "Sync again", State: fixed(nil)},
		// Not on offer for this target, so it must not appear at all.
		{Key: "absent", Type: SettingTypeBoolean, Label: "Absent", State: func(context.Context, *Bridge, BridgeSettingTarget) (*BridgeSettingState, error) {
			return nil, nil
		}},
		// Another scope's control stays out of this scope's declaration.
		{Key: "account_only", Scope: SettingScopeLogin, Type: SettingTypeBoolean, Label: "Account only", State: fixed(true)},
	} {
		if setting.Scope == "" {
			setting.Scope = SettingScopeRoom
		}
		setting.Apply = apply
		if err := registry.Register(setting); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func TestContentIsTheSameEveryTimeAndHoldsNoFloats(t *testing.T) {
	br := &Bridge{Network: fakeSettingsNetwork{}}
	ctx := context.Background()
	registry := everyTypeRegistry(t, 30)
	first, err := json.Marshal(registry.Content(ctx, br, SettingScopeRoom, BridgeSettingTarget{}))
	if err != nil {
		t.Fatal(err)
	}
	// Built from scratch each time and compared as bytes, because bytes are what decides whether
	// the state event is sent again.
	for range 20 {
		again, err := json.Marshal(registry.Content(ctx, br, SettingScopeRoom, BridgeSettingTarget{}))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("the same controls serialised differently:\n%s\n%s", first, again)
		}
	}
	const want = `{"source":{"id":"telegram","name":"Telegram"},"scope":"room","settings":[` +
		`{"key":"relay","label":"Relay","hint":"Carries other people's messages.","type":"boolean","value":true},` +
		`{"key":"media_quality","label":"Media quality","type":"enum","value":"original","options":[{"value":"original","label":"Original"},{"value":"compressed","label":"Compressed"}]},` +
		`{"key":"history_days","label":"Days to import","type":"number","value":30,"min":1,"max":365},` +
		`{"key":"resync","label":"Sync again","type":"action","value":null}]}`
	if string(first) != want {
		t.Errorf("serialised as\n%s\nwant\n%s", first, want)
	}
	// Every number in it, read as the text it was written as: a float is one with a point or an
	// exponent, and a homeserver refuses the whole event for a single one.
	decoder := json.NewDecoder(bytes.NewReader(first))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		if number, ok := token.(json.Number); ok && strings.ContainsAny(number.String(), ".eE") {
			t.Errorf("the content holds the float %s", number)
		}
	}
}

func TestAControlThatReadsWrongIsLeftOut(t *testing.T) {
	br := &Bridge{Network: fakeSettingsNetwork{}}
	for _, tc := range []struct {
		name string
		days any
	}{
		// Whole or not, a float from a control is a mistake waiting for the day it is not whole.
		{"a fraction", 30.5},
		{"a float that happens to be whole", float64(30)},
		{"a number outside its own range", 400},
		{"the wrong type altogether", "thirty"},
	} {
		content := everyTypeRegistry(t, tc.days).Content(context.Background(), br, SettingScopeRoom, BridgeSettingTarget{})
		if content.Control("history_days") != nil {
			t.Errorf("%s was published", tc.name)
		}
		// The rest of the declaration is unharmed by the one bad control.
		if content.Control("relay") == nil {
			t.Errorf("%s took the other controls down with it", tc.name)
		}
	}
}

func TestUnchangedSettingsAreNotSentAgain(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)

	portal.PublishSettings(h.ctx)
	if got := len(h.bot.settingsSent(portal.MXID)); got != 1 {
		t.Fatalf("the first publish sent %d events, want 1", got)
	}
	if content := h.declared(portal.MXID); content.Scope != SettingScopeRoom || content.Control(SettingKeyRelay) == nil || content.Control(SettingKeyBackfill) == nil {
		t.Fatalf("the chat's declaration is missing a control: %+v", content)
	}
	// State is replicated to every client and kept forever, so saying the same thing again is not
	// free and must not happen.
	for range 5 {
		portal.PublishSettings(h.ctx)
	}
	if got := len(h.bot.settingsSent(portal.MXID)); got != 1 {
		t.Errorf("publishing the same controls again sent %d events, want still 1", got)
	}

	// A restart forgets what was sent. The room has not, so it is asked rather than rewritten.
	h.br.settingsSent = settingsSentCache{}
	readsBefore := h.matrix.reads
	portal.PublishSettings(h.ctx)
	if got := len(h.bot.settingsSent(portal.MXID)); got != 1 {
		t.Errorf("a restart rewrote the room's settings: %d events, want still 1", got)
	}
	if h.matrix.reads != readsBefore+1 {
		t.Errorf("a restart should ask the room once what it holds, asked %d times", h.matrix.reads-readsBefore)
	}
	portal.PublishSettings(h.ctx)
	if h.matrix.reads != readsBefore+1 {
		t.Error("the room was asked again for something already remembered")
	}

	// A real change is sent, once.
	if err := portal.SetRelay(h.ctx, login); err != nil {
		t.Fatal(err)
	}
	portal.PublishSettings(h.ctx)
	if got := len(h.bot.settingsSent(portal.MXID)); got != 2 {
		t.Errorf("a changed control sent %d events in all, want 2", got)
	}
}

func TestNothingToDeclareWritesNothingUntilThereIsSomethingToWithdraw(t *testing.T) {
	h := newSettingsHarness(t)
	h.br.Config.Relay.Enabled = false
	h.br.Config.Backfill.Enabled = false
	user := h.user("@max:example.org", userPerms)
	portal := h.portal("chat", h.login(user, "tg1", fakeBackfillingClient{}))

	// No controls and never any: an empty declaration would be an event in every room of a bridge
	// that offers nothing.
	portal.PublishSettings(h.ctx)
	if got := len(h.bot.settingsSent(portal.MXID)); got != 0 {
		t.Fatalf("a chat with no controls was sent %d declarations", got)
	}

	// Once something was declared, taking it away has to be said, or clients keep offering it.
	h.br.Config.Relay.Enabled = true
	portal.PublishSettings(h.ctx)
	h.br.Config.Relay.Enabled = false
	portal.PublishSettings(h.ctx)
	sent := h.bot.settingsSent(portal.MXID)
	if len(sent) != 2 {
		t.Fatalf("declaring and withdrawing sent %d events, want 2", len(sent))
	}
	if !bytes.Contains(sent[1], []byte(`"settings":[]`)) {
		t.Errorf("the withdrawal should be an empty list, got %s", sent[1])
	}
}

func TestBackfillControlTakesTheChatOutOfTheQueueAndBack(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)
	if err := h.br.DB.BackfillTask.EnsureExists(h.ctx, portal.PortalKey, login.ID); err != nil {
		t.Fatal(err)
	}
	portal.PublishSettings(h.ctx)
	if !h.queued(portal) {
		t.Fatal("the chat should start out in the import queue")
	}
	if got := h.declared(portal.MXID).Control(SettingKeyBackfill); got == nil || got.Value != true {
		t.Fatalf("a queued chat should show its import as on, got %+v", got)
	}

	h.set(portal.MXID, user, `{"msgtype":"im.mxg.settings.set","body":"Import older messages: false","key":"backfill","value":false}`)
	if code := h.refusal(portal.MXID); code != "" {
		t.Fatalf("switching the import off was refused: %s", code)
	}
	if h.queued(portal) {
		t.Error("the chat is still in the import queue after its import was switched off")
	}
	if task := h.task(portal); task == nil || task.BatchCount != -2 {
		t.Errorf("the task should be marked skipped, as `backfill skip` marks it: %+v", task)
	}
	// The acknowledgement is the state, and nothing else.
	if got := h.declared(portal.MXID).Control(SettingKeyBackfill); got == nil || got.Value != false {
		t.Errorf("the room should now show the import as off, got %+v", got)
	}

	h.set(portal.MXID, user, `{"msgtype":"im.mxg.settings.set","body":"Import older messages: true","key":"backfill","value":true}`)
	if !h.queued(portal) {
		t.Error("the chat is not back in the import queue after its import was switched on")
	}
	if got := h.declared(portal.MXID).Control(SettingKeyBackfill); got == nil || got.Value != true {
		t.Errorf("the room should show the import as on again, got %+v", got)
	}
}

func TestBackfillControlFollowsTheTaskWhoeverChangedIt(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)
	read := func() any {
		state, err := roomBackfillState(h.ctx, h.br, BridgeSettingTarget{Portal: portal})
		if err != nil || state == nil {
			t.Fatalf("the control should be on offer: %v %v", state, err)
		}
		return state.Value
	}
	// Never queued: there is history to ask for, and none of it is coming.
	if read() != false {
		t.Error("a chat that was never queued shows its import as on")
	}
	task := &database.BackfillTask{PortalKey: portal.PortalKey, UserLoginID: login.ID, NextDispatchMinTS: time.Now()}
	for _, tc := range []struct {
		name              string
		batches           int
		isDone, queueDone bool
		want              bool
	}{
		{"importing", 3, false, false, true},
		{"finished", 9, true, true, true},
		// What the `backfill skip` command leaves behind.
		{"skipped", -2, false, false, false},
		// The queue gave up before the beginning: more needs asking for.
		{"stopped short", 3, false, true, false},
	} {
		task.BatchCount, task.IsDone, task.QueueDone = tc.batches, tc.isDone, tc.queueDone
		if err := h.br.DB.BackfillTask.Upsert(h.ctx, task); err != nil {
			t.Fatal(err)
		}
		if got := read(); got != tc.want {
			t.Errorf("a chat that is %s shows its import as %v", tc.name, got)
		}
	}
}

func TestBackfillControlIsOnlyOfferedWhereItCanWork(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	plain := h.portal("plain", h.login(user, "sms1", fakePlainClient{}))
	if state, _ := roomBackfillState(h.ctx, h.br, BridgeSettingTarget{Portal: plain}); state != nil {
		t.Error("a chat on a network that cannot fetch history was offered an import switch")
	}
	able := h.portal("able", h.login(user, "tg1", fakeBackfillingClient{}))
	if state, _ := roomBackfillState(h.ctx, h.br, BridgeSettingTarget{Portal: able}); state == nil {
		t.Error("a chat whose network can fetch history was not offered an import switch")
	}
	// With the queue off nothing reads the task, so the switch would be wired to nothing.
	h.br.Config.Backfill.Queue.Enabled = false
	if state, _ := roomBackfillState(h.ctx, h.br, BridgeSettingTarget{Portal: able}); state != nil {
		t.Error("an import switch was offered on a bridge whose import queue is off")
	}
}

func TestRelayControlIsTheSameSwitchAsTheCommands(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)
	// set-relay asks for the room's power to send its pretend state event; so does the switch.
	h.matrix.levels.Users = map[id.UserID]int{user.MXID: 50}
	portal.PublishSettings(h.ctx)
	if got := h.declared(portal.MXID).Control(SettingKeyRelay); got == nil || got.Value != false {
		t.Fatalf("a chat with no relay should show the relay as off, got %+v", got)
	}

	h.set(portal.MXID, user, `{"msgtype":"im.mxg.settings.set","body":"Relay: true","key":"relay","value":true,"target":"@bot:example.org"}`)
	if code := h.refusal(portal.MXID); code != "" {
		t.Fatalf("switching the relay on was refused: %s", code)
	}
	// portal.Relay is what the bridge reads when somebody who is not logged in sends a message.
	if portal.Relay != login {
		t.Fatalf("the portal's relay is %v, want the requester's own login", portal.Relay)
	}
	stored, err := h.br.DB.Portal.GetByKey(h.ctx, portal.PortalKey)
	if err != nil || stored == nil || stored.RelayLoginID != login.ID {
		t.Errorf("the relay was not saved: %+v %v", stored, err)
	}
	on := h.declared(portal.MXID).Control(SettingKeyRelay)
	if on == nil || on.Value != true || !strings.Contains(on.Hint, login.RemoteName) {
		t.Errorf("the room should show the relay as on and say whose account it is, got %+v", on)
	}

	// The other way round: what `unset-relay` does has to show up on the switch, or the two
	// would be two switches.
	if err = portal.SetRelay(h.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := h.declared(portal.MXID).Control(SettingKeyRelay); got == nil || got.Value != false {
		t.Errorf("after the relay was unset as the command unsets it, the room still shows %+v", got)
	}

	h.set(portal.MXID, user, `{"msgtype":"im.mxg.settings.set","key":"relay","value":true}`)
	h.set(portal.MXID, user, `{"msgtype":"im.mxg.settings.set","key":"relay","value":false}`)
	if portal.Relay != nil || portal.RelayLoginID != "" {
		t.Errorf("switching the relay off left it set to %v", portal.Relay)
	}
}

func TestRelayControlIsAbsentWhereRelayingIsOff(t *testing.T) {
	h := newSettingsHarness(t)
	h.br.Config.Relay.Enabled = false
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)
	h.matrix.levels.Users = map[id.UserID]int{user.MXID: 100}
	portal.PublishSettings(h.ctx)
	if h.declared(portal.MXID).Control(SettingKeyRelay) != nil {
		t.Error("a relay switch was offered on a bridge that does not allow relay mode")
	}
	// Asking anyway gets what asking for any control that was never offered gets.
	h.set(portal.MXID, user, `{"msgtype":"im.mxg.settings.set","key":"relay","value":true}`)
	if code := h.refusal(portal.MXID); code != SettingRefusedUnknown {
		t.Errorf("refused as %q, want %q", code, SettingRefusedUnknown)
	}
	if portal.Relay != nil {
		t.Error("a relay was set on a bridge that does not allow relay mode")
	}
}

func TestASetFromTheWrongUserIsRefused(t *testing.T) {
	h := newSettingsHarness(t)
	owner := h.user("@max:example.org", userPerms)
	login := h.login(owner, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)
	if err := h.br.DB.BackfillTask.EnsureExists(h.ctx, portal.PortalKey, login.ID); err != nil {
		t.Fatal(err)
	}
	portal.PublishSettings(h.ctx)
	declaredBefore := len(h.bot.settingsSent(portal.MXID))

	// In the room, allowed to talk to the bridge, but with no account of their own in this chat.
	guest := h.user("@guest:example.org", userPerms)
	h.set(portal.MXID, guest, `{"msgtype":"im.mxg.settings.set","key":"backfill","value":false}`)
	if code := h.refusal(portal.MXID); code != SettingRefusedForbidden {
		t.Errorf("a guest switching off the owner's import was answered %q, want %q", code, SettingRefusedForbidden)
	}
	if !h.queued(portal) {
		t.Error("a guest took the owner's chat out of the import queue")
	}

	// The relay takes the room's power. A member with an account of their own in the chat, so
	// that nothing but the room's power levels stands between them and the relay, does not have it.
	member := h.user("@member:example.org", userPerms)
	memberLogin := h.login(member, "tg2", fakeBackfillingClient{})
	if err := h.br.DB.UserPortal.Put(h.ctx, &database.UserPortal{BridgeID: h.br.ID, UserMXID: member.MXID, LoginID: memberLogin.ID, Portal: portal.PortalKey}); err != nil {
		t.Fatal(err)
	}
	h.set(portal.MXID, member, `{"msgtype":"im.mxg.settings.set","key":"relay","value":true}`)
	if code := h.refusal(portal.MXID); code != SettingRefusedForbidden {
		t.Errorf("a member without the room's power switching the relay on was answered %q, want %q", code, SettingRefusedForbidden)
	}
	if portal.Relay != nil {
		t.Error("a member without the room's power set a relay")
	}

	// Somebody the config does not let send commands cannot send one with a button either - even
	// with an account in the chat, which is everything else the import switch asks for.
	noCommands := h.user("@quiet:example.org", bridgeconfig.Permissions{SendEvents: true, Login: true, ManageRelay: true})
	quietLogin := h.login(noCommands, "tg3", fakeBackfillingClient{})
	if err := h.br.DB.UserPortal.Put(h.ctx, &database.UserPortal{BridgeID: h.br.ID, UserMXID: noCommands.MXID, LoginID: quietLogin.ID, Portal: portal.PortalKey}); err != nil {
		t.Fatal(err)
	}
	h.set(portal.MXID, noCommands, `{"msgtype":"im.mxg.settings.set","key":"backfill","value":false}`)
	if code := h.refusal(portal.MXID); code != SettingRefusedForbidden {
		t.Errorf("a user without command permission was answered %q, want %q", code, SettingRefusedForbidden)
	}
	if !h.queued(portal) {
		t.Error("a user without command permission took the chat out of the import queue")
	}

	// A refusal changes nothing, so it must not look like an acknowledgement.
	if got := len(h.bot.settingsSent(portal.MXID)); got != declaredBefore {
		t.Errorf("refusals rewrote the room's settings %d times", got-declaredBefore)
	}

	// From their own management room, a user reaches only their own accounts - and somebody
	// else's management room is not a place their requests are read as an account's at all.
	stopped := h.portal("stopped", login)
	err := h.br.DB.BackfillTask.Upsert(h.ctx, &database.BackfillTask{
		PortalKey: stopped.PortalKey, UserLoginID: login.ID, BatchCount: 3, QueueDone: true, NextDispatchMinTS: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.set(guest.ManagementRoom, guest, `{"msgtype":"im.mxg.settings.set","key":"backfill_all","value":null,"target":"tg1"}`)
	if code := h.refusal(guest.ManagementRoom); code != SettingRefusedForbidden {
		t.Errorf("a user asking for another user's account was answered %q, want %q", code, SettingRefusedForbidden)
	}
	h.set(owner.ManagementRoom, guest, `{"msgtype":"im.mxg.settings.set","key":"backfill_all","value":null,"target":"tg1"}`)
	if code := h.refusal(owner.ManagementRoom); code == "" {
		t.Error("a request sent into another user's management room was not refused")
	}
	if task := h.task(stopped); !task.QueueDone {
		t.Error("another user resumed the owner's imports")
	}
	// The owner asking the same thing from their own room is what does it.
	h.set(owner.ManagementRoom, owner, `{"msgtype":"im.mxg.settings.set","key":"backfill_all","value":null,"target":"tg1"}`)
	if task := h.task(stopped); task.QueueDone {
		t.Error("the owner could not resume their own imports")
	}
}

func TestARequestInAManagementRoomSaysWhichAccount(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	first := h.login(user, "tg1", fakeBackfillingClient{})
	second := h.login(user, "tg2", fakeBackfillingClient{})
	stopped := func(name networkid.PortalID, login *UserLogin) *Portal {
		portal := h.portal(name, login)
		err := h.br.DB.BackfillTask.Upsert(h.ctx, &database.BackfillTask{
			PortalKey: portal.PortalKey, UserLoginID: login.ID, BatchCount: 3, QueueDone: true, NextDispatchMinTS: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return portal
	}
	ofFirst, ofSecond := stopped("a", first), stopped("b", second)

	// Two accounts and no word on which: guessing would resume the wrong one's imports.
	h.set(user.ManagementRoom, user, `{"msgtype":"im.mxg.settings.set","key":"backfill_all","value":null}`)
	if code := h.refusal(user.ManagementRoom); code != SettingRefusedInvalid {
		t.Errorf("an ambiguous request was answered %q, want %q", code, SettingRefusedInvalid)
	}
	// The target is the state key the control was read from, which here is the login.
	h.set(user.ManagementRoom, user, `{"msgtype":"im.mxg.settings.set","key":"backfill_all","value":null,"target":"tg2"}`)
	if task := h.task(ofSecond); task.QueueDone {
		t.Error("the account that was named did not have its imports resumed")
	}
	if task := h.task(ofFirst); !task.QueueDone {
		t.Error("the account that was not named had its imports resumed")
	}
}

func TestInvalidRequestsAreAnsweredAndChangeNothing(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)
	h.matrix.levels.Users = map[id.UserID]int{user.MXID: 100}
	for _, tc := range []struct {
		name, body, code string
	}{
		{"a key that was never declared", `{"msgtype":"im.mxg.settings.set","key":"wipe_everything","value":true}`, SettingRefusedUnknown},
		{"a string for a boolean", `{"msgtype":"im.mxg.settings.set","key":"relay","value":"true"}`, SettingRefusedInvalid},
		{"no value", `{"msgtype":"im.mxg.settings.set","key":"relay"}`, SettingRefusedInvalid},
		{"no key", `{"msgtype":"im.mxg.settings.set","value":true}`, SettingRefusedUnknown},
		// A login's control is not reachable from a chat's room.
		{"the wrong scope", `{"msgtype":"im.mxg.settings.set","key":"backfill_all","value":null}`, SettingRefusedUnknown},
	} {
		h.bot.notices = nil
		h.set(portal.MXID, user, tc.body)
		if code := h.refusal(portal.MXID); code != tc.code {
			t.Errorf("%s was answered %q, want %q", tc.name, code, tc.code)
		}
	}
	if portal.Relay != nil {
		t.Error("an invalid request set the relay")
	}

	// Addressed to another bridge sharing the room: not ours to act on, and not ours to answer.
	h.bot.notices = nil
	h.set(portal.MXID, user, `{"msgtype":"im.mxg.settings.set","key":"relay","value":true,"target":"@whatsappbot:example.org"}`)
	if portal.Relay != nil || len(h.bot.notices) != 0 {
		t.Error("a request for another bridge was acted on or answered")
	}
}

func TestOnlyASetRequestIsTakenOutOfTheStream(t *testing.T) {
	message := func(body string) *event.Event {
		evt := &event.Event{Type: event.EventMessage}
		if err := json.Unmarshal([]byte(body), &evt.Content); err != nil {
			t.Fatal(err)
		}
		if err := evt.Content.ParseRaw(evt.Type); err != nil {
			t.Fatal(err)
		}
		return evt
	}
	if settingsSetRequest(message(`{"msgtype":"m.text","body":"im.mxg.settings.set"}`)) != nil {
		t.Error("an ordinary message was taken for a set request")
	}
	req := settingsSetRequest(message(`{"msgtype":"im.mxg.settings.set","body":"Days: 7","key":"history_days","value":7,"target":"@bot:example.org"}`))
	if req == nil || req.Key != "history_days" || req.Value != float64(7) || req.Target != "@bot:example.org" {
		t.Errorf("a set request was read as %+v", req)
	}
}

func TestBackfillAllResumesOnlyTheChatsThatStoppedShort(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	other := h.login(h.user("@other:example.org", userPerms), "tg2", fakeBackfillingClient{})
	never := time.Unix(0, 1<<62)
	chat := func(name networkid.PortalID, owner *UserLogin, batches int, isDone, queueDone bool) *Portal {
		portal := h.portal(name, owner)
		err := h.br.DB.BackfillTask.Upsert(h.ctx, &database.BackfillTask{
			PortalKey: portal.PortalKey, UserLoginID: owner.ID, BatchCount: batches,
			IsDone: isDone, QueueDone: queueDone, NextDispatchMinTS: never,
		})
		if err != nil {
			t.Fatal(err)
		}
		return portal
	}
	stopped := chat("stopped", login, 3, false, true)
	skipped := chat("skipped", login, -2, false, false)
	finished := chat("finished", login, 9, true, true)
	theirs := chat("theirs", other, 3, false, true)
	unqueued := h.portal("unqueued", login)

	loggedIn := BridgeSettingTarget{Login: login, LoggedIn: true}
	state, err := loginBackfillAllState(h.ctx, h.br, loggedIn)
	if err != nil || state == nil || state.DisabledReason != "" {
		t.Fatalf("a logged-in login should be offered the button: %+v %v", state, err)
	}
	// Withdrawn, in the bridge's words, once the network has ended the session.
	state, _ = loginBackfillAllState(h.ctx, h.br, BridgeSettingTarget{Login: login})
	if state == nil || state.DisabledReason == "" {
		t.Errorf("a logged-out login's button should be disabled with a reason, got %+v", state)
	}
	h.loggedOut(login)
	h.set(user.ManagementRoom, user, `{"msgtype":"im.mxg.settings.set","key":"backfill_all","value":null}`)
	if code := h.refusal(user.ManagementRoom); code != SettingRefusedDisabled {
		t.Errorf("pressing a disabled button was answered %q, want %q", code, SettingRefusedDisabled)
	}
	if task := h.task(stopped); !task.QueueDone {
		t.Fatal("a disabled button resumed an import")
	}
	// A login that is only reconnecting keeps its button: what it asks for waits in the queue.
	for _, state := range []status.BridgeStateEvent{status.StateConnected, status.StateConnecting, status.StateTransientDisconnect, status.StateBackfilling, status.StateUnknownError} {
		if !loginStillLoggedIn(state) {
			t.Errorf("a login that is %s was treated as logged out", state)
		}
	}
	for _, state := range []status.BridgeStateEvent{status.StateBadCredentials, status.StateLoggedOut} {
		if loginStillLoggedIn(state) {
			t.Errorf("a login that is %s was treated as logged in", state)
		}
	}

	if err = applyLoginBackfillAll(h.ctx, h.br, loggedIn, user, nil); err != nil {
		t.Fatal(err)
	}
	if task := h.task(stopped); task.QueueDone || task.IsDone || !task.NextDispatchMinTS.Before(time.Now().Add(time.Second)) {
		t.Errorf("the chat that stopped short was not made due: %+v", task)
	}
	if task := h.task(unqueued); task == nil || task.UserLoginID != login.ID {
		t.Errorf("the chat that was never queued did not get a task: %+v", task)
	}
	// The user's own earlier choice stands.
	if task := h.task(skipped); task.BatchCount != -2 {
		t.Errorf("a chat the user chose to skip was reopened: %+v", task)
	}
	// The network already said it has nothing older; asking again is a request for nothing.
	if task := h.task(finished); !task.IsDone {
		t.Errorf("a finished chat was reopened: %+v", task)
	}
	if task := h.task(theirs); !task.QueueDone {
		t.Errorf("another user's chat was resumed: %+v", task)
	}
}

func TestLoginSettingsArePublishedPerLoginAndOnlyOnChange(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	room := user.ManagementRoom

	h.br.publishLoginSettings(h.ctx, room, login, true)
	h.br.publishLoginSettings(h.ctx, room, login, true)
	sent := h.bot.settingsSent(room)
	if len(sent) != 1 {
		t.Fatalf("a login that stayed logged in was declared %d times, want 1", len(sent))
	}
	const want = `{"source":{"id":"telegram","name":"Telegram"},"scope":"login","settings":[` +
		`{"key":"backfill_all","label":"Import the rest of every chat",` +
		`"hint":"Carries on with every chat whose import stopped before the beginning. Chats you chose not to import are left alone.",` +
		`"type":"action","value":null}]}`
	if string(sent[0]) != want {
		t.Errorf("declared as\n%s\nwant\n%s", sent[0], want)
	}
	h.bot.lock.Lock()
	stateKey := h.bot.states[0].stateKey
	h.bot.lock.Unlock()
	if stateKey != "tg1" {
		t.Errorf("a login's declaration is keyed %q, want its login ID", stateKey)
	}
	// Being logged out withdraws the button, which is a change and is said.
	h.br.publishLoginSettings(h.ctx, room, login, false)
	if got := h.declared(room).Control(SettingKeyBackfillAll); got == nil || got.DisabledReason != "Logged out of Telegram" {
		t.Errorf("a logged-out login's button should say why it is inert, got %+v", got)
	}
}

func TestPortalSettingsAreKeyedByTheBot(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	portal := h.portal("chat", h.login(user, "tg1", fakeBackfillingClient{}))
	portal.PublishSettings(h.ctx)
	h.bot.lock.Lock()
	defer h.bot.lock.Unlock()
	if len(h.bot.states) != 1 || h.bot.states[0].stateKey != "@bot:example.org" {
		t.Errorf("a chat's declaration should be one event keyed by the bridge bot, got %+v", h.bot.states)
	}
	const want = `{"source":{"id":"telegram","name":"Telegram"},"scope":"room","settings":[` +
		`{"key":"backfill","label":"Import older messages",` +
		`"hint":"Off leaves this chat's older history on the other network and takes it out of the import queue. On imports all of it.",` +
		`"type":"boolean","value":false},` +
		`{"key":"relay","label":"Relay other people's messages",` +
		`"hint":"Lets people in this room who have not logged in write to the chat: their messages are sent through a logged-in account, with their name in front.",` +
		`"type":"boolean","value":false}]}`
	if got := string(h.bot.states[0].raw); got != want {
		t.Errorf("declared as\n%s\nwant\n%s", got, want)
	}
}

func TestTheBackfillCommandMovesTheSwitchToo(t *testing.T) {
	h := newSettingsHarness(t)
	user := h.user("@max:example.org", userPerms)
	login := h.login(user, "tg1", fakeBackfillingClient{})
	portal := h.portal("chat", login)
	if err := h.br.DB.BackfillTask.EnsureExists(h.ctx, portal.PortalKey, login.ID); err != nil {
		t.Fatal(err)
	}
	portal.PublishBackfillStatus(h.ctx, login, false)
	if got := h.declared(portal.MXID).Control(SettingKeyBackfill); got == nil || got.Value != true {
		t.Fatalf("a queued chat should show its import as on, got %+v", got)
	}
	// What `backfill skip` does: mark the task, then publish the chat's import status. The switch
	// has to follow, or it would go on showing an import the command had just called off.
	if err := h.br.DB.BackfillTask.Skip(h.ctx, portal.PortalKey, login.ID); err != nil {
		t.Fatal(err)
	}
	portal.PublishBackfillStatus(h.ctx, login, true)
	if got := h.declared(portal.MXID).Control(SettingKeyBackfill); got == nil || got.Value != false {
		t.Errorf("after the import was skipped by command the room still shows %+v", got)
	}
	// A status that only counts further along is not a reason to look at the controls again.
	sent := len(h.bot.settingsSent(portal.MXID))
	portal.PublishBackfillStatus(h.ctx, login, true)
	if got := len(h.bot.settingsSent(portal.MXID)); got != sent {
		t.Errorf("an unchanged import state rewrote the controls %d times", got-sent)
	}
}

type recordingCommands struct {
	lock    sync.Mutex
	handled []string
}

func (rc *recordingCommands) Handle(_ context.Context, _ id.RoomID, _ id.EventID, _ *User, message string, _ id.EventID) {
	rc.lock.Lock()
	defer rc.lock.Unlock()
	rc.handled = append(rc.handled, message)
}

func TestASetRequestIsNotACommandOrAMessage(t *testing.T) {
	h := newSettingsHarness(t)
	commands := &recordingCommands{}
	h.br.Commands = commands
	user := h.user("@max:example.org", userPerms)
	h.login(user, "tg1", fakeBackfillingClient{})
	evt := &event.Event{Type: event.EventMessage, RoomID: user.ManagementRoom, Sender: user.MXID, ID: "$request"}
	const body = `{"msgtype":"im.mxg.settings.set","body":"Import the rest of every chat: go","key":"no_such_setting","value":null}`
	if err := json.Unmarshal([]byte(body), &evt.Content); err != nil {
		t.Fatal(err)
	}
	if err := evt.Content.ParseRaw(evt.Type); err != nil {
		t.Fatal(err)
	}
	// In a management room every message is a command. This one must not be: its body is there for
	// clients that cannot read the msgtype, and "unknown command" is no answer to a button.
	if res := h.br.QueueMatrixEvent(h.ctx, evt); !res.Queued {
		t.Errorf("the request was not taken: %+v", res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.refusal(user.ManagementRoom) == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if code := h.refusal(user.ManagementRoom); code != SettingRefusedUnknown {
		t.Errorf("the request was answered %q, want %q", code, SettingRefusedUnknown)
	}
	commands.lock.Lock()
	defer commands.lock.Unlock()
	if len(commands.handled) != 0 {
		t.Errorf("the request was also run as a command: %q", commands.handled)
	}
}
