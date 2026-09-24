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
	"errors"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// RTCLegConfig describes how to join a MatrixRTC call's LiveKit room.
type RTCLegConfig struct {
	// URL and Token come from lk-jwt-service (the call's focus), for the ghost's Matrix identity.
	URL   string
	Token string
	Log   zerolog.Logger
	// Accept, when set, picks whose tracks this leg takes (by LiveKit identity); others are
	// unsubscribed. In a group call only one leg feeds the Matrix user's media to Messenger, and the
	// other participants' legs take nothing.
	Accept func(identity string) bool
}

// RTCLeg is the bridge's side of a MatrixRTC (Element Call / Element X) call: a LiveKit participant,
// standing in for the remote network's user, that publishes their audio (and video) and hands over
// the media the Matrix participants publish. Unlike Leg there is no SDP to shuttle: LiveKit's
// signalling happens inside the SDK.
type RTCLeg struct {
	log    zerolog.Logger
	accept func(identity string) bool
	room   *lksdk.Room

	audio *webrtc.TrackLocalStaticRTP

	mu          sync.Mutex
	video       *lksdk.LocalTrack
	remoteAudio chan *webrtc.TrackRemote
	remoteVideo chan *webrtc.TrackRemote
	owners      map[*webrtc.TrackRemote]*lksdk.RemoteParticipant
	receivers   map[*webrtc.TrackRemote]*webrtc.RTPReceiver
	onPeers     func(identities []string)
	onKeyframe  func()
	closed      bool
}

// quietSDK stops the LiveKit SDK logging every connection-state change to stderr (its default); its
// failures reach us as errors.
var quietSDK sync.Once

// JoinRTC connects to the LiveKit room and publishes an Opus audio track.
func JoinRTC(ctx context.Context, cfg RTCLegConfig) (*RTCLeg, error) {
	quietSDK.Do(func() { lksdk.SetLogger(protoLogger.LogRLogger(logr.Discard())) })
	l := &RTCLeg{
		log:         cfg.Log,
		accept:      cfg.Accept,
		remoteAudio: make(chan *webrtc.TrackRemote, 1),
		remoteVideo: make(chan *webrtc.TrackRemote, 1),
		owners:      map[*webrtc.TrackRemote]*lksdk.RemoteParticipant{},
		receivers:   map[*webrtc.TrackRemote]*webrtc.RTPReceiver{},
	}
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = l.onTrackSubscribed
	cb.OnParticipantConnected = func(*lksdk.RemoteParticipant) { l.notifyPeers() }
	cb.OnParticipantDisconnected = func(*lksdk.RemoteParticipant) { l.notifyPeers() }
	cb.OnLocalTrackSubscribed = func(pub *lksdk.LocalTrackPublication, _ *lksdk.LocalParticipant) {
		// A Matrix participant started watching our video: it needs a keyframe to start decoding.
		if pub.Kind() == lksdk.TrackKindVideo {
			l.requestKeyframe()
		}
	}

	type result struct {
		room *lksdk.Room
		err  error
	}
	done := make(chan result, 1)
	go func() {
		room, err := lksdk.ConnectToRoomWithToken(cfg.URL, cfg.Token, cb, lksdk.WithAutoSubscribe(true))
		done <- result{room, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		go func() {
			if r := <-done; r.room != nil {
				r.room.Disconnect()
			}
		}()
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, fmt.Errorf("join LiveKit room: %w", res.err)
	}
	l.room = res.room

	audio, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "bridge",
	)
	if err != nil {
		l.Close()
		return nil, err
	}
	if _, err = l.room.LocalParticipant.PublishTrack(audio, &lksdk.TrackPublicationOptions{
		Name:   "microphone",
		Source: livekit.TrackSource_MICROPHONE,
	}); err != nil {
		l.Close()
		return nil, fmt.Errorf("publish audio: %w", err)
	}
	l.audio = audio
	l.notifyPeers()
	return l, nil
}

// AudioWriter is where the other network's audio goes.
func (l *RTCLeg) AudioWriter() RTPWriter { return l.audio }

// AddVideoTrack publishes a camera track (once) and returns where the other network's video goes.
func (l *RTCLeg) AddVideoTrack(mime string) (RTPWriter, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.video != nil {
		return localTrackWriter{l.video}, nil
	}
	// A LiveKit LocalTrack rather than a static one: it hands us the subscribers' keyframe requests
	// (PLI/FIR), which the SDK otherwise swallows.
	video, err := lksdk.NewLocalTrack(videoCapability(mime), lksdk.WithRTCPHandler(func(p rtcp.Packet) {
		switch p.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			l.requestKeyframe()
		}
	}))
	if err != nil {
		return nil, err
	}
	if _, err = l.room.LocalParticipant.PublishTrack(video, &lksdk.TrackPublicationOptions{
		Name:   "camera",
		Source: livekit.TrackSource_CAMERA,
	}); err != nil {
		return nil, fmt.Errorf("publish video: %w", err)
	}
	l.video = video
	return localTrackWriter{video}, nil
}

// localTrackWriter adapts a LiveKit LocalTrack to RTPWriter.
type localTrackWriter struct{ t *lksdk.LocalTrack }

func (w localTrackWriter) WriteRTP(p *rtp.Packet) error { return w.t.WriteRTP(p, nil) }

// RemoteAudio waits for the first Matrix participant's microphone.
func (l *RTCLeg) RemoteAudio(ctx context.Context) (*webrtc.TrackRemote, error) {
	return waitTrack(ctx, l.remoteAudio)
}

// RemoteVideo waits for a Matrix participant's camera.
func (l *RTCLeg) RemoteVideo(ctx context.Context) (*webrtc.TrackRemote, error) {
	return waitTrack(ctx, l.remoteVideo)
}

func waitTrack(ctx context.Context, ch chan *webrtc.TrackRemote) (*webrtc.TrackRemote, error) {
	select {
	case tr, ok := <-ch:
		if !ok {
			return nil, errors.New("leg closed")
		}
		return tr, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// OnPeers is called with the identities of the other participants whenever they change.
func (l *RTCLeg) OnPeers(fn func(identities []string)) {
	l.mu.Lock()
	l.onPeers = fn
	l.mu.Unlock()
	l.notifyPeers()
}

// OnKeyframeRequest is called when a Matrix participant needs a keyframe of our video.
func (l *RTCLeg) OnKeyframeRequest(fn func()) {
	l.mu.Lock()
	l.onKeyframe = fn
	l.mu.Unlock()
}

// Peers lists the other participants' identities.
func (l *RTCLeg) Peers() []string {
	if l.room == nil {
		return nil
	}
	var ids []string
	for _, p := range l.room.GetRemoteParticipants() {
		ids = append(ids, p.Identity())
	}
	return ids
}

func (l *RTCLeg) notifyPeers() {
	l.mu.Lock()
	fn := l.onPeers
	l.mu.Unlock()
	if fn != nil {
		fn(l.Peers())
	}
}

func (l *RTCLeg) requestKeyframe() {
	l.mu.Lock()
	fn := l.onKeyframe
	l.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (l *RTCLeg) onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if l.accept != nil && !l.accept(rp.Identity()) {
		_ = pub.SetSubscribed(false)
		return
	}
	l.log.Debug().Str("participant", rp.Identity()).Str("kind", track.Kind().String()).
		Str("codec", track.Codec().MimeType).Msg("Subscribed to MatrixRTC track")
	ch := l.remoteAudio
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		ch = l.remoteVideo
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.owners[track] = rp
	l.receivers[track] = pub.Receiver()
	// Keep the newest: a participant who rejoins replaces their old track.
	select {
	case <-ch:
	default:
	}
	ch <- track
}

// HeaderExtensionID returns the id a subscribed track's packets carry the header extension uri
// under, or 0.
func (l *RTCLeg) HeaderExtensionID(track *webrtc.TrackRemote, uri string) uint8 {
	l.mu.Lock()
	recv := l.receivers[track]
	l.mu.Unlock()
	if recv == nil {
		return 0
	}
	return extensionID(recv.GetParameters().HeaderExtensions, uri)
}

func extensionID(exts []webrtc.RTPHeaderExtensionParameter, uri string) uint8 {
	for _, e := range exts {
		if e.URI == uri {
			return uint8(e.ID)
		}
	}
	return 0
}

// RequestKeyframe asks the publisher of a subscribed video track for a keyframe (through the SFU).
func (l *RTCLeg) RequestKeyframe(track *webrtc.TrackRemote) {
	l.mu.Lock()
	rp := l.owners[track]
	l.mu.Unlock()
	if rp != nil {
		rp.WritePLI(track.SSRC())
	}
}

// Close leaves the room.
func (l *RTCLeg) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.mu.Unlock()
	if l.room != nil {
		l.room.Disconnect()
	}
}
