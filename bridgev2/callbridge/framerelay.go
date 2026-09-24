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
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	"github.com/rs/zerolog"
)

// FrameTransform encrypts or decrypts one encoded media frame (Messenger's end-to-end encrypted group
// calls wrap every frame in SFrame, like the web client's insertable streams do). An error drops the frame.
type FrameTransform func(frame []byte) ([]byte, error)

// Frame transforms work on whole encoded frames, not RTP packets: the browser (encoded transform)
// encrypts a frame and then packetizes the ciphertext with the codec's packetizer. So video is
// depacketized into frames, transformed, and packetized again; Opus is one frame per packet.

// videoMTU is the RTP packet size frames are packetized into (Pion's and browsers' default).
const videoMTU = 1200

// videoMaxLate is how many packets the frame assembler holds while waiting for a late one (a
// keyframe can be 100+ packets); videoMaxDelay gives up on an incomplete frame once newer media is
// that far ahead in RTP time, so a loss is reported (and a keyframe requested) within ~6 frames.
const (
	videoMaxLate  = 256
	videoMaxDelay = 200 * time.Millisecond
)

func newFrameAssembler(depack rtp.Depacketizer) *samplebuilder.SampleBuilder {
	return samplebuilder.New(videoMaxLate, depack, 90000, samplebuilder.WithMaxTimeDelay(videoMaxDelay))
}

func codecParts(mime string) (rtp.Depacketizer, rtp.Payloader, error) {
	switch strings.ToLower(mime) {
	case strings.ToLower(webrtc.MimeTypeVP8):
		return &codecs.VP8Packet{}, &codecs.VP8Payloader{EnablePictureID: true}, nil
	case strings.ToLower(webrtc.MimeTypeH264):
		return &codecs.H264Packet{}, &codecs.H264Payloader{}, nil
	}
	return nil, nil, fmt.Errorf("no frame packetizer for %s", mime)
}

// FrameRelayStats counts the frames of one transformed direction.
type FrameRelayStats struct {
	RelayStats
	Frames      atomic.Uint64
	Failed      atomic.Uint64 // transform errors (not decryptable yet, tampered, …)
	LostPackets atomic.Uint64
}

// RelayAudioTransformed relays Opus like Relay, running every payload (one Opus frame) through xf.
func RelayAudioTransformed(ctx context.Context, src RTPReader, srcOpusPT uint8, dst RTPWriter, xf FrameTransform, stats *FrameRelayStats, log zerolog.Logger) error {
	return RelayAudioTransformedWith(ctx, src, srcOpusPT, dst, xf, stats, log, &Rewriter{})
}

// RelayAudioTransformedWith relays like RelayAudioTransformed through rw.
func RelayAudioTransformedWith(ctx context.Context, src RTPReader, srcOpusPT uint8, dst RTPWriter, xf FrameTransform, stats *FrameRelayStats, log zerolog.Logger, rw *Rewriter) error {
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
		if p.PayloadType != srcOpusPT || len(p.Payload) == 0 {
			stats.Dropped.Add(1)
			continue
		}
		out, err := xf(p.Payload)
		stats.Frames.Add(1)
		if err != nil {
			stats.Failed.Add(1)
			stats.Dropped.Add(1)
			continue
		}
		if !loggedFirst {
			loggedFirst = true
			log.Info().Ints("source_extensions", extIDs(p)).Msg("First transformed audio frame relayed")
		}
		p.Payload = out
		rw.Rewrite(p)
		if err = dst.WriteRTP(p); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		stats.Forwarded.Add(1)
	}
}

// RelayVideoTransformed relays video of codec mime (payload type srcPT) from src to dst: packets are
// assembled into frames, each frame goes through xf, and the result is packetized again with the
// codec's payloader under the frame's own timestamp. onLoss (optional) is called when packets were
// lost, so the sender can be asked for a keyframe.
func RelayVideoTransformed(ctx context.Context, src RTPReader, srcPT uint8, mime string, dst RTPWriter, xf FrameTransform, onLoss func(), stats *FrameRelayStats, log zerolog.Logger) error {
	depack, payloader, err := codecParts(mime)
	if err != nil {
		return err
	}
	sb := newFrameAssembler(depack)
	var seq uint16
	loggedFirst := false
	var inSSRC uint32
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
		if p.PayloadType != srcPT {
			stats.Dropped.Add(1)
			continue
		}
		if inSSRC != p.SSRC {
			// A new source (camera restarted): don't let the old stream's frames mix with the new.
			if inSSRC != 0 {
				sb = newFrameAssembler(depack)
			}
			inSSRC = p.SSRC
		}
		sb.Push(p)
		for s := sb.Pop(); s != nil; s = sb.Pop() {
			if s.PrevDroppedPackets > 0 {
				stats.LostPackets.Add(uint64(s.PrevDroppedPackets))
				if onLoss != nil {
					onLoss()
				}
			}
			frame, err := xf(s.Data)
			stats.Frames.Add(1)
			if err != nil {
				stats.Failed.Add(1)
				continue
			}
			payloads := payloader.Payload(videoMTU-12, frame)
			for i, pl := range payloads {
				out := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						Marker:         i == len(payloads)-1,
						PayloadType:    srcPT,
						SequenceNumber: seq,
						Timestamp:      s.PacketTimestamp,
					},
					Payload: pl,
				}
				seq++
				if err = dst.WriteRTP(out); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					return err
				}
				stats.Forwarded.Add(1)
			}
			if !loggedFirst {
				loggedFirst = true
				log.Info().Str("codec", mime).Msg("First transformed video frame relayed")
			}
		}
	}
}
