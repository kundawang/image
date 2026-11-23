// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ignore

package main

import (
	"encoding/binary"
)

// COLR v1 paint format identifiers (matches the OpenType spec).
const (
	v1PaintColrLayers = 1
	v1PaintSolid      = 2
	v1PaintVarSolid   = 3
	v1PaintGlyph      = 4
	v1PaintColrGlyph  = 11
	// v1PaintUnsupported is a format number this implementation deliberately
	// does not handle (PaintScaleUniform is format 28).
	v1PaintUnsupported = 28
)

// paintWriter assembles a contiguous block of Paint tables and LayerLists,
// tracking each item's offset relative to the block start (the BaseGlyphList).
// Offsets inside an emitted Paint are filled in by the caller, since their
// reference base differs: a Paint's child offset is relative to that Paint,
// while a LayerList's entries are relative to the LayerList.
type paintWriter struct {
	data []byte
}

// write appends b, padding to 4-byte alignment, and returns its offset.
func (pw *paintWriter) write(b []byte) int {
	off := len(pw.data)
	pw.data = append(pw.data, b...)
	for len(pw.data)&3 != 0 {
		pw.data = append(pw.data, 0)
	}
	return off
}

func solidPaint(colorIndex uint16, alphaF2D14 uint16) []byte {
	b := make([]byte, 8)
	b[0] = v1PaintSolid
	b[1] = 0
	put16(b[2:], 0) // reserved paintOffset
	put16(b[4:], colorIndex)
	put16(b[6:], alphaF2D14)
	return b
}

func varSolidPaint(colorIndex uint16, alphaF2D14, varColorIndex uint16) []byte {
	b := make([]byte, 12)
	b[0] = v1PaintVarSolid
	b[1] = 0
	put16(b[2:], 0)
	put16(b[4:], colorIndex)
	put16(b[6:], alphaF2D14)
	put16(b[10:], varColorIndex)
	return b
}

// glyphPaint wraps child with PaintGlyph for gid. childRelOff is the child
// paint's offset relative to this PaintGlyph table.
func glyphPaint(gid uint16, childRelOff uint16) []byte {
	b := make([]byte, 6)
	b[0] = v1PaintGlyph
	b[1] = 0
	put16(b[2:], childRelOff)
	put16(b[4:], gid)
	return b
}

func colrGlyphPaint(baseGID uint16) []byte {
	b := make([]byte, 4)
	b[0] = v1PaintColrGlyph
	b[1] = 0
	put16(b[2:], baseGID)
	return b
}

func layerList(children []uint32) []byte {
	b := []byte{byte(len(children))}
	for _, c := range children {
		b = binary.BigEndian.AppendUint32(b, c)
	}
	return b
}

func colrLayersPaint(numLayers uint8, layerListRelOff uint16) []byte {
	return []byte{v1PaintColrLayers, numLayers, byte(layerListRelOff >> 8), byte(layerListRelOff)}
}

func unsupportedPaint() []byte {
	// PaintScaleUniform (28): format, then scale F2Dot14 at offset 2.
	b := make([]byte, 6)
	b[0] = v1PaintUnsupported
	put16(b[2:], 0x4000) // scale = 1.0
	return b
}

// buildV1Font builds a COLR v1 font exercising solid paints, PaintGlyph,
// PaintColrLayers, PaintColrGlyph, PaintVarSolid, a cycle, a deep chain and an
// unsupported paint format.
func buildV1Font() []byte {
	fb := newFontBuilder()
	fb.buildOutlineTables()

	pal0 := []cpalColor{
		{0x00, 0x00, 0xFF, 0xFF}, // 0 red
		{0x00, 0xFF, 0x00, 0xFF}, // 1 green
		{0xFF, 0x00, 0x00, 0xFF}, // 2 blue
		{0x00, 0x00, 0xFF, 0x80}, // 3 semi red
		{0x00, 0x00, 0xFF, 0x00}, // 4 transparent
	}
	fb.buildCPAL(0, [][]cpalColor{pal0})

	// Layout plan. COLR paint offsets are unsigned Offset16/Offset24 values,
	// so every child table is emitted after its parent and the offsets are
	// patched forward once both ends are placed.
	//
	// Base glyphs:
	//   gidSquareA: PaintColrLayers { PaintGlyph(gidSquareB, solid red),
	//                                 PaintGlyph(gidCircle, varSolid green 0.5) }
	//   gidSquareB: PaintGlyph(gidDiamond, solid blue)
	//   gidCircle:  PaintColrGlyph(gidSquareB)   (shared subgraph)
	//   gidDiamond: PaintGlyph(gidSquareA, unsupported paint)
	//   gidLayerEmpty: PaintColrLayers {} (empty)
	//   gidDeepChain: self-referential PaintColrGlyph (cycle)
	//   gidOutOfRange: PaintGlyph(gid=60000, solid red) (bad glyph id)
	pw := &paintWriter{}

	// Reserve space for the BaseGlyphList header (u32 count) and records
	// (6 bytes each, sorted by glyph id). The paint graph begins right after.
	baseGlyphs := []uint16{gidSquareA, gidSquareB, gidCircle, gidDiamond, gidLayerEmpty, gidDeepChain, gidOutOfRange}
	numBase := len(baseGlyphs)
	listHeaderSize := 4 + 6*numBase
	pw.data = make([]byte, listHeaderSize)

	// --- gidSquareA's graph, parent first ---
	squareAPaintOff := pw.write(colrLayersPaint(2, 0))
	lListOff := pw.write(layerList([]uint32{0, 0}))
	redWrapOff := pw.write(glyphPaint(gidSquareB, 0))
	redChildOff := pw.write(solidPaint(colorRed, 1<<14))
	greenWrapOff := pw.write(glyphPaint(gidCircle, 0))
	greenChildOff := pw.write(varSolidPaint(colorGreen, 1<<13, 0)) // alpha 0.5

	// Patch: the ColrLayers paint points at its LayerList (relative to the
	// paint), LayerList entries point at the wraps (relative to the list),
	// and each wrap points at its solid child (relative to the wrap).
	patchU16(pw.data, squareAPaintOff+2, uint16(lListOff-squareAPaintOff))
	put32(pw.data[lListOff+1:], uint32(redWrapOff-lListOff))
	put32(pw.data[lListOff+5:], uint32(greenWrapOff-lListOff))
	patchU16(pw.data, redWrapOff+2, uint16(redChildOff-redWrapOff))
	patchU16(pw.data, greenWrapOff+2, uint16(greenChildOff-greenWrapOff))

	// --- gidSquareB: PaintGlyph(gidDiamond, solid blue), parent first ---
	blueWrapOff := pw.write(glyphPaint(gidDiamond, 0))
	blueChildOff := pw.write(solidPaint(colorBlue, 1<<14))
	patchU16(pw.data, blueWrapOff+2, uint16(blueChildOff-blueWrapOff))
	squareBPaintOff := blueWrapOff

	// --- gidOutOfRange: PaintGlyph with an invalid glyph id ---
	badWrapOff := pw.write(glyphPaint(60000, 0))
	badChildOff := pw.write(solidPaint(colorRed, 1<<14))
	patchU16(pw.data, badWrapOff+2, uint16(badChildOff-badWrapOff))
	outOfRangePaintOff := badWrapOff

	// --- gidDiamond: PaintGlyph(gidSquareA, unsupported) ---
	unsupWrapOff := pw.write(glyphPaint(gidSquareA, 0))
	unsupOff := pw.write(unsupportedPaint())
	patchU16(pw.data, unsupWrapOff+2, uint16(unsupOff-unsupWrapOff))
	diamondPaintOff := unsupWrapOff

	// --- gidCircle: PaintColrGlyph(gidSquareB); no offset to patch ---
	circlePaintOff := pw.write(colrGlyphPaint(gidSquareB))

	// --- gidLayerEmpty: empty PaintColrLayers ---
	emptyPaintOff := pw.write(colrLayersPaint(0, 0))

	// --- gidDeepChain: self-referential PaintColrGlyph (cycle) ---
	cyclePaintOff := pw.write(colrGlyphPaint(gidDeepChain))

	// Fill in the BaseGlyphList header and records.
	binary.BigEndian.PutUint32(pw.data[0:], uint32(numBase))
	paintOffsets := map[uint16]int{
		gidSquareA:    squareAPaintOff,
		gidSquareB:    squareBPaintOff,
		gidCircle:     circlePaintOff,
		gidDiamond:    diamondPaintOff,
		gidLayerEmpty: emptyPaintOff,
		gidDeepChain:  cyclePaintOff,
		gidOutOfRange: outOfRangePaintOff,
	}
	for i, gid := range baseGlyphs {
		rec := 4 + 6*i
		put16(pw.data[rec:], gid)
		put32(pw.data[rec+2:], uint32(paintOffsets[gid]))
	}

	// Also build a v0 BaseGlyphRecord array so the table is valid for v0-only
	// consumers. Include one record (gidSquareA) pointing at a single layer.
	v0Layers := [][2]uint16{{gidSquareB, colorRed}}
	colr := assembleCOLRv1(pw.data, baseGlyphs, v0Layers)
	fb.tables[tag("COLR")] = colr
	return fb.serialize()
}

func patchU16(b []byte, off int, v uint16) { binary.BigEndian.PutUint16(b[off:], v) }

// buildV1DeepFont builds a COLR v1 font whose base glyph is a chain of nested
// PaintGlyph tables deeper than the parser's maxColorPaintDepth limit. The
// chain is acyclic: each wrap points at the next, terminating in a solid
// paint, so it exercises the depth guard rather than cycle detection.
func buildV1DeepFont() []byte {
	const chainLen = 10 // exceeds maxColorPaintDepth (8) in the parser.
	fb := newFontBuilder()
	fb.buildOutlineTables()
	fb.buildCPAL(0, [][]cpalColor{{
		{0x00, 0x00, 0xFF, 0xFF}, // 0 red
	}})

	pw := &paintWriter{}
	const numBase = 1
	pw.data = make([]byte, 4+6*numBase) // BaseGlyphList header + one record.

	// Emit the chain parent-first: chainLen wraps then the terminal solid.
	wrapOff := make([]int, chainLen)
	for i := 0; i < chainLen; i++ {
		wrapOff[i] = pw.write(glyphPaint(gidSquareA, 0))
	}
	solidOff := pw.write(solidPaint(colorRed, 1<<14))
	// Each wrap's child is the next wrap; the last wraps the solid.
	for i := 0; i < chainLen; i++ {
		next := solidOff
		if i+1 < chainLen {
			next = wrapOff[i+1]
		}
		patchU16(pw.data, wrapOff[i]+2, uint16(next-wrapOff[i]))
	}

	binary.BigEndian.PutUint32(pw.data[0:], numBase)
	put16(pw.data[4:], gidSquareA)
	put32(pw.data[6:], uint32(wrapOff[0]))

	colr := assembleCOLRv1(pw.data, []uint16{gidSquareA}, nil)
	fb.tables[tag("COLR")] = colr
	return fb.serialize()
}

// assembleCOLRv1 builds the full COLR v1 table around the BaseGlyphList blob.
// The v0-style BaseGlyphRecord/LayerRecord arrays are optional in a v1 table;
// this builder emits exactly one BaseGlyphRecord (for the first base glyph)
// backed by v0Layers. The v1 BaseGlyphList blob carries its own, independent
// record count.
func assembleCOLRv1(baseGlyphList []byte, baseGlyphs []uint16, v0Layers [][2]uint16) []byte {
	const numV0BaseRecords = 1
	const headerSize = 30
	baseRecordSize := 6 * numV0BaseRecords
	layerArraySize := 4 * len(v0Layers)
	baseGlyphRecOff := uint32(headerSize)
	layerArrayOff := baseGlyphRecOff + uint32(baseRecordSize)
	baseGlyphListOff := layerArrayOff + uint32(layerArraySize)
	// 4-byte align the BaseGlyphList.
	if baseGlyphListOff&3 != 0 {
		baseGlyphListOff = (baseGlyphListOff + 3) &^ 3
	}
	total := int(baseGlyphListOff) + len(baseGlyphList)
	colr := make([]byte, total)

	put16(colr[0:], 1)                      // version 1
	put16(colr[2:], numV0BaseRecords)       // numBaseGlyphRecords (v0 array)
	put32(colr[4:], baseGlyphRecOff)        // baseGlyphRecordOffset
	put32(colr[8:], layerArrayOff)          // layerRecordsOffset
	put16(colr[12:], uint16(len(v0Layers))) // numLayerRecords
	put32(colr[14:], 0)                     // clipListOffset
	put32(colr[18:], 0)                     // varIndexMapOffset
	put32(colr[22:], 0)                     // itemVariationStoreOffset
	put32(colr[26:], baseGlyphListOff)      // baseGlyphListOffset

	// A BaseGlyphRecord is three Offset16 values: glyphID, firstLayerIndex,
	// numLayers.
	rec := baseGlyphRecOff
	put16(colr[rec:], baseGlyphs[0])
	put16(colr[rec+2:], 0) // firstLayerIndex
	put16(colr[rec+4:], uint16(len(v0Layers)))
	for i, l := range v0Layers {
		off := layerArrayOff + uint32(4*i)
		put16(colr[off:], l[0])
		put16(colr[off+2:], l[1])
	}
	copy(colr[baseGlyphListOff:], baseGlyphList)
	return colr
}

// buildTTC wraps two fonts in a TrueType Collection. Each font's table
// directory is rewritten so its table offsets are absolute from the start of
// the collection file, which is the convention this sfnt package expects for
// non-dfont collections.
func buildTTC(fonts ...[]byte) []byte {
	numFonts := len(fonts)
	headerSize := 12 + 4*numFonts
	offset := (headerSize + 3) &^ 3
	fontOffsets := make([]uint32, numFonts)
	var blobs [][]byte
	for i, f := range fonts {
		fontOffsets[i] = uint32(offset)
		blobs = append(blobs, f)
		offset += len(f)
		offset = (offset + 3) &^ 3
	}
	out := make([]byte, offset)
	put32(out[0:], 0x74746366) // "ttcf"
	put32(out[4:], 0x00010000) // version
	put32(out[8:], uint32(numFonts))
	for i, o := range fontOffsets {
		put32(out[12+4*i:], o)
	}
	for i, f := range blobs {
		fontBase := int(fontOffsets[i])
		copy(out[fontBase:], f)
		// Rewrite each table record's offset (at record+8) to be file-absolute.
		numTables := int(binary.BigEndian.Uint16(f[4:6]))
		for j := 0; j < numTables; j++ {
			rec := fontBase + 12 + 16*j
			rel := binary.BigEndian.Uint32(out[rec+8 : rec+12])
			binary.BigEndian.PutUint32(out[rec+8:rec+12], rel+uint32(fontBase))
		}
	}
	return out
}
