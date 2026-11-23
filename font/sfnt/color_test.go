// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sfnt_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/color"
	"io/ioutil"
	"path/filepath"
	"testing"

	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// Glyph IDs and CPAL color indices built into the test fixtures by
// testdata-generator.
const (
	gidNotdef     = sfnt.GlyphIndex(0)
	gidSquareA    = sfnt.GlyphIndex(1)
	gidSquareB    = sfnt.GlyphIndex(2)
	gidCircle     = sfnt.GlyphIndex(3)
	gidDiamond    = sfnt.GlyphIndex(4)
	gidLayerEmpty = sfnt.GlyphIndex(5)
	gidDeepChain  = sfnt.GlyphIndex(6)
	gidOutOfRange = sfnt.GlyphIndex(7)

	colorIndexForeground = 0xFFFF
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := ioutil.ReadFile(filepath.FromSlash("../testdata/" + name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func parseFixture(t *testing.T, name string) *sfnt.Font {
	t.Helper()
	f, err := sfnt.Parse(readFixture(t, name))
	if err != nil {
		t.Fatalf("Parse %s: %v", name, err)
	}
	return f
}

func TestColorGlyphV0Layers(t *testing.T) {
	f := parseFixture(t, "colrv0.ttf")

	want := []sfnt.ColorGlyphLayer{{
		GlyphIndex:   gidSquareA,
		PaletteIndex: 0,
		Color:        color.RGBA{R: 255, G: 0, B: 0, A: 255},
		Alpha:        255,
	}, {
		GlyphIndex:   gidSquareB,
		PaletteIndex: 1,
		Color:        color.RGBA{R: 0, G: 255, B: 0, A: 255},
		Alpha:        255,
	}, {
		GlyphIndex:   gidCircle,
		PaletteIndex: 3,
		Color:        color.RGBA{R: 255, G: 0, B: 0, A: 128},
		Alpha:        128,
	}, {
		GlyphIndex:   gidDiamond,
		PaletteIndex: 4,
		Color:        color.RGBA{R: 255, G: 0, B: 0, A: 0},
		Alpha:        0,
	}}
	got, err := f.LoadColorGlyph(nil, gidSquareA, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d layers, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("layer %d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestColorGlyphV0ForegroundAndMissingColor(t *testing.T) {
	f := parseFixture(t, "colrv0.ttf")

	// Default foreground is opaque black. The second layer's palette index
	// (600) is out of range and must fall back to the same foreground color.
	got, err := f.LoadColorGlyph(nil, gidSquareB, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph: %v", err)
	}
	want := []sfnt.ColorGlyphLayer{{
		GlyphIndex:   gidSquareA,
		PaletteIndex: colorIndexForeground,
		Color:        color.RGBA{R: 0, G: 0, B: 0, A: 255},
		Alpha:        255,
	}, {
		GlyphIndex:   gidCircle,
		PaletteIndex: 600,
		Color:        color.RGBA{R: 0, G: 0, B: 0, A: 255},
		Alpha:        255,
	}}
	if len(got) != len(want) {
		t.Fatalf("got %d layers, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("layer %d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}

	// A caller-supplied foreground overrides both the explicit foreground
	// index and the missing-color fallback.
	fg := color.RGBA{R: 10, G: 20, B: 30, A: 255}
	got, err = f.LoadColorGlyph(nil, gidSquareB, &sfnt.LoadColorGlyphOptions{
		Foreground: fg,
	})
	if err != nil {
		t.Fatalf("LoadColorGlyph with foreground: %v", err)
	}
	for i, l := range got {
		if l.Color != fg || l.Alpha != 255 {
			t.Errorf("layer %d: got %+v, want foreground %+v", i, l.Color, fg)
		}
	}
}

func TestColorGlyphV0EmptyLayers(t *testing.T) {
	f := parseFixture(t, "colrv0.ttf")
	got, err := f.LoadColorGlyph(nil, gidLayerEmpty, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d layers, want 0", len(got))
	}
}

func TestColorPaletteSelection(t *testing.T) {
	f := parseFixture(t, "colrv0.ttf")

	n, err := f.NumColorPalettes(nil)
	if err != nil {
		t.Fatalf("NumColorPalettes: %v", err)
	}
	if n != 2 {
		t.Fatalf("NumColorPalettes: got %d, want 2", n)
	}

	pal0, err := f.ColorPalette(nil, 0)
	if err != nil {
		t.Fatalf("ColorPalette(0): %v", err)
	}
	if pal0[0] != (color.RGBA{R: 255, G: 0, B: 0, A: 255}) {
		t.Errorf("palette 0 entry 0: got %+v, want red", pal0[0])
	}
	pal1, err := f.ColorPalette(nil, 1)
	if err != nil {
		t.Fatalf("ColorPalette(1): %v", err)
	}
	if pal1[0] != (color.RGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Errorf("palette 1 entry 0: got %+v, want white", pal1[0])
	}

	// Selecting palette 1 changes resolved layer colors.
	got, err := f.LoadColorGlyph(nil, gidSquareA, &sfnt.LoadColorGlyphOptions{
		PaletteIndex: 1,
	})
	if err != nil {
		t.Fatalf("LoadColorGlyph palette 1: %v", err)
	}
	if got[0].Color != pal1[0] {
		t.Errorf("palette selection: got %+v, want %+v", got[0].Color, pal1[0])
	}

	// An out-of-range palette index falls back to palette 0.
	fallback, err := f.ColorPalette(nil, 42)
	if err != nil {
		t.Fatalf("ColorPalette(42): %v", err)
	}
	if fallback[0] != pal0[0] {
		t.Errorf("palette fallback: got %+v, want %+v", fallback[0], pal0[0])
	}
	got, err = f.LoadColorGlyph(nil, gidSquareA, &sfnt.LoadColorGlyphOptions{
		PaletteIndex: 42,
	})
	if err != nil {
		t.Fatalf("LoadColorGlyph bad palette: %v", err)
	}
	if got[0].Color != pal0[0] {
		t.Errorf("load palette fallback: got %+v, want %+v", got[0].Color, pal0[0])
	}
}

func TestColorPaletteIsACopy(t *testing.T) {
	f := parseFixture(t, "colrv0.ttf")
	pal, err := f.ColorPalette(nil, 0)
	if err != nil {
		t.Fatalf("ColorPalette: %v", err)
	}
	pal[0] = color.RGBA{R: 1, G: 2, B: 3, A: 4}

	again, err := f.ColorPalette(nil, 0)
	if err != nil {
		t.Fatalf("ColorPalette again: %v", err)
	}
	if again[0] == pal[0] {
		t.Fatalf("returned palette is shared mutable state: %+v", again[0])
	}
}

func TestColorGlyphV1Paints(t *testing.T) {
	f := parseFixture(t, "colrv1.ttf")

	// gidSquareA is a PaintColrLayers of two PaintGlyph wraps. Layers are
	// emitted bottom to top; the second uses PaintVarSolid at alpha 0.5.
	got, err := f.LoadColorGlyph(nil, gidSquareA, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph: %v", err)
	}
	want := []sfnt.ColorGlyphLayer{{
		GlyphIndex:   gidSquareB,
		PaletteIndex: 0,
		Color:        color.RGBA{R: 255, G: 0, B: 0, A: 255},
		Alpha:        255,
	}, {
		GlyphIndex:   gidCircle,
		PaletteIndex: 1,
		Color:        color.RGBA{R: 0, G: 255, B: 0, A: 128},
		Alpha:        128,
	}}
	if len(got) != len(want) {
		t.Fatalf("got %d layers, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("layer %d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}

	// gidSquareB is a single PaintGlyph around a solid blue paint.
	got, err = f.LoadColorGlyph(nil, gidSquareB, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph gidSquareB: %v", err)
	}
	if len(got) != 1 || got[0] != (sfnt.ColorGlyphLayer{
		GlyphIndex:   gidDiamond,
		PaletteIndex: 2,
		Color:        color.RGBA{R: 0, G: 0, B: 255, A: 255},
		Alpha:        255,
	}) {
		t.Fatalf("gidSquareB layers: %+v", got)
	}

	// gidCircle is a PaintColrGlyph reference to gidSquareB's paint graph, so
	// it resolves to the same single layer.
	ref, err := f.LoadColorGlyph(nil, gidCircle, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph gidCircle: %v", err)
	}
	if len(ref) != 1 || ref[0] != got[0] {
		t.Fatalf("PaintColrGlyph resolution: got %+v, want %+v", ref, got)
	}

	// gidLayerEmpty is an empty PaintColrLayers: no layers, no error.
	empty, err := f.LoadColorGlyph(nil, gidLayerEmpty, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty layer list: got %d layers", len(empty))
	}
}

func TestColorGlyphV1Errors(t *testing.T) {
	f := parseFixture(t, "colrv1.ttf")

	// gidDiamond contains an unsupported paint format.
	if _, err := f.LoadColorGlyph(nil, gidDiamond, nil); err == nil {
		t.Fatal("unsupported paint: expected error, got nil")
	} else if !bytes.Contains([]byte(err.Error()), []byte("unsupported COLR paint")) {
		t.Fatalf("unsupported paint: got %v", err)
	}

	// gidDeepChain is a self-referential PaintColrGlyph.
	if _, err := f.LoadColorGlyph(nil, gidDeepChain, nil); err != sfnt.ErrColorCycle {
		t.Fatalf("cycle: got %v, want ErrColorCycle", err)
	}

	// gidOutOfRange wraps a glyph id beyond the font's glyph count.
	if _, err := f.LoadColorGlyph(nil, gidOutOfRange, nil); err == nil {
		t.Fatal("invalid glyph id: expected error, got nil")
	} else if !bytes.Contains([]byte(err.Error()), []byte("invalid COLR layer")) {
		t.Fatalf("invalid glyph id: got %v", err)
	}
}

func TestColorGlyphDepthLimit(t *testing.T) {
	f := parseFixture(t, "colrv1-deep.ttf")
	_, err := f.LoadColorGlyph(nil, gidSquareA, nil)
	if err == nil {
		t.Fatal("deep chain: expected error, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("nesting depth")) {
		t.Fatalf("deep chain: got %v, want a nesting-depth error", err)
	}
}

func TestHasColorGlyph(t *testing.T) {
	f := parseFixture(t, "colrv1.ttf")

	has, err := f.HasColorGlyph(nil, gidSquareA)
	if err != nil || !has {
		t.Errorf("gidSquareA: got (%v, %v), want (true, nil)", has, err)
	}
	has, err = f.HasColorGlyph(nil, gidNotdef)
	if err != nil || has {
		t.Errorf("notdef: got (%v, %v), want (false, nil)", has, err)
	}
	if _, err := f.HasColorGlyph(nil, sfnt.GlyphIndex(f.NumGlyphs())); err != sfnt.ErrNotFound {
		t.Errorf("out-of-range glyph: got %v, want ErrNotFound", err)
	}

	// A monochrome font without a COLR table never reports a color glyph.
	plain, err := sfnt.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	gi, err := plain.GlyphIndex(nil, 'A')
	if err != nil {
		t.Fatal(err)
	}
	has, err = plain.HasColorGlyph(nil, gi)
	if err != nil || has {
		t.Errorf("goregular 'A': got (%v, %v), want (false, nil)", has, err)
	}
	if _, err := plain.LoadColorGlyph(nil, gi, nil); err != sfnt.ErrNotFound {
		t.Errorf("goregular LoadColorGlyph: got %v, want ErrNotFound", err)
	}
	if n, err := plain.NumColorPalettes(nil); err != nil || n != 0 {
		t.Errorf("goregular NumColorPalettes: got (%d, %v), want (0, nil)", n, err)
	}
	if _, err := plain.ColorPalette(nil, 0); err != sfnt.ErrNotFound {
		t.Errorf("goregular ColorPalette: got %v, want ErrNotFound", err)
	}
}

func TestColorGlyphIndexErrors(t *testing.T) {
	f := parseFixture(t, "colrv0.ttf")
	if _, err := f.LoadColorGlyph(nil, sfnt.GlyphIndex(f.NumGlyphs()), nil); err != sfnt.ErrNotFound {
		t.Errorf("out-of-range glyph: got %v, want ErrNotFound", err)
	}
}

// TestMonochromeOutlineUnchanged checks that adding COLR/CPAL support does not
// affect the ordinary glyph outline path: every layer's referenced glyph still
// loads through LoadGlyph, and the returned segments are non-empty.
func TestMonochromeOutlineUnchanged(t *testing.T) {
	f := parseFixture(t, "colrv1.ttf")
	ppem := fixed.Int26_6(f.UnitsPerEm())
	for _, gid := range []sfnt.GlyphIndex{gidSquareA, gidSquareB, gidCircle, gidDiamond} {
		segs, err := f.LoadGlyph(nil, gid, ppem, nil)
		if err != nil {
			t.Errorf("LoadGlyph(%d): %v", gid, err)
			continue
		}
		if len(segs) == 0 {
			t.Errorf("LoadGlyph(%d): empty outline", gid)
		}
	}

	// cmap mapping still resolves the rune used by the fixture.
	gi, err := f.GlyphIndex(nil, 'A')
	if err != nil {
		t.Fatalf("GlyphIndex: %v", err)
	}
	if gi != gidSquareA {
		t.Errorf("GlyphIndex('A'): got %d, want %d", gi, gidSquareA)
	}
}

func TestColorGlyphReaderAt(t *testing.T) {
	// Exercise the io.ReaderAt code path, which allocates scratch buffers
	// instead of slicing the source directly.
	data := readFixture(t, "colrv0.ttf")
	f, err := sfnt.ParseReaderAt(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseReaderAt: %v", err)
	}
	layers, err := f.LoadColorGlyph(nil, gidSquareA, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph: %v", err)
	}
	if len(layers) != 4 || layers[0].Color != (color.RGBA{R: 255, G: 0, B: 0, A: 255}) {
		t.Fatalf("ReaderAt layers: %+v", layers)
	}
	pal, err := f.ColorPalette(nil, 1)
	if err != nil {
		t.Fatalf("ColorPalette: %v", err)
	}
	if pal[0] != (color.RGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Fatalf("ReaderAt palette: %+v", pal)
	}
}

func TestColorGlyphBufferReuse(t *testing.T) {
	f := parseFixture(t, "colrv1.ttf")
	var b sfnt.Buffer
	first, err := f.LoadColorGlyph(&b, gidSquareA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("first call: got %d layers", len(first))
	}
	second, err := f.LoadColorGlyph(&b, gidSquareB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].GlyphIndex != gidDiamond {
		t.Fatalf("second call after buffer reuse: %+v", second)
	}
}

// TestColorGlyphConcurrent checks that Font methods remain safe to call
// concurrently when each goroutine uses its own Buffer, as the Font docs
// promise.
func TestColorGlyphConcurrent(t *testing.T) {
	f := parseFixture(t, "colrv1.ttf")
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			var b sfnt.Buffer
			for k := 0; k < 50; k++ {
				layers, err := f.LoadColorGlyph(&b, gidSquareA, nil)
				if err != nil {
					done <- err
					return
				}
				if len(layers) != 2 {
					done <- fmt.Errorf("got %d layers, want 2", len(layers))
					return
				}
				if _, err := f.ColorPalette(&b, 0); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent access: %v", err)
		}
	}
}

// TestColorGlyphSharedSubgraph verifies that a PaintColrGlyph reference can
// reuse another base glyph's paint graph without a spurious cycle error. In
// the fixture, gidCircle's paint is PaintColrGlyph(gidSquareB), so loading it
// walks gidSquareB's graph and yields gidSquareB's layer.
func TestColorGlyphSharedSubgraph(t *testing.T) {
	f := parseFixture(t, "colrv1.ttf")

	direct, err := f.LoadColorGlyph(nil, gidSquareB, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph(gidSquareB): %v", err)
	}
	shared, err := f.LoadColorGlyph(nil, gidCircle, nil)
	if err != nil {
		t.Fatalf("LoadColorGlyph(gidCircle) via PaintColrGlyph: %v", err)
	}
	if len(shared) != 1 || len(direct) != 1 || shared[0] != direct[0] {
		t.Fatalf("shared subgraph:\nshared %+v\ndirect %+v", shared, direct)
	}

	// Re-loading the shared graph repeatedly must keep succeeding: the
	// cycle-detection stack is unwound as each reference frame completes.
	for i := 0; i < 5; i++ {
		if _, err := f.LoadColorGlyph(nil, gidCircle, nil); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
	}
}

func TestColorGlyphCollection(t *testing.T) {
	c, err := sfnt.ParseCollection(readFixture(t, "colr.ttc"))
	if err != nil {
		t.Fatalf("ParseCollection: %v", err)
	}
	if c.NumFonts() != 2 {
		t.Fatalf("NumFonts: got %d, want 2", c.NumFonts())
	}

	// Font 0 is the COLR v0 fixture (4 layers for gidSquareA).
	f0, err := c.Font(0)
	if err != nil {
		t.Fatal(err)
	}
	l0, err := f0.LoadColorGlyph(nil, gidSquareA, nil)
	if err != nil || len(l0) != 4 {
		t.Fatalf("font 0 layers: %+v, %v", l0, err)
	}

	// Font 1 is the COLR v1 fixture (2 layers).
	f1, err := c.Font(1)
	if err != nil {
		t.Fatal(err)
	}
	l1, err := f1.LoadColorGlyph(nil, gidSquareA, nil)
	if err != nil || len(l1) != 2 {
		t.Fatalf("font 1 layers: %+v, %v", l1, err)
	}
}

// tableRange locates a table record in an SFNT font and returns the file
// offset and length of its data.
func tableRange(data []byte, wantTag string) (offset, length int, ok bool) {
	numTables := int(binary.BigEndian.Uint16(data[4:6]))
	for i := 0; i < numTables; i++ {
		rec := data[12+16*i : 12+16*(i+1)]
		if string(rec[0:4]) == wantTag {
			return int(binary.BigEndian.Uint32(rec[8:12])), int(binary.BigEndian.Uint32(rec[12:16])), true
		}
	}
	return 0, 0, false
}

func TestColorMalformedTables(t *testing.T) {
	// Unknown COLR version.
	data := readFixture(t, "colrv0.ttf")
	bad := append([]byte(nil), data...)
	off, _, ok := tableRange(bad, "COLR")
	if !ok {
		t.Fatal("COLR table not found")
	}
	binary.BigEndian.PutUint16(bad[off:], 99)
	if f, err := sfnt.Parse(bad); err != nil {
		t.Fatalf("Parse: %v", err)
	} else if _, err := f.LoadColorGlyph(nil, gidSquareA, nil); err == nil {
		t.Fatal("unknown COLR version: expected error, got nil")
	}

	// CPAL with zero palette entries.
	data = readFixture(t, "colrv0.ttf")
	bad = append([]byte(nil), data...)
	cpalOff, _, ok := tableRange(bad, "CPAL")
	if !ok {
		t.Fatal("CPAL table not found")
	}
	binary.BigEndian.PutUint16(bad[cpalOff+2:], 0)
	if f, err := sfnt.Parse(bad); err != nil {
		t.Fatalf("Parse: %v", err)
	} else if _, err := f.NumColorPalettes(nil); err == nil {
		t.Fatal("zero-entry CPAL: expected error, got nil")
	}

	// COLR v0 layer record offset pointing outside the table.
	data = readFixture(t, "colrv0.ttf")
	bad = append([]byte(nil), data...)
	colrOff, colrLen, ok := tableRange(bad, "COLR")
	if !ok {
		t.Fatal("COLR table not found")
	}
	binary.BigEndian.PutUint32(bad[colrOff+8:], uint32(colrLen)+100)
	if f, err := sfnt.Parse(bad); err != nil {
		t.Fatalf("Parse: %v", err)
	} else if _, err := f.LoadColorGlyph(nil, gidSquareA, nil); err == nil {
		t.Fatal("out-of-table layer offset: expected error, got nil")
	}

	// Truncated font: chop the file in half so table views run past the end.
	data = readFixture(t, "colrv1.ttf")
	if f, err := sfnt.Parse(data[:len(data)/2]); err != nil {
		// A parse error is an acceptable outcome.
	} else if _, err := f.LoadColorGlyph(nil, gidSquareA, nil); err == nil {
		t.Fatal("truncated font: expected error, got nil")
	}
}
