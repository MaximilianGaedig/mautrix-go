// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// BackfillStatusEventType is the room state event in which the bridge records how much of a chat's
// history it has imported, so clients can tell "this is everything" from "there is more to load".
var BackfillStatusEventType = event.Type{Type: "im.mxg.backfill", Class: event.StateEventType}

// Values of [BackfillStatusContent.State].
const (
	// BackfillStateComplete: the network said there is nothing older than what was imported.
	BackfillStateComplete = "complete"
	// BackfillStateRunning: older history is being imported (or is queued to be).
	BackfillStateRunning = "running"
	// BackfillStateManual: importing more needs a manual request (the network's fetches are slow).
	BackfillStateManual = "manual"
	// BackfillStateSkipped: the user chose not to import this chat's older history.
	BackfillStateSkipped = "skipped"
	// BackfillStateUnavailable: the network offers no way to fetch this chat's older history, so
	// what was imported is what exists.
	BackfillStateUnavailable = "unavailable"
)

// BackfillStatusContent is the content of the [BackfillStatusEventType] state event.
type BackfillStatusContent struct {
	State string `json:"state"`
	// Messages of this chat the bridge has imported.
	BridgedMessages int `json:"bridged_messages"`
	// The times of the oldest and newest imported message, in milliseconds.
	OldestTS int64 `json:"oldest_ts,omitempty"`
	NewestTS int64 `json:"newest_ts,omitempty"`
	// How many messages the network says the chat has, when it can say.
	RemoteTotal *int `json:"remote_total,omitempty"`
	// How many batches of older history have been imported so far.
	Batches int `json:"batches"`
	// Whether this chat is being imported right now (a batch was fetched in the last two minutes), as
	// opposed to waiting its turn in the queue. Only meaningful in the running state.
	Active bool `json:"active,omitempty"`
	// For a chat waiting its turn: how many chats are in front of it, and how many wait in all.
	QueueAhead int `json:"queue_ahead,omitempty"`
	QueueSize  int `json:"queue_size,omitempty"`
	// Messages imported per minute since this import was first seen running, once that is long enough
	// to mean something.
	// Messages per minute, a whole number: Matrix event content may not hold floats (servers answer 400).
	RatePerMinute int64 `json:"rate_per_min,omitempty"`
	// What to send in the room to ask for the rest: "<prefix> backfill".
	CommandPrefix string `json:"command_prefix"`
	Network       string `json:"network"`
	UpdatedTS     int64  `json:"updated_ts"`
}

// BackfillCountingNetworkAPI is implemented by network connectors that can say how many messages a
// chat has on the network, which lets the bridge show, and check, that all of them were imported.
type BackfillCountingNetworkAPI interface {
	NetworkAPI
	// CountRemoteMessages returns the number of messages the chat has on the network.
	CountRemoteMessages(ctx context.Context, portal *Portal) (int, error)
}

type backfillStatusState struct {
	lock        sync.Mutex
	last        *BackfillStatusContent
	lastSent    time.Time
	remoteTotal *int
	// Whether the total published earlier has been looked for in the room's state yet (once per run).
	remoteLoaded bool
	// When the network was last asked for the total, so a failing count isn't retried on every update.
	remoteTriedAt time.Time
	// Where the import's pace is measured from: the first time it was seen running, and its count then.
	rateSince time.Time
	rateCount int
}

// How often the count may be refreshed while a backfill is running; state changes go out at once.
const backfillStatusMinInterval = 30 * time.Second

func (portal *Portal) computeBackfillStatus(ctx context.Context, source *UserLogin, withRemote bool) (*BackfillStatusContent, error) {
	db := portal.Bridge.DB
	count, err := db.Message.CountMessagesInPortal(ctx, portal.PortalKey)
	if err != nil {
		return nil, err
	}
	status := &BackfillStatusContent{
		BridgedMessages: count,
		CommandPrefix:   portal.Bridge.Config.CommandPrefix,
		Network:         portal.Bridge.Network.GetName().DisplayName,
		UpdatedTS:       time.Now().UnixMilli(),
	}
	if first, err := db.Message.GetFirstPortalMessage(ctx, portal.PortalKey); err == nil && first != nil {
		status.OldestTS = first.Timestamp.UnixMilli()
	}
	if last, err := db.Message.GetLastNInPortal(ctx, portal.PortalKey, 1); err == nil && len(last) > 0 {
		status.NewestTS = last[0].Timestamp.UnixMilli()
	}
	task, err := db.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, true)
	if err != nil {
		return nil, err
	}
	switch {
	case task == nil || task.UserLoginID == "":
		// No task yet is not the same as nothing to fetch: if the network can fetch history at all, the
		// chat simply hasn't been queued, and asking for it (or the next resync) will.
		status.State = BackfillStateUnavailable
		if portal.Bridge.Config.Backfill.Enabled {
			for _, login := range portal.Bridge.GetAllCachedUserLogins() {
				if _, ok := login.Client.(BackfillingNetworkAPI); ok {
					status.State = BackfillStateManual
					break
				}
			}
		}
	case task.IsDone:
		status.State = BackfillStateComplete
		status.Batches = task.BatchCount
	case task.BatchCount == -2:
		status.State = BackfillStateSkipped
		status.Batches = task.BatchCount
	case task.QueueDone:
		status.State = BackfillStateManual
		status.Batches = task.BatchCount
	default:
		status.State = BackfillStateRunning
		status.Batches = task.BatchCount
		status.Active = !task.DispatchedAt.IsZero() && time.Since(task.DispatchedAt) < 2*time.Minute
		if !status.Active {
			if ahead, total, err := db.BackfillTask.QueuePosition(ctx, task); err == nil {
				status.QueueAhead, status.QueueSize = ahead, total
			}
		}
	}

	// Pace: measured from when this process first saw the chat importing, and only once that is long
	// enough (and moved enough) to be a rate; a chat that stops being imported starts over.
	portal.backfillStatus.lock.Lock()
	if status.State == BackfillStateRunning && status.Active {
		if portal.backfillStatus.rateSince.IsZero() {
			portal.backfillStatus.rateSince = time.Now()
			portal.backfillStatus.rateCount = count
		} else if elapsed := time.Since(portal.backfillStatus.rateSince); elapsed >= time.Minute && count > portal.backfillStatus.rateCount {
			status.RatePerMinute = int64(math.Round(float64(count-portal.backfillStatus.rateCount) / elapsed.Minutes()))
		}
	} else if status.State != BackfillStateRunning {
		portal.backfillStatus.rateSince = time.Time{}
	}
	portal.backfillStatus.lock.Unlock()

	portal.backfillStatus.lock.Lock()
	status.RemoteTotal = portal.backfillStatus.remoteTotal
	loadRemote := status.RemoteTotal == nil && !portal.backfillStatus.remoteLoaded
	portal.backfillStatus.lock.Unlock()
	// A total the bridge worked out before outlives this process in the room's own state, so it is read
	// back once per run. Without this a chat loses its "x of y" on every restart, and for good once the
	// network can no longer count it - Signal empties the backup archive as it imports it, so a chat that
	// finished can never be counted again. It also saves asking networks that charge API calls to count.
	if loadRemote && portal.MXID != "" {
		if dp, ok := source.doublePuppetForStatus(ctx); ok {
			var previous BackfillStatusContent
			if err := dp.GetRoomAccountData(ctx, portal.MXID, BackfillStatusEventType.Type, &previous); err != nil {
				zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to read the chat's last import status")
			} else {
				portal.rememberPrevious(&previous, status)
			}
		} else if api, ok := portal.Bridge.Matrix.(MatrixConnectorWithArbitraryRoomState); ok {
			if evt, err := api.GetStateEvent(ctx, portal.MXID, BackfillStatusEventType, ""); err != nil {
				zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to read the chat's last import status")
			} else if evt != nil {
				var previous BackfillStatusContent
				if err := json.Unmarshal(evt.Content.VeryRaw, &previous); err != nil {
					zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to parse the chat's last import status")
				} else {
					portal.rememberPrevious(&previous, status)
				}
			}
		}
		portal.backfillStatus.lock.Lock()
		portal.backfillStatus.remoteLoaded = true
		portal.backfillStatus.lock.Unlock()
	}
	// The total is what makes "x of y" and a time estimate possible, so it is fetched once while the
	// import runs (and again, if asked, when it ends), not on every update.
	portal.backfillStatus.lock.Lock()
	needTotal := status.RemoteTotal == nil && status.State == BackfillStateRunning &&
		time.Since(portal.backfillStatus.remoteTriedAt) > 10*time.Minute
	if needTotal || withRemote {
		portal.backfillStatus.remoteTriedAt = time.Now()
	}
	portal.backfillStatus.lock.Unlock()
	if (withRemote || needTotal) && source != nil {
		if counter, ok := source.Client.(BackfillCountingNetworkAPI); ok {
			if total, err := counter.CountRemoteMessages(ctx, portal); err != nil {
				zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to count the chat's messages on the network")
			} else {
				portal.backfillStatus.lock.Lock()
				portal.backfillStatus.remoteTotal = &total
				portal.backfillStatus.lock.Unlock()
				status.RemoteTotal = &total
			}
		}
	}
	return status, nil
}

// ComputeBackfillStatus reports how much of the portal's history has been imported.
func (portal *Portal) ComputeBackfillStatus(ctx context.Context, source *UserLogin, withRemote bool) (*BackfillStatusContent, error) {
	return portal.computeBackfillStatus(ctx, source, withRemote)
}

// PublishBackfillStatus records the portal's import progress in its room. Changes of state go out at
// once; a growing count at most every half minute. force sends it regardless.
func (portal *Portal) PublishBackfillStatus(ctx context.Context, source *UserLogin, force bool) {
	if portal.MXID == "" || portal.Bridge.IsStopping() {
		return
	}
	log := zerolog.Ctx(ctx).With().Str("action", "publish backfill status").Logger()
	ctx = log.WithContext(ctx)
	state := &portal.backfillStatus
	status, err := portal.computeBackfillStatus(ctx, source, force)
	if err != nil {
		log.Err(err).Msg("Failed to compute backfill status")
		return
	}
	state.lock.Lock()
	defer state.lock.Unlock()
	if state.last != nil {
		unchanged := state.last.State == status.State && state.last.BridgedMessages == status.BridgedMessages &&
			state.last.Batches == status.Batches && state.last.Active == status.Active
		// Nothing to say is nothing to send, even when forced: this state event is rewritten often and
		// every copy stays in the room's timeline for every client to store. A bridge restart used to
		// republish an identical status for every chat it has.
		if unchanged {
			return
		}
		if !force && time.Since(state.lastSent) < backfillStatusMinInterval && state.last.State == status.State {
			return
		}
	}
	if err := portal.writeBackfillStatus(ctx, source, status); err != nil {
		log.Err(err).Msg("Failed to send backfill status")
		return
	}
	state.last = status
	state.lastSent = time.Now()
}

// doublePuppetForStatus is the user's own intent, when the bridge has one and it can carry the status.
func (ul *UserLogin) doublePuppetForStatus(ctx context.Context) (RoomAccountDataMatrixAPI, bool) {
	if ul == nil {
		return nil, false
	}
	dp, ok := ul.User.DoublePuppet(ctx).(RoomAccountDataMatrixAPI)
	return dp, ok
}

// rememberPrevious carries what was published before into this run: the total the network gave then,
// which it may not be able to give again, and the content itself so an identical status isn't rewritten.
func (portal *Portal) rememberPrevious(previous, status *BackfillStatusContent) {
	portal.backfillStatus.lock.Lock()
	defer portal.backfillStatus.lock.Unlock()
	portal.backfillStatus.last = previous
	if previous.RemoteTotal != nil {
		portal.backfillStatus.remoteTotal = previous.RemoteTotal
		status.RemoteTotal = previous.RemoteTotal
	}
}

// writeBackfillStatus puts the status where the user's clients will see it.
//
// It belongs to the user, not to the room: nobody else in a chat needs to know how much of its history
// has been imported. Written as the user's own account data for the room it syncs just as well while
// staying out of the room's timeline, where a status rewritten as an import progresses would otherwise
// pile up in every client's copy of the room forever (measured at over a third of all replayed timeline
// events) and leave the bridge bot with a read marker at the bottom of every chat.
//
// Without a double puppet the bridge cannot write the user's account data, so it falls back to the room
// state the bot can write.
func (portal *Portal) writeBackfillStatus(ctx context.Context, source *UserLogin, status *BackfillStatusContent) error {
	if dp, ok := source.doublePuppetForStatus(ctx); ok {
		return dp.SetRoomAccountData(ctx, portal.MXID, BackfillStatusEventType.Type, status)
	}
	_, err := portal.Bridge.Bot.SendState(
		ctx, portal.MXID, BackfillStatusEventType, "", &event.Content{Parsed: status}, time.Time{},
	)
	return err
}

// PublishAllBackfillStatuses brings every portal's status event up to date: at startup, and after
// a bulk change to backfill tasks.
func (br *Bridge) PublishAllBackfillStatuses(ctx context.Context) {
	portals, err := br.GetAllPortalsWithMXID(ctx)
	if err != nil {
		br.Log.Err(err).Msg("Failed to list portals to publish backfill statuses")
		return
	}
	for _, portal := range portals {
		if br.IsStopping() || ctx.Err() != nil {
			return
		}
		var source *UserLogin
		if task, err := br.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, true); err == nil && task != nil && task.UserLoginID != "" {
			source, _ = br.GetExistingUserLoginByID(ctx, task.UserLoginID)
		}
		// Ask the network for the chat's total too (when the connector can say), so "x of y" is there from
		// the start and for chats that finished before totals were tracked.
		portal.PublishBackfillStatus(ctx, source, true)
		time.Sleep(50 * time.Millisecond)
	}
	br.Log.Info().Int("portals", len(portals)).Msg("Published backfill statuses")
}

// RequestFullBackfill makes the queue import the portal's whole remaining history.
func (portal *Portal) RequestFullBackfill(ctx context.Context, login networkid.UserLoginID) error {
	if err := portal.Bridge.DB.BackfillTask.EnsureExists(ctx, portal.PortalKey, login); err != nil {
		return err
	}
	if err := portal.Bridge.DB.BackfillTask.RequestFull(ctx, portal.PortalKey, login); err != nil {
		return err
	}
	portal.Bridge.WakeupBackfillQueue()
	return nil
}

// ChatCountingNetworkAPI is implemented by network connectors that can say how many chats the account
// has on the network, which lets the bridge check that every one of them has a room.
type ChatCountingNetworkAPI interface {
	NetworkAPI
	// CountRemoteChats returns how many chats the account has on the network.
	CountRemoteChats(ctx context.Context) (int, error)
}

// BackfillAudit is a summary of how completely a bridge has imported its account's chats.
type BackfillAudit struct {
	// Chats the network says the account has (-1 if the connector can't say), and portals with a room.
	RemoteChats int
	Portals     int
	WithRoom    int
	// Portals by import state.
	ByState map[string]int
	// Messages imported, and the network's own total across the chats that reported one.
	BridgedMessages int
	RemoteMessages  int
	// Chats whose network total is known, and how many of those have fewer imported than the network has.
	Counted    int
	Incomplete []AuditChat
}

// AuditChat is one chat that hasn't been fully imported.
type AuditChat struct {
	PortalID    string
	State       string
	Bridged     int
	RemoteTotal *int
}

// AuditBackfill checks every portal of the bridge: the state of its import and, when the network can
// say, whether as many messages were imported as the chat has. withRemote asks the network per chat,
// which takes a request each.
func (br *Bridge) AuditBackfill(ctx context.Context, withRemote bool) (*BackfillAudit, error) {
	audit := &BackfillAudit{ByState: map[string]int{}, RemoteChats: -1}
	portals, err := br.DB.Portal.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	audit.Portals = len(portals)
	for _, dbPortal := range portals {
		if dbPortal.MXID == "" {
			continue
		}
		audit.WithRoom++
		portal, err := br.GetExistingPortalByKey(ctx, dbPortal.PortalKey)
		if err != nil || portal == nil {
			continue
		}
		var source *UserLogin
		if task, err := br.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, true); err == nil && task != nil && task.UserLoginID != "" {
			source, _ = br.GetExistingUserLoginByID(ctx, task.UserLoginID)
		}
		status, err := portal.computeBackfillStatus(ctx, source, withRemote)
		if err != nil {
			continue
		}
		audit.ByState[status.State]++
		audit.BridgedMessages += status.BridgedMessages
		incomplete := status.State != BackfillStateComplete && status.State != BackfillStateUnavailable && status.State != BackfillStateSkipped
		if status.RemoteTotal != nil {
			audit.Counted++
			audit.RemoteMessages += *status.RemoteTotal
			// The network counts service messages the bridge doesn't import, so a few fewer is normal;
			// a chat is short when it is missing more than one in twenty.
			if status.BridgedMessages*20 < *status.RemoteTotal*19 {
				incomplete = true
			}
		}
		if incomplete {
			audit.Incomplete = append(audit.Incomplete, AuditChat{
				PortalID: string(portal.ID), State: status.State, Bridged: status.BridgedMessages, RemoteTotal: status.RemoteTotal,
			})
		}
	}
	if withRemote {
		for _, login := range br.GetAllCachedUserLogins() {
			if counter, ok := login.Client.(ChatCountingNetworkAPI); ok {
				if n, err := counter.CountRemoteChats(ctx); err == nil {
					if audit.RemoteChats < 0 {
						audit.RemoteChats = 0
					}
					audit.RemoteChats += n
				}
			}
		}
	}
	return audit, nil
}
