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
	"io"
	"sync/atomic"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"
)

// RTPReader is the reading half of a *webrtc.TrackRemote.
type RTPReader interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
}

// RTPWriter is the writing half of a *webrtc.TrackLocalStaticRTP, which
// stamps each packet with the SSRC and payload type negotiated on its own
// PeerConnection.
type RTPWriter interface {
	WriteRTP(*rtp.Packet) error
}

// Rewriter keeps an outgoing stream continuous while the incoming one may
// change: it strips header extensions (their ids are negotiated per leg),
// and re-bases sequence numbers and timestamps when the source SSRC
// changes (e.g. after a peer renegotiates), so the receiver sees one
// monotonic stream. SSRC and payload type are replaced by the writer.
type Rewriter struct {
	// AudioLevel, if To is set, stamps every packet with an RFC 6464 audio level under that id: the
	// level from the source's From id when it has one, otherwise one derived from the payload (an
	// Opus comfort-noise frame is a few bytes, speech is not). Messenger's SFU forwards only the
	// streams it takes for active speakers, and an unlabelled stream is forwarded at a trickle.
	AudioLevel struct{ From, To uint8 }
	// FrameTicks spaces the first packet of a new source after the last one
	// of the old source; 0 means one 20 ms Opus frame.
	FrameTicks uint32

	started    bool
	inSSRC     uint32
	seqOffset  uint16
	tsOffset   uint32
	lastOutSeq uint16
	lastOutTS  uint32
}

// opusFrameTicks is one 20 ms Opus frame at 48 kHz, used to space the first
// packet of a new source after the last one of the old source.
const opusFrameTicks = 960

// Rewrite adjusts p in place.
func (r *Rewriter) Rewrite(p *rtp.Packet) {
	var level []byte
	if r.AudioLevel.To != 0 {
		if r.AudioLevel.From != 0 {
			level = append(level, p.Header.GetExtension(r.AudioLevel.From)...)
		}
		if len(level) == 0 {
			level = []byte{audioLevelOf(p.Payload)}
		}
	}
	defer func() {
		if len(level) > 0 {
			_ = p.Header.SetExtension(r.AudioLevel.To, level)
		}
	}()
	p.Header.Extension = false
	p.Header.Extensions = nil
	p.Header.ExtensionProfile = 0
	p.Header.Padding = false
	p.PaddingSize = 0
	if !r.started {
		r.started = true
		r.inSSRC = p.SSRC
	} else if p.SSRC != r.inSSRC {
		r.inSSRC = p.SSRC
		r.seqOffset = r.lastOutSeq + 1 - p.SequenceNumber
		ticks := r.FrameTicks
		if ticks == 0 {
			ticks = opusFrameTicks
		}
		r.tsOffset = r.lastOutTS + ticks - p.Timestamp
	}
	p.SequenceNumber += r.seqOffset
	p.Timestamp += r.tsOffset
	r.lastOutSeq = p.SequenceNumber
	r.lastOutTS = p.Timestamp
}

// extIDs lists the header extension ids a packet carries, for logging.
func extIDs(p *rtp.Packet) []int {
	var out []int
	for _, id := range p.Header.GetExtensionIDs() {
		out = append(out, int(id))
	}
	return out
}

// audioLevelOf is the RFC 6464 byte (V bit plus the level in -dBov) for an Opus payload: a frame of
// a few bytes is comfort noise or silence, anything larger is speech at a middling level.
func audioLevelOf(payload []byte) byte {
	if len(payload) <= 12 {
		return 127
	}
	return 0x80 | 30
}

// RelayStats counts relayed and dropped packets of one direction.
type RelayStats struct {
	Forwarded atomic.Uint64
	Dropped   atomic.Uint64
}

// Relay forwards Opus RTP from src to dst until ctx ends or src fails.
// Packets whose payload type is not srcOpusPT (RED, CN, DTMF) are dropped,
// since only Opus is negotiated on the other leg.
func Relay(ctx context.Context, src RTPReader, srcOpusPT uint8, dst RTPWriter, stats *RelayStats, log zerolog.Logger) error {
	return relay(ctx, src, srcOpusPT, dst, stats, log, &Rewriter{}, "audio")
}

// RelayWith relays Opus like Relay through rw.
func RelayWith(ctx context.Context, src RTPReader, srcOpusPT uint8, dst RTPWriter, stats *RelayStats, log zerolog.Logger, rw *Rewriter) error {
	return relay(ctx, src, srcOpusPT, dst, stats, log, rw, "audio")
}

// VideoFrameTicks is one frame at 30 fps on the 90 kHz video clock.
const VideoFrameTicks = 3000

// RelayVideo forwards video RTP of payload type srcPT (the negotiated codec;
// retransmissions under their own payload type are dropped, the receiving
// leg's NACK responder resends from its own buffer) from src to dst.
func RelayVideo(ctx context.Context, src RTPReader, srcPT uint8, dst RTPWriter, stats *RelayStats, log zerolog.Logger) error {
	return relay(ctx, src, srcPT, dst, stats, log, &Rewriter{FrameTicks: VideoFrameTicks}, "video")
}

// RelayVideoWith relays like RelayVideo through rw, which keeps the outgoing stream continuous when
// the next source track (a camera turned off and on again) is relayed with the same rewriter.
func RelayVideoWith(ctx context.Context, src RTPReader, srcPT uint8, dst RTPWriter, stats *RelayStats, log zerolog.Logger, rw *Rewriter) error {
	return relay(ctx, src, srcPT, dst, stats, log, rw, "video")
}

func relay(ctx context.Context, src RTPReader, srcOpusPT uint8, dst RTPWriter, stats *RelayStats, log zerolog.Logger, rw *Rewriter, kind string) error {
	loggedFirst := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p, _, err := src.ReadRTP()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if p.PayloadType != srcOpusPT {
			stats.Dropped.Add(1)
			continue
		}
		if !loggedFirst {
			loggedFirst = true
			log.Info().Ints("source_extensions", extIDs(p)).Msg("First " + kind + " packet relayed")
		}
		rw.Rewrite(p)
		if err = dst.WriteRTP(p); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		stats.Forwarded.Add(1)
	}
}
