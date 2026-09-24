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

// Package callbridge contains reusable media legs and RTP relays for bridging
// calls between MatrixRTC and remote networks.
package callbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/rs/zerolog"
)

// OpusPT is the Opus payload type both Chrome and Element use.
const OpusPT = 111

// Opus fmtp offered on the bridge's legs; answers adopt the offerer's.
const webOpusFmtp = "minptime=10;useinbandfec=1"

// LegConfig configures one PeerConnection.
type LegConfig struct {
	Name       string
	ICEServers []webrtc.ICEServer
	// OpusPT is the payload type Opus is registered under when this leg
	// makes the offer. Answers use the offerer's payload type.
	OpusPT uint8
	// WebShape registers the web client's video codecs and adds a recvonly
	// video transceiver and a data channel to offers, so an offer has the
	// same m-lines (audio, video, application) as facebook.com's.
	WebShape bool
	// SFU declares the web client's RTP header extensions (MID among them), which Messenger's SFU
	// routes participants' streams by.
	SFU bool
	// PlanB creates the PeerConnection with Plan B semantics, for answering a
	// Plan B offer (Messenger's mobile apps); see IsPlanB. Everything else is
	// Unified Plan. Pion's "Unified Plan with fallback" is not used: it guesses
	// Plan B from Unified Plan descriptions too and then crashes reading a
	// track whose stream was never set up (RTPReceiver.readRTP nil deref).
	PlanB bool
	// VideoCodec is the video codec (webrtc.MimeTypeVP8 or MimeTypeH264)
	// this leg sends and receives, or "" for an audio call. Both legs of a
	// call use the same one, so video is relayed without transcoding. Only
	// that codec is registered, which forces the negotiation.
	VideoCodec string
	// AllowVideo registers the video codecs even for an audio call, so a video
	// track can be added later (AddVideoTrack) when the other side upgrades.
	AllowVideo bool
	// Settings optionally overrides the Pion setting engine (tests use it to
	// restrict ICE to loopback).
	Settings *webrtc.SettingEngine
	Log      zerolog.Logger
}

// Leg is one side of a bridged call.
type Leg struct {
	Name  string
	PC    *webrtc.PeerConnection
	Local *webrtc.TrackLocalStaticRTP
	// TrackID and StreamID are the msid of Local.
	TrackID, StreamID string
	// LocalVideo is the video track relayed to this leg (nil for audio
	// calls), VideoTrackID its msid track id.
	LocalVideo   *webrtc.TrackLocalStaticRTP
	VideoTrackID string

	log     zerolog.Logger
	cfg     LegConfig
	sender  *webrtc.RTPSender
	vsender *webrtc.RTPSender
	remote  chan *webrtc.TrackRemote
	remoteV chan *webrtc.TrackRemote

	onKeyframeRequest func()
	lock              sync.Mutex
	pending           []webrtc.ICECandidateInit
	haveDesc          bool
	closed            bool

	onCandidate func(*webrtc.ICECandidateInit)
	onState     func(webrtc.PeerConnectionState)
	gathered    chan struct{}
	// onRemoteTrack, when set, gets every remote track (SFU calls carry one
	// per participant) instead of RemoteTrack/RemoteVideoTrack.
	onRemoteTrack func(*webrtc.TrackRemote)
	onData        func(label string, data []byte)
	channels      map[string]*webrtc.DataChannel
	// unappliedOffer is a mid-call offer sent but not yet answered. It is
	// applied (SetLocalDescription) only together with its answer: Pion
	// has no rollback, so an offer the peer rejects or talks over would
	// otherwise leave the connection stuck in have-local-offer.
	unappliedOffer string
	// localCandidates are our gathered ICE candidates ("candidate:…"), put
	// into renegotiation offers (CreateOffer's SDP has none).
	localCandidates []string
	gatheringDone   bool
}

// NewLeg creates a PeerConnection with Opus audio (and, for WebShape, the
// web client's video codecs).
func NewLeg(cfg LegConfig) (*Leg, error) {
	if cfg.OpusPT == 0 {
		cfg.OpusPT = OpusPT
	}
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: webOpusFmtp,
		},
		PayloadType: webrtc.PayloadType(cfg.OpusPT),
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if cfg.WebShape || cfg.VideoCodec != "" || cfg.AllowVideo {
		for _, c := range videoCodecs {
			if cfg.VideoCodec != "" && c.MimeType != cfg.VideoCodec {
				continue
			}
			if err := me.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, err
			}
		}
	}
	if cfg.SFU {
		// The web client's SFU offer declares these; the SFU routes each participant's stream by
		// its MID extension, and without it Pion can't tell streams it has no a=ssrc for apart.
		for _, ext := range webHeaderExtensions {
			if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: ext.uri}, ext.kind); err != nil {
				return nil, err
			}
		}
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, err
	}
	var se webrtc.SettingEngine
	if cfg.Settings != nil {
		se = *cfg.Settings
	}
	// Pion holds back nominating a working pair for its candidate type's
	// "acceptance min wait": 2 s for relay and 1 s for peer-reflexive by
	// default, which was most of the time between answering and hearing the
	// other side. Browsers nominate as soon as a check succeeds; keep only a
	// short head start for the better (direct) pairs.
	se.SetHostAcceptanceMinWait(0)
	se.SetSrflxAcceptanceMinWait(50 * time.Millisecond)
	se.SetPrflxAcceptanceMinWait(100 * time.Millisecond)
	se.SetRelayAcceptanceMinWait(200 * time.Millisecond)
	// Pion's DTLS retransmits after 1 s; a lost first flight cost a second
	// of silence after ICE connected.
	se.SetDTLSRetransmissionInterval(100 * time.Millisecond)
	// Pion's own warnings (e.g. RTP for an SSRC no transceiver claims, so
	// no track ever starts) go to the call log.
	se.LoggerFactory = &pionLoggerFactory{log: cfg.Log.With().Str("leg", cfg.Name).Logger()}
	opts := []func(*webrtc.API){
		webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se),
	}
	api := webrtc.NewAPI(opts...)
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:    cfg.ICEServers,
		SDPSemantics:  semantics(cfg.PlanB),
		BundlePolicy:  webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire,
	})
	if err != nil {
		return nil, err
	}
	l := &Leg{
		Name:     cfg.Name,
		PC:       pc,
		TrackID:  uuid.NewString(),
		StreamID: uuid.NewString(),
		log:      cfg.Log.With().Str("leg", cfg.Name).Logger(),
		cfg:      cfg,
		channels: map[string]*webrtc.DataChannel{},
		remote:   make(chan *webrtc.TrackRemote, 1),
		remoteV:  make(chan *webrtc.TrackRemote, 1),
		gathered: make(chan struct{}),
	}
	l.Local, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, l.TrackID, l.StreamID)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if cfg.VideoCodec != "" {
		vcap := videoCapability(cfg.VideoCodec)
		l.VideoTrackID = uuid.NewString()
		l.LocalVideo, err = webrtc.NewTrackLocalStaticRTP(vcap, l.VideoTrackID, l.StreamID)
		if err != nil {
			_ = pc.Close()
			return nil, err
		}
	}
	var gatherOnce sync.Once
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			l.lock.Lock()
			l.gatheringDone = true
			l.lock.Unlock()
			gatherOnce.Do(func() { close(l.gathered) })
			l.log.Debug().Msg("ICE gathering complete")
			return
		}
		// Only the candidate type is logged; addresses stay out of the logs.
		l.log.Debug().Stringer("cand_type", c.Typ).Stringer("protocol", c.Protocol).Msg("Local ICE candidate")
		init := c.ToJSON()
		l.lock.Lock()
		l.localCandidates = append(l.localCandidates, init.Candidate)
		cb := l.onCandidate
		l.lock.Unlock()
		if cb != nil {
			cb(&init)
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		l.log.Info().Stringer("state", s).Msg("PeerConnection state changed")
		l.lock.Lock()
		cb := l.onState
		l.lock.Unlock()
		if cb != nil {
			cb(s)
		}
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		l.log.Debug().Stringer("ice_state", s).Msg("ICE connection state changed")
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		l.log.Info().
			Stringer("kind", tr.Kind()).
			Str("codec", tr.Codec().MimeType).
			Uint8("pt", uint8(tr.PayloadType())).
			Msg("Remote track started")
		l.lock.Lock()
		each := l.onRemoteTrack
		l.lock.Unlock()
		if each != nil {
			// SFU: every participant's track, not just the first audio/video one.
			go each(tr)
			return
		}
		ch := l.remote
		if tr.Kind() == webrtc.RTPCodecTypeVideo {
			ch = l.remoteV
		}
		select {
		case ch <- tr:
		default:
		}
	})
	return l, nil
}

// OnRemoteTrack makes every remote track go to fn instead of RemoteTrack /
// RemoteVideoTrack, for SFU calls where each participant brings its own.
func (l *Leg) OnRemoteTrack(fn func(*webrtc.TrackRemote)) {
	l.lock.Lock()
	l.onRemoteTrack = fn
	l.lock.Unlock()
}

// OnData sets the callback for messages on the leg's data channels (Messenger's SFU sends some
// signalling over them). It must be set before the offer.
func (l *Leg) OnData(fn func(label string, data []byte)) {
	l.lock.Lock()
	l.onData = fn
	l.lock.Unlock()
	l.PC.OnDataChannel(l.watchDataChannel)
}

func (l *Leg) watchDataChannel(dc *webrtc.DataChannel) {
	label := dc.Label()
	l.lock.Lock()
	l.channels[label] = dc
	l.lock.Unlock()
	dc.OnOpen(func() { l.log.Debug().Str("channel", label).Msg("Data channel open") })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		l.lock.Lock()
		fn := l.onData
		l.lock.Unlock()
		if fn != nil {
			fn(label, msg.Data)
		}
	})
}

// SendData sends a message on one of the leg's data channels.
func (l *Leg) SendData(label string, data []byte) error {
	l.lock.Lock()
	dc := l.channels[label]
	l.lock.Unlock()
	if dc == nil {
		return fmt.Errorf("no %q data channel", label)
	}
	return dc.Send(data)
}

// OnCandidate sets the callback for local ICE candidates (trickle).
func (l *Leg) OnCandidate(fn func(*webrtc.ICECandidateInit)) {
	l.lock.Lock()
	l.onCandidate = fn
	l.lock.Unlock()
}

// OnState sets the callback for PeerConnection state changes.
func (l *Leg) OnState(fn func(webrtc.PeerConnectionState)) {
	l.lock.Lock()
	l.onState = fn
	l.lock.Unlock()
}

func (l *Leg) addLocalTrack() error {
	if l.sender != nil {
		return nil
	}
	sender, err := l.PC.AddTrack(l.Local)
	if err != nil {
		return err
	}
	l.sender = sender
	// Drain RTCP so the interceptors (reports, NACK) keep working, logging how the other side
	// receives our audio.
	go l.readAudioRTCP(sender)
	if l.LocalVideo != nil {
		vsender, err := l.PC.AddTrack(l.LocalVideo)
		if err != nil {
			return err
		}
		l.vsender = vsender
		go l.readVideoRTCP(vsender)
	}
	return nil
}

// AddVideoTrack adds a video track mid-call (an audio call upgraded to
// video); renegotiate afterwards. The codec must be registered (AllowVideo).
func (l *Leg) AddVideoTrack(mime string) error {
	l.lock.Lock()
	if l.LocalVideo != nil {
		l.lock.Unlock()
		return nil
	}
	l.lock.Unlock()
	l.VideoTrackID = uuid.NewString()
	track, err := webrtc.NewTrackLocalStaticRTP(videoCapability(mime), l.VideoTrackID, l.StreamID)
	if err != nil {
		return err
	}
	vsender, err := l.PC.AddTrack(track)
	if err != nil {
		return err
	}
	l.lock.Lock()
	l.LocalVideo, l.vsender = track, vsender
	l.lock.Unlock()
	go l.readVideoRTCP(vsender)
	return nil
}

// Renegotiate makes a new local offer on an established connection (e.g.
// after AddVideoTrack), with the connection's ICE candidates in it. The offer
// is applied when its answer arrives (SetAnswer); dropping it (RollbackOffer,
// or the peer's own offer winning in AnswerRenegotiation) needs no undo.
func (l *Leg) Renegotiate() (string, error) {
	cur := l.PC.CurrentLocalDescription()
	if cur == nil {
		return "", errors.New("renegotiating before the first negotiation finished")
	}
	offer, err := l.PC.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	l.lock.Lock()
	l.unappliedOffer = offer.SDP
	lines := make([]string, 0, len(l.localCandidates)+1)
	for _, c := range l.localCandidates {
		lines = append(lines, "a="+c)
	}
	if l.gatheringDone && len(lines) > 0 {
		lines = append(lines, "a=end-of-candidates")
	}
	l.lock.Unlock()
	if len(lines) == 0 {
		// Nothing recorded (candidates came in before the callback was set): take the description's.
		return WithCandidatesFrom(offer.SDP, cur.SDP), nil
	}
	return withCandidateLines(offer.SDP, lines), nil
}

// RollbackOffer drops a local offer the peer never answered (rejected or
// superseded), so the connection can take the next offer either way.
func (l *Leg) RollbackOffer() {
	l.lock.Lock()
	l.unappliedOffer = ""
	l.lock.Unlock()
}

// readVideoRTCP drains the video sender's RTCP and reports the receiver's
// keyframe requests (PLI/FIR), which the relay passes on to the sending peer.
// readAudioRTCP drains the audio sender's RTCP and logs the receiver reports about our audio (every
// 5 s at most): the loss and jitter the other side sees.
func (l *Leg) readAudioRTCP(sender *webrtc.RTPSender) {
	var last time.Time
	nacks := 0
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			var reports []rtcp.ReceptionReport
			switch p := pkt.(type) {
			case *rtcp.ReceiverReport:
				reports = p.Reports
			case *rtcp.SenderReport:
				reports = p.Reports
			case *rtcp.TransportLayerNack:
				nacks += len(p.Nacks)
			}
			for _, r := range reports {
				if time.Since(last) < 5*time.Second {
					continue
				}
				last = time.Now()
				l.log.Debug().Uint32("ssrc", r.SSRC).Float64("fraction_lost", float64(r.FractionLost)/256).
					Uint32("total_lost", r.TotalLost).Float64("jitter_ms", float64(r.Jitter)/48).
					Uint32("highest_seq", r.LastSequenceNumber).Int("nacked", nacks).
					Msg("Receiver report on our audio")
			}
		}
	}
}

func (l *Leg) readVideoRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				l.lock.Lock()
				cb := l.onKeyframeRequest
				l.lock.Unlock()
				if cb != nil {
					cb()
				}
			}
		}
	}
}

// OnKeyframeRequest sets the callback for keyframe requests from this leg's
// peer about the video we send it.
func (l *Leg) OnKeyframeRequest(fn func()) {
	l.lock.Lock()
	l.onKeyframeRequest = fn
	l.lock.Unlock()
}

// RequestKeyframe asks this leg's peer for a keyframe of its video.
func (l *Leg) RequestKeyframe(ssrc webrtc.SSRC) {
	_ = l.PC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
}

// RemoteVideoTrack waits for the peer's video track.
func (l *Leg) RemoteVideoTrack(ctx context.Context) (*webrtc.TrackRemote, error) {
	select {
	case tr := <-l.remoteV:
		return tr, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("callbridge: no remote video track: %w", ctx.Err())
	}
}

var videoFeedback = []webrtc.RTCPFeedback{
	{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"},
}

// videoCodecs are the web client's video codecs (H264 first, as it offers
// them), with its payload types.
var videoCodecs = []webrtc.RTPCodecParameters{
	{RTPCodecCapability: videoCapability(webrtc.MimeTypeH264), PayloadType: 108},
	{RTPCodecCapability: videoCapability(webrtc.MimeTypeVP8), PayloadType: 96},
}

func videoCapability(mime string) webrtc.RTPCodecCapability {
	c := webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000, RTCPFeedback: videoFeedback}
	if mime == webrtc.MimeTypeH264 {
		c.SDPFmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
	}
	return c
}

// AudioLevelURI is the RTP header extension carrying an audio packet's level (RFC 6464).
const AudioLevelURI = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"

// AudioHeaderExtensionID returns the id our audio packets carry the header extension uri under, or 0
// if it wasn't negotiated.
func (l *Leg) AudioHeaderExtensionID(uri string) uint8 {
	if l.sender == nil {
		return 0
	}
	return extensionID(l.sender.GetParameters().HeaderExtensions, uri)
}

// webHeaderExtensions are the RTP header extensions of the web client's SFU offer that Pion supports.
var webHeaderExtensions = []struct {
	uri  string
	kind webrtc.RTPCodecType
}{
	{"urn:ietf:params:rtp-hdrext:ssrc-audio-level", webrtc.RTPCodecTypeAudio},
	{"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time", webrtc.RTPCodecTypeAudio},
	{"urn:ietf:params:rtp-hdrext:sdes:mid", webrtc.RTPCodecTypeAudio},
	{"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time", webrtc.RTPCodecTypeVideo},
	{"urn:ietf:params:rtp-hdrext:sdes:mid", webrtc.RTPCodecTypeVideo},
	{"urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id", webrtc.RTPCodecTypeVideo},
	{"urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id", webrtc.RTPCodecTypeVideo},
}

// CreateOffer makes and applies a local offer: audio sendrecv, plus for
// WebShape a recvonly video m-line and a data channel, in that order.
func (l *Leg) CreateOffer() (string, error) {
	if err := l.addLocalTrack(); err != nil {
		return "", err
	}
	if l.cfg.WebShape {
		if l.LocalVideo == nil {
			if _, err := l.PC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionRecvonly,
			}); err != nil {
				return "", err
			}
		}
		dc, err := l.PC.CreateDataChannel("signaling_channel", nil)
		if err != nil {
			return "", err
		}
		l.watchDataChannel(dc)
	}
	offer, err := l.PC.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	if err = l.PC.SetLocalDescription(offer); err != nil {
		return "", err
	}
	return l.PC.LocalDescription().SDP, nil
}

// AnswerOffer applies a remote offer and makes the local answer. Pion answers
// every offered m-line: audio sendrecv with our track, a recvonly video
// offer inactive, and the data channel.
func (l *Leg) AnswerOffer(offer string) (string, error) {
	if err := l.setRemote(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", fmt.Errorf("set remote offer: %w", err)
	}
	if err := l.addLocalTrack(); err != nil {
		return "", err
	}
	answer, err := l.PC.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if err = l.PC.SetLocalDescription(answer); err != nil {
		return "", err
	}
	return l.PC.LocalDescription().SDP, nil
}

// SetAnswer applies the remote answer to our offer (applying a mid-call
// offer from Renegotiate first).
func (l *Leg) SetAnswer(answer string) error {
	l.lock.Lock()
	offer := l.unappliedOffer
	l.unappliedOffer = ""
	l.lock.Unlock()
	if offer != "" {
		if err := l.PC.SetLocalDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
			return fmt.Errorf("apply our offer: %w", err)
		}
	}
	return l.setRemote(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer})
}

func (l *Leg) setRemote(desc webrtc.SessionDescription) error {
	if err := l.PC.SetRemoteDescription(desc); err != nil {
		return err
	}
	l.lock.Lock()
	l.haveDesc = true
	pending := l.pending
	l.pending = nil
	l.lock.Unlock()
	for _, c := range pending {
		if err := l.PC.AddICECandidate(c); err != nil {
			l.log.Debug().Err(err).Msg("Failed to add queued remote ICE candidate")
		}
	}
	return nil
}

// AddCandidate adds a remote candidate, queueing it until the remote
// description is known. An empty candidate (end of candidates) is ignored.
func (l *Leg) AddCandidate(c webrtc.ICECandidateInit) error {
	if c.Candidate == "" {
		return nil
	}
	l.lock.Lock()
	if !l.haveDesc {
		l.pending = append(l.pending, c)
		l.lock.Unlock()
		return nil
	}
	l.lock.Unlock()
	return l.PC.AddICECandidate(c)
}

// WaitGathering waits until ICE gathering finishes or the timeout passes and
// returns the current local SDP (with the candidates gathered so far).
func (l *Leg) WaitGathering(ctx context.Context, timeout time.Duration) string {
	select {
	case <-l.gathered:
	case <-time.After(timeout):
	case <-ctx.Done():
	}
	if d := l.PC.LocalDescription(); d != nil {
		return d.SDP
	}
	return ""
}

// ErrNoRemoteTrack is returned when the peer never started sending audio.
var ErrNoRemoteTrack = errors.New("callbridge: no remote audio track")

// RemoteTrack waits for the remote audio track.
func (l *Leg) RemoteTrack(ctx context.Context) (*webrtc.TrackRemote, error) {
	select {
	case tr := <-l.remote:
		return tr, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrNoRemoteTrack, ctx.Err())
	}
}

// Close closes the PeerConnection. It is safe to call more than once.
func (l *Leg) Close() {
	if l == nil {
		return
	}
	l.lock.Lock()
	if l.closed {
		l.lock.Unlock()
		return
	}
	l.closed = true
	l.onCandidate = nil
	l.onState = nil
	l.lock.Unlock()
	if err := l.PC.Close(); err != nil {
		l.log.Debug().Err(err).Msg("Error closing PeerConnection")
	}
}

// IsPlanB reports whether the leg runs with Plan B semantics.
func (l *Leg) IsPlanB() bool { return l.cfg.PlanB }

// adaptRemote makes a remote description from a Plan B peer fit a Unified Plan leg (see
// AdaptPlanBOffer); the mapping undoes the renaming on our own reply.
func (l *Leg) adaptRemote(sdp string) (string, map[string]string) {
	if l.cfg.PlanB || !IsPlanB(sdp) {
		return sdp, nil
	}
	local := ""
	if d := l.PC.LocalDescription(); d != nil {
		local = d.SDP
	}
	return AdaptPlanBOffer(sdp, local)
}

// AnswerRenegotiation answers a mid-call offer, adapting a Plan B one from Messenger's mobile
// apps to a Unified Plan leg, and returns the answer in the peer's own terms.
func (l *Leg) AnswerRenegotiation(offer string) (string, error) {
	// Glare: the peer's offer wins over one of ours still waiting for an
	// answer (Messenger can reject ours or never answer it); ours was never
	// applied, so dropping it is enough.
	l.RollbackOffer()
	offer, mapping := l.adaptRemote(offer)
	answer, err := l.AnswerOffer(offer)
	if err != nil {
		return "", err
	}
	if mapping != nil {
		answer = RenameMIDs(answer, InvertMIDs(mapping))
	}
	return answer, nil
}

// SetRenegotiationAnswer applies the peer's answer to our mid-call offer, adapting a Plan B one.
func (l *Leg) SetRenegotiationAnswer(answer string) error {
	answer, _ = l.adaptRemote(answer)
	return l.SetAnswer(answer)
}

func semantics(planB bool) webrtc.SDPSemantics {
	if planB {
		return webrtc.SDPSemanticsPlanB
	}
	return webrtc.SDPSemanticsUnifiedPlan
}
