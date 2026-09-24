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
	"errors"
	"io"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// packetSource replays packets, then io.EOF.
type packetSource struct{ pkts []*rtp.Packet }

func (s *packetSource) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if len(s.pkts) == 0 {
		return nil, nil, io.EOF
	}
	p := s.pkts[0]
	s.pkts = s.pkts[1:]
	return p, nil, nil
}

type packetSink struct{ pkts []*rtp.Packet }

func (s *packetSink) WriteRTP(p *rtp.Packet) error {
	s.pkts = append(s.pkts, p)
	return nil
}

// fakeSeal "encrypts" like a codec-aware frame handler: bytes after each Annex-B NAL header are
// XORed, start codes and NAL header bytes stay readable (for VP8 there are none: all bytes after
// the first three). A trailing 0xA5 byte stands in for the tag, so a wrong frame fails to open.
func fakeSeal(frame []byte) ([]byte, error) {
	out := append([]byte{}, frame...)
	keep := nalHeaderPositions(out)
	for i := range out {
		if !keep[i] {
			out[i] ^= 0x5A
		}
	}
	return append(out, 0xA5), nil
}

func fakeOpen(frame []byte) ([]byte, error) {
	if len(frame) == 0 || frame[len(frame)-1] != 0xA5 {
		return nil, errors.New("tag mismatch")
	}
	out := append([]byte{}, frame[:len(frame)-1]...)
	keep := nalHeaderPositions(out)
	for i := range out {
		if !keep[i] {
			out[i] ^= 0x5A
		}
	}
	return out, nil
}

// nalHeaderPositions marks Annex-B start codes and the NAL header byte after each; for a frame
// without start codes (VP8) it keeps the first three bytes.
func nalHeaderPositions(b []byte) map[int]bool {
	keep := map[int]bool{}
	found := false
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			found = true
			for j := i; j <= i+4 && j < len(b); j++ {
				keep[j] = true
			}
		}
	}
	if !found {
		for j := 0; j < 3 && j < len(b); j++ {
			keep[j] = true
		}
	}
	return keep
}

func packetize(t *testing.T, mime string, frames [][]byte, seal FrameTransform) []*rtp.Packet {
	t.Helper()
	_, payloader, err := codecParts(mime)
	if err != nil {
		t.Fatal(err)
	}
	var out []*rtp.Packet
	var seq uint16 = 1000
	for i, f := range frames {
		sealed, err := seal(f)
		if err != nil {
			t.Fatal(err)
		}
		pls := payloader.Payload(videoMTU-12, sealed)
		for j, pl := range pls {
			out = append(out, &rtp.Packet{Header: rtp.Header{
				Version: 2, PayloadType: 96, SSRC: 1234, SequenceNumber: seq,
				Timestamp: uint32(i) * VideoFrameTicks, Marker: j == len(pls)-1,
			}, Payload: pl})
			seq++
		}
	}
	return out
}

func depacketize(t *testing.T, mime string, pkts []*rtp.Packet) [][]byte {
	t.Helper()
	depack, _, err := codecParts(mime)
	if err != nil {
		t.Fatal(err)
	}
	sb := newFrameAssembler(depack)
	var frames [][]byte
	for _, p := range pkts {
		sb.Push(p)
		for s := sb.Pop(); s != nil; s = sb.Pop() {
			frames = append(frames, s.Data)
		}
	}
	return frames
}

func vp8Frames(n, size int) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		f := make([]byte, size+i*37)
		f[0] = 0x50 // keyframe-ish VP8 header bits
		for j := 1; j < len(f); j++ {
			f[j] = byte(i*7 + j)
		}
		frames[i] = f
	}
	return frames
}

func h264Frames(n int) [][]byte {
	sc := []byte{0, 0, 0, 1}
	frames := make([][]byte, n)
	for i := range frames {
		var f []byte
		if i%3 == 0 { // SPS, PPS, IDR
			f = append(f, sc...)
			f = append(f, 0x67, 0x42, 0xc0, 0x1f, 0xda, 0x01, 0x40)
			f = append(f, sc...)
			f = append(f, 0x68, 0xce, 0x3c, 0x80)
			f = append(f, sc...)
			f = append(f, 0x65)
		} else {
			f = append(f, sc...)
			f = append(f, 0x41)
		}
		for j := 0; j < 3000+i*11; j++ {
			b := byte(i + j*3)
			if b == 0 { // keep the body free of accidental start codes
				b = 1
			}
			f = append(f, b)
		}
		frames[i] = f
	}
	return frames
}

// The last frame never pops (the assembler waits for the next frame's first packet), so a relay
// followed by depacketizing returns all frames but the last two.
func checkFrames(t *testing.T, got, want [][]byte) {
	t.Helper()
	if len(got) < len(want)-2 {
		t.Fatalf("got %d frames of %d", len(got), len(want))
	}
	for i, f := range got {
		if !bytes.Equal(f, want[i]) {
			t.Fatalf("frame %d differs (len %d vs %d)", i, len(f), len(want[i]))
		}
	}
}

func TestRelayVideoTransformedRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		mime   string
		frames [][]byte
	}{
		{webrtc.MimeTypeVP8, vp8Frames(12, 5000)},
		{webrtc.MimeTypeH264, h264Frames(12)},
	} {
		t.Run(tc.mime, func(t *testing.T) {
			in := packetize(t, tc.mime, tc.frames, fakeSeal)
			var sink packetSink
			var stats FrameRelayStats
			err := RelayVideoTransformed(context.Background(), &packetSource{pkts: in}, 96, tc.mime, &sink, fakeOpen, nil, &stats, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			if stats.Failed.Load() != 0 {
				t.Fatalf("%d frames failed to open", stats.Failed.Load())
			}
			for _, p := range sink.pkts {
				if len(p.Payload)+12 > videoMTU {
					t.Fatalf("packet of %d bytes exceeds the MTU", len(p.Payload)+12)
				}
			}
			checkFrames(t, depacketize(t, tc.mime, sink.pkts), tc.frames)
		})
	}
}

func TestRelayVideoTransformedReorderAndLoss(t *testing.T) {
	frames := vp8Frames(20, 4000) // enough frames after the loss for the 200 ms give-up
	in := packetize(t, webrtc.MimeTypeVP8, frames, fakeSeal)
	// Swap two packets inside frame 1 (reordering), and drop the second packet of frame 4 (loss).
	var perFrame [][]*rtp.Packet
	for _, p := range in {
		idx := int(p.Timestamp / VideoFrameTicks)
		for len(perFrame) <= idx {
			perFrame = append(perFrame, nil)
		}
		perFrame[idx] = append(perFrame[idx], p)
	}
	perFrame[1][0], perFrame[1][1] = perFrame[1][1], perFrame[1][0]
	perFrame[4] = append(perFrame[4][:1], perFrame[4][2:]...)
	var mixed []*rtp.Packet
	for _, f := range perFrame {
		mixed = append(mixed, f...)
	}

	var sink packetSink
	var stats FrameRelayStats
	losses := 0
	err := RelayVideoTransformed(context.Background(), &packetSource{pkts: mixed}, 96, webrtc.MimeTypeVP8, &sink, fakeOpen,
		func() { losses++ }, &stats, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if losses == 0 || stats.LostPackets.Load() == 0 {
		t.Fatal("the lost packet wasn't reported (no keyframe would be requested)")
	}
	// Frames 0–3 (incl. the reordered one) arrive intact; frame 4 is gone. The assembler gives up on
	// it once newer media is videoMaxDelay ahead, which can take the next frame along; that's harmless
	// (it references the lost frame and can't be decoded until the keyframe the loss asks for). Every
	// frame that does come out is intact and in order, and everything after that window arrives.
	got := depacketize(t, webrtc.MimeTypeVP8, sink.pkts)
	checkFrames(t, got[:4], frames[:4])
	next := 4
	for _, f := range got[4:] {
		for next < len(frames) && !bytes.Equal(f, frames[next]) {
			if next != 4 && next != 5 {
				t.Fatalf("frame %d missing after the loss window", next)
			}
			next++
		}
		if next == len(frames) {
			t.Fatal("a relayed frame matches no source frame (corrupted or out of order)")
		}
		if next == 4 {
			t.Fatal("the lost frame came out")
		}
		next++
	}
}

func TestRelayTransformedDropsFramesThatDontOpen(t *testing.T) {
	frames := vp8Frames(6, 2000)
	good := packetize(t, webrtc.MimeTypeVP8, frames, fakeSeal)
	// A frame sealed "with another key" (no tag) in the middle.
	bad := packetize(t, webrtc.MimeTypeVP8, [][]byte{frames[2]}, func(b []byte) ([]byte, error) { return b, nil })
	var in []*rtp.Packet
	for _, p := range good {
		if p.Timestamp/VideoFrameTicks == 2 {
			continue
		}
		in = append(in, p)
		if p.Timestamp/VideoFrameTicks == 1 && p.Marker {
			for _, b := range bad {
				q := *b
				q.Timestamp = 2 * VideoFrameTicks
				q.SequenceNumber = p.SequenceNumber + 1 + (b.SequenceNumber - bad[0].SequenceNumber)
				in = append(in, &q)
			}
		}
	}
	// Renumber so sequence numbers stay contiguous.
	for i, p := range in {
		p.SequenceNumber = uint16(5000 + i)
	}
	var sink packetSink
	var stats FrameRelayStats
	if err := RelayVideoTransformed(context.Background(), &packetSource{pkts: in}, 96, webrtc.MimeTypeVP8, &sink, fakeOpen, nil, &stats, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	if stats.Failed.Load() != 1 {
		t.Fatalf("expected exactly the one bad frame to fail, got %d", stats.Failed.Load())
	}
	got := depacketize(t, webrtc.MimeTypeVP8, sink.pkts)
	want := [][]byte{frames[0], frames[1], frames[3], frames[4], frames[5]}
	checkFrames(t, got, want)
}

func TestRelayAudioTransformed(t *testing.T) {
	var in []*rtp.Packet
	var want [][]byte
	for i := range 20 {
		frame := bytes.Repeat([]byte{byte(i + 1)}, 60+i)
		want = append(want, frame)
		sealed, _ := fakeSeal(frame)
		in = append(in, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 9, SequenceNumber: uint16(i), Timestamp: uint32(i) * 960}, Payload: sealed})
	}
	in = append(in, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 63, SequenceNumber: 20}, Payload: []byte{1}}) // RED: dropped
	var sink packetSink
	var stats FrameRelayStats
	if err := RelayAudioTransformed(context.Background(), &packetSource{pkts: in}, 111, &sink, fakeOpen, &stats, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	if len(sink.pkts) != 20 || stats.Failed.Load() != 0 || stats.Dropped.Load() != 1 {
		t.Fatalf("relayed %d, failed %d, dropped %d", len(sink.pkts), stats.Failed.Load(), stats.Dropped.Load())
	}
	for i, p := range sink.pkts {
		if !bytes.Equal(p.Payload, want[i]) {
			t.Fatalf("audio frame %d differs", i)
		}
	}
}
