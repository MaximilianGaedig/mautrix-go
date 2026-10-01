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
	"sync/atomic"
	"time"

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
	video       *videoOut
	screen      *videoOut
	remoteAudio chan *webrtc.TrackRemote
	remoteVideo chan *webrtc.TrackRemote
	// remoteScreen is a Matrix participant's shared screen: a track of its own beside the camera,
	// so sharing a screen doesn't take the camera's place (or wait behind it).
	remoteScreen chan *webrtc.TrackRemote
	owners       map[*webrtc.TrackRemote]*lksdk.RemoteParticipant
	receivers    map[*webrtc.TrackRemote]*webrtc.RTPReceiver
	onPeers      func(identities []string)
	onKeyframe   func()
	onScreenKey  func()
	onMedia      func(audioOn, videoOn bool)
	closed       bool
}

// quietSDK stops the LiveKit SDK logging every connection-state change to stderr (its default); its
// failures reach us as errors.
var quietSDK sync.Once

// JoinRTC connects to the LiveKit room and publishes an Opus audio track.
func JoinRTC(ctx context.Context, cfg RTCLegConfig) (*RTCLeg, error) {
	quietSDK.Do(func() { lksdk.SetLogger(protoLogger.LogRLogger(logr.Discard())) })
	l := &RTCLeg{
		log:          cfg.Log,
		accept:       cfg.Accept,
		remoteAudio:  make(chan *webrtc.TrackRemote, 1),
		remoteVideo:  make(chan *webrtc.TrackRemote, 1),
		remoteScreen: make(chan *webrtc.TrackRemote, 1),
		owners:       map[*webrtc.TrackRemote]*lksdk.RemoteParticipant{},
		receivers:    map[*webrtc.TrackRemote]*webrtc.RTPReceiver{},
	}
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = l.onTrackSubscribed
	cb.OnParticipantConnected = func(*lksdk.RemoteParticipant) { l.notifyPeers() }
	cb.OnParticipantDisconnected = func(*lksdk.RemoteParticipant) { l.notifyPeers() }
	// A camera turned off in Element Call is a muted track, not a stopped one: the far side of the
	// bridge has to be told, or it keeps showing the last frame.
	cb.OnTrackMuted = func(lksdk.TrackPublication, lksdk.Participant) { l.notifyMedia() }
	cb.OnTrackUnmuted = func(lksdk.TrackPublication, lksdk.Participant) { l.notifyMedia() }
	cb.OnTrackUnpublished = func(*lksdk.RemoteTrackPublication, *lksdk.RemoteParticipant) { l.notifyMedia() }
	cb.OnTrackPublished = func(*lksdk.RemoteTrackPublication, *lksdk.RemoteParticipant) { l.notifyMedia() }
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
	return l.addVideo(mime, &l.video, livekit.TrackSource_CAMERA, "camera", videoIdle, l.requestKeyframe)
}

// AddScreenTrack publishes a screen share track (once) and returns where the other network's shared
// screen goes: a source of its own, so Element Call shows it as a screen share beside the camera.
func (l *RTCLeg) AddScreenTrack(mime string) (RTPWriter, error) {
	return l.addVideo(mime, &l.screen, livekit.TrackSource_SCREEN_SHARE, "screen", screenIdle, l.requestScreenKeyframe)
}

func (l *RTCLeg) addVideo(mime string, slot **videoOut, source livekit.TrackSource, name string, idle time.Duration, onKeyframe func()) (RTPWriter, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if *slot != nil {
		return *slot, nil
	}
	// A LiveKit LocalTrack rather than a static one: it hands us the subscribers' keyframe requests
	// (PLI/FIR), which the SDK otherwise swallows.
	track, err := lksdk.NewLocalTrack(videoCapability(mime), lksdk.WithRTCPHandler(func(p rtcp.Packet) {
		switch p.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			onKeyframe()
		}
	}))
	if err != nil {
		return nil, err
	}
	pub, err := l.room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   name,
		Source: source,
	})
	if err != nil {
		return nil, fmt.Errorf("publish %s: %w", name, err)
	}
	out := &videoOut{t: track}
	out.gate.setMuted = func(muted bool) {
		l.log.Debug().Str("track", name).Bool("muted", muted).Msg("The other network's video stopped or came back")
		pub.SetMuted(muted)
	}
	// Whoever is watching needs a keyframe to pick the picture up again.
	out.gate.onResume = onKeyframe
	*slot = out
	go l.watchFlow(&out.gate, idle)
	return out, nil
}

// videoIdle is how long the other network may send nothing of a camera before the Matrix call is
// told it is off. screenIdle is the same for a shared screen, which is given longer: a screen that
// isn't changing is sent at a frame or so a second, and at times less.
const (
	videoIdle  = 2 * time.Second
	screenIdle = 8 * time.Second
)

// watchFlow mutes a published video while nothing is being written to it.
func (l *RTCLeg) watchFlow(gate *flowGate, idle time.Duration) {
	tick := time.NewTicker(videoIdle / 4)
	defer tick.Stop()
	for now := range tick.C {
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return
		}
		gate.check(now, idle)
	}
}

// flowGate tells a call that a video is off while nothing arrives for it, and on again when it does.
//
// The other network says a camera was turned off, or a screen share ended, in its own signalling -
// or not at all: a phone that goes to the background just stops sending. Either way the packets
// stop, and a track that stays published and unmuted with nothing in it is drawn as its last frame,
// frozen, for as long as the call lasts. Muting it on silence makes the Matrix side show what is
// true (the camera is off, the share is over), whatever the reason and whichever bridge it is.
type flowGate struct {
	last     atomic.Int64 // when the last packet was written, in unix nanoseconds; 0 before the first
	muted    atomic.Bool
	setMuted func(bool)
	onResume func()
}

// wrote notes a packet, and unmutes the video if it had gone quiet.
func (g *flowGate) wrote(now time.Time) {
	g.last.Store(now.UnixNano())
	if g.muted.CompareAndSwap(true, false) {
		g.setMuted(false)
		if g.onResume != nil {
			g.onResume()
		}
	}
}

// check mutes the video if nothing has been written for idle. Before the first packet there is
// nothing to freeze on, so nothing to mute.
func (g *flowGate) check(now time.Time, idle time.Duration) {
	last := g.last.Load()
	if last == 0 || now.Sub(time.Unix(0, last)) < idle {
		return
	}
	if g.muted.CompareAndSwap(false, true) {
		g.setMuted(true)
	}
}

// videoOut is a published video: an RTPWriter over a LiveKit LocalTrack that keeps its flowGate.
type videoOut struct {
	t    *lksdk.LocalTrack
	gate flowGate
}

func (w *videoOut) WriteRTP(p *rtp.Packet) error {
	w.gate.wrote(time.Now())
	return w.t.WriteRTP(p, nil)
}

// RemoteAudio waits for the first Matrix participant's microphone.
func (l *RTCLeg) RemoteAudio(ctx context.Context) (*webrtc.TrackRemote, error) {
	return waitTrack(ctx, l.remoteAudio)
}

// RemoteVideo waits for a Matrix participant's camera.
func (l *RTCLeg) RemoteVideo(ctx context.Context) (*webrtc.TrackRemote, error) {
	return waitTrack(ctx, l.remoteVideo)
}

// RemoteScreen waits for a Matrix participant's shared screen.
func (l *RTCLeg) RemoteScreen(ctx context.Context) (*webrtc.TrackRemote, error) {
	return waitTrack(ctx, l.remoteScreen)
}

// ScreenOn says whether any other participant is sharing their screen.
func (l *RTCLeg) ScreenOn() bool {
	if l.room == nil {
		return false
	}
	for _, p := range l.room.GetRemoteParticipants() {
		if l.accept != nil && !l.accept(p.Identity()) {
			continue
		}
		if p.IsScreenShareEnabled() {
			return true
		}
	}
	return false
}

// trackRoute is where a subscribed MatrixRTC track goes.
type trackRoute int

const (
	routeAudio trackRoute = iota
	routeVideo
	routeScreen
	// routeNone is a track the bridge has no place for: a shared screen's sound, which the other
	// network has no track for and which must not take the microphone's place.
	routeNone
)

// routeOf sorts a track by what it is a picture or sound of, not only by its kind: a shared screen
// is video like a camera, and its sound is audio like a microphone, and each used to land where
// the camera and the microphone go - the screen waiting behind the camera, never sent, and its
// sound replacing the voice.
func routeOf(kind webrtc.RTPCodecType, source livekit.TrackSource) trackRoute {
	switch source {
	case livekit.TrackSource_SCREEN_SHARE:
		return routeScreen
	case livekit.TrackSource_SCREEN_SHARE_AUDIO:
		return routeNone
	}
	if kind == webrtc.RTPCodecTypeVideo {
		return routeVideo
	}
	return routeAudio
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

// OnScreenKeyframeRequest is called when a Matrix participant needs a keyframe of our screen share.
func (l *RTCLeg) OnScreenKeyframeRequest(fn func()) {
	l.mu.Lock()
	l.onScreenKey = fn
	l.mu.Unlock()
}

func (l *RTCLeg) requestScreenKeyframe() {
	l.mu.Lock()
	fn := l.onScreenKey
	l.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// OnMediaState is called with whether the other participants' microphone and camera are on, each
// time one is muted, unmuted, published or unpublished.
func (l *RTCLeg) OnMediaState(fn func(audioOn, videoOn bool)) {
	l.mu.Lock()
	l.onMedia = fn
	l.mu.Unlock()
}

// MediaState says whether any other participant's microphone and camera are on.
func (l *RTCLeg) MediaState() (audioOn, videoOn bool) {
	if l.room == nil {
		return false, false
	}
	for _, p := range l.room.GetRemoteParticipants() {
		if l.accept != nil && !l.accept(p.Identity()) {
			continue
		}
		audioOn = audioOn || p.IsMicrophoneEnabled()
		videoOn = videoOn || p.IsCameraEnabled()
	}
	return audioOn, videoOn
}

// notifyMedia runs on its own goroutine: the SDK calls the track callbacks with its room lock held
// (a participant joining is added under it), and MediaState takes that lock again - called inline,
// the first Matrix participant to join froze the whole leg.
func (l *RTCLeg) notifyMedia() {
	l.mu.Lock()
	fn := l.onMedia
	l.mu.Unlock()
	if fn != nil {
		go func() { fn(l.MediaState()) }()
	}
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
		Stringer("source", pub.Source()).Str("codec", track.Codec().MimeType).Msg("Subscribed to MatrixRTC track")
	var ch chan *webrtc.TrackRemote
	switch routeOf(track.Kind(), pub.Source()) {
	case routeAudio:
		ch = l.remoteAudio
	case routeVideo:
		ch = l.remoteVideo
	case routeScreen:
		ch = l.remoteScreen
	case routeNone:
		_ = pub.SetSubscribed(false)
		return
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
