// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sfnt

import (
	"sync"
	"sync/atomic"

	"golang.org/x/image/math/fixed"
)

// segmentSize is the size in bytes of one Segment value, as cached by a
// GlyphCache. A Segment is a uint32 Op plus three fixed.Point26_6 values
// (each two int32s), i.e. 28 bytes on all supported platforms.
const segmentSize = 28

// Default limits for a GlyphCache created with NewGlyphCache. They are
// deliberately small enough for a typical screen of text, while bounding the
// cache's memory footprint on malicious or unusually large fonts.
const (
	// defaultGlyphCacheMaxEntries bounds the number of cached glyphs.
	defaultGlyphCacheMaxEntries = 256

	// defaultGlyphCacheMaxBytes bounds the total size of the cached glyph
	// vectors. 16 MiB is roughly 600 thousand Segment values.
	defaultGlyphCacheMaxBytes = 16 << 20

	// defaultGlyphCacheMaxEntryBytes bounds the size of a single cached glyph
	// vector. Glyphs larger than this are decoded on every request and are not
	// stored in the cache, so one oversized outline cannot evict everything
	// else.
	defaultGlyphCacheMaxEntryBytes = 1 << 20
)

// GlyphCacheOptions configures a GlyphCache.
//
// A zero value uses the package defaults. For each field, a negative value
// means that the corresponding limit is disabled (unlimited). A value of zero
// means that the corresponding default limit is used.
type GlyphCacheOptions struct {
	// MaxEntries is the maximum number of glyph vectors held by the cache.
	// Least recently used entries are evicted first.
	MaxEntries int

	// MaxBytes is the maximum total size, in bytes, of the glyph vectors held
	// by the cache. Least recently used entries are evicted first.
	MaxBytes int

	// MaxEntryBytes is the maximum size, in bytes, of a single cached glyph
	// vector. Glyph vectors larger than this are never cached.
	MaxEntryBytes int
}

// GlyphCacheStats reports GlyphCache statistics.
//
// The counters are monotonically increasing, except that Clear resets them
// all to zero.
type GlyphCacheStats struct {
	// Hits is the number of cache lookups that found an entry.
	Hits uint64

	// Misses is the number of successful glyph decodes for a key that was
	// absent. This includes glyphs that were decoded successfully but not
	// stored because they exceeded MaxEntryBytes or MaxBytes (see Rejected).
	// A failed decode is neither a hit nor a miss. Under concurrent load, a
	// key decoded simultaneously by several goroutines counts as at most one
	// miss.
	Misses uint64

	// Evictions is the number of entries removed to enforce the MaxEntries or
	// MaxBytes limits.
	Evictions uint64

	// Rejected is the number of decoded glyph vectors that were not stored
	// because they exceeded MaxEntryBytes.
	Rejected uint64

	// Entries is the current number of cached glyph vectors.
	Entries int

	// Bytes is the current total size, in bytes, of the cached glyph vectors.
	Bytes int
}

// GlyphCache is a size-bounded, concurrency-safe cache of decoded glyph
// vector outlines.
//
// A cache is associated with zero or more Font values via Font.SetGlyphCache.
// Entries are keyed by the font, the glyph index and the ppem (the glyph size
// in 26.6 fixed point pixels per em), so different fonts, different glyphs
// and different sizes never share an entry.
//
// The Segments returned from a cache hit are either a read-only view of
// cached data or, when a non-nil *Buffer is supplied, a private copy owned by
// that *Buffer. Callers must never modify a Segments slice returned with a
// nil *Buffer; the same backing array may be returned to other goroutines.
//
// Only successfully decoded outlines are cached: parse errors, unsupported
// compound glyphs and ErrColoredGlyph are re-evaluated on every request, so
// their behavior is unchanged from Font.LoadGlyph without a cache.
//
// A GlyphCache must not be copied after first use.
type GlyphCache struct {
	mu sync.Mutex

	maxEntries    int
	maxBytes      int
	maxEntryBytes int

	entries map[glyphCacheKey]*cachedGlyph

	// head and tail are the sentinel nodes of a least-recently-used doubly
	// linked list. head.next is the most recently used entry and tail.prev is
	// the least recently used one.
	head, tail cachedGlyph

	hits       uint64
	misses     uint64
	evictions  uint64
	rejections uint64
	bytes      int
}

// NewGlyphCache returns a new GlyphCache configured with opts.
//
// A zero GlyphCacheOptions uses the package default limits. See the
// GlyphCacheOptions documentation for the meaning of zero and negative field
// values.
func NewGlyphCache(opts GlyphCacheOptions) *GlyphCache {
	c := &GlyphCache{
		maxEntries:    opts.MaxEntries,
		maxBytes:      opts.MaxBytes,
		maxEntryBytes: opts.MaxEntryBytes,
		entries:       map[glyphCacheKey]*cachedGlyph{},
	}
	if c.maxEntries == 0 {
		c.maxEntries = defaultGlyphCacheMaxEntries
	}
	if c.maxBytes == 0 {
		c.maxBytes = defaultGlyphCacheMaxBytes
	}
	if c.maxEntryBytes == 0 {
		c.maxEntryBytes = defaultGlyphCacheMaxEntryBytes
	}
	c.head.next = &c.tail
	c.tail.prev = &c.head
	return c
}

// glyphCacheKey identifies a cached outline. Every field participates in the
// key, which keeps the entries of distinct fonts, font collections, glyph
// indices and glyph sizes separate.
type glyphCacheKey struct {
	fontID uint64
	ppem   fixed.Int26_6
	glyph  GlyphIndex
}

// cachedGlyph is one cache entry: an owned, never-mutated copy of a glyph's
// Segments plus its LRU list links.
type cachedGlyph struct {
	key      glyphCacheKey
	segments Segments
	// size is len(segments) * segmentSize.
	size int

	prev, next *cachedGlyph
}

// lookup returns a cached entry's segments and records a hit. The returned
// Segments are owned by the cache and must not be modified; callers copy them
// before handing them to user code. A missing entry is not counted here: a
// miss is only recorded by put once the corresponding decode has succeeded,
// so a failed decode never skews the hit/miss statistics.
func (c *GlyphCache) lookup(key glyphCacheKey) (Segments, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return nil, false
	}
	c.hits++
	c.moveToFront(e)
	return e.segments, true
}

// put stores a copy of segments under key, evicting least recently used
// entries as needed to respect the configured limits. An entry that alone
// exceeds MaxEntryBytes, or an effectively full cache that cannot hold any
// entry, is rejected rather than stored. Empty outlines (such as space
// glyphs) are cached like any other.
//
// put is only called after a successful decode of an absent key, so every
// call (including a rejected one) is recorded as a miss, unless another
// goroutine inserted the same key during the decode.
func (c *GlyphCache) put(key glyphCacheKey, segments Segments) {
	size := len(segments) * segmentSize
	tooLargeEntry := c.maxEntryBytes >= 0 && size > c.maxEntryBytes

	// Normalize empty outlines to a nil slice so that empty glyphs (which
	// LoadGlyph can represent as either nil or a non-nil, zero-length slice
	// depending on Buffer re-use) compare equal across cache hits.
	var owned Segments
	if len(segments) != 0 {
		owned = append(Segments(nil), segments...)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// A concurrent caller may have inserted the same key while we were
	// decoding. Replace it rather than duplicating it. Only the first
	// insertion for a key counts as a miss: a losing goroutine's lookup raced
	// with another one for the same key, and its decoded copy is discarded.
	if old := c.entries[key]; old != nil {
		c.removeLocked(old)
		delete(c.entries, key)
	} else {
		c.misses++
	}

	if tooLargeEntry {
		c.rejections++
		return
	}

	e := &cachedGlyph{
		key:      key,
		segments: owned,
		size:     size,
	}

	// Evict least recently used entries until the new entry fits. Entries are
	// evicted before insertion so that an insertion that exactly fills the
	// cache does not immediately evict itself.
	for {
		if c.maxEntries < 0 || len(c.entries) < c.maxEntries {
			if c.maxBytes < 0 || c.bytes+size <= c.maxBytes {
				break
			}
		}
		victim := c.tail.prev
		if victim == &c.head {
			// The cache is empty yet the entry still does not fit, e.g. a
			// single entry larger than MaxBytes. Reject it.
			c.rejections++
			return
		}
		c.removeLocked(victim)
		delete(c.entries, victim.key)
		c.evictions++
	}

	c.entries[key] = e
	c.pushFront(e)
	c.bytes += size
}

// Clear removes all cached outlines and resets the statistics counters.
func (c *GlyphCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[glyphCacheKey]*cachedGlyph{}
	c.head.next = &c.tail
	c.tail.prev = &c.head
	c.bytes = 0
	c.hits = 0
	c.misses = 0
	c.evictions = 0
	c.rejections = 0
}

// Stats returns a snapshot of the cache statistics.
func (c *GlyphCache) Stats() GlyphCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return GlyphCacheStats{
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
		Rejected:  c.rejections,
		Entries:   len(c.entries),
		Bytes:     c.bytes,
	}
}

// removeLocked unlinks e from the LRU list and accounts for its size. The
// caller must hold c.mu.
func (c *GlyphCache) removeLocked(e *cachedGlyph) {
	e.prev.next = e.next
	e.next.prev = e.prev
	e.prev = nil
	e.next = nil
	c.bytes -= e.size
}

// pushFront inserts e at the front of the LRU list. The caller must hold
// c.mu.
func (c *GlyphCache) pushFront(e *cachedGlyph) {
	e.prev = &c.head
	e.next = c.head.next
	c.head.next.prev = e
	c.head.next = e
}

// moveToFront marks e as most recently used. The caller must hold c.mu.
func (c *GlyphCache) moveToFront(e *cachedGlyph) {
	e.prev.next = e.next
	e.next.prev = e.prev
	c.pushFront(e)
}

// nextGlyphCacheID returns a process-wide unique identifier for a Font. The
// identifier is what keeps the outlines of distinct fonts and of fonts inside
// a collection from being confused with one another.
func nextGlyphCacheID() uint64 {
	return glyphCacheIDCounter.Add(1)
}

var glyphCacheIDCounter atomic.Uint64
