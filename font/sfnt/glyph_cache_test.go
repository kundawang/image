// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sfnt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/math/fixed"
)

func TestGlyphCacheSegmentSize(t *testing.T) {
	if got, want := unsafe.Sizeof(Segment{}), uintptr(segmentSize); got != want {
		t.Fatalf("segmentSize = %d, but unsafe.Sizeof(Segment{}) = %d", want, got)
	}
}

// loadAllGlyphs runs fn for every glyph index in f, exercising simple,
// compound and empty glyphs.
func loadAllGlyphs(t *testing.T, f *Font, fn func(x GlyphIndex) error) {
	t.Helper()
	for x, n := GlyphIndex(0), GlyphIndex(f.NumGlyphs()); x < n; x++ {
		if err := fn(x); err != nil {
			t.Fatalf("glyph %d: %v", x, err)
		}
	}
}

// segmentsEqual reports whether two outlines are equivalent, treating a nil
// slice and a non-nil empty slice as equal (both are an empty outline).
func segmentsEqual(a, b Segments) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// TestGlyphCacheMatchesUncached compares cached and uncached LoadGlyph,
// GlyphBounds and GlyphAdvance over several fonts (TrueType with simple and
// compound glyphs, empty glyphs, and PostScript/CFF outlines), glyph indices
// and ppem values.
func TestGlyphCacheMatchesUncached(t *testing.T) {
	fonts := map[string][]byte{
		"goregular": goregular.TTF,
		"gobold":    gobold.TTF,
		"gomono":    gomono.TTF,
		"glyfTest":  readTestData(t, "glyfTest.ttf"),
		"CFFTest":   readTestData(t, "CFFTest.otf"),
		"cmapTest":  readTestData(t, "cmapTest.ttf"),
	}
	ppems := []fixed.Int26_6{
		fixed.Int26_6(8 << 6),
		fixed.Int26_6(12<<6 + 32), // Fractional ppem.
		fixed.Int26_6(64 << 6),
	}

	for name, data := range fonts {
		t.Run(name, func(t *testing.T) {
			cache := NewGlyphCache(GlyphCacheOptions{})
			cached, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			cached.SetGlyphCache(cache)
			uncached, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse (uncached): %v", err)
			}

			var cb, ub Buffer
			loadAllGlyphs(t, cached, func(x GlyphIndex) error {
				for _, ppem := range ppems {
					// First call: miss and decode. Second call: cache hit.
					got1, err := cached.LoadGlyph(&cb, x, ppem, nil)
					if err != nil {
						return fmt.Errorf("cached LoadGlyph: %w", err)
					}
					got2, err := cached.LoadGlyph(&cb, x, ppem, nil)
					if err != nil {
						return fmt.Errorf("cached LoadGlyph (hit): %w", err)
					}
					want, err := uncached.LoadGlyph(&ub, x, ppem, nil)
					if err != nil {
						return fmt.Errorf("uncached LoadGlyph: %w", err)
					}
					if !segmentsEqual(got1, want) {
						return fmt.Errorf("ppem=%d: cached segments %v, want %v", ppem, got1, want)
					}
					if !segmentsEqual(got2, want) {
						return fmt.Errorf("ppem=%d: cache-hit segments %v, want %v", ppem, got2, want)
					}

					// A nil Buffer on a cache hit returns the shared
					// read-only entry, which must equal the uncached result.
					shared, err := cached.LoadGlyph(nil, x, ppem, nil)
					if err != nil {
						return fmt.Errorf("nil-buffer LoadGlyph: %w", err)
					}
					if !segmentsEqual(shared, want) {
						return fmt.Errorf("ppem=%d: shared segments differ from uncached", ppem)
					}

					gotBounds, gotAdv, err := cached.GlyphBounds(&cb, x, ppem, font.HintingNone)
					if err != nil {
						return fmt.Errorf("cached GlyphBounds: %w", err)
					}
					wantBounds, wantAdv, err := uncached.GlyphBounds(&ub, x, ppem, font.HintingNone)
					if err != nil {
						return fmt.Errorf("uncached GlyphBounds: %w", err)
					}
					if gotBounds != wantBounds || gotAdv != wantAdv {
						return fmt.Errorf("ppem=%d: cached bounds/adv (%v, %d), want (%v, %d)",
							ppem, gotBounds, gotAdv, wantBounds, wantAdv)
					}

					gotAdvance, err := cached.GlyphAdvance(&cb, x, ppem, font.HintingFull)
					if err != nil {
						return fmt.Errorf("cached GlyphAdvance: %w", err)
					}
					wantAdvance, err := uncached.GlyphAdvance(&ub, x, ppem, font.HintingFull)
					if err != nil {
						return fmt.Errorf("uncached GlyphAdvance: %w", err)
					}
					if gotAdvance != wantAdvance {
						return fmt.Errorf("ppem=%d: cached advance %d, want %d", ppem, gotAdvance, wantAdvance)
					}
				}
				return nil
			})

			stats := cache.Stats()
			if stats.Hits == 0 {
				t.Errorf("expected cache hits, got stats %+v", stats)
			}
			if stats.Misses == 0 {
				t.Errorf("expected cache misses, got stats %+v", stats)
			}
			if stats.Entries == 0 || stats.Bytes == 0 {
				t.Errorf("expected populated cache, got stats %+v", stats)
			}
		})
	}
}

// TestGlyphCacheHitRate checks hit/miss accounting and the key components:
// glyph index and ppem produce distinct entries.
func TestGlyphCacheHitRate(t *testing.T) {
	f, err := Parse(goregular.TTF)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cache := NewGlyphCache(GlyphCacheOptions{})
	f.SetGlyphCache(cache)
	ppem := fixed.Int26_6(f.UnitsPerEm())

	var b Buffer
	const a, x = GlyphIndex(4), GlyphIndex(5) // Exact indices do not matter.

	check := func(wantHits, wantMisses uint64) {
		t.Helper()
		got := cache.Stats()
		if got.Hits != wantHits || got.Misses != wantMisses {
			t.Fatalf("hits/misses: got (%d, %d), want (%d, %d)", got.Hits, got.Misses, wantHits, wantMisses)
		}
	}

	if _, err := f.LoadGlyph(&b, a, ppem, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	check(0, 1)
	if _, err := f.LoadGlyph(&b, a, ppem, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	check(1, 1)
	// A different glyph is a separate entry.
	if _, err := f.LoadGlyph(&b, x, ppem, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	check(1, 2)
	// A different ppem is a separate entry.
	if _, err := f.LoadGlyph(&b, a, ppem+64, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	check(1, 3)
	// Repeating the new ppem hits.
	if _, err := f.LoadGlyph(&b, a, ppem+64, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	check(2, 3)
	// A nil Buffer also participates in the cache.
	if _, err := f.LoadGlyph(nil, a, ppem, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	check(3, 3)

	stats := cache.Stats()
	if stats.Entries != 3 {
		t.Errorf("Entries: got %d, want 3", stats.Entries)
	}

	cache.Clear()
	if stats = cache.Stats(); stats != (GlyphCacheStats{}) {
		t.Errorf("after Clear: stats %+v, want zero value", stats)
	}
	if _, err := f.LoadGlyph(&b, a, ppem, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	if stats = cache.Stats(); stats.Hits != 0 || stats.Misses != 1 {
		t.Errorf("after Clear: stats %+v, want one miss", stats)
	}

	// Detaching the cache restores fully uncached behavior.
	f.SetGlyphCache(nil)
	if _, err := f.LoadGlyph(&b, a, ppem, nil); err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	if stats = cache.Stats(); stats.Hits != 0 || stats.Misses != 1 {
		t.Errorf("detached cache changed stats: %+v", stats)
	}
}

// TestGlyphCacheEviction verifies MaxEntries and MaxBytes enforcement and
// least-recently-used eviction order.
func TestGlyphCacheEviction(t *testing.T) {
	f, err := Parse(goregular.TTF)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ppem := fixed.Int26_6(f.UnitsPerEm())

	t.Run("max entries", func(t *testing.T) {
		const maxEntries = 2
		cache := NewGlyphCache(GlyphCacheOptions{MaxEntries: maxEntries})
		f.SetGlyphCache(cache)
		var b Buffer
		keys := []GlyphIndex{10, 20, 30, 40}
		for _, x := range keys {
			if _, err := f.LoadGlyph(&b, x, ppem, nil); err != nil {
				t.Fatalf("LoadGlyph(%d): %v", x, err)
			}
		}
		stats := cache.Stats()
		if stats.Entries != maxEntries {
			t.Fatalf("Entries: got %d, want %d", stats.Entries, maxEntries)
		}
		if stats.Evictions != uint64(len(keys)-maxEntries) {
			t.Fatalf("Evictions: got %d, want %d", stats.Evictions, len(keys)-maxEntries)
		}
		// Immediately after insertion the cache holds keys 30 and 40; keys
		// 10 and 20 were evicted in LRU order.
		for _, x := range keys[2:] {
			if _, err := f.LoadGlyph(&b, x, ppem, nil); err != nil {
				t.Fatalf("LoadGlyph(%d): %v", x, err)
			}
		}
		for _, x := range keys[:2] {
			if _, err := f.LoadGlyph(&b, x, ppem, nil); err != nil {
				t.Fatalf("LoadGlyph(%d): %v", x, err)
			}
		}
		stats = cache.Stats()
		// 2 hits from keys 30/40, 2 misses from re-inserting keys 10/20.
		if stats.Hits != 2 || stats.Misses != 6 || stats.Entries != maxEntries {
			t.Fatalf("LRU order: hits=%d misses=%d entries=%d, want hits=2 misses=6 entries=%d",
				stats.Hits, stats.Misses, stats.Entries, maxEntries)
		}

		// The re-insertions displaced keys 30 and 40; the entries are now
		// keys 10 and 20. Touch 10 so it becomes most recently used, then
		// inserting a new glyph must evict 20 and leave 10 cached.
		if _, err := f.LoadGlyph(&b, 10, ppem, nil); err != nil {
			t.Fatalf("LoadGlyph: %v", err)
		}
		if _, err := f.LoadGlyph(&b, 50, ppem, nil); err != nil {
			t.Fatalf("LoadGlyph: %v", err)
		}
		if _, err := f.LoadGlyph(&b, 10, ppem, nil); err != nil {
			t.Fatalf("LoadGlyph: %v", err)
		}
		if got := cache.Stats().Hits; got != 4 {
			t.Fatalf("hits after LRU refresh: got %d, want 4", got)
		}
	})

	t.Run("max bytes", func(t *testing.T) {
		// One segment per entry budget, so every non-empty glyph is evicted
		// by the byte limit. Empty glyphs (zero bytes) still fit.
		cache := NewGlyphCache(GlyphCacheOptions{
			MaxEntries:    -1, // Unlimited entry count.
			MaxBytes:      segmentSize,
			MaxEntryBytes: -1,
		})
		f.SetGlyphCache(cache)
		var b Buffer
		var evicted, nonEmpty int
		loadAllGlyphs(t, f, func(x GlyphIndex) error {
			segs, err := f.LoadGlyph(&b, x, ppem, nil)
			if err != nil {
				return err
			}
			if len(segs) > 0 {
				nonEmpty++
			}
			return nil
		})
		stats := cache.Stats()
		if stats.Bytes > segmentSize {
			t.Errorf("Bytes: got %d, want <= %d", stats.Bytes, segmentSize)
		}
		evicted = int(stats.Evictions)
		if nonEmpty > 1 && evicted == 0 {
			t.Errorf("expected byte-budget evictions, got stats %+v (non-empty glyphs: %d)", stats, nonEmpty)
		}
	})

	t.Run("entry too large", func(t *testing.T) {
		// Reject everything but empty outlines. Use a separate uncached Font
		// to obtain reference results.
		cache := NewGlyphCache(GlyphCacheOptions{MaxEntryBytes: 1})
		f.SetGlyphCache(cache)
		refFont, err := Parse(goregular.TTF)
		if err != nil {
			t.Fatalf("Parse reference: %v", err)
		}
		var b, ub Buffer
		var rejected, emptyGlyphs int
		loadAllGlyphs(t, f, func(x GlyphIndex) error {
			got, err := f.LoadGlyph(&b, x, ppem, nil)
			if err != nil {
				return err
			}
			// A rejected entry must still be fully correct.
			want, err := refFont.LoadGlyph(&ub, x, ppem, nil)
			if err != nil {
				return err
			}
			if !segmentsEqual(got, want) {
				return fmt.Errorf("glyph %d: rejected-entry result differs from uncached", x)
			}
			if len(got) == 0 {
				emptyGlyphs++
			} else {
				rejected++
			}
			return nil
		})
		stats := cache.Stats()
		if stats.Rejected != uint64(rejected) || rejected == 0 {
			t.Errorf("Rejected: got %d, want %d; stats %+v", stats.Rejected, rejected, stats)
		}
		if stats.Entries != emptyGlyphs {
			t.Errorf("Entries: got %d, want %d (the empty glyphs)", stats.Entries, emptyGlyphs)
		}
		// Repeating an empty glyph hits its cached entry.
		var emptyIndex GlyphIndex
		foundEmpty := false
		for i := GlyphIndex(0); i < GlyphIndex(f.NumGlyphs()); i++ {
			segs, err := f.LoadGlyph(&b, i, ppem, nil)
			if err != nil {
				t.Fatalf("LoadGlyph(%d): %v", i, err)
			}
			if len(segs) == 0 {
				emptyIndex = i
				foundEmpty = true
				break
			}
		}
		if foundEmpty {
			before := cache.Stats().Hits
			if _, err := f.LoadGlyph(&b, emptyIndex, ppem, nil); err != nil {
				t.Fatalf("empty glyph reload: %v", err)
			}
			if cache.Stats().Hits != before+1 {
				t.Errorf("empty glyph was not cached: stats %+v", cache.Stats())
			}
		}
	})
}

// TestGlyphCacheIsolation checks that fonts, font collections, compound
// glyphs and empty glyphs never share cache entries.
func TestGlyphCacheIsolation(t *testing.T) {
	cache := NewGlyphCache(GlyphCacheOptions{})
	ppem := fixed.Int26_6(2048)

	// Two Font values parsed from the same bytes are distinct cache tenants.
	f1, err := Parse(goregular.TTF)
	if err != nil {
		t.Fatalf("Parse f1: %v", err)
	}
	f2, err := Parse(goregular.TTF)
	if err != nil {
		t.Fatalf("Parse f2: %v", err)
	}
	f1.SetGlyphCache(cache)
	f2.SetGlyphCache(cache)

	var b Buffer
	if _, err := f1.LoadGlyph(&b, 4, ppem, nil); err != nil {
		t.Fatalf("f1 LoadGlyph: %v", err)
	}
	if stats := cache.Stats(); stats.Misses != 1 || stats.Hits != 0 {
		t.Fatalf("f1 first load: stats %+v, want 1 miss", stats)
	}
	// The same glyph index/ppem on a different font must not hit f1's entry.
	if _, err := f2.LoadGlyph(&b, 4, ppem, nil); err != nil {
		t.Fatalf("f2 LoadGlyph: %v", err)
	}
	if stats := cache.Stats(); stats.Misses != 2 || stats.Hits != 0 {
		t.Fatalf("f2 load leaked f1 entry: stats %+v", stats)
	}
	if _, err := f1.LoadGlyph(&b, 4, ppem, nil); err != nil {
		t.Fatalf("f1 reload: %v", err)
	}
	if stats := cache.Stats(); stats.Hits != 1 {
		t.Fatalf("f1 reload: stats %+v, want 1 hit", stats)
	}

	// A Collection and its Fonts share the cache but remain isolated per
	// font. ParseCollection also accepts a single TTF.
	coll, err := ParseCollection(goregular.TTF)
	if err != nil {
		t.Fatalf("ParseCollection: %v", err)
	}
	cf, err := coll.Font(0)
	if err != nil {
		t.Fatalf("Collection.Font: %v", err)
	}
	cf.SetGlyphCache(cache)
	if _, err := cf.LoadGlyph(&b, 4, ppem, nil); err != nil {
		t.Fatalf("collection font LoadGlyph: %v", err)
	}
	if stats := cache.Stats(); stats.Misses != 3 {
		t.Fatalf("collection font reused another font's entry: stats %+v", stats)
	}

	// glyfTest.ttf contains empty glyphs (indices 1 and 2) and compound
	// glyphs (indices 6 through 9). Both kinds must round-trip through the
	// cache with identical segments.
	gf, err := Parse(readTestData(t, "glyfTest.ttf"))
	if err != nil {
		t.Fatalf("Parse glyfTest: %v", err)
	}
	gf.SetGlyphCache(cache)
	var gb, ub Buffer
	for _, x := range []GlyphIndex{0, 1, 2, 3, 4, 5, 6, 7, 8, 9} {
		got, err := gf.LoadGlyph(&gb, x, ppem, nil)
		if err != nil {
			t.Fatalf("glyph %d: LoadGlyph: %v", x, err)
		}
		if _, err := gf.LoadGlyph(&gb, x, ppem, nil); err != nil {
			t.Fatalf("glyph %d: reload: %v", x, err)
		}
		ref, err := gf.LoadGlyph(&ub, x, ppem, nil)
		if err != nil {
			t.Fatalf("glyph %d: uncached LoadGlyph: %v", x, err)
		}
		if !segmentsEqual(got, ref) {
			t.Fatalf("glyph %d (compound=%v empty=%v): cached %v, uncached %v",
				x, x >= 6, len(got) == 0, got, ref)
		}
	}
	// Empty and compound glyphs each got at least one hit.
	if stats := cache.Stats(); stats.Hits < 2 {
		t.Fatalf("expected hits for empty/compound glyphs, got %+v", stats)
	}
}

// TestGlyphCacheBufferOwnership verifies that entries returned through a
// Buffer are private copies: mutating them cannot corrupt the shared cache,
// while nil-Buffer returns are shared read-only views.
func TestGlyphCacheBufferOwnership(t *testing.T) {
	f, err := Parse(goregular.TTF)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cache := NewGlyphCache(GlyphCacheOptions{})
	f.SetGlyphCache(cache)
	ppem := fixed.Int26_6(f.UnitsPerEm())
	x := GlyphIndex(4)

	var b1, b2 Buffer
	first, err := f.LoadGlyph(&b1, x, ppem, nil)
	if err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("test glyph has no segments")
	}
	snapshot := append(Segments(nil), first...)

	// Nil-buffer hits hand back the same shared backing array.
	s1, err := f.LoadGlyph(nil, x, ppem, nil)
	if err != nil {
		t.Fatalf("nil LoadGlyph 1: %v", err)
	}
	s2, err := f.LoadGlyph(nil, x, ppem, nil)
	if err != nil {
		t.Fatalf("nil LoadGlyph 2: %v", err)
	}
	if len(s1) == 0 || &s1[0] != &s2[0] {
		t.Fatal("nil-buffer hits did not return the shared cached slice")
	}

	// Mutating a Buffer-owned copy must not change the cached entry.
	first[0].Args[0].X += 12345
	first[0].Args[0].Y -= 12345
	again, err := f.LoadGlyph(&b2, x, ppem, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(again, snapshot) {
		t.Fatal("mutating a Buffer-owned copy corrupted the cached outline")
	}
}

// errCanceled is returned by cancelableReaderAt after cancellation.
var errCanceled = errors.New("sfnt: test read canceled")

// cancelableReaderAt is an io.ReaderAt whose reads can be canceled and whose
// byte content is verified never to change.
type cancelableReaderAt struct {
	data     []byte
	canceled atomic.Bool
	reads    atomic.Int64
}

func (r *cancelableReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads.Add(1)
	if r.canceled.Load() {
		return 0, errCanceled
	}
	if off < 0 || off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// TestGlyphCacheReaderAt verifies that cached loading over an io.ReaderAt
// never mutates the input, that canceled reads produce a stable error (and
// are not cached), and that reads recover once cancellation is lifted.
func TestGlyphCacheReaderAt(t *testing.T) {
	original := append([]byte(nil), goregular.TTF...)
	r := &cancelableReaderAt{data: append([]byte(nil), original...)}
	f, err := ParseReaderAt(r)
	if err != nil {
		t.Fatalf("ParseReaderAt: %v", err)
	}
	cache := NewGlyphCache(GlyphCacheOptions{})
	f.SetGlyphCache(cache)
	ppem := fixed.Int26_6(f.UnitsPerEm())
	var b Buffer

	// Populate one entry so the cache is demonstrably in use.
	const cachedGlyph, blockedGlyph = GlyphIndex(4), GlyphIndex(5)
	cachedWant, err := f.LoadGlyph(&b, cachedGlyph, ppem, nil)
	if err != nil {
		t.Fatalf("LoadGlyph: %v", err)
	}
	if !bytes.Equal(r.data, original) {
		t.Fatal("font input bytes were modified while reading")
	}

	// Cancel reads: an uncached glyph must fail with the reader's error on
	// every attempt, and the error must never be stored in the cache.
	r.canceled.Store(true)
	for i := 0; i < 3; i++ {
		if _, err := f.LoadGlyph(&b, blockedGlyph, ppem, nil); !errors.Is(err, errCanceled) {
			t.Fatalf("attempt %d: got %v, want %v", i, err, errCanceled)
		}
	}
	stats := cache.Stats()
	if stats.Entries != 1 {
		t.Fatalf("errored load changed entry count: stats %+v", stats)
	}
	// Only the one successful decode counts as a miss; failed decodes do not.
	if stats.Misses != 1 || stats.Hits != 0 {
		t.Fatalf("errored loads skewed the counters: stats %+v", stats)
	}
	// Already-cached outlines remain readable while reads are canceled.
	got, err := f.LoadGlyph(&b, cachedGlyph, ppem, nil)
	if err != nil {
		t.Fatalf("cached glyph while canceled: %v", err)
	}
	if !segmentsEqual(got, cachedWant) {
		t.Fatal("cached outline changed while reads were canceled")
	}

	// Recovery: lift cancellation, the blocked glyph must decode correctly
	// and then populate the cache.
	r.canceled.Store(false)
	want, err := f.LoadGlyph(&b, blockedGlyph, ppem, nil)
	if err != nil {
		t.Fatalf("LoadGlyph after recovery: %v", err)
	}
	if _, err := f.LoadGlyph(&b, blockedGlyph, ppem, nil); err != nil {
		t.Fatalf("cached reload: %v", err)
	}
	// Cross-check against an uncached decode of the same font data.
	ref, err := Parse(goregular.TTF)
	if err != nil {
		t.Fatalf("Parse reference: %v", err)
	}
	var rb Buffer
	refWant, err := ref.LoadGlyph(&rb, blockedGlyph, ppem, nil)
	if err != nil {
		t.Fatalf("reference LoadGlyph: %v", err)
	}
	if !segmentsEqual(want, refWant) {
		t.Fatal("recovered segments differ from the uncached decode")
	}
	if !bytes.Equal(r.data, original) {
		t.Fatal("font input bytes were modified while reading")
	}
	if r.reads.Load() == 0 {
		t.Fatal("no ReadAt calls observed")
	}
}

// patchGlyf returns a modified copy of data with fn applied to the font's
// internal table locations.
func patchGlyf(t *testing.T, data []byte, fn func(f *Font, buf []byte)) []byte {
	t.Helper()
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out := append([]byte(nil), data...)
	fn(f, out)
	return out
}

// TestGlyphCacheBadGlyf corrupts a simple glyph's header and checks that the
// parse error is stable across repeated cached calls and is never cached.
func TestGlyphCacheBadGlyf(t *testing.T) {
	data := patchGlyf(t, readTestData(t, "glyfTest.ttf"), func(f *Font, buf []byte) {
		// Glyph 3 ("zero") is a simple glyph. Claim an impossible number of
		// contours so the parser runs past the end of the glyph data.
		start := f.cached.glyphData.locations[3]
		buf[start+0] = 0x7f
		buf[start+1] = 0xff
	})
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cache := NewGlyphCache(GlyphCacheOptions{})
	f.SetGlyphCache(cache)
	ppem := fixed.Int26_6(f.UnitsPerEm())
	var b Buffer

	var first error
	for i := 0; i < 4; i++ {
		_, err := f.LoadGlyph(&b, 3, ppem, nil)
		if err == nil {
			t.Fatalf("attempt %d: expected error, got nil", i)
		}
		if i == 0 {
			first = err
		} else if err.Error() != first.Error() {
			t.Fatalf("attempt %d: got %v, want stable %v", i, err, first)
		}
	}
	if !errors.Is(first, errInvalidGlyphData) {
		t.Fatalf("got %v, want %v", first, errInvalidGlyphData)
	}
	// Failed decodes are neither hits nor misses and never enter the cache.
	stats := cache.Stats()
	if stats.Entries != 0 || stats.Hits != 0 || stats.Misses != 0 {
		t.Fatalf("errored glyph was cached: stats %+v", stats)
	}
	// Out-of-range indices keep their ErrNotFound semantics and are likewise
	// neither hits nor misses.
	for i := 0; i < 2; i++ {
		if _, err := f.LoadGlyph(&b, GlyphIndex(f.NumGlyphs()), ppem, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("out-of-range LoadGlyph: got %v, want %v", err, ErrNotFound)
		}
	}
	if stats = cache.Stats(); stats.Hits != 0 || stats.Misses != 0 || stats.Entries != 0 {
		t.Fatalf("not-found lookups changed cache stats: %+v", stats)
	}
}

// TestGlyphCacheCompoundRecursion patches a compound glyph to reference
// itself and checks for a stable recursion-depth error.
func TestGlyphCacheCompoundRecursion(t *testing.T) {
	data := patchGlyf(t, readTestData(t, "glyfTest.ttf"), func(f *Font, buf []byte) {
		// Glyph 6 ("six") is compound. Rewrite its first component's glyph
		// index to 6, making it self-referential.
		start := f.cached.glyphData.locations[6]
		componentGlyphIndexOffset := start + glyfHeaderLen + 2
		buf[componentGlyphIndexOffset+0] = 0
		buf[componentGlyphIndexOffset+1] = 6
	})
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cache := NewGlyphCache(GlyphCacheOptions{})
	f.SetGlyphCache(cache)
	ppem := fixed.Int26_6(f.UnitsPerEm())
	var b Buffer

	for i := 0; i < 3; i++ {
		_, err := f.LoadGlyph(&b, 6, ppem, nil)
		if !errors.Is(err, errUnsupportedCompoundGlyph) {
			t.Fatalf("attempt %d: got %v, want %v", i, err, errUnsupportedCompoundGlyph)
		}
	}
	if stats := cache.Stats(); stats.Entries != 0 || stats.Hits != 0 {
		t.Fatalf("recursing compound glyph was cached: stats %+v", stats)
	}
}

// TestGlyphCacheUnsupportedGlyphDataLength inflates a loca entry so the
// glyph claims more than maxGlyphDataLength bytes.
func TestGlyphCacheUnsupportedGlyphDataLength(t *testing.T) {
	data := patchGlyf(t, readTestData(t, "glyfTest.ttf"), func(f *Font, buf []byte) {
		var inflated uint32 = 70000 // Larger than maxGlyphDataLength (64 KiB).
		start := f.cached.glyphData.locations[3]
		if f.cached.indexToLocFormat {
			off := f.loca.offset + 4*4
			buf[off+0] = byte(inflated >> 24)
			buf[off+1] = byte(inflated >> 16)
			buf[off+2] = byte(inflated >> 8)
			buf[off+3] = byte(inflated)
		} else {
			// Short loca stores half the glyph-relative offset.
			half := uint16((start - f.glyf.offset + inflated) / 2)
			off := f.loca.offset + 2*4
			buf[off+0] = byte(half >> 8)
			buf[off+1] = byte(half)
		}
	})
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cache := NewGlyphCache(GlyphCacheOptions{})
	f.SetGlyphCache(cache)
	ppem := fixed.Int26_6(f.UnitsPerEm())
	var b Buffer
	for i := 0; i < 2; i++ {
		if _, err := f.LoadGlyph(&b, 3, ppem, nil); !errors.Is(err, errUnsupportedGlyphDataLength) {
			t.Fatalf("attempt %d: got %v, want %v", i, err, errUnsupportedGlyphDataLength)
		}
	}
	if stats := cache.Stats(); stats.Entries != 0 {
		t.Fatalf("oversize glyph was cached: stats %+v", stats)
	}
}

// TestGlyphCacheConcurrent hammers one cached font from many goroutines with
// separate Buffers under the race detector, checking that every result
// matches an uncached decode and that the cache stays within its limits.
func TestGlyphCacheConcurrent(t *testing.T) {
	data := goregular.TTF
	cached, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	const maxEntries = 8
	cache := NewGlyphCache(GlyphCacheOptions{MaxEntries: maxEntries})
	cached.SetGlyphCache(cache)
	uncached, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse uncached: %v", err)
	}
	upem := fixed.Int26_6(uncached.UnitsPerEm())

	// Warm the cache sequentially with exactly maxEntries keys so that the
	// subsequent concurrent run can only produce hits and LRU evictions.
	const numGlyphs = maxEntries
	var warmup Buffer
	for x := GlyphIndex(0); x < numGlyphs; x++ {
		if _, err := cached.LoadGlyph(&warmup, x, upem, nil); err != nil {
			t.Fatalf("warmup LoadGlyph(%d): %v", x, err)
		}
	}
	if stats := cache.Stats(); stats.Entries != maxEntries || stats.Misses != uint64(numGlyphs) {
		t.Fatalf("warmup stats %+v, want %d entries and misses", stats, numGlyphs)
	}
	missesAtStart := cache.Stats().Misses

	const (
		goroutines = 16
		iterations = 200
	)
	var wg sync.WaitGroup
	var lookups atomic.Uint64
	var failed atomic.Bool
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			var cb, ub Buffer
			for i := 0; i < iterations; i++ {
				x := GlyphIndex((seed*7 + i) % numGlyphs)
				p := upem
				if i%3 == 0 {
					p = fixed.Int26_6(12 << 6) // Distinct ppem: a miss path.
				}
				got, err := cached.LoadGlyph(&cb, x, p, nil)
				if err != nil {
					t.Errorf("cached LoadGlyph(%d): %v", x, err)
					failed.Store(true)
					return
				}
				want, err := uncached.LoadGlyph(&ub, x, p, nil)
				if err != nil {
					t.Errorf("uncached LoadGlyph(%d): %v", x, err)
					failed.Store(true)
					return
				}
				if !segmentsEqual(got, want) {
					t.Errorf("glyph %d ppem %d: cached/uncached mismatch", x, p)
					failed.Store(true)
					return
				}
				lookups.Add(1)
			}
		}(g)
	}
	wg.Wait()
	if failed.Load() {
		t.Fatal("concurrent cached loads failed")
	}

	stats := cache.Stats()
	// Under concurrency, several goroutines can decode the same absent key
	// before the first one inserts it; the losing decodes are discarded and
	// are neither hits nor misses, so the counters can only bound the
	// lookups. The exact hit+miss identity is checked sequentially in
	// TestGlyphCacheHitRate.
	newMisses := stats.Misses - missesAtStart
	if got, want := stats.Hits+newMisses, lookups.Load(); got > want {
		t.Fatalf("hits+new misses = %d exceeds lookups = %d (stats %+v)", got, want, stats)
	}
	if stats.Hits == 0 || newMisses == 0 {
		t.Fatalf("expected both hits and concurrent misses, got %+v", stats)
	}
	if stats.Entries > maxEntries {
		t.Fatalf("Entries %d exceeds MaxEntries", stats.Entries)
	}

	// Concurrent Clear must not corrupt subsequent lookups.
	var wg2 sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg2.Add(1)
		go func(seed int) {
			defer wg2.Done()
			var b Buffer
			for i := 0; i < 100; i++ {
				if i%10 == 0 {
					cache.Clear()
				}
				if _, err := cached.LoadGlyph(&b, GlyphIndex(seed+i%numGlyphs), upem, nil); err != nil {
					t.Errorf("post-Clear LoadGlyph: %v", err)
					return
				}
			}
		}(g)
	}
	wg2.Wait()
}

func readTestData(t *testing.T, name string) []byte {
	t.Helper()
	data, err := ioutil.ReadFile(filepath.FromSlash("../testdata/" + name))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", name, err)
	}
	return data
}

func BenchmarkGlyphCacheLoad(b *testing.B) {
	f, err := Parse(goregular.TTF)
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	ppem := fixed.Int26_6(f.UnitsPerEm())
	const x = GlyphIndex(32)

	b.Run("uncached", func(b *testing.B) {
		var buf Buffer
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := f.LoadGlyph(&buf, x, ppem, nil); err != nil {
				b.Fatal(err)
			}
		}
	})

	cache := NewGlyphCache(GlyphCacheOptions{})
	f.SetGlyphCache(cache)
	if _, err := f.LoadGlyph(nil, x, ppem, nil); err != nil {
		b.Fatal(err)
	}
	b.Run("cached hit", func(b *testing.B) {
		var buf Buffer
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := f.LoadGlyph(&buf, x, ppem, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
