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
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// videoArrivals counts, from a leg's log, the remote video tracks that have started, so a test can
// wait until a track has arrived without taking it.
type videoArrivals struct {
	lock sync.Mutex
	n    int
}

func (v *videoArrivals) Write(line []byte) (int, error) {
	if bytes.Contains(line, []byte(`"Remote track started"`)) && bytes.Contains(line, []byte(`"kind":"video"`)) {
		v.lock.Lock()
		v.n++
		v.lock.Unlock()
	}
	return len(line), nil
}

func (v *videoArrivals) wait(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	for {
		v.lock.Lock()
		got := v.n
		v.lock.Unlock()
		if got >= n {
			// The log line comes just before the track is handed over.
			time.Sleep(50 * time.Millisecond)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%d remote video tracks arrived, want %d", got, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A screen shared beside the camera is a second video track on the same leg (Messenger's web client
// in a 1:1 call). Both have to be there for whoever takes the leg's video, in the order they came,
// also when the second arrives before the first has been taken: the leg used to keep one and drop
// the other.
func TestSecondVideoTrackIsKept(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	arrivals := &videoArrivals{}
	mk := func(name string, log zerolog.Logger) *Leg {
		l, err := NewLeg(LegConfig{Name: name, AllowVideo: true, Settings: loopbackSettings(), Log: log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.Close)
		return l
	}
	sender, receiver := mk("peer", zerolog.Nop()), mk("messenger", zerolog.New(arrivals))
	connect(t, ctx, sender, receiver)
	renegotiate := func() {
		t.Helper()
		offer, err := sender.Renegotiate()
		if err != nil {
			t.Fatal(err)
		}
		answer, err := receiver.AnswerOffer(offer)
		if err != nil {
			t.Fatal(err)
		}
		if err = sender.SetAnswer(answer); err != nil {
			t.Fatal(err)
		}
	}
	send := func(track *webrtc.TrackLocalStaticRTP, payload string) {
		go func() {
			for i := uint16(0); ctx.Err() == nil; i++ {
				_ = track.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: i, Timestamp: uint32(i) * VideoFrameTicks, Marker: true},
					Payload: []byte(payload),
				})
				time.Sleep(33 * time.Millisecond)
			}
		}()
	}

	if err := sender.AddVideoTrack(webrtc.MimeTypeVP8); err != nil {
		t.Fatal(err)
	}
	renegotiate()
	send(sender.LocalVideo, "camera")
	arrivals.wait(t, ctx, 1)
	if err := sender.AddScreenTrack(webrtc.MimeTypeVP8); err != nil {
		t.Fatal(err)
	}
	renegotiate()
	send(sender.LocalScreen, "screen")
	arrivals.wait(t, ctx, 2)

	// Only now does anyone ask for the leg's video.
	for _, want := range []struct{ id, payload string }{
		{sender.VideoTrackID, "camera"},
		{sender.ScreenTrackID, "screen"},
	} {
		takeCtx, takeCancel := context.WithTimeout(ctx, 5*time.Second)
		tr, err := receiver.RemoteVideoTrack(takeCtx)
		takeCancel()
		if err != nil {
			t.Fatalf("the %s's track: %v", want.payload, err)
		}
		if tr.ID() != want.id {
			t.Fatalf("got track %s, want the %s's (%s)", tr.ID(), want.payload, want.id)
		}
		if p, _, err := tr.ReadRTP(); err != nil || string(p.Payload) != want.payload {
			t.Fatalf("read from the %s's track: %v %v", want.payload, p, err)
		}
	}
}

// Whoever already waits for the leg's video gets a track that arrives later, and the one after it
// when it asks again.
func TestWaitingForVideoGetsLaterTracks(t *testing.T) {
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
	sender, receiver := mk("peer"), mk("messenger")
	connect(t, ctx, sender, receiver)
	taken := make(chan string, 2)
	go func() {
		for range 2 {
			tr, err := receiver.RemoteVideoTrack(ctx)
			if err != nil {
				return
			}
			taken <- tr.ID()
		}
	}()
	add := func(add func(string) error, track func() *webrtc.TrackLocalStaticRTP) {
		t.Helper()
		if err := add(webrtc.MimeTypeVP8); err != nil {
			t.Fatal(err)
		}
		offer, err := sender.Renegotiate()
		if err != nil {
			t.Fatal(err)
		}
		answer, err := receiver.AnswerOffer(offer)
		if err != nil {
			t.Fatal(err)
		}
		if err = sender.SetAnswer(answer); err != nil {
			t.Fatal(err)
		}
		local := track()
		go func() {
			for i := uint16(0); ctx.Err() == nil; i++ {
				_ = local.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: i, Timestamp: uint32(i) * VideoFrameTicks, Marker: true},
					Payload: []byte("frame"),
				})
				time.Sleep(33 * time.Millisecond)
			}
		}()
	}
	expect := func(what, id string) {
		t.Helper()
		select {
		case got := <-taken:
			if got != id {
				t.Fatalf("took track %s, want the %s's (%s)", got, what, id)
			}
		case <-ctx.Done():
			t.Fatalf("the %s's track never reached the one waiting for it", what)
		}
	}
	add(sender.AddVideoTrack, func() *webrtc.TrackLocalStaticRTP { return sender.LocalVideo })
	expect("camera", sender.VideoTrackID)
	add(sender.AddScreenTrack, func() *webrtc.TrackLocalStaticRTP { return sender.LocalScreen })
	expect("screen", sender.ScreenTrackID)
}

// Closing the leg ends the wait for its tracks, also for a caller whose context outlives the leg.
func TestCloseEndsTheWaitForTracks(t *testing.T) {
	leg, err := NewLeg(LegConfig{Name: "messenger", AllowVideo: true, Settings: loopbackSettings(), Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(leg.Close)
	errs := make(chan error, 2)
	go func() {
		_, err := leg.RemoteVideoTrack(context.Background())
		errs <- err
	}()
	go func() {
		_, err := leg.RemoteTrack(context.Background())
		errs <- err
	}()
	// Let both get to waiting.
	time.Sleep(50 * time.Millisecond)
	leg.Close()
	sawNoAudio := false
	for range 2 {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("a track from a leg that never had one")
			}
			sawNoAudio = sawNoAudio || errors.Is(err, ErrNoRemoteTrack)
		case <-time.After(5 * time.Second):
			t.Fatal("still waiting for a track of a closed leg")
		}
	}
	if !sawNoAudio {
		t.Fatal("the wait for audio didn't end with ErrNoRemoteTrack")
	}
	// And asking after the close doesn't wait either.
	if _, err := leg.RemoteVideoTrack(context.Background()); err == nil {
		t.Fatal("a track from a closed leg")
	}
}
