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
	"sync"

	"github.com/pion/webrtc/v4"
)

// errLegClosed is what waiting for a remote track returns once the leg is closed.
var errLegClosed = errors.New("leg closed")

// trackQueue holds a leg's remote tracks of one kind until someone takes them, in the order they
// arrived. It has no bound: a channel of one dropped the second track whenever the first was still
// waiting or its taker was busy relaying it, and a screen shared beside the camera is exactly that,
// a second video track. The peer's SDP bounds how many there can be.
type trackQueue struct {
	lock   sync.Mutex
	tracks []*webrtc.TrackRemote
	// wake has room for one token, which is there whenever tracks is not empty (or a taker is on
	// its way to find that out), so put never blocks and needs no goroutine of its own.
	wake   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newTrackQueue() *trackQueue {
	return &trackQueue{wake: make(chan struct{}, 1), closed: make(chan struct{})}
}

// put adds a track and returns how many are now waiting to be taken.
func (q *trackQueue) put(tr *webrtc.TrackRemote) int {
	q.lock.Lock()
	q.tracks = append(q.tracks, tr)
	n := len(q.tracks)
	q.lock.Unlock()
	q.signal()
	return n
}

func (q *trackQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns the oldest track not yet taken, waiting for one if there is none.
func (q *trackQueue) take(ctx context.Context) (*webrtc.TrackRemote, error) {
	for {
		select {
		case <-q.closed:
			return nil, errLegClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-q.wake:
		}
		q.lock.Lock()
		if len(q.tracks) == 0 {
			q.lock.Unlock()
			continue
		}
		tr := q.tracks[0]
		q.tracks[0] = nil
		q.tracks = q.tracks[1:]
		more := len(q.tracks) > 0
		q.lock.Unlock()
		if more {
			// The token taken stood for all of them; put it back for the next call (or the next
			// taker, if several wait).
			q.signal()
		}
		return tr, nil
	}
}

// close makes every take, waiting or later, return: the tracks of a closed connection carry
// nothing, and a taker with a context that outlives the leg would otherwise wait for good.
func (q *trackQueue) close() {
	q.once.Do(func() { close(q.closed) })
}
