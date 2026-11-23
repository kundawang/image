// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ignore

// colrtestdata-generator builds the minimal SFNT font fixtures used by the
// COLR/CPAL tests in the parent package. The fixtures are hand-built rather
// than produced by a font editor so that their byte layout, glyph IDs and
// color indices are fully deterministic.
//
// Run with:
//
//	go run gen.go
package main

import (
	"encoding/binary"
	"log"
	"os"
	"path/filepath"
)

const (
	numGlyphs = 8

	// Glyph IDs.
	gidNotdef     = 0
	gidSquareA    = 1
	gidSquareB    = 2
	gidCircle     = 3
	gidDiamond    = 4
	gidLayerEmpty = 5
	gidDeepChain  = 6
	gidOutOfRange = 7

	// CPAL color indices.
	colorRed         = 0
	colorGreen       = 1
	colorBlue        = 2
	colorSemiRed     = 3
	colorTransparent = 4
	colorForeground  = 0xFFFF

	// cmap code points (Macintosh Roman subset, cmap format 0).
	runeSquareA = 'A'
	runeSquareB = 'B'
	runeCircle  = 'C'
	runeDiamond = 'D'
	runeEmpty   = 'E'
	runeCycle   = 'Y'
	runeDeep    = 'Z'
	runeUnsupp  = 'U'
)

// fontBuilder assembles one minimal SFNT (TrueType) font.
type fontBuilder struct {
	tables map[uint32][]byte
}

func newFontBuilder() *fontBuilder {
	return &fontBuilder{tables: map[uint32][]byte{}}
}

func put16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func put32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

// simpleGlyf builds a glyf table entry for a simple 4-point rectangle with the
// given bounding box. All points are on-curve; no hints. Coordinates use
// explicit int16 deltas, matching the flag encodings the sfnt parser expects.
func simpleGlyf(xMin, yMin, xMax, yMax int16) []byte {
	// Header: numberOfContours (1), bounds.
	header := make([]byte, 10)
	put16(header[0:], 1)
	put16(header[2:], uint16(xMin))
	put16(header[4:], uint16(yMin))
	put16(header[6:], uint16(xMax))
	put16(header[8:], uint16(yMax))
	// endPtsOfContours[0] = 3.
	endPts := make([]byte, 2)
	put16(endPts, 3)
	// instructionLength = 0.
	instr := []byte{0, 0}
	// Four on-curve points. The first point carries explicit signed-short
	// coordinates; the remaining three carry explicit signed-short deltas
	// (flag = flagOnCurve only).
	flags := []byte{flagOnCurve, flagOnCurve, flagOnCurve, flagOnCurve}
	xs := make([]byte, 0, 8)
	ys := make([]byte, 0, 8)
	type pt struct{ x, y int16 }
	pts := []pt{
		{xMin, yMin},
		{xMax, yMin},
		{xMax, yMax},
		{xMin, yMax},
	}
	var prevX, prevY int16
	for _, p := range pts {
		xs = binary.BigEndian.AppendUint16(xs, uint16(p.x-prevX))
		ys = binary.BigEndian.AppendUint16(ys, uint16(p.y-prevY))
		prevX, prevY = p.x, p.y
	}
	out := append([]byte{}, header...)
	out = append(out, endPts...)
	out = append(out, instr...)
	out = append(out, flags...)
	out = append(out, xs...)
	out = append(out, ys...)
	return out
}

const (
	flagOnCurve              = 0x01
	flagXShortVector         = 0x02
	flagYShortVector         = 0x04
	flagPositiveXShortVector = 0x10
	flagPositiveYShortVector = 0x20
	// flagThisXIsSame / flagThisYIsSame share their bits with the positive
	// short-vector flags; see the TrueType glyf specification.
	flagThisXIsSame = flagPositiveXShortVector
	flagThisYIsSame = flagPositiveYShortVector
)

// emptyGlyf returns a zero-length glyf entry (used for gidLayerEmpty).
func emptyGlyf() []byte { return nil }

func (fb *fontBuilder) buildOutlineTables() {
	// glyf entries, 2-byte aligned. The loca table uses short offsets (each
	// offset is the byte offset divided by 2), so glyf must fit in 2*65535
	// bytes.
	glyfEntries := [numGlyphs][]byte{
		gidNotdef:     simpleGlyf(0, 0, 100, 100),
		gidSquareA:    simpleGlyf(0, 0, 400, 400),
		gidSquareB:    simpleGlyf(100, 100, 500, 500),
		gidCircle:     simpleGlyf(50, 50, 350, 350),
		gidDiamond:    simpleGlyf(0, 200, 400, 600),
		gidLayerEmpty: emptyGlyf(),
		gidDeepChain:  simpleGlyf(10, 10, 90, 90),
		gidOutOfRange: simpleGlyf(0, 0, 200, 200),
	}
	var glyf []byte
	loca := make([]byte, 2*(numGlyphs+1))
	for i, e := range glyfEntries {
		put16(loca[2*i:], uint16(len(glyf)/2))
		glyf = append(glyf, e...)
		if len(glyf)&1 != 0 {
			glyf = append(glyf, 0)
		}
	}
	put16(loca[2*numGlyphs:], uint16(len(glyf)/2))
	fb.tables[tag("glyf")] = glyf
	fb.tables[tag("loca")] = loca

	// head, 54 bytes. indexToLocFormat = 0 (short).
	head := make([]byte, 54)
	put32(head[0:], 0x00010000)  // version
	put32(head[4:], 0x00010000)  // fontRevision
	put32(head[12:], 0)          // checksumAdjustment (ignored)
	put32(head[16:], 0x5F0F3CF5) // magicNumber
	put16(head[20:], 0)          // flags
	put16(head[22:], 1000)       // unitsPerEm
	put16(head[36:], 0)          // xMin
	put16(head[38:], 0)          // yMin
	put16(head[40:], 500)        // xMax
	put16(head[42:], 600)        // yMax
	put16(head[44:], 0)          // macStyle
	put16(head[46:], 8)          // lowestRecPPEM
	put16(head[48:], 2)          // fontDirectionHint
	put16(head[50:], 0)          // indexToLocFormat: short
	put16(head[52:], 0)          // glyphDataFormat
	fb.tables[tag("head")] = head

	// maxp, 32 bytes (TrueType).
	maxp := make([]byte, 32)
	put32(maxp[0:], 0x00010000)
	put16(maxp[4:], numGlyphs)
	fb.tables[tag("maxp")] = maxp

	// hhea, 36 bytes. numHMetrics = numGlyphs.
	hhea := make([]byte, 36)
	put32(hhea[0:], 0x00010000)
	put16(hhea[4:], 800) // ascent
	put16(hhea[6:], 200) // descent (unsigned here; parser sign-extends)
	put16(hhea[34:], numGlyphs)
	fb.tables[tag("hhea")] = hhea

	// hmtx: one full metric per glyph (advance 600, lsb 0).
	hmtx := make([]byte, 4*numGlyphs)
	for i := 0; i < numGlyphs; i++ {
		put16(hmtx[4*i:], 600)
	}
	fb.tables[tag("hmtx")] = hmtx

	// post, 32 bytes, format 3.0 (no glyph names).
	post := make([]byte, 32)
	put32(post[0:], 0x00030000)
	put32(post[4:], 0) // italicAngle
	put16(post[12:], 0)
	put16(post[16:], 0)
	put32(post[20:], 0)
	put32(post[24:], 0)
	put32(post[28:], 0)
	fb.tables[tag("post")] = post

	// name: minimal 6-byte header, zero records.
	name := make([]byte, 6)
	put16(name[0:], 0)
	put16(name[2:], 0)
	put16(name[4:], 6)
	fb.tables[tag("name")] = name

	// OS/2, 68 bytes (version 0), so parseOS2 takes the legacy path.
	os2 := make([]byte, 68)
	put16(os2[0:], 0)
	fb.tables[tag("OS/2")] = os2

	fb.buildCmap()
}

func (fb *fontBuilder) buildCmap() {
	// cmap format 0 (Macintosh Roman), 262 bytes subtable.
	sub := make([]byte, 6+256)
	put16(sub[0:], 0)   // format
	put16(sub[2:], 262) // length
	put16(sub[4:], 0)   // language
	mapping := map[rune]uint8{
		runeSquareA: gidSquareA,
		runeSquareB: gidSquareB,
		runeCircle:  gidCircle,
		runeDiamond: gidDiamond,
		runeEmpty:   gidLayerEmpty,
		runeCycle:   gidSquareB, // cycle glyph base maps to gidSquareB's code path
		runeDeep:    gidDeepChain,
		runeUnsupp:  gidSquareA,
	}
	for r, g := range mapping {
		// All chosen runes are ASCII, where Macintosh Roman matches ASCII.
		if r < 0x80 {
			sub[6+int(r)] = g
		}
	}
	cmap := make([]byte, 4+8)
	put16(cmap[0:], 0)  // version
	put16(cmap[2:], 1)  // numTables
	put16(cmap[4:], 1)  // platformID: Macintosh
	put16(cmap[6:], 0)  // encodingID: Roman
	put32(cmap[8:], 12) // offset to subtable
	cmap = append(cmap, sub...)
	fb.tables[tag("cmap")] = cmap
}

// cpalColor is a BGRA record.
type cpalColor struct{ b, g, r, a uint8 }

// buildCPAL builds a CPAL table. The CPAL v0 header is 16 bytes; the v1
// header appends three 4-byte array offsets for 28 bytes total.
func (fb *fontBuilder) buildCPAL(version uint16, palettes [][]cpalColor) {
	numEntries := len(palettes[0])
	numPalettes := len(palettes)
	headerSize := 16
	if version == 1 {
		headerSize = 28
	}
	paletteArrayOff := uint32(headerSize)
	colorRecsOff := paletteArrayOff + 4*uint32(numPalettes)
	numColorRecs := numEntries * numPalettes
	total := int(colorRecsOff) + numColorRecs*4

	cpal := make([]byte, total)
	put16(cpal[0:], version)
	put16(cpal[2:], uint16(numEntries))
	put32(cpal[4:], colorRecsOff)    // colorRecordsArrayOffset
	put32(cpal[8:], paletteArrayOff) // paletteRecordsArrayOffset
	put16(cpal[12:], uint16(numPalettes))
	put16(cpal[14:], uint16(numColorRecs))
	if version == 1 {
		// paletteTypeArray, paletteLabelArray, colorLabelArray offsets = 0.
	}
	for i := 0; i < numPalettes; i++ {
		put32(cpal[paletteArrayOff+4*uint32(i):], uint32(i*numEntries))
	}
	off := colorRecsOff
	for _, pal := range palettes {
		for _, c := range pal {
			cpal[off+0] = c.b
			cpal[off+1] = c.g
			cpal[off+2] = c.r
			cpal[off+3] = c.a
			off += 4
		}
	}
	fb.tables[tag("CPAL")] = cpal
}

// buildCOLRv0 builds a version-0 COLR table.
func (fb *fontBuilder) buildCOLRv0(records []v0BaseRecord, layers [][2]uint16) {
	numBase := len(records)
	numLayers := len(layers)
	// The COLR v0 header is 14 bytes: version(2), numBaseGlyphRecords(2),
	// baseGlyphRecordOffset(4), layerRecordOffset(4), numLayerRecords(2).
	const headerLen = 14
	baseOff := uint32(headerLen)
	layerOff := baseOff + 6*uint32(numBase)
	total := int(layerOff) + 4*numLayers

	colr := make([]byte, total)
	put16(colr[0:], 0) // version 0
	put16(colr[2:], uint16(numBase))
	put32(colr[4:], baseOff)
	put32(colr[8:], layerOff)
	put16(colr[12:], uint16(numLayers))
	// A BaseGlyphRecord is three Offset16 values: glyphID, firstLayerIndex,
	// numLayers.
	for i, rec := range records {
		recOff := baseOff + 6*uint32(i)
		put16(colr[recOff:], rec.glyph)
		put16(colr[recOff+2:], uint16(rec.firstLayer))
		put16(colr[recOff+4:], uint16(rec.numLayers))
	}
	for i, l := range layers {
		lOff := layerOff + 4*uint32(i)
		put16(colr[lOff:], l[0])
		put16(colr[lOff+2:], l[1])
	}
	fb.tables[tag("COLR")] = colr
}

type v0BaseRecord struct {
	glyph      uint16
	firstLayer uint32
	numLayers  uint32
}

// serialize emits the SFNT binary with sorted table records and 4-byte aligned
// table data.
func (fb *fontBuilder) serialize() []byte {
	var tags []uint32
	for t := range fb.tables {
		tags = append(tags, t)
	}
	// Sort ascending (simple insertion sort; few tables).
	for i := 1; i < len(tags); i++ {
		for j := i; j > 0 && tags[j-1] > tags[j]; j-- {
			tags[j-1], tags[j] = tags[j], tags[j-1]
		}
	}
	numTables := len(tags)
	headerLen := 12 + 16*numTables
	offset := (headerLen + 3) &^ 3
	type placement struct {
		tag    uint32
		offset uint32
		data   []byte
	}
	placements := make([]placement, 0, numTables)
	for _, t := range tags {
		data := fb.tables[t]
		placements = append(placements, placement{t, uint32(offset), data})
		offset += len(data)
		offset = (offset + 3) &^ 3
	}
	out := make([]byte, offset)
	// Offset table.
	put32(out[0:], 0x00010000)
	put16(out[4:], uint16(numTables))
	// searchRange, entrySelector, rangeShift values (not validated by parser).
	put16(out[6:], 0)
	put16(out[8:], 0)
	put16(out[10:], 0)
	for i, p := range placements {
		rec := 12 + 16*i
		put32(out[rec:], p.tag)
		put32(out[rec+4:], 0) // checksum ignored
		put32(out[rec+8:], p.offset)
		put32(out[rec+12:], uint32(len(p.data)))
		copy(out[p.offset:], p.data)
	}
	return out
}

func tag(s string) uint32 { return binary.BigEndian.Uint32([]byte(s)) }

// buildV0Font builds the COLR v0 + CPAL v1 fixture.
func buildV0Font() []byte {
	fb := newFontBuilder()
	fb.buildOutlineTables()

	// Two palettes, 5 entries each.
	pal0 := []cpalColor{
		{0x00, 0x00, 0xFF, 0xFF}, // 0 red
		{0x00, 0xFF, 0x00, 0xFF}, // 1 green
		{0xFF, 0x00, 0x00, 0xFF}, // 2 blue
		{0x00, 0x00, 0xFF, 0x80}, // 3 semi-transparent red
		{0x00, 0x00, 0xFF, 0x00}, // 4 fully transparent red
	}
	pal1 := []cpalColor{
		{0xFF, 0xFF, 0xFF, 0xFF}, // 0 white
		{0x00, 0x00, 0x00, 0xFF}, // 1 black
		{0x00, 0xFF, 0xFF, 0xFF}, // 2 yellow
		{0x00, 0xFF, 0xFF, 0x80}, // 3 semi-transparent yellow
		{0x00, 0xFF, 0xFF, 0x00}, // 4 transparent yellow
	}
	fb.buildCPAL(1, [][]cpalColor{pal0, pal1})

	// Layers (bottom to top within each base glyph).
	layers := [][2]uint16{
		{gidSquareA, colorRed},         // 0: base A layer 0
		{gidSquareB, colorGreen},       // 1: base A layer 1
		{gidCircle, colorSemiRed},      // 2: base A layer 2 (alpha 0x80)
		{gidDiamond, colorTransparent}, // 3: base A layer 3 (alpha 0)
		{gidSquareA, colorForeground},  // 4: base B layer 0 (foreground)
		{gidCircle, 600},               // 5: base B layer 1 (missing color index)
		// base gidLayerEmpty has zero layers.
	}
	records := []v0BaseRecord{
		{glyph: gidSquareA, firstLayer: 0, numLayers: 4},
		{glyph: gidSquareB, firstLayer: 4, numLayers: 2},
		{glyph: gidLayerEmpty, firstLayer: 6, numLayers: 0},
	}
	fb.buildCOLRv0(records, layers)
	return fb.serialize()
}

func main() {
	outDir := os.Args[1]
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	v0 := buildV0Font()
	if err := os.WriteFile(filepath.Join(outDir, "colrv0.ttf"), v0, 0o644); err != nil {
		log.Fatal(err)
	}

	v1 := buildV1Font()
	if err := os.WriteFile(filepath.Join(outDir, "colrv1.ttf"), v1, 0o644); err != nil {
		log.Fatal(err)
	}

	ttc := buildTTC(v0, v1)
	if err := os.WriteFile(filepath.Join(outDir, "colr.ttc"), ttc, 0o644); err != nil {
		log.Fatal(err)
	}

	deep := buildV1DeepFont()
	if err := os.WriteFile(filepath.Join(outDir, "colrv1-deep.ttf"), deep, 0o644); err != nil {
		log.Fatal(err)
	}
}
