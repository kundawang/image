// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sfnt

// Color glyph (COLR/CPAL) support.
//
// The wire formats are documented at:
//	https://learn.microsoft.com/en-us/typography/opentype/spec/colr
//	https://learn.microsoft.com/en-us/typography/opentype/spec/cpal

import (
	"errors"
	"image/color"
)

// These constants are not part of the specifications, but are limitations used
// by this implementation to defend against malicious font files.
const (
	// maxColorLayers bounds the number of ColorGlyphLayers emitted for a single
	// glyph, including layers reached through nested COLR v1 PaintColrLayers /
	// PaintColrGlyph references. For reference, the COLR v0 BaseGlyphRecord
	// count is a uint32 and the LayerRecord count is a uint32.
	maxColorLayers = 4096

	// maxColorPaletteEntries bounds the number of color records in a single
	// CPAL palette.
	maxColorPaletteEntries = 65536

	// maxColorPalettes bounds the number of palettes in a CPAL table.
	maxColorPalettes = 1024

	// maxColorPaintDepth bounds the nesting depth of the COLR v1 paint graph
	// and the number of PaintColrGlyph (base glyph) references followed.
	maxColorPaintDepth = 8

	// colorPaletteEntrySize is the size in bytes of one CPAL color record.
	colorPaletteEntrySize = 4
)

var (
	// ErrColorCycle indicates that resolving a colored glyph's paint graph
	// required following a COLR PaintColrGlyph reference to a base glyph that
	// is already being resolved, i.e. the paint graph contains a cycle.
	ErrColorCycle = errors.New("sfnt: color glyph paint graph contains a cycle")

	errColorPaintTooDeep         = errors.New("sfnt: unsupported COLR paint nesting depth")
	errInvalidColorLayer         = errors.New("sfnt: invalid COLR layer")
	errInvalidCOLRTable          = errors.New("sfnt: invalid COLR table")
	errInvalidCPALTable          = errors.New("sfnt: invalid CPAL table")
	errUnsupportedCOLRTable      = errors.New("sfnt: unsupported COLR table")
	errUnsupportedColorPaint     = errors.New("sfnt: unsupported COLR paint format")
	errUnsupportedNumberOfLayers = errors.New("sfnt: unsupported number of color layers")
)

// COLR table paint format identifiers handled by this implementation. The
// COLR v1 specification defines additional paint formats (linear/radial
// sweeps, transforms, composites, scales) which are rejected with
// errUnsupportedColorPaint rather than mis-decoded.
const (
	paintFormatColrLayers = 1
	paintFormatSolid      = 2
	paintFormatVarSolid   = 3
	paintFormatGlyph      = 4
	paintFormatColrGlyph  = 11
)

// ColorGlyphLayer is one layer of a colored glyph. Layers are ordered bottom
// to top: the first layer is painted first and subsequent layers are painted
// on top of it.
//
// Each layer references another glyph (the layer's outline, resolvable through
// Font.LoadGlyph) and a palette color combined with Alpha.
//
// ColorGlyphLayer values returned by Font.LoadColorGlyph are owned by the
// *Buffer passed to that method and must be treated as read-only. They become
// invalid to use once the Buffer is re-used by another Font method call.
type ColorGlyphLayer struct {
	// GlyphIndex is the index of the glyph providing this layer's vector
	// outline.
	GlyphIndex GlyphIndex

	// PaletteIndex is the index of the color in the selected CPAL palette, or
	// 0xFFFF for the palette's foreground color.
	PaletteIndex uint16

	// Color is the fully resolved RGBA color, applying the CPAL palette entry
	// (or the foreground color for PaletteIndex 0xFFFF) and Alpha.
	Color color.RGBA

	// Alpha is the opacity of the layer in the range [0, 255]. It combines the
	// CPAL palette entry's alpha and any COLR v1 paint alpha. A value of 0
	// means the layer is fully transparent.
	Alpha uint8
}

// LoadColorGlyphOptions are the options to the Font.LoadColorGlyph method.
type LoadColorGlyphOptions struct {
	// PaletteIndex selects one of the font's CPAL palettes. If it is out of
	// range, palette 0 is used. It is ignored if the font has no CPAL table.
	PaletteIndex int

	// Foreground is the color used for COLR layers whose color index is
	// 0xFFFF (the CPAL foreground color). The zero value, whose Alpha is 0, is
	// replaced by opaque black (RGBA{0, 0, 0, 255}).
	Foreground color.RGBA
}

// HasColorGlyph reports whether the font contains a COLR table and whether the
// glyph x has a colored (layered) representation. A false result with a nil
// error means that the glyph should be rendered through Font.LoadGlyph.
//
// It returns ErrNotFound if the glyph index is out of range.
func (f *Font) HasColorGlyph(b *Buffer, x GlyphIndex) (bool, error) {
	if int(x) >= f.NumGlyphs() {
		return false, ErrNotFound
	}
	if f.colr.length == 0 {
		return false, nil
	}
	if b == nil {
		b = &Buffer{}
	}
	version, _, err := f.parseCOLRHeader(b)
	if err != nil {
		return false, err
	}
	if version >= 1 {
		if off, _, err := f.colrV1BaseGlyphPaintOffset(b, x); err != nil {
			return false, err
		} else if off != 0 {
			return true, nil
		}
	}
	off, _, err := f.colrV0BaseGlyphRange(b, x)
	if err != nil {
		return false, err
	}
	return off != 0, nil
}

// NumColorPalettes returns the number of color palettes in the font's CPAL
// table. It returns 0 if the font has no CPAL table.
func (f *Font) NumColorPalettes(b *Buffer) (int, error) {
	if f.cpal.length == 0 {
		return 0, nil
	}
	if b == nil {
		b = &Buffer{}
	}
	hdr, err := f.parseCPALHeader(b)
	if err != nil {
		return 0, err
	}
	return hdr.numPalettes, nil
}

// ColorPalette returns a copy of the i'th CPAL palette. The returned slice is
// owned by the caller and may be modified.
//
// If i is out of range, palette 0 is returned instead. It returns
// ErrNotFound if the font has no CPAL table, or if neither the requested
// palette nor palette 0 contains any colors.
func (f *Font) ColorPalette(b *Buffer, i int) ([]color.RGBA, error) {
	if f.cpal.length == 0 {
		return nil, ErrNotFound
	}
	if b == nil {
		b = &Buffer{}
	}
	hdr, err := f.parseCPALHeader(b)
	if err != nil {
		return nil, err
	}
	if i < 0 || i >= hdr.numPalettes {
		i = 0
	}
	return f.loadCPALPalette(b, hdr, i)
}

// LoadColorGlyph returns the ordered color layers of the x'th glyph. The
// layers are ordered bottom to top.
//
// If b is non-nil, the returned slice becomes invalid to use once b is
// re-used. The returned ColorGlyphLayer values must be treated as read-only;
// copy them if a mutable or longer-lived representation is needed.
//
// It returns ErrNotFound if the glyph index is out of range or if the glyph
// has no colored representation (in which case Font.LoadGlyph should be used).
// It returns ErrColorCycle if the COLR v1 paint graph contains a cycle,
// errUnsupportedColorPaint (or a wrapping error) if the paint graph uses a
// paint format that this implementation does not support, and
// errColorPaintTooDeep if the paint graph nests more deeply than the
// implementation's resource limit.
func (f *Font) LoadColorGlyph(b *Buffer, x GlyphIndex, opts *LoadColorGlyphOptions) ([]ColorGlyphLayer, error) {
	if int(x) >= f.NumGlyphs() {
		return nil, ErrNotFound
	}
	if f.colr.length == 0 {
		return nil, ErrNotFound
	}
	if b == nil {
		b = &Buffer{}
	}
	b.colorLayers = b.colorLayers[:0]

	paletteIndex := 0
	foreground := color.RGBA{R: 0, G: 0, B: 0, A: 255}
	if opts != nil {
		paletteIndex = opts.PaletteIndex
		foreground = opts.Foreground
		if foreground.A == 0 {
			foreground = color.RGBA{R: 0, G: 0, B: 0, A: 255}
		}
	}

	var hdr cpalHeader
	if f.cpal.length != 0 {
		var err error
		hdr, err = f.parseCPALHeader(b)
		if err != nil {
			return nil, err
		}
		if paletteIndex < 0 || paletteIndex >= hdr.numPalettes {
			paletteIndex = 0
		}
	}

	version, _, err := f.parseCOLRHeader(b)
	if err != nil {
		return nil, err
	}

	// COLR v1 (and v2) takes precedence when the glyph is in the
	// BaseGlyphList; otherwise fall back to the v0 BaseGlyphRecord array.
	if version >= 1 {
		paintOffset, _, err := f.colrV1BaseGlyphPaintOffset(b, x)
		if err != nil {
			return nil, err
		}
		if paintOffset != 0 {
			walker := colorPaintWalker{
				f:            f,
				b:            b,
				cpal:         hdr,
				paletteIndex: paletteIndex,
				foreground:   foreground,
				currentGlyph: x,
			}
			if err := walker.run(paintOffset); err != nil {
				return nil, err
			}
			// A glyph whose paint graph contains only empty layer lists
			// legitimately produces no layers; return the empty slice rather
			// than ErrNotFound so callers can distinguish it from a glyph with
			// no color representation at all.
			return b.colorLayers, nil
		}
	}

	rangeOffset, numLayers, err := f.colrV0BaseGlyphRange(b, x)
	if err != nil {
		return nil, err
	}
	if rangeOffset == 0 {
		return nil, ErrNotFound
	}
	if numLayers > maxColorLayers {
		return nil, errUnsupportedNumberOfLayers
	}
	// A LayerRecord is two Offset16 values: glyphID and paletteIndex.
	for i := 0; i < numLayers; i++ {
		rec, err := b.view(&f.src, int(f.colr.offset)+rangeOffset+4*i, 4)
		if err != nil {
			return nil, err
		}
		glyph := GlyphIndex(u16(rec))
		palIdx := u16(rec[2:])
		if int(glyph) >= f.NumGlyphs() {
			return nil, errInvalidColorLayer
		}
		c, alpha, err := f.resolveColor(b, hdr, paletteIndex, palIdx, 1<<14, foreground)
		if err != nil {
			return nil, err
		}
		b.colorLayers = append(b.colorLayers, ColorGlyphLayer{
			GlyphIndex:   glyph,
			PaletteIndex: palIdx,
			Color:        c,
			Alpha:        alpha,
		})
	}
	return b.colorLayers, nil
}

// cpalHeader holds the parsed fields of a CPAL table header.
type cpalHeader struct {
	version      uint16
	numEntries   int    // colors per palette (numPaletteEntries)
	paletteFirst uint32 // paletteRecordsArrayOffset
	numPalettes  int    // numPaletteRecords
	numColorRecs int    // numColorRecords
	colorRecsOff uint32 // colorRecordsArrayOffset, relative to the CPAL table
}

// parseCPALHeader parses and validates the CPAL table header.
func (f *Font) parseCPALHeader(b *Buffer) (cpalHeader, error) {
	const headerSizeV0 = 16
	const headerSizeV1 = 28
	if f.cpal.length < headerSizeV0 {
		return cpalHeader{}, errInvalidCPALTable
	}
	buf, err := b.view(&f.src, int(f.cpal.offset), headerSizeV0)
	if err != nil {
		return cpalHeader{}, err
	}
	hdr := cpalHeader{
		version:      u16(buf),
		numEntries:   int(u16(buf[2:])),
		colorRecsOff: u32(buf[4:]),
		paletteFirst: u32(buf[8:]),
		numPalettes:  int(u16(buf[12:])),
		numColorRecs: int(u16(buf[14:])),
	}
	if hdr.version > 1 {
		return cpalHeader{}, errUnsupportedCOLRTable
	}
	if hdr.numEntries == 0 || hdr.numEntries > maxColorPaletteEntries {
		return cpalHeader{}, errInvalidCPALTable
	}
	if hdr.numPalettes == 0 || hdr.numPalettes > maxColorPalettes {
		return cpalHeader{}, errInvalidCPALTable
	}
	// numColorRecords is the size of the shared color records array; each
	// palette indexes numEntries consecutive records into it.
	if hdr.numColorRecs < hdr.numEntries || hdr.numColorRecs > maxColorPaletteEntries {
		return cpalHeader{}, errInvalidCPALTable
	}
	if hdr.version == 1 {
		if f.cpal.length < headerSizeV1 {
			return cpalHeader{}, errInvalidCPALTable
		}
		// The v1-only fields (paletteTypeArray, paletteLabelArray,
		// colorLabelArray offsets) are read but not needed for color
		// resolution; their offsets must still fit inside the table.
		extra, err := b.view(&f.src, int(f.cpal.offset)+headerSizeV0, headerSizeV1-headerSizeV0)
		if err != nil {
			return cpalHeader{}, err
		}
		for i := 0; i < 3; i++ {
			if off := u32(extra[4*i:]); off != 0 && int(off) >= int(f.cpal.length) {
				return cpalHeader{}, errInvalidCPALTable
			}
		}
	}
	if hdr.colorRecsOff >= f.cpal.length {
		return cpalHeader{}, errInvalidCPALTable
	}
	if uint64(hdr.colorRecsOff)+uint64(hdr.numColorRecs)*colorPaletteEntrySize > uint64(f.cpal.length) {
		return cpalHeader{}, errInvalidCPALTable
	}
	// The palette index array has numPalettes uint32 entries starting at
	// paletteFirst.
	if hdr.paletteFirst >= f.cpal.length {
		return cpalHeader{}, errInvalidCPALTable
	}
	if uint64(hdr.paletteFirst)+uint64(hdr.numPalettes)*4 > uint64(f.cpal.length) {
		return cpalHeader{}, errInvalidCPALTable
	}
	return hdr, nil
}

// loadCPALPalette returns a copy of the i'th palette.
func (f *Font) loadCPALPalette(b *Buffer, hdr cpalHeader, i int) ([]color.RGBA, error) {
	first, err := b.view(&f.src, int(f.cpal.offset)+int(hdr.paletteFirst)+4*i, 4)
	if err != nil {
		return nil, err
	}
	start := u32(first)
	if uint64(start)+uint64(hdr.numEntries)*colorPaletteEntrySize > uint64(f.cpal.length) {
		return nil, errInvalidCPALTable
	}
	recs, err := b.view(&f.src, int(f.cpal.offset)+int(hdr.colorRecsOff)+int(start)*colorPaletteEntrySize,
		hdr.numEntries*colorPaletteEntrySize)
	if err != nil {
		return nil, err
	}
	out := make([]color.RGBA, hdr.numEntries)
	for j := range out {
		// CPAL color records are stored as blue, green, red, alpha.
		out[j] = color.RGBA{
			B: recs[4*j+0],
			G: recs[4*j+1],
			R: recs[4*j+2],
			A: recs[4*j+3],
		}
	}
	return out, nil
}

// resolveColor resolves a CPAL color index to an RGBA color, applying the
// paint alpha (a 2.14 fixed point value in [0, 16384]). colorIndex 0xFFFF
// selects the foreground color. The returned alpha is the palette entry alpha
// scaled by paintAlpha.
func (f *Font) resolveColor(b *Buffer, hdr cpalHeader, paletteIndex int, colorIndex uint16, paintAlpha int32, foreground color.RGBA) (color.RGBA, uint8, error) {
	if paintAlpha < 0 {
		paintAlpha = 0
	}
	if paintAlpha > 1<<14 {
		paintAlpha = 1 << 14
	}

	var base color.RGBA
	switch {
	case colorIndex == 0xFFFF:
		base = foreground
	case f.cpal.length == 0:
		// A COLR table without a CPAL table can only name the foreground
		// color. A concrete index is unresolvable, so fall back to the
		// foreground color rather than dropping the layer.
		base = foreground
	case int(colorIndex) >= hdr.numEntries:
		// Out-of-range palette index: use the foreground color as the stable
		// fallback instead of failing the whole glyph.
		base = foreground
	default:
		first, err := b.view(&f.src, int(f.cpal.offset)+int(hdr.paletteFirst)+4*paletteIndex, 4)
		if err != nil {
			return color.RGBA{}, 0, err
		}
		start := u32(first)
		off := int(hdr.colorRecsOff) + int(start)*colorPaletteEntrySize + int(colorIndex)*colorPaletteEntrySize
		if off+colorPaletteEntrySize > int(f.cpal.length) {
			return color.RGBA{}, 0, errInvalidCPALTable
		}
		rec, err := b.view(&f.src, int(f.cpal.offset)+off, colorPaletteEntrySize)
		if err != nil {
			return color.RGBA{}, 0, err
		}
		base = color.RGBA{B: rec[0], G: rec[1], R: rec[2], A: rec[3]}
	}

	// Scale the palette alpha by the paint alpha. Round to nearest with the
	// standard (x + half)/scale formula.
	alpha := uint8((int32(base.A)*paintAlpha + (1 << 13)) >> 14)
	c := base
	c.A = alpha
	return c, alpha, nil
}

// parseCOLRHeader validates the COLR table and returns its version and the
// size of the version-specific header.
func (f *Font) parseCOLRHeader(b *Buffer) (version uint16, headerLength int, err error) {
	const headerSizeV0 = 14
	if f.colr.length < headerSizeV0 {
		return 0, 0, errInvalidCOLRTable
	}
	buf, err := b.view(&f.src, int(f.colr.offset), headerSizeV0)
	if err != nil {
		return 0, 0, err
	}
	version = u16(buf)
	switch version {
	case 0:
		return 0, headerSizeV0, nil
	case 1, 2:
		// The COLR v1 header appends four Offset32 fields (clip list, variation
		// index map, item variation store and base glyph list) to the 14-byte
		// v0 header, for 30 bytes total.
		const headerSizeV1 = 30
		if f.colr.length < headerSizeV1 {
			return 0, 0, errInvalidCOLRTable
		}
		return version, headerSizeV1, nil
	default:
		return 0, 0, errUnsupportedCOLRTable
	}
}

// colrV0BaseGlyphRange returns the offset (relative to the COLR table) of the
// first LayerRecord of glyph x and the number of layers. A zero offset means
// that glyph x has no v0 color representation.
func (f *Font) colrV0BaseGlyphRange(b *Buffer, x GlyphIndex) (firstLayerOffset int, numLayers int, err error) {
	const headerSize = 14
	buf, err := b.view(&f.src, int(f.colr.offset), headerSize)
	if err != nil {
		return 0, 0, err
	}
	numBase := int(u16(buf[2:]))
	if numBase > maxColorLayers {
		return 0, 0, errUnsupportedNumberOfLayers
	}
	baseOff := int(u32(buf[4:]))
	layerOff := int(u32(buf[8:]))
	if baseOff >= int(f.colr.length) || layerOff >= int(f.colr.length) {
		return 0, 0, errInvalidCOLRTable
	}
	// A BaseGlyphRecord is three Offset16 values: glyphID, firstLayerIndex,
	// numLayers.
	if uint64(baseOff)+uint64(numBase)*6 > uint64(f.colr.length) {
		return 0, 0, errInvalidCOLRTable
	}

	// BaseGlyphRecords are sorted by glyph ID, so binary search.
	lo, hi := 0, numBase
	for lo < hi {
		mid := lo + (hi-lo)/2
		rec, err := b.view(&f.src, int(f.colr.offset)+baseOff+6*mid, 6)
		if err != nil {
			return 0, 0, err
		}
		gid := GlyphIndex(u16(rec))
		switch {
		case gid < x:
			lo = mid + 1
		case gid > x:
			hi = mid
		default:
			first := int(u16(rec[2:]))
			count := int(u16(rec[4:]))
			if count > maxColorLayers {
				return 0, 0, errUnsupportedNumberOfLayers
			}
			if uint64(layerOff)+uint64(first+count)*4 > uint64(f.colr.length) {
				return 0, 0, errInvalidCOLRTable
			}
			return layerOff + 4*first, count, nil
		}
	}
	return 0, 0, nil
}

// colrV1BaseGlyphPaintOffset returns the absolute file offset of the base
// glyph's top-level Paint table and the absolute file offset of the
// BaseGlyphList (the base that the list's paint offsets are relative to). A
// zero paintOffset means the glyph has no v1 paint representation.
func (f *Font) colrV1BaseGlyphPaintOffset(b *Buffer, x GlyphIndex) (paintOffset int, listBase int, err error) {
	// The BaseGlyphListOffset is at byte offset 26 of the COLR v1 header.
	buf, err := b.view(&f.src, int(f.colr.offset)+26, 4)
	if err != nil {
		return 0, 0, err
	}
	listRel := int(u32(buf))
	if listRel == 0 {
		return 0, 0, nil
	}
	listOff := int(f.colr.offset) + listRel
	if listRel+4 > int(f.colr.length) {
		return 0, 0, errInvalidCOLRTable
	}
	head, err := b.view(&f.src, listOff, 4)
	if err != nil {
		return 0, 0, err
	}
	numRecords := int(u32(head))
	if numRecords > maxColorLayers {
		return 0, 0, errUnsupportedNumberOfLayers
	}
	recordsRel := listRel + 4
	if uint64(recordsRel)+uint64(numRecords)*6 > uint64(f.colr.length) {
		return 0, 0, errInvalidCOLRTable
	}

	// BaseGlyphPaintRecords are sorted by glyph ID, so binary search.
	lo, hi := 0, numRecords
	for lo < hi {
		mid := lo + (hi-lo)/2
		rec, err := b.view(&f.src, int(f.colr.offset)+recordsRel+6*mid, 6)
		if err != nil {
			return 0, 0, err
		}
		gid := GlyphIndex(u16(rec))
		switch {
		case gid < x:
			lo = mid + 1
		case gid > x:
			hi = mid
		default:
			paintRel := int(u32(rec[2:]))
			if paintRel == 0 {
				return 0, 0, nil
			}
			if listRel+paintRel >= int(f.colr.length) {
				return 0, 0, errInvalidCOLRTable
			}
			// Paint offsets within the BaseGlyphList are relative to the
			// list's start.
			return listOff + paintRel, listOff, nil
		}
	}
	return 0, 0, nil
}

// colorPaintFrame is one level of the iterative COLR v1 paint walk.
type colorPaintFrame struct {
	// offset is the absolute (file-relative) offset of the current Paint
	// table. Offsets inside a Paint table are relative to its own start, so a
	// child paint lives at offset + the child's relative offset.
	offset int
	// remainingLayers is the index of the next PaintColrLayers child paint to
	// dispatch, relative to numLayers (stored in totalLayers). It is zero for
	// every other paint format.
	remainingLayers int
	// totalLayers is the child count of a PaintColrLayers frame.
	totalLayers int
	// spent marks a frame whose work has already been dispatched. A
	// PaintGlyph sets it when its single child is pushed; a PaintColrLayers
	// sets it once its last child has been dispatched. When such a frame
	// resurfaces after its children complete, it is popped instead of being
	// evaluated again.
	spent bool
	// layerBase is the absolute offset of the current LayerList; each
	// LayerList entry's offset is relative to it.
	layerBase int
	// firstLayerOffset is the absolute offset of the LayerList's
	// PaintOffset array for the current PaintColrLayers.
	firstLayerOffset int
	// currentGlyph is the glyph ID whose outline applies to emitted layers.
	currentGlyph GlyphIndex
	// depth is the nesting depth of this frame.
	depth int
	// pushedSeen records that the frame followed a PaintColrGlyph reference and
	// appended currentGlyph to the cycle-detection stack.
	pushedSeen bool
}

// colorPaintWalker walks a COLR v1 paint graph, appending solid-color layers
// to the Buffer.
type colorPaintWalker struct {
	f            *Font
	b            *Buffer
	cpal         cpalHeader
	paletteIndex int
	foreground   color.RGBA

	currentGlyph GlyphIndex
}

// run walks the paint graph rooted at the Paint table at the given absolute
// file offset.
func (w *colorPaintWalker) run(rootOffset int) error {
	colrEnd := int(w.f.colr.offset + w.f.colr.length)
	frames := w.b.colorPaintStack[:0]
	seen := w.b.colorPaintSeen[:0]
	frames = append(frames, colorPaintFrame{
		offset:       rootOffset,
		currentGlyph: w.currentGlyph,
	})
	seen = append(seen, w.currentGlyph)

	defer func() {
		w.b.colorPaintStack = frames[:0]
		w.b.colorPaintSeen = seen[:0]
	}()

	for len(frames) != 0 {
		frame := &frames[len(frames)-1]

		if frame.remainingLayers > 0 {
			// Continue a PaintColrLayers: push the next child paint. The
			// LayerList stores its children in paint order; remainingLayers is
			// the index of the next entry (counting down from totalLayers), so
			// children are dispatched and processed in paint order. Each entry
			// is relative to the LayerList.
			idx := frame.totalLayers - frame.remainingLayers
			entry, err := w.b.view(&w.f.src, frame.firstLayerOffset+4*idx, 4)
			if err != nil {
				return err
			}
			childRel := int(u32(entry))
			childOff := frame.layerBase + childRel
			if childRel == 0 || childOff >= colrEnd {
				return errInvalidCOLRTable
			}
			frame.remainingLayers--
			if frame.remainingLayers == 0 {
				// All children dispatched; pop this frame once they finish.
				frame.spent = true
			}
			if frame.depth+1 > maxColorPaintDepth {
				return errColorPaintTooDeep
			}
			frames = append(frames, colorPaintFrame{
				offset:       childOff,
				currentGlyph: frame.currentGlyph,
				depth:        frame.depth + 1,
			})
			continue
		}
		if frame.spent {
			w.popFrame(&frames, &seen)
			continue
		}

		// Read the Paint table header: format (u8) then a format-specific u8
		// and u16 offset.
		hdr, err := w.b.view(&w.f.src, frame.offset, 4)
		if err != nil {
			return err
		}
		format := hdr[0]
		childRel := int(u16(hdr[2:]))

		switch format {
		case paintFormatSolid, paintFormatVarSolid:
			if err := w.emitSolid(frame, hdr); err != nil {
				return err
			}
			w.popFrame(&frames, &seen)

		case paintFormatGlyph:
			// PaintGlyph is 6 bytes: format, reserved, paintOffset (u16),
			// glyphID (u16). The paintOffset is relative to this table.
			if frame.offset+6 > colrEnd {
				return errInvalidCOLRTable
			}
			full, err := w.b.view(&w.f.src, frame.offset, 6)
			if err != nil {
				return err
			}
			gid := GlyphIndex(u16(full[4:]))
			if int(gid) >= w.f.NumGlyphs() {
				return errInvalidColorLayer
			}
			childOff := frame.offset + childRel
			if childRel == 0 || childOff > colrEnd {
				return errInvalidCOLRTable
			}
			if frame.depth+1 > maxColorPaintDepth {
				return errColorPaintTooDeep
			}
			// This frame has dispatched its only child; once the child
			// completes the frame pops rather than re-reading itself.
			frame.spent = true
			frames = append(frames, colorPaintFrame{
				offset:       childOff,
				currentGlyph: gid,
				depth:        frame.depth + 1,
			})

		case paintFormatColrGlyph:
			// PaintColrGlyph is 4 bytes: format, reserved, baseGlyphID (u16).
			gid := GlyphIndex(u16(hdr[2:]))
			if int(gid) >= w.f.NumGlyphs() {
				return errInvalidColorLayer
			}
			for _, g := range seen {
				if g == gid {
					return ErrColorCycle
				}
			}
			paintOffset, _, err := w.f.colrV1BaseGlyphPaintOffset(w.b, gid)
			if err != nil {
				return err
			}
			if paintOffset == 0 {
				return errInvalidCOLRTable
			}
			if frame.depth+1 > maxColorPaintDepth {
				return errColorPaintTooDeep
			}
			seen = append(seen, gid)
			// The referenced base glyph's paint is this frame's single child;
			// mark spent so the frame pops when the child completes.
			frame.spent = true
			frames = append(frames, colorPaintFrame{
				offset:       paintOffset,
				currentGlyph: gid,
				depth:        frame.depth + 1,
				pushedSeen:   true,
			})

		case paintFormatColrLayers:
			numLayers := int(hdr[1])
			if numLayers == 0 {
				// An empty layer list paints nothing.
				w.popFrame(&frames, &seen)
				continue
			}
			if len(w.b.colorLayers)+numLayers > maxColorLayers {
				return errUnsupportedNumberOfLayers
			}
			// The LayerList offset is relative to this PaintColrLayers table.
			layerListOff := frame.offset + childRel
			if childRel == 0 || layerListOff >= colrEnd {
				return errInvalidCOLRTable
			}
			// The LayerList's first u8 is numLayers (u8), followed by the
			// PaintOffset array.
			listHead, err := w.b.view(&w.f.src, layerListOff, 1)
			if err != nil {
				return err
			}
			if int(listHead[0]) != numLayers {
				return errInvalidCOLRTable
			}
			if layerListOff+1+4*numLayers > colrEnd {
				return errInvalidCOLRTable
			}
			frame.remainingLayers = numLayers
			frame.totalLayers = numLayers
			frame.layerBase = layerListOff
			frame.firstLayerOffset = layerListOff + 1

		default:
			return errUnsupportedColorPaint
		}
	}
	return nil
}

// popFrame removes the top frame, undoing any cycle-detection entry it made.
func (w *colorPaintWalker) popFrame(frames *[]colorPaintFrame, seen *[]GlyphIndex) {
	top := len(*frames) - 1
	if (*frames)[top].pushedSeen {
		*seen = (*seen)[:len(*seen)-1]
	}
	*frames = (*frames)[:top]
}

// emitSolid resolves a PaintSolid/PaintVarSolid color and appends a layer for
// the frame's current glyph.
func (w *colorPaintWalker) emitSolid(frame *colorPaintFrame, hdr []byte) error {
	const (
		paintSolidSize    = 8
		paintVarSolidSize = 12
	)
	format := hdr[0]
	size := paintSolidSize
	if format == paintFormatVarSolid {
		size = paintVarSolidSize
	}
	if frame.offset+size > int(w.f.colr.offset+w.f.colr.length) {
		return errInvalidCOLRTable
	}
	body, err := w.b.view(&w.f.src, frame.offset, size)
	if err != nil {
		return err
	}
	colorIndex := u16(body[4:])
	alpha := int32(u16(body[6:])) // F2Dot14
	if format == paintFormatVarSolid {
		// VarSolid appends a VarColorIndex (u16 delta-set index). Non-variable
		// renderers ignore it; validate only that it fits.
		_ = body[10:]
	}
	c, a, err := w.f.resolveColor(w.b, w.cpal, w.paletteIndex, colorIndex, alpha, w.foreground)
	if err != nil {
		return err
	}
	if len(w.b.colorLayers) >= maxColorLayers {
		return errUnsupportedNumberOfLayers
	}
	w.b.colorLayers = append(w.b.colorLayers, ColorGlyphLayer{
		GlyphIndex:   frame.currentGlyph,
		PaletteIndex: colorIndex,
		Color:        c,
		Alpha:        a,
	})
	return nil
}
