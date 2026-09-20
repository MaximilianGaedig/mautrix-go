// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package commands

import (
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
)

var CommandBackfill = &FullHandler{
	Func: fnBackfill,
	Name: "backfill",
	Help: HelpMeta{
		Section:     HelpSectionChats,
		Description: "Import this chat's whole history (`backfill`), or show how much of it has been imported (`backfill status`)",
		Args:        "[status]",
	},
	RequiresPortal: true,
	RequiresLogin:  true,
}

func backfillStatusText(status *bridgev2.BackfillStatusContent) string {
	var sb strings.Builder
	switch status.State {
	case bridgev2.BackfillStateComplete:
		sb.WriteString("All of this chat's history has been imported.")
	case bridgev2.BackfillStateRunning:
		sb.WriteString("Older history is still being imported.")
	case bridgev2.BackfillStateManual:
		sb.WriteString("There is older history that needs a manual request to import. Send `$cmdprefix backfill`.")
	default:
		sb.WriteString("The network does not offer older history for this chat, so what is here is all there is.")
	}
	fmt.Fprintf(&sb, "\n\n%d messages imported", status.BridgedMessages)
	if status.RemoteTotal != nil {
		fmt.Fprintf(&sb, " of %d on %s", *status.RemoteTotal, status.Network)
	}
	if status.OldestTS > 0 {
		fmt.Fprintf(&sb, ", back to %s", time.UnixMilli(status.OldestTS).UTC().Format("2006-01-02"))
	}
	sb.WriteString(".")
	return sb.String()
}

func fnBackfill(ce *Event) {
	login := ce.User.GetDefaultLogin()
	if task, err := ce.Bridge.DB.BackfillTask.GetNextForPortal(ce.Ctx, ce.Portal.PortalKey, true); err == nil && task != nil && task.UserLoginID != "" {
		if taskLogin, _ := ce.Bridge.GetExistingUserLoginByID(ce.Ctx, task.UserLoginID); taskLogin != nil {
			login = taskLogin
		}
	}
	if len(ce.Args) > 0 && strings.EqualFold(ce.Args[0], "status") {
		status, err := ce.Portal.ComputeBackfillStatus(ce.Ctx, login, true)
		if err != nil {
			ce.Reply("Failed to look up the import status: %v", err)
			return
		}
		ce.Reply("%s", backfillStatusText(status))
		return
	}
	if err := ce.Portal.RequestFullBackfill(ce.Ctx, login.ID); err != nil {
		ce.Reply("Failed to request the import: %v", err)
		return
	}
	go ce.Portal.PublishBackfillStatus(ce.Bridge.BackgroundCtx, login, true)
	ce.Reply("Importing this chat's whole history. This can take a while for a long chat; send `$cmdprefix backfill status` to see how far it is.")
}

var CommandBackfillAll = &FullHandler{
	Func: func(ce *Event) {
		if err := ce.Bridge.DB.BackfillTask.RequestFullAll(ce.Ctx); err != nil {
			ce.Reply("Failed to request the import: %v", err)
			return
		}
		ce.Bridge.WakeupBackfillQueue()
		go ce.Bridge.PublishAllBackfillStatuses(ce.Bridge.BackgroundCtx)
		ce.Reply("Importing the whole history of every chat.")
	},
	Name: "backfill-all",
	Help: HelpMeta{
		Section:     HelpSectionAdmin,
		Description: "Import the whole history of every bridged chat",
	},
	RequiresAdmin: true,
}
