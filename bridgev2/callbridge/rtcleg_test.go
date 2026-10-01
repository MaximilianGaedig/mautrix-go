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
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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
