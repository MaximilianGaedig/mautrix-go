// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/event"
)

func TestBeaconContent(t *testing.T) {
	ts := time.UnixMilli(1_700_000_000_000)
	info, err := json.Marshal(event.BeaconInfoContent("Live location", true, ts, 15*time.Minute))
	require.NoError(t, err)
	// What Element's MBeaconBody reads.
	assert.JSONEq(t, `{
		"description": "Live location",
		"live": true,
		"timeout": 900000,
		"org.matrix.msc3488.ts": 1700000000000,
		"org.matrix.msc3488.asset": {"type": "m.self"}
	}`, string(info))

	beacon, err := json.Marshal(event.BeaconContent("$info", event.GeoURI(52.5, 13.4, 12), ts))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"m.relates_to": {"rel_type": "m.reference", "event_id": "$info"},
		"org.matrix.msc3488.location": {"uri": "geo:52.500000,13.400000;u=12"},
		"org.matrix.msc3488.ts": 1700000000000
	}`, string(beacon))
	assert.Equal(t, "geo:1.000000,2.000000", event.GeoURI(1, 2, 0))
}
