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
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	protoCodecs "github.com/livekit/protocol/codecs"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/rs/zerolog"
)

// startLiveKit runs livekit-server (key "devkey") on a free port; the test is skipped when
// the binary isn't installed (e.g. `nix shell nixpkgs#livekit`).
func startLiveKit(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("LIVEKIT_SERVER_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("livekit-server"); err != nil {
			t.Skip("livekit-server not installed")
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpPort := udp.LocalAddr().(*net.UDPAddr).Port
	udp.Close()
	cfg := fmt.Sprintf("port: %d\nbind_addresses: [127.0.0.1]\nrtc:\n  udp_port: %d\n  tcp_port: 0\n  node_ip: 127.0.0.1\n  use_external_ip: false\nkeys:\n  devkey: secretsecretsecretsecretsecretsecret\nlogging:\n  level: warn\n", port, udpPort)
	cfgPath := t.TempDir() + "/livekit.yaml"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config", cfgPath)
	cmd.Stdout = zerologWriter{t}
	cmd.Stderr = zerologWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	url := fmt.Sprintf("ws://127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return url
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("livekit-server did not start")
	return ""
}

// zerologWriter sends the server's output to the test log.
type zerologWriter struct{ t *testing.T }

func (w zerologWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func devToken(t *testing.T, room, identity string) string {
	t.Helper()
	tok, err := auth.NewAccessToken("devkey", "secretsecretsecretsecretsecretsecret").
		SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: room}).
		SetIdentity(identity).
		ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRTCLegRelaysAudio(t *testing.T) {
	url := startLiveKit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.InfoLevel)

	// The bridge's ghost and, standing in for Element X, a second participant.
	ghost, err := JoinRTC(ctx, RTCLegConfig{URL: url, Token: devToken(t, "call", "@ghost:x:BRIDGE"), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer ghost.Close()
	user, err := JoinRTC(ctx, RTCLegConfig{URL: url, Token: devToken(t, "call", "@mg:x:PHONE"), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()

	// Each side sees the other.
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(ghost.Peers(), "@mg:x:PHONE") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !slices.Contains(ghost.Peers(), "@mg:x:PHONE") {
		t.Fatalf("ghost doesn't see the user: %v", ghost.Peers())
	}

	// Messenger's audio, written into the ghost's track, reaches the user.
	go func() {
		payload := []byte{0xf8, 0xff, 0xfe} // an Opus DTX-ish frame; content doesn't matter
		for seq := uint16(0); ctx.Err() == nil; seq++ {
			_ = ghost.AudioWriter().WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
				Payload: payload,
			})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	track, err := user.RemoteAudio(ctx)
	if err != nil {
		t.Fatalf("user never got the ghost's audio: %v", err)
	}
	if got := track.Codec().MimeType; got != "audio/opus" {
		t.Fatalf("codec %q", got)
	}
	for i := 0; i < 10; i++ {
		if _, _, err := track.ReadRTP(); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

// What a participant restricted to one video codec offers LiveKit: Opus and that codec, with the
// very parameters the bridge publishes it under - a different H264 profile or packetization mode
// would be another codec to Pion, and the publication would find nothing to send as.
func TestRTCCodecs(t *testing.T) {
	for _, mime := range []string{webrtc.MimeTypeVP8, webrtc.MimeTypeH264, "video/h264"} {
		codecs := rtcCodecs(mime)
		if len(codecs) != 2 {
			t.Fatalf("%s: %d codecs, want Opus and the video codec", mime, len(codecs))
		}
		audio := protoCodecs.ToWebrtcCodecParameters(&codecs[0])
		if audio.MimeType != webrtc.MimeTypeOpus || audio.PayloadType != OpusPT {
			t.Errorf("%s: audio %s under %d, want Opus under %d", mime, audio.MimeType, audio.PayloadType, OpusPT)
		}
		video := protoCodecs.ToWebrtcCodecParameters(&codecs[1])
		want := videoCapability(video.MimeType)
		if !strings.EqualFold(video.MimeType, mime) || video.ClockRate != want.ClockRate || video.SDPFmtpLine != want.SDPFmtpLine {
			t.Errorf("%s: registered as %s/%d %q, published as %s/%d %q", mime,
				video.MimeType, video.ClockRate, video.SDPFmtpLine, want.MimeType, want.ClockRate, want.SDPFmtpLine)
		}
		if video.PayloadType == 0 {
			t.Errorf("%s: no payload type", mime)
		}
	}
	// No codec asked for, or one the bridge can't relay: the SDK's defaults, as before.
	for _, mime := range []string{"", webrtc.MimeTypeAV1, "video/unheard-of", webrtc.MimeTypeOpus} {
		if codecs := rtcCodecs(mime); codecs != nil {
			t.Errorf("%q restricts the participant to %d codecs, want no restriction", mime, len(codecs))
		}
	}
}

// A participant that joined with one video codec can't send another: asked to, the SDK publishes a
// track that never reaches anyone. The camera or screen is refused instead, where it can be seen.
func TestRTCLegRefusesAnotherVideoCodec(t *testing.T) {
	leg := &RTCLeg{videoCodec: webrtc.MimeTypeVP8}
	if _, err := leg.AddScreenTrack(webrtc.MimeTypeH264); err == nil {
		t.Error("an H264 screen was published by a participant that joined with VP8 only")
	}
	if _, err := leg.AddVideoTrack(webrtc.MimeTypeH264); err == nil {
		t.Error("an H264 camera was published by a participant that joined with VP8 only")
	}
}

// keyFrames are the smallest key frames of each codec, as the LiveKit SDK's own tests send them: the
// server forwards a video to a subscriber from a key frame on, so anything else never arrives.
var keyFrames = map[string][]byte{
	webrtc.MimeTypeVP8: {
		0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x08, 0x00, 0x08, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88,
		0x85, 0x84, 0x88, 0x02, 0x02, 0x00, 0x0c, 0x0d, 0x60, 0x00, 0xfe, 0xff, 0xab, 0x50, 0x80,
	},
	// SPS, PPS and an IDR slice, in Annex B.
	webrtc.MimeTypeH264: {
		0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00, 0x03, 0x00,
		0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
		0, 0, 0, 1, 0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20,
		0, 0, 0, 1, 0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe,
	},
}

// keyFrameTrack is a LiveKit track of the codec that sends its key frame ten times a second.
func keyFrameTrack(t *testing.T, ctx context.Context, mime string) *lksdk.LocalTrack {
	t.Helper()
	track, err := lksdk.NewLocalTrack(videoCapability(mime))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for ctx.Err() == nil {
			_ = track.WriteSample(media.Sample{Data: keyFrames[mime], Duration: 100 * time.Millisecond}, nil)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return track
}

// keyFramePackets writes the codec's key frame as RTP into a bridge leg's video, ten times a second.
func keyFramePackets(ctx context.Context, mime string, to RTPWriter) {
	// The frame as RTP payloads: VP8 behind its one-byte descriptor (start of partition 0), H264 one
	// NAL unit to a packet.
	var payloads [][]byte
	if mime == webrtc.MimeTypeVP8 {
		payloads = [][]byte{append([]byte{0x10}, keyFrames[mime]...)}
	} else {
		for _, nalu := range bytes.Split(keyFrames[mime], []byte{0, 0, 0, 1})[1:] {
			payloads = append(payloads, nalu)
		}
	}
	seq := uint16(0)
	for frame := uint32(0); ctx.Err() == nil; frame++ {
		for i, payload := range payloads {
			_ = to.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: frame * 9000, Marker: i == len(payloads)-1},
				Payload: payload,
			})
			seq++
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Element Call sends its camera as H264 and, in a call that isn't encrypted, a VP8 copy for whoever
// can't take H264. The bridge can't transcode: joined with every codec it is handed the H264, which
// a network that only does VP8 has no use for. Restricted to the other leg's codec it gets that
// codec, and what it publishes itself still arrives.
func TestRTCLegTakesOnlyItsVideoCodec(t *testing.T) {
	url := startLiveKit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	log := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.InfoLevel)

	// Element Call's stand-in, which also watches what the bridge's participants publish.
	type seen struct {
		identity string
		codec    webrtc.RTPCodecCapability
	}
	subscribed := make(chan seen, 16)
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		subscribed <- seen{rp.Identity(), track.Codec().RTPCodecCapability}
	}
	element, err := lksdk.ConnectToRoomWithToken(url, devToken(t, "call", "@mg:x:ELEMENT"), cb)
	if err != nil {
		t.Fatal(err)
	}
	defer element.Disconnect()
	if _, err = element.LocalParticipant.PublishTrack(
		keyFrameTrack(t, ctx, webrtc.MimeTypeH264),
		&lksdk.TrackPublicationOptions{Name: "camera", Source: livekit.TrackSource_CAMERA, BackupCodecPolicy: livekit.BackupCodecPolicy_SIMULCAST},
		lksdk.WithBackupCodec(keyFrameTrack(t, ctx, webrtc.MimeTypeVP8)),
	); err != nil {
		t.Fatal(err)
	}
	// waitFor returns the codec Element was handed the identity's audio or video track in.
	waitFor := func(identity, kind string) webrtc.RTPCodecCapability {
		t.Helper()
		for {
			select {
			case s := <-subscribed:
				if s.identity == identity && strings.HasPrefix(s.codec.MimeType, kind+"/") {
					return s.codec
				}
			case <-ctx.Done():
				t.Fatalf("Element never got %s's %s", identity, kind)
			}
		}
	}

	for _, c := range []struct{ name, restrict, takes string }{
		{"unrestricted", "", webrtc.MimeTypeH264},
		{"VP8", webrtc.MimeTypeVP8, webrtc.MimeTypeVP8},
		{"H264", webrtc.MimeTypeH264, webrtc.MimeTypeH264},
	} {
		t.Run(c.name, func(t *testing.T) {
			identity := "@ghost:x:" + c.name
			ghost, err := JoinRTC(ctx, RTCLegConfig{URL: url, Token: devToken(t, "call", identity), VideoCodec: c.restrict, Log: log})
			if err != nil {
				t.Fatal(err)
			}
			defer ghost.Close()

			track, err := ghost.RemoteVideo(ctx)
			if err != nil {
				t.Fatalf("no video from Element: %v", err)
			}
			if got := track.Codec().MimeType; got != c.takes {
				t.Fatalf("handed Element's camera as %s, want %s", got, c.takes)
			}
			if _, _, err = track.ReadRTP(); err != nil {
				t.Fatalf("read: %v", err)
			}

			// The other network's audio and video, published by the same participant, reach Element.
			if got := waitFor(identity, "audio"); got.MimeType != webrtc.MimeTypeOpus {
				t.Errorf("Element got the bridge's audio as %s", got.MimeType)
			}
			out, err := ghost.AddVideoTrack(c.takes)
			if err != nil {
				t.Fatalf("publishing %s: %v", c.takes, err)
			}
			go keyFramePackets(ctx, c.takes, out)
			if got, want := waitFor(identity, "video"), videoCapability(c.takes); got.MimeType != want.MimeType || got.SDPFmtpLine != want.SDPFmtpLine {
				t.Errorf("Element got the bridge's video as %s %q, want %s %q", got.MimeType, got.SDPFmtpLine, want.MimeType, want.SDPFmtpLine)
			}
		})
	}
}

// A screen shared in Element Call is video like the camera, and its sound is audio like the
// microphone: sorted by kind alone, the screen queued behind the camera and was never sent, and its
// sound took the voice's place.
func TestRouteOf(t *testing.T) {
	for _, c := range []struct {
		kind   webrtc.RTPCodecType
		source livekit.TrackSource
		want   trackRoute
	}{
		{webrtc.RTPCodecTypeAudio, livekit.TrackSource_MICROPHONE, routeAudio},
		{webrtc.RTPCodecTypeVideo, livekit.TrackSource_CAMERA, routeVideo},
		{webrtc.RTPCodecTypeVideo, livekit.TrackSource_SCREEN_SHARE, routeScreen},
		{webrtc.RTPCodecTypeAudio, livekit.TrackSource_SCREEN_SHARE_AUDIO, routeNone},
		// A client that names no source is sorted as before.
		{webrtc.RTPCodecTypeVideo, livekit.TrackSource_UNKNOWN, routeVideo},
		{webrtc.RTPCodecTypeAudio, livekit.TrackSource_UNKNOWN, routeAudio},
	} {
		if got := routeOf(c.kind, c.source); got != c.want {
			t.Errorf("routeOf(%v, %v) = %v, want %v", c.kind, c.source, got, c.want)
		}
	}
}

// When the other side turns its camera off, or a phone goes to the background, the packets stop -
// and a track left published and unmuted with nothing in it was drawn as its last frame, frozen,
// for the rest of the call.
func TestFlowGateMutesASilentVideo(t *testing.T) {
	var states []bool
	resumed := 0
	gate := flowGate{setMuted: func(muted bool) { states = append(states, muted) }, onResume: func() { resumed++ }}
	start := time.Unix(1_700_000_000, 0)

	// Nothing has arrived yet: there is no last frame to freeze on.
	gate.check(start.Add(time.Minute), 2*time.Second)
	if len(states) != 0 {
		t.Fatalf("muted before the first packet: %v", states)
	}

	gate.wrote(start)
	gate.check(start.Add(time.Second), 2*time.Second)
	if len(states) != 0 {
		t.Fatalf("muted while the video was flowing: %v", states)
	}

	// Silence: off, once, however often it is checked.
	gate.check(start.Add(3*time.Second), 2*time.Second)
	gate.check(start.Add(4*time.Second), 2*time.Second)
	if len(states) != 1 || !states[0] {
		t.Fatalf("after silence: %v, want muted once", states)
	}

	// It comes back: on again, and a keyframe is asked for so the picture can be picked up.
	gate.wrote(start.Add(10 * time.Second))
	gate.wrote(start.Add(10*time.Second + 30*time.Millisecond))
	if len(states) != 2 || states[1] || resumed != 1 {
		t.Fatalf("after it came back: %v, resumed %d times; want unmuted once and one keyframe request", states, resumed)
	}
}
