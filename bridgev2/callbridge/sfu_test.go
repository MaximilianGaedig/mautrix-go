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
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// The shape of the captured SFU answer and PARTICIPANT_ADDED delta (addresses and keys replaced).
const sfuAnswer = "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\na=ice-lite\r\na=group:BUNDLE 0 1 2\r\na=msid-semantic: WMS\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\na=recvonly\r\na=rtpmap:111 opus/48000/2\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=sendonly\r\na=rtpmap:96 VP8/90000\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:2\r\n"

func TestApplySDPDelta(t *testing.T) {
	update := &SessionDescriptionUpdate{Media: map[int32]MediaDescriptionUpdate{
		1: {Body: "m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=inactive\r\na=rtpmap:96 VP8/90000\r\n", MID: "1"},
		3: {
			Body: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:3\r\na=sendonly\r\na=rtpmap:111 opus/48000/2\r\n" +
				"a=msid:100010105602888:hZvK:eef6 b36a\r\na=ssrc:1393957939 cname:100010105602888:hZvK\r\n",
			MSID: "100010105602888:hZvK:eef6",
			MID:  "3",
		},
	}}
	offer, err := ApplySDPDelta(sfuAnswer, update)
	if err != nil {
		t.Fatal(err)
	}
	if got := MIDs(offer); strings.Join(got, ",") != "0,1,2,3" {
		t.Fatalf("mids %v", got)
	}
	if !strings.Contains(offer, "a=group:BUNDLE 0 1 2 3\r\n") {
		t.Errorf("BUNDLE not extended:\n%s", offer)
	}
	if !strings.Contains(offer, "a=msid-semantic: WMS 100010105602888:hZvK:eef6\r\n") {
		t.Errorf("WMS token not updated:\n%s", offer)
	}
	if !strings.Contains(offer, "a=mid:1\r\na=inactive") {
		t.Errorf("m-line 1 not replaced:\n%s", offer)
	}
	if !strings.Contains(offer, "a=ice-lite\r\n") || !strings.HasSuffix(offer, "\r\n") {
		t.Errorf("session attributes lost or bad line ends:\n%q", offer)
	}
	if owners := TrackOwners(offer); owners["b36a"] != "100010105602888" {
		t.Errorf("owners %v", owners)
	}
	if _, err = ApplySDPDelta(sfuAnswer, &SessionDescriptionUpdate{}); err == nil {
		t.Error("expected an error for an empty delta")
	}
}

// TestSFUDeltaLoopback plays Messenger's SFU with a local Pion peer: the bridge's leg offers the web
// client's shape (audio sendrecv, video recvonly, data channel), the "SFU" answers, then a participant
// joins and the SFU sends only that participant's new m-section as a delta. The leg must answer the
// delta and hand the participant's track to OnRemoteTrack with the owner in its msid.
func TestSFUDeltaLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bridge, err := NewLeg(LegConfig{Name: "bridge", OpusPT: 111, WebShape: true, SFU: true, Settings: loopbackSettings(), Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	tracks := make(chan *webrtc.TrackRemote, 4)
	bridge.OnRemoteTrack(func(tr *webrtc.TrackRemote) { tracks <- tr })

	// The "SFU": a plain Pion peer that answers the bridge's offer.
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	sfu, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(*loopbackSettings())).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sfu.Close() })
	gathered := func(pc *webrtc.PeerConnection) string {
		<-webrtc.GatheringCompletePromise(pc)
		return pc.LocalDescription().SDP
	}

	if _, err = bridge.CreateOffer(); err != nil {
		t.Fatal(err)
	}
	offer := bridge.WaitGathering(ctx, 5*time.Second)
	if err = sfu.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		t.Fatal(err)
	}
	ans, err := sfu.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = sfu.SetLocalDescription(ans); err != nil {
		t.Fatal(err)
	}
	if err = bridge.SetAnswer(gathered(sfu)); err != nil {
		t.Fatal(err)
	}
	before := len(MIDs(bridge.PC.RemoteDescription().SDP))

	// A participant joins: the SFU adds their audio, with the owner as the first msid token.
	part, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "b36a554f", "100010105602888:hZvK:eef606b6")
	if err != nil {
		t.Fatal(err)
	}
	// A new sendonly transceiver (AddTrack would reuse the recvonly audio one), like the SFU's new m-section.
	if _, err = sfu.AddTransceiverFromTrack(part, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		t.Fatal(err)
	}
	reoffer, err := sfu.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = sfu.SetLocalDescription(reoffer); err != nil {
		t.Fatal(err)
	}
	// Keep only what the SFU sends: the new m-section(s), keyed by m-line index.
	_, sections := splitSDP(gathered(sfu))
	update := &SessionDescriptionUpdate{Media: map[int32]MediaDescriptionUpdate{}}
	for i := before; i < len(sections); i++ {
		update.Media[int32(i)] = MediaDescriptionUpdate{Body: sections[i], MID: sectionAttr(sections[i], "mid")}
	}
	if len(update.Media) != 1 {
		t.Fatalf("expected one new m-section, got %d", len(update.Media))
	}

	offer, err = ApplySDPDelta(bridge.PC.RemoteDescription().SDP, update)
	if err != nil {
		t.Fatalf("applying the delta: %v", err)
	}
	answer, err := bridge.AnswerRenegotiation(offer)
	if err != nil {
		t.Fatalf("answering the delta: %v", err)
	}
	if err = sfu.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("SFU applying the answer: %v", err)
	}
	go func() {
		for i := uint16(0); ctx.Err() == nil; i++ {
			_ = part.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: i, Timestamp: uint32(i) * 960}, Payload: []byte{0xfc}})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	select {
	case tr := <-tracks:
		if owner, _, _ := strings.Cut(tr.StreamID(), ":"); owner != "100010105602888" {
			t.Fatalf("track stream %q doesn't name the owner", tr.StreamID())
		}
		if tr.ID() != "b36a554f" {
			t.Fatalf("track id %q", tr.ID())
		}
	case <-ctx.Done():
		t.Fatal("the participant's track never arrived")
	}
}
