// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package event_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestPollEndParses(t *testing.T) {
	content := event.Content{VeryRaw: []byte(`{
		"m.relates_to": {"rel_type": "m.reference", "event_id": "$poll"},
		"org.matrix.msc3381.poll.end": {},
		"org.matrix.msc1767.text": "The poll has ended."
	}`)}
	require.NoError(t, content.ParseRaw(event.EventUnstablePollEnd))
	end, ok := content.Parsed.(*event.PollEndEventContent)
	require.True(t, ok, "parsed as %T", content.Parsed)
	assert.Equal(t, id.EventID("$poll"), end.RelatesTo.GetReferenceID())
	assert.Equal(t, "The poll has ended.", end.GetText())
}
