// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package event

import (
	"fmt"
	"time"

	"maunium.net/go/mautrix/id"
)

// BeaconInfoContent is the content of an MSC3672 beacon_info state event: a live location share by the
// state key's user, which lasts timeout from ts unless it's stopped earlier (live false).
func BeaconInfoContent(description string, live bool, ts time.Time, timeout time.Duration) map[string]any {
	content := map[string]any{
		"live":                     live,
		"timeout":                  timeout.Milliseconds(),
		"org.matrix.msc3488.ts":    ts.UnixMilli(),
		"org.matrix.msc3488.asset": map[string]any{"type": "m.self"},
	}
	if description != "" {
		content["description"] = description
	}
	return content
}

// GeoURI is an RFC 5870 geo: URI, with the uncertainty in metres when it's known.
func GeoURI(lat, long, accuracy float64) string {
	if accuracy > 0 {
		return fmt.Sprintf("geo:%f,%f;u=%.0f", lat, long, accuracy)
	}
	return fmt.Sprintf("geo:%f,%f", lat, long)
}

// BeaconContent is the content of an MSC3672 beacon event: one position of the live location share started
// by the beacon_info event beaconInfo.
func BeaconContent(beaconInfo id.EventID, geoURI string, ts time.Time) map[string]any {
	return map[string]any{
		"m.relates_to": map[string]any{
			"rel_type": RelReference,
			"event_id": beaconInfo,
		},
		"org.matrix.msc3488.location": map[string]any{"uri": geoURI},
		"org.matrix.msc3488.ts":       ts.UnixMilli(),
	}
}
