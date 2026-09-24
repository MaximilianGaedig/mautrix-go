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
	"slices"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// AudioTrackID returns the msid track id of the first audio m-line, which is
// what the web client uses as the key of JoinRequest.mediaStatus.
func AudioTrackID(sdp string) string {
	inAudio := false
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "m="):
			if inAudio {
				return ""
			}
			inAudio = strings.HasPrefix(line, "m=audio")
		case inAudio && strings.HasPrefix(line, "a=msid:"):
			parts := strings.Fields(strings.TrimPrefix(line, "a=msid:"))
			if len(parts) == 2 {
				return parts[1]
			}
		}
	}
	return ""
}

// RedactSDP drops an SDP's ICE password and x-dtls-auth, for logging.
func RedactSDP(sdp string) string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "a=ice-pwd:") && !strings.HasPrefix(line, "a=x-dtls-auth:") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// SSRCCname returns the cname of the SDP's first a=ssrc line. A client's cname names it in
// end-to-end encrypted group calls: its E2EE id is "<userId>:<cname>".
func SSRCCname(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "a=ssrc:") {
			continue
		}
		if _, v, ok := strings.Cut(line, " cname:"); ok && v != "" {
			return v
		}
	}
	return ""
}

// E2eeIDOfStream returns the E2EE id ("<userId>:<cname>") of a group call participant's track
// from its msid stream id: the SFU names remote streams "<ownerUserId>:<senderCname>:<streamId>".
func E2eeIDOfStream(streamID string) string {
	parts := strings.SplitN(streamID, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + ":" + parts[1]
}

// DisableVideoSending rewrites the direction of video m-lines so the SDP never
// claims to send video: the bridge has no video track, but Pion answers a
// recvonly video offer with sendonly once video codecs are registered. The
// web client itself answers inactive.
func DisableVideoSending(sdp string) string {
	lines := strings.SplitAfter(sdp, "\n")
	inVideo := false
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		nl := line[len(trimmed):]
		switch {
		case strings.HasPrefix(trimmed, "m="):
			inVideo = strings.HasPrefix(trimmed, "m=video")
		case inVideo && trimmed == "a=sendonly":
			lines[i] = "a=inactive" + nl
		case inVideo && trimmed == "a=sendrecv":
			lines[i] = "a=recvonly" + nl
		}
	}
	return strings.Join(lines, "")
}

// PickVideoCodec chooses the video codec for bridging a call whose video is
// described by sdp: VP8 when offered (every browser running Element has it),
// else H264, else "" (no video). It only looks at the video m-section.
func PickVideoCodec(sdp string) string {
	codecs := videoRtpmap(sdp)
	switch {
	case codecs["VP8"]:
		return webrtc.MimeTypeVP8
	case codecs["H264"]:
		return webrtc.MimeTypeH264
	default:
		return ""
	}
}

// SendsVideo reports whether sdp has a video m-section that sends (sendrecv
// or sendonly) and isn't disabled (port 0).
func SendsVideo(sdp string) bool {
	inVideo, sending := false, false
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "m="):
			if inVideo && sending {
				return true
			}
			inVideo = strings.HasPrefix(line, "m=video") && !strings.HasPrefix(line, "m=video 0 ")
			sending = inVideo // sendrecv is the default direction
		case inVideo && (line == "a=recvonly" || line == "a=inactive"):
			sending = false
		}
	}
	return inVideo && sending
}

func videoRtpmap(sdp string) map[string]bool {
	out := map[string]bool{}
	inVideo := false
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "m="):
			inVideo = strings.HasPrefix(line, "m=video") && !strings.HasPrefix(line, "m=video 0 ")
		case inVideo && strings.HasPrefix(line, "a=rtpmap:"):
			fields := strings.Fields(line)
			if len(fields) == 2 {
				out[strings.ToUpper(strings.SplitN(fields[1], "/", 2)[0])] = true
			}
		}
	}
	return out
}

// LogSDPShape adds the shape of an SDP to a log event without anything
// identifying (no addresses, ICE credentials, fingerprints or keys): per
// m-section the kind, direction, codecs, SSRC count and whether it uses
// simulcast/RIDs, plus the header extensions and the semantics hint.
func LogSDPShape(e *zerolog.Event, sdp string) *zerolog.Event {
	type section struct {
		Kind      string   `json:"kind"`
		Port0     bool     `json:"port0,omitempty"`
		Dir       string   `json:"dir,omitempty"`
		MID       string   `json:"mid,omitempty"`
		Codecs    []string `json:"codecs,omitempty"`
		SSRCs     int      `json:"ssrcs,omitempty"`
		MSIDs     int      `json:"msids,omitempty"`
		Simulcast bool     `json:"simulcast,omitempty"`
		Exts      []string `json:"exts,omitempty"`
	}
	var secs []*section
	var cur *section
	ssrcs := map[string]bool{}
	// Names only, never values: enough to spot e.g. end-to-end encryption markers.
	attrs := map[string]bool{}
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "a=") {
			name := line[2:]
			if i := strings.IndexAny(name, ": "); i >= 0 {
				name = name[:i]
			}
			attrs[name] = true
		}
		switch {
		case strings.HasPrefix(line, "m="):
			f := strings.Fields(line[2:])
			cur = &section{Kind: f[0], Port0: len(f) > 1 && f[1] == "0"}
			secs = append(secs, cur)
			ssrcs = map[string]bool{}
		case cur == nil:
		case line == "a=sendrecv" || line == "a=sendonly" || line == "a=recvonly" || line == "a=inactive":
			cur.Dir = line[2:]
		case strings.HasPrefix(line, "a=mid:"):
			cur.MID = line[6:]
		case strings.HasPrefix(line, "a=rtpmap:"):
			if f := strings.Fields(line); len(f) == 2 {
				cur.Codecs = append(cur.Codecs, strings.TrimPrefix(f[0], "a=rtpmap:")+" "+f[1])
			}
		case strings.HasPrefix(line, "a=ssrc:"):
			if f := strings.Fields(line[7:]); len(f) > 0 && !ssrcs[f[0]] {
				ssrcs[f[0]] = true
				cur.SSRCs++
			}
		case strings.HasPrefix(line, "a=msid:"):
			cur.MSIDs++
		case strings.HasPrefix(line, "a=simulcast") || strings.HasPrefix(line, "a=rid:"):
			cur.Simulcast = true
		case strings.HasPrefix(line, "a=extmap:"):
			if f := strings.Fields(line); len(f) >= 2 {
				cur.Exts = append(cur.Exts, f[1])
			}
		}
	}
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	slices.Sort(names)
	return e.Interface("sdp_shape", secs).Strs("sdp_attrs", names)
}

// IsPlanB reports whether an SDP uses Plan B: a media section carrying more
// than one track (several distinct msid track ids; RTX/FEC SSRCs of one track
// share its msid), or the Plan B section names "audio"/"video" as mids.
func IsPlanB(sdp string) bool {
	tracks := map[string]bool{}
	check := func() bool { return len(tracks) > 1 }
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "m="):
			if check() {
				return true
			}
			tracks = map[string]bool{}
		case strings.HasPrefix(line, "a=mid:"):
			// Pion's own test (descriptionIsPlanB): any mid named audio, video or data, in any case.
			switch strings.ToLower(strings.TrimSpace(line[len("a=mid:"):])) {
			case "audio", "video", "data":
				return true
			}
		case strings.HasPrefix(line, "a=msid:"):
			if f := strings.Fields(line[len("a=msid:"):]); len(f) == 2 {
				tracks[f[1]] = true
			}
		case strings.HasPrefix(line, "a=ssrc:") && strings.Contains(line, " msid:"):
			if f := strings.Fields(line[strings.Index(line, " msid:")+len(" msid:"):]); len(f) == 2 {
				tracks[f[1]] = true
			}
		}
	}
	return check()
}

// MIDs lists the a=mid of each media section in order ("" for a section without one).
func MIDs(sdp string) []string {
	var mids []string
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "m="):
			mids = append(mids, "")
		case strings.HasPrefix(line, "a=mid:") && len(mids) > 0:
			mids[len(mids)-1] = strings.TrimSpace(line[len("a=mid:"):])
		}
	}
	return mids
}

// RenameMIDs rewrites the a=mid lines and the BUNDLE group of an SDP with mapping (old -> new).
func RenameMIDs(sdp string, mapping map[string]string) string {
	lines := strings.SplitAfter(sdp, "\n")
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		eol := line[len(body):]
		switch {
		case strings.HasPrefix(body, "a=mid:"):
			if to, ok := mapping[strings.TrimSpace(body[len("a=mid:"):])]; ok {
				lines[i] = "a=mid:" + to + eol
			}
		case strings.HasPrefix(body, "a=group:"):
			f := strings.Fields(body)
			for j := 1; j < len(f); j++ {
				if to, ok := mapping[f[j]]; ok {
					f[j] = to
				}
			}
			lines[i] = strings.Join(f, " ") + eol
		}
	}
	return strings.Join(lines, "")
}

// PlanBToUnifiedMIDs maps the mids of a Plan B description (Messenger's mobile apps name them
// audio/video/data) to the ones a Unified Plan connection uses at the same m-line positions (from
// its current local description), so the connection can take the Plan B side's renegotiation. New
// trailing sections get their position as mid. Renaming our answer with the inverse mapping gives
// the Plan B side its own names back. With one track per section (what a 1:1 call has), that is the
// whole difference between the two.
func PlanBToUnifiedMIDs(remote, local string) map[string]string {
	remoteMIDs, localMIDs := MIDs(remote), MIDs(local)
	used := map[string]bool{}
	for _, mid := range localMIDs {
		used[mid] = true
	}
	mapping := map[string]string{}
	for i, from := range remoteMIDs {
		if from == "" {
			continue
		}
		var to string
		if i < len(localMIDs) && localMIDs[i] != "" {
			to = localMIDs[i]
		} else {
			to = strconv.Itoa(i)
			for used[to] {
				to += "_"
			}
			used[to] = true
		}
		if to != from {
			mapping[from] = to
		}
	}
	return mapping
}

// InvertMIDs swaps a mid mapping's keys and values.
func InvertMIDs(mapping map[string]string) map[string]string {
	out := make(map[string]string, len(mapping))
	for from, to := range mapping {
		out[to] = from
	}
	return out
}

// CollapseSimulcast keeps one track per media section of a Plan B offer: where a section sends
// simulcast (a=ssrc-group:SIM low mid high), only the first layer stays, with its RTX partner
// (a=ssrc-group:FID) and its a=ssrc lines. Pion takes several SSRCs of one section as several
// tracks, which it rejects on a Unified Plan connection; the SFU-less 1:1 call needs one layer anyway.
func CollapseSimulcast(sdp string) string {
	lines := strings.SplitAfter(sdp, "\n")
	type section struct{ start, end int }
	var secs []section
	for i, line := range lines {
		if strings.HasPrefix(line, "m=") {
			if len(secs) > 0 {
				secs[len(secs)-1].end = i
			}
			secs = append(secs, section{start: i, end: len(lines)})
		}
	}
	drop := map[int]bool{}
	for _, sec := range secs {
		var sim []string
		fid := map[string]string{} // primary -> rtx
		for i := sec.start; i < sec.end; i++ {
			body := strings.TrimRight(lines[i], "\r\n")
			if strings.HasPrefix(body, "a=ssrc-group:SIM ") {
				sim = strings.Fields(body)[1:]
			} else if f := strings.Fields(body); len(f) == 3 && f[0] == "a=ssrc-group:FID" {
				fid[f[1]] = f[2]
			}
		}
		if len(sim) < 2 {
			continue
		}
		keep := map[string]bool{sim[0]: true}
		if rtx, ok := fid[sim[0]]; ok {
			keep[rtx] = true
		}
		for i := sec.start; i < sec.end; i++ {
			body := strings.TrimRight(lines[i], "\r\n")
			switch {
			case strings.HasPrefix(body, "a=ssrc-group:SIM "):
				drop[i] = true
			case strings.HasPrefix(body, "a=ssrc-group:FID "):
				if f := strings.Fields(body); len(f) == 3 && !keep[f[1]] {
					drop[i] = true
				}
			case strings.HasPrefix(body, "a=ssrc:"):
				if f := strings.Fields(body[len("a=ssrc:"):]); len(f) > 0 && !keep[f[0]] {
					drop[i] = true
				}
			}
		}
	}
	var b strings.Builder
	for i, line := range lines {
		if !drop[i] {
			b.WriteString(line)
		}
	}
	return b.String()
}

// AdaptPlanBOffer makes a Plan B renegotiation offer acceptable to a Unified Plan connection whose
// current local description is `local`: one simulcast layer per section, and our mids. It returns
// the offer and the mapping to undo on our answer (RenameMIDs(answer, InvertMIDs(mapping))).
func AdaptPlanBOffer(offer, local string) (string, map[string]string) {
	offer = CollapseSimulcast(offer)
	mapping := PlanBToUnifiedMIDs(offer, local)
	return RenameMIDs(offer, mapping), mapping
}

// SDPVersion is the session version from the o= line, which Messenger's
// clients use as the media state version of a CLIENT_MEDIA_UPDATE carrying
// that SDP (web client getSdpVersion: line 2, third token).
func SDPVersion(sdp string) (int64, error) {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "o=") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return 0, fmt.Errorf("short o= line: %d fields", len(f))
		}
		return strconv.ParseInt(f[2], 10, 64)
	}
	return 0, errors.New("no o= line")
}

// WithCandidatesFrom copies the ICE candidate lines of `from` (the current
// local description) into every media section of `offer` that has none, the
// way Pion's LocalDescription fills them in: CreateOffer's SDP has none, and
// a renegotiation reuses the same ICE session.
func WithCandidatesFrom(offer, from string) string {
	var cands []string
	inMedia := false
	for _, line := range strings.Split(from, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "m=") {
			if inMedia && len(cands) > 0 {
				break // the first media section's candidates (all of them under BUNDLE)
			}
			inMedia = true
			continue
		}
		if inMedia && (strings.HasPrefix(line, "a=candidate:") || line == "a=end-of-candidates") {
			cands = append(cands, line)
		}
	}
	if len(cands) == 0 {
		return offer
	}
	return withCandidateLines(offer, cands)
}

// withCandidateLines adds the candidate lines (a=candidate:…, a=end-of-candidates)
// to every media section of `offer` that has none.
func withCandidateLines(offer string, cands []string) string {
	sections := strings.Split(offer, "\r\nm=")
	for i := 1; i < len(sections); i++ {
		if strings.Contains(sections[i], "\r\na=candidate:") {
			continue
		}
		sec := strings.TrimSuffix(sections[i], "\r\n")
		sections[i] = sec + "\r\n" + strings.Join(cands, "\r\n")
		if i < len(sections)-1 || strings.HasSuffix(offer, "\r\n") {
			sections[i] += "\r\n"
		}
	}
	return strings.Join(sections, "\r\nm=")
}
