// Copyright (c) 2026 Maximilian Gaedig
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type albumTestClock struct {
	now time.Time
}

func (c *albumTestClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func newTestAlbumCollector() (*AlbumCollector[string, string], *albumTestClock) {
	clock := &albumTestClock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	collector := NewAlbumCollector[string, string](10*time.Second, time.Minute, 3, 20)
	collector.Now = func() time.Time { return clock.now }
	return collector, clock
}

func TestAlbumCollector_CompleteBatch(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	for i, item := range []string{"a", "b"} {
		result, released := collector.Add("album", i, 3, item)
		assert.Equal(t, AlbumPartHeld, result)
		assert.Empty(t, released, "an incomplete album is not released")
		clock.advance(time.Second)
	}
	result, released := collector.Add("album", 2, 3, "c")
	assert.Equal(t, AlbumPartHeld, result)
	assert.Equal(t, [][]string{{"a", "b", "c"}}, released)
	_, pending := collector.NextDeadline()
	assert.False(t, pending, "nothing is held after the album was released")
	clock.advance(time.Hour)
	assert.Empty(t, collector.Due())
}

func TestAlbumCollector_OutOfOrder(t *testing.T) {
	collector, _ := newTestAlbumCollector()
	collector.Add("album", 2, 4, "c")
	collector.Add("album", 0, 4, "a")
	collector.Add("album", 3, 4, "d")
	_, released := collector.Add("album", 1, 4, "b")
	assert.Equal(t, [][]string{{"a", "b", "c", "d"}}, released, "a batch is in index order, not arrival order")
}

func TestAlbumCollector_TimeoutWithGap(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	start := clock.now
	collector.Add("album", 0, 4, "a")
	clock.advance(4 * time.Second)
	collector.Add("album", 3, 4, "d")

	deadline, pending := collector.NextDeadline()
	require.True(t, pending)
	assert.Equal(t, start.Add(14*time.Second), deadline, "the wait restarts with every part")
	clock.advance(9 * time.Second)
	assert.Empty(t, collector.Due(), "not due before the deadline")
	clock.advance(time.Second)
	assert.Equal(t, [][]string{{"a", "d"}}, collector.Due(), "what arrived is released, the gap is skipped")
	assert.Empty(t, collector.Due(), "released only once")
	_, pending = collector.NextDeadline()
	assert.False(t, pending)
}

func TestAlbumCollector_LatePartsAfterTimeout(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	collector.Add("album", 0, 4, "a")
	collector.Add("album", 1, 4, "b")
	clock.advance(10 * time.Second)
	require.Equal(t, [][]string{{"a", "b"}}, collector.Due())

	// The rest of the album turns up after all. It is collected like a new, smaller album,
	// and doesn't wait for the parts that are already gone.
	result, released := collector.Add("album", 2, 4, "c")
	assert.Equal(t, AlbumPartHeld, result)
	assert.Empty(t, released)
	_, released = collector.Add("album", 3, 4, "d")
	assert.Equal(t, [][]string{{"c", "d"}}, released)
}

func TestAlbumCollector_SingleLatePartIsABatchOfOne(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	collector.Add("album", 0, 2, "a")
	clock.advance(10 * time.Second)
	require.Equal(t, [][]string{{"a"}}, collector.Due(), "a batch can hold a single part")
	_, released := collector.Add("album", 1, 2, "b")
	assert.Equal(t, [][]string{{"b"}}, released, "the last part doesn't wait for anything")
}

func TestAlbumCollector_MaxHold(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	start := clock.now
	// The parts keep trickling in just fast enough to never hit the idle timeout.
	for i := range 6 {
		collector.Add("album", i, 20, "x")
		clock.advance(9 * time.Second)
	}
	collector.Add("album", 6, 20, "x")
	deadline, pending := collector.NextDeadline()
	require.True(t, pending)
	assert.Equal(t, start.Add(time.Minute), deadline, "the first part is not held longer than MaxHold")
	clock.advance(6 * time.Second)
	released := collector.Due()
	require.Len(t, released, 1)
	assert.Len(t, released[0], 7)
}

func TestAlbumCollector_Duplicates(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	collector.Add("album", 0, 2, "a")
	result, released := collector.Add("album", 0, 2, "a again")
	assert.Equal(t, AlbumPartDuplicate, result)
	assert.Empty(t, released)
	_, released = collector.Add("album", 1, 2, "b")
	assert.Equal(t, [][]string{{"a", "b"}}, released, "the duplicate is not in the batch")

	result, released = collector.Add("album", 1, 2, "b again")
	assert.Equal(t, AlbumPartDuplicate, result, "a released album still recognises its parts")
	assert.Empty(t, released)

	clock.advance(11 * time.Minute)
	collector.Due()
	result, _ = collector.Add("album", 1, 2, "b much later")
	assert.Equal(t, AlbumPartHeld, result, "a released album is not remembered forever")
}

func TestAlbumCollector_UnusableMarkers(t *testing.T) {
	collector, _ := newTestAlbumCollector()
	for name, marker := range map[string][2]int{
		"an album of one":         {0, 1},
		"no count":                {0, 0},
		"an index past the count": {2, 2},
		"a negative index":        {-1, 2},
		"more parts than allowed": {0, 21},
	} {
		result, released := collector.Add("album", marker[0], marker[1], "x")
		assert.Equal(t, AlbumPartRejected, result, name)
		assert.Empty(t, released, name)
	}
	collector.Add("album", 0, 3, "a")
	result, _ := collector.Add("album", 1, 4, "b")
	assert.Equal(t, AlbumPartRejected, result, "the count of an album can't change")
	_, pending := collector.NextDeadline()
	assert.True(t, pending, "a rejected part leaves the album alone")
}

func TestAlbumCollector_InterleavedAlbums(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	collector.Add("one", 0, 2, "1a")
	clock.advance(time.Second)
	collector.Add("two", 0, 3, "2a")
	clock.advance(time.Second)
	collector.Add("two", 1, 3, "2b")
	_, released := collector.Add("one", 1, 2, "1b")
	assert.Equal(t, [][]string{{"1a", "1b"}}, released, "albums never mix")
	_, released = collector.Add("two", 2, 3, "2c")
	assert.Equal(t, [][]string{{"2a", "2b", "2c"}}, released)
}

func TestAlbumCollector_DueReleasesOldestFirst(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	collector.Add("one", 0, 2, "1a")
	clock.advance(time.Second)
	collector.Add("two", 0, 2, "2a")
	clock.advance(9 * time.Second)
	assert.Equal(t, [][]string{{"1a"}}, collector.Due(), "only the album whose time is up")
	clock.advance(time.Minute)
	collector.Add("three", 0, 2, "3a")
	clock.advance(time.Minute)
	assert.Equal(t, [][]string{{"2a"}, {"3a"}}, collector.Due())
}

func TestAlbumCollector_Cap(t *testing.T) {
	collector, clock := newTestAlbumCollector()
	for _, key := range []string{"one", "two", "three"} {
		_, released := collector.Add(key, 0, 2, key)
		assert.Empty(t, released)
		clock.advance(time.Second)
	}
	// The collector tracks three albums. A fourth pushes out the one held the longest.
	result, released := collector.Add("four", 0, 2, "four")
	assert.Equal(t, AlbumPartHeld, result)
	assert.Equal(t, [][]string{{"one"}}, released)

	// A finished album is only a memory, and is given up before any album that holds parts.
	_, released = collector.Add("three", 1, 2, "three again")
	require.Equal(t, [][]string{{"three", "three again"}}, released)
	_, released = collector.Add("five", 0, 2, "five")
	assert.Empty(t, released, "forgetting the finished album made room")
	_, released = collector.Add("six", 0, 2, "six")
	assert.Equal(t, [][]string{{"two"}}, released)
}
