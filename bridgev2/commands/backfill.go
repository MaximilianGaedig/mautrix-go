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
		Args:        "[status|skip]",
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
	case bridgev2.BackfillStateSkipped:
		sb.WriteString("Importing this chat's older history was skipped. Send `$cmdprefix backfill` to import it.")
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
	if len(ce.Args) > 0 && strings.EqualFold(ce.Args[0], "skip") {
		if err := ce.Bridge.DB.BackfillTask.EnsureExists(ce.Ctx, ce.Portal.PortalKey, login.ID); err != nil {
			ce.Reply("Failed to skip the import: %v", err)
			return
		}
		if err := ce.Bridge.DB.BackfillTask.Skip(ce.Ctx, ce.Portal.PortalKey, login.ID); err != nil {
			ce.Reply("Failed to skip the import: %v", err)
			return
		}
		go ce.Portal.PublishBackfillStatus(ce.Bridge.BackgroundCtx, login, true)
		ce.Reply("Older history of this chat will not be imported. Send `$cmdprefix backfill` to import it after all.")
		return
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

var CommandAuditBackfill = &FullHandler{
	Func: func(ce *Event) {
		ce.Reply("Checking every chat. Asking the network for each chat's message count takes a while.")
		go func() {
			audit, err := ce.Bridge.AuditBackfill(ce.Bridge.BackgroundCtx, true)
			if err != nil {
				ce.Reply("The check failed: %v", err)
				return
			}
			var sb strings.Builder
			if audit.RemoteChats >= 0 {
				fmt.Fprintf(&sb, "**Chats:** %d on %s, %d bridged", audit.RemoteChats, ce.Bridge.Network.GetName().DisplayName, audit.WithRoom)
				if audit.WithRoom < audit.RemoteChats {
					fmt.Fprintf(&sb, " (**%d missing**)", audit.RemoteChats-audit.WithRoom)
				}
				sb.WriteString("\n\n")
			} else {
				fmt.Fprintf(&sb, "**Chats bridged:** %d (the network can't say how many exist)\n\n", audit.WithRoom)
			}
			fmt.Fprintf(&sb, "**History:** %d complete, %d importing, %d need a request, %d have nothing older to give\n\n",
				audit.ByState[bridgev2.BackfillStateComplete], audit.ByState[bridgev2.BackfillStateRunning],
				audit.ByState[bridgev2.BackfillStateManual], audit.ByState[bridgev2.BackfillStateUnavailable])
			fmt.Fprintf(&sb, "**Messages:** %d imported", audit.BridgedMessages)
			if audit.Counted > 0 {
				fmt.Fprintf(&sb, "; the network counts %d in the %d chats it can count", audit.RemoteMessages, audit.Counted)
			}
			sb.WriteString("\n\n")
			if len(audit.Incomplete) == 0 {
				sb.WriteString("Nothing is missing.")
			} else {
				fmt.Fprintf(&sb, "**%d chats not fully imported:**\n", len(audit.Incomplete))
				for i, chat := range audit.Incomplete {
					if i == 30 {
						fmt.Fprintf(&sb, "- …and %d more\n", len(audit.Incomplete)-30)
						break
					}
					if chat.RemoteTotal != nil {
						fmt.Fprintf(&sb, "- `%s`: %d of %d (%s)\n", chat.PortalID, chat.Bridged, *chat.RemoteTotal, chat.State)
					} else {
						fmt.Fprintf(&sb, "- `%s`: %d imported (%s)\n", chat.PortalID, chat.Bridged, chat.State)
					}
				}
			}
			ce.Reply("%s", sb.String())
		}()
	},
	Name: "audit-backfill",
	Help: HelpMeta{
		Section:     HelpSectionAdmin,
		Description: "Check that every chat and message of the account has been imported",
	},
	RequiresAdmin: true,
}
