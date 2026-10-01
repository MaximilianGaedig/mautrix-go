// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"slices"
	"sync"
	"time"
)

// AlbumAddResult says what [AlbumCollector.Add] did with a part.
type AlbumAddResult int

const (
	// AlbumPartHeld means the collector took the part. It comes back in a batch, either from
	// this very call or from a later [AlbumCollector.Add] or [AlbumCollector.Due].
	AlbumPartHeld AlbumAddResult = iota
	// AlbumPartDuplicate means the album already saw a part with this index. The collector did
	// not take it, so the caller still has to do something with it.
	AlbumPartDuplicate
	// AlbumPartRejected means the marker doesn't describe a usable album (a count below two,
	// an index outside the count, a count that changed, or an album too large to hold).
	// The collector did not take the part.
	AlbumPartRejected
)

// AlbumCollector gathers the parts of albums that arrive one by one, and gives them back as
// batches in index order.
//
// A batch is released as soon as every part of the album has been seen. An album that stays
// incomplete is released with whatever arrived once it has waited IdleTimeout for its next part
// or MaxHold in total. Parts that arrive after such an early release are collected again under
// the same rules, so a slow album degrades into a few smaller batches and nothing is ever kept
// forever. A batch may hold a single part.
//
// The collector never starts timers itself: the owner asks [AlbumCollector.NextDeadline] when
// to call [AlbumCollector.Due]. That, and the replaceable clock, keep it testable without waiting.
//
// K identifies an album. Everything that must never be mixed (the room, the sender) belongs in
// the key or in separate collectors.
type AlbumCollector[K comparable, T any] struct {
	// IdleTimeout is how long an incomplete album waits for its next part.
	IdleTimeout time.Duration
	// MaxHold is the longest a part is held, however steadily other parts keep arriving.
	MaxHold time.Duration
	// MaxAlbums is how many albums are tracked at once. When a new album would exceed it,
	// the oldest one is forgotten, and released first if it still holds parts.
	MaxAlbums int
	// MaxParts is the largest album the collector accepts.
	MaxParts int
	// ForgetAfter is how long a released album is remembered. While it is, late parts know
	// how many are still missing and duplicates are recognised.
	ForgetAfter time.Duration
	// Now is the clock. It is only replaced in tests.
	Now func() time.Time

	lock   sync.Mutex
	albums map[K]*collectingAlbum[T]
}

type collectingPart[T any] struct {
	index int
	item  T
}

type collectingAlbum[T any] struct {
	count int
	seen  map[int]struct{}
	held  []collectingPart[T]
	// heldSince is when the oldest currently held part arrived.
	heldSince time.Time
	// lastPart is when the newest part arrived, held or already released.
	lastPart time.Time
}

// NewAlbumCollector creates a collector with the given limits and the real clock.
func NewAlbumCollector[K comparable, T any](idleTimeout, maxHold time.Duration, maxAlbums, maxParts int) *AlbumCollector[K, T] {
	return &AlbumCollector[K, T]{
		IdleTimeout: idleTimeout,
		MaxHold:     maxHold,
		MaxAlbums:   maxAlbums,
		MaxParts:    maxParts,
		ForgetAfter: 10 * time.Minute,
		Now:         time.Now,
	}
}

func (a *collectingAlbum[T]) release() []T {
	slices.SortFunc(a.held, func(x, y collectingPart[T]) int {
		return x.index - y.index
	})
	batch := make([]T, len(a.held))
	for i, part := range a.held {
		batch[i] = part.item
	}
	a.held = nil
	return batch
}

func (ac *AlbumCollector[K, T]) deadline(a *collectingAlbum[T]) time.Time {
	idle := a.lastPart.Add(ac.IdleTimeout)
	hold := a.heldSince.Add(ac.MaxHold)
	if hold.Before(idle) {
		return hold
	}
	return idle
}

// Add offers one part of an album to the collector.
//
// The released batches are the ones that this part made ready: the part's own album if it is
// now complete, and before it an older album that had to make room. Usually there are none.
func (ac *AlbumCollector[K, T]) Add(key K, index, count int, item T) (result AlbumAddResult, released [][]T) {
	ac.lock.Lock()
	defer ac.lock.Unlock()
	if count < 2 || index < 0 || index >= count || count > ac.MaxParts {
		return AlbumPartRejected, nil
	}
	now := ac.Now()
	album, known := ac.albums[key]
	if known && album.count != count {
		return AlbumPartRejected, nil
	} else if known {
		if _, dup := album.seen[index]; dup {
			return AlbumPartDuplicate, nil
		}
	} else {
		ac.forgetReleased(now)
		if evicted := ac.makeRoom(); evicted != nil {
			released = append(released, evicted)
		}
		album = &collectingAlbum[T]{count: count, seen: make(map[int]struct{}, count)}
		if ac.albums == nil {
			ac.albums = make(map[K]*collectingAlbum[T])
		}
		ac.albums[key] = album
	}
	if len(album.held) == 0 {
		album.heldSince = now
	}
	album.seen[index] = struct{}{}
	album.held = append(album.held, collectingPart[T]{index: index, item: item})
	album.lastPart = now
	if len(album.seen) == album.count {
		released = append(released, album.release())
	}
	return AlbumPartHeld, released
}

// forgetReleased drops the memory of albums that hold nothing and have been quiet for long enough.
func (ac *AlbumCollector[K, T]) forgetReleased(now time.Time) {
	for key, album := range ac.albums {
		if len(album.held) == 0 && !now.Before(album.lastPart.Add(ac.ForgetAfter)) {
			delete(ac.albums, key)
		}
	}
}

// makeRoom forgets one album if the collector is full. An album that holds nothing goes first,
// as forgetting it only costs the memory of what was already released. Otherwise the album that
// has been holding parts the longest is released early.
func (ac *AlbumCollector[K, T]) makeRoom() []T {
	if len(ac.albums) < ac.MaxAlbums {
		return nil
	}
	var oldestKey K
	var oldest *collectingAlbum[T]
	for key, album := range ac.albums {
		switch {
		case oldest == nil:
		case len(album.held) == 0 && len(oldest.held) > 0:
		case len(album.held) > 0 && len(oldest.held) == 0:
			continue
		case len(album.held) == 0 && album.lastPart.Before(oldest.lastPart):
		case len(album.held) > 0 && album.heldSince.Before(oldest.heldSince):
		default:
			continue
		}
		oldestKey, oldest = key, album
	}
	if oldest == nil {
		return nil
	}
	delete(ac.albums, oldestKey)
	if len(oldest.held) == 0 {
		return nil
	}
	return oldest.release()
}

// Due releases every album that has waited long enough, the one waiting longest first.
func (ac *AlbumCollector[K, T]) Due() (released [][]T) {
	ac.lock.Lock()
	defer ac.lock.Unlock()
	now := ac.Now()
	var due []*collectingAlbum[T]
	for _, album := range ac.albums {
		if len(album.held) > 0 && !now.Before(ac.deadline(album)) {
			due = append(due, album)
		}
	}
	slices.SortFunc(due, func(x, y *collectingAlbum[T]) int {
		return x.heldSince.Compare(y.heldSince)
	})
	for _, album := range due {
		released = append(released, album.release())
	}
	ac.forgetReleased(now)
	return released
}

// NextDeadline is when [AlbumCollector.Due] will next have something to release.
// It reports false while no parts are held.
func (ac *AlbumCollector[K, T]) NextDeadline() (next time.Time, ok bool) {
	ac.lock.Lock()
	defer ac.lock.Unlock()
	for _, album := range ac.albums {
		if len(album.held) == 0 {
			continue
		}
		if deadline := ac.deadline(album); !ok || deadline.Before(next) {
			next, ok = deadline, true
		}
	}
	return next, ok
}
