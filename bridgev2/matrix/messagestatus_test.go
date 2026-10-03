// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package matrix

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestSendsStatusEvent(t *testing.T) {
	all := &bridgeconfig.MatrixConfig{MessageStatusEvents: true}
	failuresOnly := &bridgeconfig.MatrixConfig{MessageStatusEvents: true, MessageStatusFailuresOnly: true}
	off := &bridgeconfig.MatrixConfig{MessageStatusFailuresOnly: true}

	success := &bridgev2.MessageStatus{Status: event.MessageStatusSuccess}
	delivered := &bridgev2.MessageStatus{Status: event.MessageStatusSuccess, DeliveredTo: []id.UserID{"@ghost:example.com"}}
	failed := &bridgev2.MessageStatus{Status: event.MessageStatusFail}
	pending := &bridgev2.MessageStatus{Status: event.MessageStatusPending}

	assert.True(t, sendsStatusEvent(all, success))
	assert.False(t, sendsStatusEvent(failuresOnly, success), "a plain success stays out of the room")
	assert.True(t, sendsStatusEvent(failuresOnly, delivered), "delivery to the recipient is still told")
	assert.True(t, sendsStatusEvent(failuresOnly, failed))
	assert.True(t, sendsStatusEvent(failuresOnly, pending))
	assert.False(t, sendsStatusEvent(off, failed))
	assert.False(t, sendsStatusEvent(all, &bridgev2.MessageStatus{Status: event.MessageStatusFail, DisableMSS: true}))
}
