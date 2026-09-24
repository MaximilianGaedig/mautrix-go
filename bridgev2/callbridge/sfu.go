// mautrix - A full-featured Matrix client library.
// Copyright (C) 2026 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package callbridge

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// MediaDescriptionUpdate is one changed media section in an SDP delta.
type MediaDescriptionUpdate struct {
	Body string
	MSID string
	MID  string
}

// SessionDescriptionUpdate is a set of media sections keyed by m-line index.
type SessionDescriptionUpdate struct {
	Media map[int32]MediaDescriptionUpdate
}

// Indexes returns the changed m-line indexes in order.
func (u *SessionDescriptionUpdate) Indexes() []int32 {
	return slices.Sorted(maps.Keys(u.Media))
}

// splitSDP cuts an SDP into its session part and its media sections, each
// ending in CRLF.
func splitSDP(sdp string) (session string, media []string) {
	sdp = strings.ReplaceAll(sdp, "\r\n", "\n")
	parts := strings.Split(sdp, "\nm=")
	session = parts[0]
	for _, p := range parts[1:] {
		media = append(media, "m="+p)
	}
	crlf := func(s string) string {
		s = strings.TrimRight(s, "\n")
		return strings.ReplaceAll(s, "\n", "\r\n") + "\r\n"
	}
	session = crlf(session)
	for i := range media {
		media[i] = crlf(media[i])
	}
	return session, media
}

func sectionAttr(section, name string) string {
	for _, line := range strings.Split(section, "\r\n") {
		if v, ok := strings.CutPrefix(line, "a="+name+":"); ok {
			return v
		}
	}
	return ""
}

// ApplySDPDelta applies a delta SERVER_MEDIA_UPDATE (the SFU adding or
// changing m-sections) to the current remote description and returns the
// SFU's new offer, like the web client's applyRemoteOfferAndSetLocalAnswer
// (ZenonSDP.updateMSection, updateMids, updateMsidSemantic): an m-line index
// inside the current list replaces that section, any other is appended; the
// BUNDLE group gains the delta's mids; the WMS token lists every stream id.
func ApplySDPDelta(remote string, update *SessionDescriptionUpdate) (string, error) {
	if update == nil || len(update.Media) == 0 {
		return "", errors.New("empty SDP delta")
	}
	session, media := splitSDP(remote)
	var newMids []string
	for _, idx := range update.Indexes() {
		body := update.Media[idx].Body
		_, parsed := splitSDP("v=0\r\n" + strings.TrimPrefix(body, "\r\n"))
		if len(parsed) < 1 {
			return "", fmt.Errorf("could not parse body of delta m-line %d", idx)
		}
		section := parsed[0]
		if int(idx) < len(media) {
			media[idx] = section
		} else {
			media = append(media, section)
		}
		if mid := sectionAttr(section, "mid"); mid != "" {
			newMids = append(newMids, mid)
		} else if mid = update.Media[idx].MID; mid != "" {
			newMids = append(newMids, mid)
		}
	}

	var streams []string
	for _, m := range media {
		if msid := sectionAttr(m, "msid"); msid != "" {
			if stream := strings.Fields(msid)[0]; stream != "-" && !slices.Contains(streams, stream) {
				streams = append(streams, stream)
			}
		}
	}

	lines := strings.Split(strings.TrimSuffix(session, "\r\n"), "\r\n")
	sawBundle, sawSemantic := false, false
	for i, line := range lines {
		if mids, ok := strings.CutPrefix(line, "a=group:BUNDLE"); ok {
			sawBundle = true
			all := strings.Fields(mids)
			for _, mid := range newMids {
				if !slices.Contains(all, mid) {
					all = append(all, mid)
				}
			}
			lines[i] = "a=group:BUNDLE " + strings.Join(all, " ")
		} else if strings.HasPrefix(line, "a=msid-semantic:") {
			sawSemantic = true
			lines[i] = strings.TrimSpace("a=msid-semantic: WMS " + strings.Join(streams, " "))
		}
	}
	if !sawBundle {
		lines = append(lines, "a=group:BUNDLE "+strings.Join(newMids, " "))
	}
	if !sawSemantic {
		lines = append(lines, strings.TrimSpace("a=msid-semantic: WMS "+strings.Join(streams, " ")))
	}
	return strings.Join(lines, "\r\n") + "\r\n" + strings.Join(media, ""), nil
}

// TrackOwners maps the msid track ids of an SFU description to the Messenger
// user id owning them: the SFU names the owner as the first token of the
// msid stream id and of the ssrc cname ("<userId>:<cname>:<stream> <track>").
// The SMU mediaStatus owner field is authoritative when present; this covers
// the rest.
func TrackOwners(sdp string) map[string]string {
	out := map[string]string{}
	_, media := splitSDP(sdp)
	for _, m := range media {
		msid := sectionAttr(m, "msid")
		f := strings.Fields(msid)
		if len(f) != 2 {
			continue
		}
		if owner, _, ok := strings.Cut(f[0], ":"); ok && owner != "" {
			out[f[1]] = owner
		}
	}
	return out
}
