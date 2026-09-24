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
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// loopbackSettings keeps ICE on 127.0.0.1 without mDNS, so tests need no
// network.
func loopbackSettings() *webrtc.SettingEngine {
	se := &webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetInterfaceFilter(func(name string) bool { return name == "lo" || name == "lo0" })
	return se
}

func newTestLeg(t *testing.T, name string, pt uint8, webShape bool) *Leg {
	t.Helper()
	l, err := NewLeg(LegConfig{Name: name, OpusPT: pt, WebShape: webShape, Settings: loopbackSettings(), Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

// mLines returns "kind direction" per m-line of an SDP.
func mLines(sdp string) []string {
	var out []string
	for _, sec := range strings.Split(sdp, "\r\nm=")[1:] {
		kind, _, _ := strings.Cut(sec, " ")
		sec += "\r\n"
		dir := ""
		if kind == "application" {
			out = append(out, kind)
			continue
		}
		for _, d := range []string{"sendrecv", "sendonly", "recvonly", "inactive"} {
			if strings.Contains(sec, "\r\na="+d+"\r\n") {
				dir = d
			}
		}
		out = append(out, strings.TrimSpace(kind+" "+dir))
	}
	return out
}

// TestWebShapedSDP checks a browser-shaped offer and answer: audio, video and
// application m-lines in one BUNDLE with Opus 111.
func TestWebShapedSDP(t *testing.T) {
	web := newTestLeg(t, "web", OpusPT, true)
	webOffer, err := web.CreateOffer()
	if err != nil {
		t.Fatal(err)
	}
	if got := mLines(webOffer); fmt.Sprint(got) != "[audio sendrecv video recvonly application]" {
		t.Fatalf("offer m-lines: %v", got)
	}
	if strings.Contains(webOffer, "ssrc-audio-level") {
		t.Error("a 1:1 leg's offer declares the SFU header extensions")
	}

	meta := newTestLeg(t, "meta", OpusPT, true)
	answer, err := meta.AnswerOffer(webOffer)
	if err != nil {
		t.Fatal(err)
	}
	answer = DisableVideoSending(answer)
	if got := mLines(answer); fmt.Sprint(got) != "[audio sendrecv video inactive application]" {
		t.Errorf("answer m-lines: %v", got)
	}
	for _, want := range []string{"a=group:BUNDLE 0 1 2", "a=rtpmap:111 opus/48000/2", "a=msid:" + meta.StreamID + " " + meta.TrackID} {
		if !strings.Contains(answer, want+"\r\n") {
			t.Errorf("answer lacks %q", want)
		}
	}
	if AudioTrackID(answer) != meta.TrackID {
		t.Errorf("AudioTrackID = %q", AudioTrackID(answer))
	}
	if err = web.SetAnswer(answer); err != nil {
		t.Fatalf("web peer rejects the answer: %v", err)
	}

	// As caller the bridge offers the same shape.
	caller := newTestLeg(t, "meta-caller", OpusPT, true)
	offer, err := caller.CreateOffer()
	if err != nil {
		t.Fatal(err)
	}
	if got := mLines(offer); fmt.Sprint(got) != "[audio sendrecv video recvonly application]" {
		t.Errorf("caller offer m-lines: %v", got)
	}
}

func TestRewriterSourceSwitch(t *testing.T) {
	var rw Rewriter
	pkt := func(ssrc uint32, seq uint16, ts uint32) *rtp.Packet {
		p := &rtp.Packet{Header: rtp.Header{SSRC: ssrc, SequenceNumber: seq, Timestamp: ts}}
		_ = p.Header.SetExtension(1, []byte{0x80})
		return p
	}
	p1 := pkt(1, 100, 5000)
	rw.Rewrite(p1)
	p2 := pkt(1, 101, 5960)
	rw.Rewrite(p2)
	if p2.SequenceNumber != 101 || p2.Timestamp != 5960 || p2.Extension || p2.Extensions != nil {
		t.Fatalf("same-source packet changed: %+v", p2.Header)
	}
	p3 := pkt(2, 60000, 123)
	rw.Rewrite(p3)
	if p3.SequenceNumber != 102 || p3.Timestamp != 5960+opusFrameTicks {
		t.Fatalf("switched source not re-based: seq %d ts %d", p3.SequenceNumber, p3.Timestamp)
	}
	p4 := pkt(2, 60001, 123+960)
	rw.Rewrite(p4)
	if p4.SequenceNumber != 103 || p4.Timestamp != 5960+2*opusFrameTicks {
		t.Fatalf("continuation wrong: seq %d ts %d", p4.SequenceNumber, p4.Timestamp)
	}
}

// connect runs a full offer/answer (no trickle) between an offerer and an
// answerer leg.
func connect(t *testing.T, ctx context.Context, offerer, answerer *Leg) {
	t.Helper()
	if _, err := offerer.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	offer := offerer.WaitGathering(ctx, 5*time.Second)
	if _, err := answerer.AnswerOffer(offer); err != nil {
		t.Fatal(err)
	}
	answer := answerer.WaitGathering(ctx, 5*time.Second)
	if err := offerer.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
}

// TestLoopbackRelay connects two local peers through the bridge's two legs
// and the relay, with different Opus payload types on each side, and checks
// that Opus payloads arrive intact in both directions with the bridge's own
// SSRCs and the receiving leg's payload type.
func TestLoopbackRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log := zerolog.Nop()

	// Messenger side: peer A offers with PT 111, the bridge's meta leg answers.
	peerA := newTestLeg(t, "peerA", 111, true)
	metaLeg := newTestLeg(t, "meta", 111, true)
	// Matrix side: the bridge's matrix leg offers with PT 109, peer B answers.
	mxLeg := newTestLeg(t, "matrix", 109, false)
	peerB := newTestLeg(t, "peerB", 109, false)

	connect(t, ctx, peerA, metaLeg)
	connect(t, ctx, mxLeg, peerB)

	// Both peers send; the bridge relays whatever arrives.
	send := func(l *Leg, tag string) {
		go func() {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for i := uint16(0); ; i++ {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				_ = l.Local.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: 1000 + i, Timestamp: uint32(i) * 960, Marker: i == 0},
					Payload: fmt.Appendf(nil, "%s-opus-%d", tag, i),
				})
			}
		}()
	}
	send(peerA, "A")
	send(peerB, "B")

	var statsAB, statsBA RelayStats
	go func() {
		tr, err := metaLeg.RemoteTrack(ctx)
		if err != nil {
			return
		}
		_ = Relay(ctx, tr, uint8(tr.PayloadType()), mxLeg.Local, &statsAB, log)
	}()
	go func() {
		tr, err := mxLeg.RemoteTrack(ctx)
		if err != nil {
			return
		}
		_ = Relay(ctx, tr, uint8(tr.PayloadType()), metaLeg.Local, &statsBA, log)
	}()

	expect := func(receiver *Leg, wantPT uint8, tag string, notSSRC uint32) {
		t.Helper()
		tr, err := receiver.RemoteTrack(ctx)
		if err != nil {
			t.Fatalf("%s: %v", receiver.Name, err)
		}
		got := 0
		for got < 10 {
			p, _, err := tr.ReadRTP()
			if err != nil {
				t.Fatalf("%s: read: %v", receiver.Name, err)
			}
			if p.PayloadType != wantPT {
				t.Fatalf("%s: payload type %d, want %d", receiver.Name, p.PayloadType, wantPT)
			}
			if p.SSRC == notSSRC {
				t.Fatalf("%s: SSRC was not remapped", receiver.Name)
			}
			if !bytes.HasPrefix(p.Payload, []byte(tag+"-opus-")) {
				t.Fatalf("%s: unexpected payload %q", receiver.Name, p.Payload)
			}
			got++
		}
	}
	senderSSRC := func(l *Leg) uint32 {
		for _, s := range l.PC.GetSenders() {
			if s.Track() != nil {
				return uint32(s.GetParameters().Encodings[0].SSRC)
			}
		}
		return 0
	}
	expect(peerB, 109, "A", senderSSRC(peerA))
	expect(peerA, 111, "B", senderSSRC(peerB))
	if statsAB.Forwarded.Load() == 0 || statsBA.Forwarded.Load() == 0 {
		t.Fatalf("relay stats: A->B %d, B->A %d", statsAB.Forwarded.Load(), statsBA.Forwarded.Load())
	}
}

func newVideoTestLeg(t *testing.T, name string, webShape bool) *Leg {
	t.Helper()
	l, err := NewLeg(LegConfig{
		Name: name, WebShape: webShape, VideoCodec: webrtc.MimeTypeVP8, Settings: loopbackSettings(), Log: zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

// TestLoopbackVideoRelay relays VP8 both ways through the two legs and checks
// that a keyframe request from one peer reaches the leg facing the other.
func TestLoopbackVideoRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log := zerolog.Nop()
	peerA := newVideoTestLeg(t, "peerA", true)
	metaLeg := newVideoTestLeg(t, "meta", true)
	mxLeg := newVideoTestLeg(t, "matrix", false)
	peerB := newVideoTestLeg(t, "peerB", false)
	connect(t, ctx, peerA, metaLeg)
	connect(t, ctx, mxLeg, peerB)
	if !SendsVideo(metaLeg.PC.LocalDescription().SDP) || !SendsVideo(mxLeg.PC.LocalDescription().SDP) {
		t.Fatal("bridge legs don't send video")
	}

	send := func(l *Leg, tag string) {
		go func() {
			ticker := time.NewTicker(33 * time.Millisecond)
			defer ticker.Stop()
			for i := uint16(0); ; i++ {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				_ = l.LocalVideo.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: 500 + i, Timestamp: uint32(i) * VideoFrameTicks, Marker: true},
					Payload: fmt.Appendf(nil, "%s-vp8-%d", tag, i),
				})
			}
		}()
	}
	send(peerA, "A")
	send(peerB, "B")
	keyframeRequested := make(chan struct{}, 1)
	mxLeg.OnKeyframeRequest(func() {
		select {
		case keyframeRequested <- struct{}{}:
		default:
		}
	})
	for _, dir := range [][2]*Leg{{metaLeg, mxLeg}, {mxLeg, metaLeg}} {
		go func() {
			tr, err := dir[0].RemoteVideoTrack(ctx)
			if err != nil {
				return
			}
			var stats RelayStats
			_ = RelayVideo(ctx, tr, uint8(tr.PayloadType()), dir[1].LocalVideo, &stats, log)
		}()
	}
	expect := func(receiver *Leg, tag string) *webrtc.TrackRemote {
		t.Helper()
		tr, err := receiver.RemoteVideoTrack(ctx)
		if err != nil {
			t.Fatalf("%s: %v", receiver.Name, err)
		}
		if tr.Codec().MimeType != webrtc.MimeTypeVP8 {
			t.Fatalf("%s: codec %s", receiver.Name, tr.Codec().MimeType)
		}
		for got := 0; got < 5; {
			p, _, err := tr.ReadRTP()
			if err != nil {
				t.Fatalf("%s: read: %v", receiver.Name, err)
			}
			if bytes.HasPrefix(p.Payload, []byte(tag+"-vp8-")) {
				got++
			}
		}
		return tr
	}
	trB := expect(peerB, "A")
	expect(peerA, "B")
	peerB.RequestKeyframe(trB.SSRC())
	select {
	case <-keyframeRequested:
	case <-ctx.Done():
		t.Fatal("keyframe request from peer B didn't reach the matrix leg")
	}
}

// TestPlanBOffer answers a Plan B offer, as Messenger's mobile apps send.
func TestPlanBOffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	api := webrtc.NewAPI(webrtc.WithSettingEngine(*loopbackSettings()))
	peer, err := api.NewPeerConnection(webrtc.Configuration{SDPSemantics: webrtc.SDPSemanticsPlanB})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "a", "s")
	if err != nil {
		t.Fatal(err)
	}
	// Two tracks in one m-section is what makes an offer (certainly) Plan B.
	track2, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "b", "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range []webrtc.TrackLocal{track, track2} {
		if _, err = peer.AddTrack(tr); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(peer)
	if err = peer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if !strings.Contains(peer.LocalDescription().SDP, "a=ssrc:") {
		t.Fatal("test offer isn't Plan B")
	}
	if !IsPlanB(peer.LocalDescription().SDP) {
		t.Fatal("IsPlanB missed a Plan B offer")
	}
	leg, err := NewLeg(LegConfig{Name: "meta", WebShape: true, PlanB: true, Settings: loopbackSettings(), Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(leg.Close)
	if _, err = leg.AnswerOffer(peer.LocalDescription().SDP); err != nil {
		t.Fatalf("answering a Plan B offer: %v", err)
	}
	answer := leg.WaitGathering(ctx, 5*time.Second)
	if err = peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := uint16(0); ctx.Err() == nil; i++ {
			_ = track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: i, Timestamp: uint32(i) * 960}, Payload: []byte("planb")})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	tr, err := leg.RemoteTrack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := tr.ReadRTP(); err != nil || string(p.Payload) != "planb" {
		t.Fatalf("read: %v %v", p, err)
	}
}

func TestVideoSDPHelpers(t *testing.T) {
	web := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\na=sendrecv\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 108 96\r\na=rtpmap:108 H264/90000\r\na=rtpmap:96 VP8/90000\r\na=sendrecv\r\n"
	if PickVideoCodec(web) != webrtc.MimeTypeVP8 || !SendsVideo(web) {
		t.Fatal("web video offer")
	}
	h264 := strings.Replace(web, "a=rtpmap:96 VP8/90000\r\n", "", 1)
	if PickVideoCodec(h264) != webrtc.MimeTypeH264 {
		t.Fatal("H264-only offer")
	}
	voice := strings.TrimSuffix(web, "a=sendrecv\r\n") + "a=recvonly\r\n"
	if SendsVideo(voice) {
		t.Fatal("recvonly video counted as sending")
	}
	if SendsVideo("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n") || PickVideoCodec("m=video 0 RTP 96\r\na=rtpmap:96 VP8/90000\r\n") != "" {
		t.Fatal("audio-only / disabled video")
	}
}

func TestLogSDPShapeHasNoSecrets(t *testing.T) {
	sdp := "v=0\r\no=- 1 2 IN IP4 203.0.113.9\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\nc=IN IP4 203.0.113.9\r\n" +
		"a=ice-ufrag:SECRETUFRAG\r\na=ice-pwd:SECRETPWD\r\na=fingerprint:sha-256 AB:CD\r\na=x-dtls-auth:SECRETAUTH\r\n" +
		"a=candidate:1 1 udp 1 203.0.113.9 5000 typ host\r\na=mid:0\r\na=sendrecv\r\na=rtpmap:111 opus/48000/2\r\n" +
		"a=ssrc:1 cname:x\r\na=ssrc:2 cname:x\r\na=extmap:1 urn:ietf:params:rtp-hdrext:sdes:mid\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=recvonly\r\na=rtpmap:96 VP8/90000\r\n"
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	LogSDPShape(log.Info(), sdp).Msg("x")
	out := buf.String()
	for _, secret := range []string{"SECRET", "203.0.113", "AB:CD"} {
		if strings.Contains(out, secret) {
			t.Fatalf("shape log leaks %q: %s", secret, out)
		}
	}
	for _, want := range []string{`"ssrcs":2`, `"dir":"recvonly"`, "VP8/90000", "sdes:mid"} {
		if !strings.Contains(out, want) {
			t.Fatalf("shape log misses %q: %s", want, out)
		}
	}
}

func TestIsPlanB(t *testing.T) {
	unified := "v=0\r\nm=audio 9 X 111\r\na=mid:0\r\na=msid:s a\r\na=ssrc:1 msid:s a\r\n" +
		"m=video 9 X 96 97\r\na=mid:1\r\na=msid:s v\r\na=ssrc-group:FID 2 3\r\na=ssrc:2 msid:s v\r\na=ssrc:3 msid:s v\r\n"
	if IsPlanB(unified) {
		t.Fatal("Unified Plan with an RTX SSRC group taken for Plan B")
	}
	if !IsPlanB("m=audio 9 X 111\r\na=mid:audio\r\n") {
		t.Fatal("Plan B mid names not detected")
	}
	// What Pion itself calls Plan B (descriptionIsPlanB): any mid named audio/video/data, any case.
	// Messenger's mobile offers slipped past an audio/video-only check through their data channel.
	for _, sdp := range []string{
		"m=audio 9 X 111\r\na=mid:0\r\nm=application 9 X webrtc-datachannel\r\na=mid:data\r\n",
		"m=audio 9 X 111\r\na=mid:Audio\r\n",
		"m=video 9 X 96\r\na=mid:VIDEO \r\n",
	} {
		if !IsPlanB(sdp) {
			t.Fatalf("Plan B not detected in %q", sdp)
		}
	}
	twoTracks := "m=audio 9 X 111\r\na=mid:0\r\na=ssrc:1 msid:s a\r\na=ssrc:2 msid:s b\r\n"
	if !IsPlanB(twoTracks) {
		t.Fatal("two tracks in one section not detected")
	}
}

// TestVideoUpgradeRenegotiation: an audio call whose leg adds video mid-call
// and renegotiates (Messenger turned on the camera), and the peer receives it.
func TestVideoUpgradeRenegotiation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mk := func(name string) *Leg {
		l, err := NewLeg(LegConfig{Name: name, AllowVideo: true, Settings: loopbackSettings(), Log: zerolog.Nop()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.Close)
		return l
	}
	mxLeg, element := mk("matrix"), mk("element")
	connect(t, ctx, mxLeg, element)
	if SendsVideo(mxLeg.PC.LocalDescription().SDP) {
		t.Fatal("audio call already sends video")
	}
	if err := mxLeg.AddVideoTrack(webrtc.MimeTypeVP8); err != nil {
		t.Fatal(err)
	}
	offer, err := mxLeg.Renegotiate()
	if err != nil {
		t.Fatal(err)
	}
	answer, err := element.AnswerOffer(offer)
	if err != nil {
		t.Fatal(err)
	}
	if err = mxLeg.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := uint16(0); ctx.Err() == nil; i++ {
			_ = mxLeg.LocalVideo.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: i, Timestamp: uint32(i) * VideoFrameTicks, Marker: true},
				Payload: []byte("upgraded"),
			})
			time.Sleep(33 * time.Millisecond)
		}
	}()
	tr, err := element.RemoteVideoTrack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := tr.ReadRTP(); err != nil || string(p.Payload) != "upgraded" {
		t.Fatalf("read: %v %v", p, err)
	}
}

// planBCameraOffer turns a Unified Plan renegotiation offer into what Messenger's mobile apps send
// when the camera turns on: Plan B section names, and the video sent as three simulcast layers
// (each with an RTX partner) in one section, which Pion takes as three tracks.
func planBCameraOffer(t *testing.T, unified string) (string, map[string]string) {
	t.Helper()
	planB := map[string]string{}
	for i, mid := range MIDs(unified) {
		planB[mid] = []string{"audio", "video", "data"}[i]
	}
	offer := RenameMIDs(unified, planB)
	var b strings.Builder
	inVideo, added := false, false
	for _, line := range strings.SplitAfter(offer, "\n") {
		if strings.HasPrefix(line, "m=") {
			inVideo = strings.HasPrefix(line, "m=video")
		}
		if inVideo && !added && strings.HasPrefix(line, "a=ssrc:") {
			primary := strings.Fields(line[len("a=ssrc:"):])[0]
			b.WriteString("a=ssrc-group:SIM " + primary + " 1111 2222\r\n")
			b.WriteString(line)
			b.WriteString("a=ssrc-group:FID 1111 1112\r\na=ssrc-group:FID 2222 2223\r\n")
			for _, s := range []string{"1111", "1112", "2222", "2223"} {
				b.WriteString("a=ssrc:" + s + " cname:layer\r\na=ssrc:" + s + " msid:stream layer" + s + "\r\n")
			}
			added = true
			continue
		}
		b.WriteString(line)
	}
	if !added {
		t.Fatal("no video ssrc to extend")
	}
	return b.String(), planB
}

// TestPlanBRenegotiationOnUnifiedLeg: a Messenger mobile peer turns its camera on mid-call and
// renegotiates in Plan B (audio/video names, simulcast layers in one section) on a leg we opened as
// Unified Plan. Pion rejects that as it is; adapted (one layer, our mids) it's accepted, our answer
// goes back with the peer's own names, and the peer's video arrives.
func TestPlanBRenegotiationOnUnifiedLeg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mk := func(name string) *Leg {
		l, err := NewLeg(LegConfig{Name: name, AllowVideo: true, Settings: loopbackSettings(), Log: zerolog.Nop()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.Close)
		return l
	}
	cameraOffer := func(phone *Leg) (string, map[string]string) {
		if err := phone.AddVideoTrack(webrtc.MimeTypeVP8); err != nil {
			t.Fatal(err)
		}
		unified, err := phone.Renegotiate()
		if err != nil {
			t.Fatal(err)
		}
		return planBCameraOffer(t, unified)
	}

	metaLeg, phone := mk("messenger"), mk("phone")
	connect(t, ctx, metaLeg, phone)
	offer, _ := cameraOffer(phone)
	if _, err := metaLeg.AnswerOffer(offer); err == nil {
		t.Fatal("expected Pion to reject the Plan B offer as it is")
	}

	// A fresh pair, since the failed attempt left the first leg half-applied.
	metaLeg, phone = mk("messenger2"), mk("phone2")
	connect(t, ctx, metaLeg, phone)
	offer, planB := cameraOffer(phone)
	adapted, mapping := AdaptPlanBOffer(offer, metaLeg.PC.LocalDescription().SDP)
	answer, err := metaLeg.AnswerOffer(adapted)
	if err != nil {
		t.Fatalf("answering the adapted offer: %v", err)
	}
	answer = RenameMIDs(answer, InvertMIDs(mapping))
	for _, mid := range MIDs(answer) {
		if mid != "audio" && mid != "video" && mid != "data" {
			t.Fatalf("answer mids not renamed back: %v", MIDs(answer))
		}
	}
	// The phone reads our answer with its own names.
	if err = phone.SetAnswer(RenameMIDs(answer, InvertMIDs(planB))); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := uint16(0); ctx.Err() == nil; i++ {
			_ = phone.LocalVideo.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: i, Timestamp: uint32(i) * VideoFrameTicks, Marker: true},
				Payload: []byte("camera"),
			})
			time.Sleep(33 * time.Millisecond)
		}
	}()
	tr, err := metaLeg.RemoteVideoTrack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := tr.ReadRTP(); err != nil || string(p.Payload) != "camera" {
		t.Fatalf("read: %v %v", p, err)
	}
}

// TestSimulcastFirstOffer: Messenger's mobile video call rings with simulcast layers in the video
// section but Unified Plan mids. Pion rejects that as Plan B on a Unified Plan leg; after
// CollapseSimulcast (one layer) it answers.
func TestSimulcastFirstOffer(t *testing.T) {
	mk := func(name string) *Leg {
		l, err := NewLeg(LegConfig{Name: name, AllowVideo: true, VideoCodec: webrtc.MimeTypeVP8, Settings: loopbackSettings(), Log: zerolog.Nop()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.Close)
		return l
	}
	withSimulcast := func(offer string) string {
		var b strings.Builder
		inVideo, added := false, false
		for _, line := range strings.SplitAfter(offer, "\n") {
			if strings.HasPrefix(line, "m=") {
				inVideo = strings.HasPrefix(line, "m=video")
			}
			if inVideo && !added && strings.HasPrefix(line, "a=ssrc:") {
				primary := strings.Fields(line[len("a=ssrc:"):])[0]
				b.WriteString("a=ssrc-group:SIM " + primary + " 1111 2222\r\n")
				b.WriteString(line)
				for _, s := range []string{"1111", "2222"} {
					b.WriteString("a=ssrc:" + s + " cname:layer\r\na=ssrc:" + s + " msid:stream layer" + s + "\r\n")
				}
				added = true
				continue
			}
			b.WriteString(line)
		}
		if !added {
			t.Fatal("no video ssrc")
		}
		return b.String()
	}
	phone := mk("phone")
	if err := phone.AddVideoTrack(webrtc.MimeTypeVP8); err != nil {
		t.Fatal(err)
	}
	offer, err := phone.CreateOffer()
	if err != nil {
		t.Fatal(err)
	}
	offer = withSimulcast(offer)
	if _, err = mk("raw").AnswerOffer(offer); err == nil {
		t.Fatal("expected Pion to reject the simulcast offer as it is")
	}
	if _, err = mk("bridge").AnswerOffer(CollapseSimulcast(offer)); err != nil {
		t.Fatalf("answering the prepared offer: %v", err)
	}
}

// The live failure: our camera offer was rejected (CLIENT_MEDIA_UPDATE 409),
// leaving the leg in have-local-offer, so Messenger's own camera offer then
// failed with an invalid signaling state transition. Our pending offer must
// yield to theirs, and each new offer must carry a higher o= version (the
// CLIENT_MEDIA_UPDATE version).
func TestRenegotiationGlareAndVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mk := func(name string) *Leg {
		l, err := NewLeg(LegConfig{Name: name, AllowVideo: true, Settings: loopbackSettings(), Log: zerolog.Nop()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.Close)
		return l
	}
	bridge, phone := mk("bridge"), mk("phone")
	connect(t, ctx, bridge, phone)
	v0, err := SDPVersion(bridge.PC.LocalDescription().SDP)
	if err != nil {
		t.Fatal(err)
	}

	// Our camera offer, never answered (Messenger rejected it).
	if err = bridge.AddVideoTrack(webrtc.MimeTypeVP8); err != nil {
		t.Fatal(err)
	}
	ours, err := bridge.Renegotiate()
	if err != nil {
		t.Fatal(err)
	}
	v1, err := SDPVersion(ours)
	if err != nil {
		t.Fatal(err)
	}
	if v1 <= v0 {
		t.Fatalf("offer version %d not above the previous %d", v1, v0)
	}

	// Meanwhile the phone turns its camera on: its offer must still apply.
	if err = phone.AddVideoTrack(webrtc.MimeTypeVP8); err != nil {
		t.Fatal(err)
	}
	theirs, err := phone.Renegotiate()
	if err != nil {
		t.Fatal(err)
	}
	answer, err := bridge.AnswerRenegotiation(theirs)
	if err != nil {
		t.Fatalf("answering the peer's offer while ours was pending: %v", err)
	}
	if err = phone.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}

	// And our camera can be offered again afterwards (a retry after the
	// rejection), this time answered and applied.
	again, err := bridge.Renegotiate()
	if err != nil {
		t.Fatalf("re-offering after the glare: %v", err)
	}
	if v2, _ := SDPVersion(again); v2 <= v1 {
		t.Fatalf("retry version %d not above %d", v2, v1)
	}
	if !strings.Contains(again, "a=candidate:") {
		t.Fatal("renegotiation offer without ICE candidates")
	}
	reply, err := phone.AnswerRenegotiation(again)
	if err != nil {
		t.Fatalf("phone answering the retry: %v", err)
	}
	if err = bridge.SetAnswer(reply); err != nil {
		t.Fatalf("applying the retry's answer: %v", err)
	}
	if bridge.PC.SignalingState() != webrtc.SignalingStateStable {
		t.Fatalf("signaling state %s", bridge.PC.SignalingState())
	}
}

func TestSDPVersion(t *testing.T) {
	v, err := SDPVersion("v=0\r\no=- 4611731400430051336 7 IN IP4 127.0.0.1\r\ns=-\r\n")
	if err != nil || v != 7 {
		t.Fatalf("got %d, %v", v, err)
	}
	if _, err = SDPVersion("v=0\r\ns=-\r\n"); err == nil {
		t.Fatal("expected an error without an o= line")
	}
}

// TestSSRCCname checks the cname used as part of a participant's E2EE id.
func TestSSRCCname(t *testing.T) {
	leg := newTestLeg(t, "web", OpusPT, true)
	offer, err := leg.CreateOffer()
	if err != nil {
		t.Fatal(err)
	}
	if got := SSRCCname(offer); got == "" || got != leg.StreamID {
		t.Errorf("cname %q, want the stream id %q", got, leg.StreamID)
	}
	if got := SSRCCname("v=0\r\na=ssrc:645305178 cname:utngOpj/Sto42obD\r\n"); got != "utngOpj/Sto42obD" {
		t.Errorf("web client cname: %q", got)
	}
	for stream, want := range map[string]string{
		"100010105602888:hZvKyjdfjh6GgP1x:eef606b6-0b0c-4d4e-8f3a-1c2d3e4f5a6b": "100010105602888:hZvKyjdfjh6GgP1x",
		"100010105602888:hZvKyjdfjh6GgP1x":                                      "100010105602888:hZvKyjdfjh6GgP1x",
		"eef606b6-0b0c-4d4e-8f3a-1c2d3e4f5a6b":                                  "",
		":x:y":                                                                  "",
	} {
		if got := E2eeIDOfStream(stream); got != want {
			t.Errorf("E2eeIDOfStream(%q) = %q, want %q", stream, got, want)
		}
	}
}

// TestSFUOfferExtensions: an SFU leg's offer declares the MID extension in both media sections, which
// the SFU routes streams by.
func TestSFUOfferExtensions(t *testing.T) {
	l, err := NewLeg(LegConfig{Name: "sfu", WebShape: true, SFU: true, AllowVideo: true, Settings: loopbackSettings(), Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	offer, err := l.CreateOffer()
	if err != nil {
		t.Fatal(err)
	}
	// Pion offers MID for video on its own, but not for audio.
	audio, _, _ := strings.Cut(offer, "m=video")
	if !strings.Contains(audio, "urn:ietf:params:rtp-hdrext:sdes:mid") {
		t.Error("the audio section doesn't declare the MID extension")
	}
}

// TestRewriterAudioLevel: the audio level survives the extension stripping, under the destination's id.
func TestRewriterAudioLevel(t *testing.T) {
	rw := &Rewriter{}
	rw.AudioLevel.From, rw.AudioLevel.To = 10, 1
	p := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1}}
	_ = p.Header.SetExtension(10, []byte{0x85})
	_ = p.Header.SetExtension(3, []byte{1, 2, 3})
	rw.Rewrite(p)
	if got := p.Header.GetExtension(1); len(got) != 1 || got[0] != 0x85 {
		t.Errorf("audio level %x under id 1, want 85", got)
	}
	if p.Header.GetExtension(10) != nil || p.Header.GetExtension(3) != nil {
		t.Error("other extensions weren't stripped")
	}
	if _, err := p.Marshal(); err != nil {
		t.Fatal(err)
	}
	plain := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1}}
	_ = plain.Header.SetExtension(10, []byte{0x85})
	(&Rewriter{}).Rewrite(plain)
	if plain.Header.Extension {
		t.Error("a rewriter without AudioLevel kept an extension")
	}

	// Without a level from the source, one is derived from the payload.
	synth := &Rewriter{}
	synth.AudioLevel.To = 1
	speech := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1}, Payload: make([]byte, 120)}
	synth.Rewrite(speech)
	if got := speech.Header.GetExtension(1); len(got) != 1 || got[0]&0x80 == 0 || got[0]&0x7f == 127 {
		t.Errorf("speech level %x, want a voice-active middling level", got)
	}
	silence := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 2}, Payload: []byte{0xf8, 0xff, 0xfe}}
	synth.Rewrite(silence)
	if got := silence.Header.GetExtension(1); len(got) != 1 || got[0] != 127 {
		t.Errorf("comfort noise level %x, want 7f", got)
	}
}
