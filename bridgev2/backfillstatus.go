// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
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
		status.State = BackfillStateUnavailable
	case task.IsDone:
		status.State = BackfillStateComplete
		status.Batches = task.BatchCount
	case task.QueueDone:
		status.State = BackfillStateManual
		status.Batches = task.BatchCount
	default:
		status.State = BackfillStateRunning
		status.Batches = task.BatchCount
	}

	portal.backfillStatus.lock.Lock()
	status.RemoteTotal = portal.backfillStatus.remoteTotal
	portal.backfillStatus.lock.Unlock()
	if withRemote && source != nil {
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
	if !force && state.last != nil {
		unchanged := state.last.State == status.State && state.last.BridgedMessages == status.BridgedMessages &&
			state.last.Batches == status.Batches
		recent := time.Since(state.lastSent) < backfillStatusMinInterval
		if unchanged || (recent && state.last.State == status.State) {
			return
		}
	}
	_, err = portal.Bridge.Bot.SendState(ctx, portal.MXID, BackfillStatusEventType, "", &event.Content{Parsed: status}, time.Time{})
	if err != nil {
		log.Err(err).Msg("Failed to send backfill status")
		return
	}
	state.last = status
	state.lastSent = time.Now()
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
		portal.PublishBackfillStatus(ctx, source, false)
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
