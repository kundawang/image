// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"strings"
	"testing"
)

// testPage describes one page of a synthetic multi-page TIFF.
type testPage struct {
	entries map[uint16]interface{}
	pixels  []byte // Pixel data, already compressed if the page is compressed.
}

// buildMultiPage builds a TIFF file with one IFD per page, chained together
// via the next-IFD offsets. Pixel data is placed just before its page's IFD
// and referenced through the StripOffsets and StripByteCounts tags.
func buildMultiPage(enc byteOrder, pages ...testPage) []byte {
	b := newTIFF(enc)
	linkPos := 4
	for _, p := range pages {
		entries := make(map[uint16]interface{}, len(p.entries)+2)
		for k, v := range p.entries {
			entries[k] = v
		}
		if p.pixels != nil {
			entries[tStripOffsets] = uint32(len(b))
			entries[tStripByteCounts] = uint32(len(p.pixels))
			b = append(b, p.pixels...)
		}
		b, linkPos = appendIFDAt(b, enc, entries, linkPos)
	}
	return b
}

func deflateBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mixedPages returns a three-page fixture: an uncompressed RGB page, a
// deflate-compressed gray page and a PackBits-compressed paletted page, all
// of different sizes, plus the expected decoded images.
func mixedPages(t *testing.T) ([]byte, []image.Image) {
	t.Helper()
	enc := binary.LittleEndian

	// Page 0: 4x3 RGB, 8 bits per sample, no compression.
	rgb := make([]byte, 4*3*3)
	for i := range rgb {
		rgb[i] = byte(7*i + 3)
	}
	want0 := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for i := 0; i < 4*3; i++ {
		want0.Pix[4*i+0] = rgb[3*i+0]
		want0.Pix[4*i+1] = rgb[3*i+1]
		want0.Pix[4*i+2] = rgb[3*i+2]
		want0.Pix[4*i+3] = 0xff
	}
	page0 := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(4),
			tImageLength:               uint32(3),
			tBitsPerSample:             []uint16{8, 8, 8},
			tCompression:               uint16(cNone),
			tPhotometricInterpretation: uint16(pRGB),
		},
		pixels: rgb,
	}

	// Page 1: 2x5 gray, 8 bits per sample, deflate compression.
	gray := make([]byte, 2*5)
	for i := range gray {
		gray[i] = byte(255 - 17*i)
	}
	want1 := image.NewGray(image.Rect(0, 0, 2, 5))
	copy(want1.Pix, gray)
	page1 := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(2),
			tImageLength:               uint32(5),
			tBitsPerSample:             uint16(8),
			tCompression:               uint16(cDeflate),
			tPhotometricInterpretation: uint16(pBlackIsZero),
		},
		pixels: deflateBytes(t, gray),
	}

	// Page 2: 4x1 paletted, 8 bits per sample, PackBits compression.
	palette := color.Palette{
		color.RGBA64{0xffff, 0x0000, 0x0000, 0xffff},
		color.RGBA64{0x0000, 0xffff, 0x0000, 0xffff},
	}
	want2 := image.NewPaletted(image.Rect(0, 0, 4, 1), palette)
	want2.SetColorIndex(0, 0, 0)
	want2.SetColorIndex(1, 0, 1)
	want2.SetColorIndex(2, 0, 1)
	want2.SetColorIndex(3, 0, 0)
	page2 := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(4),
			tImageLength:               uint32(1),
			tBitsPerSample:             uint16(8),
			tCompression:               uint16(cPackBits),
			tPhotometricInterpretation: uint16(pPaletted),
			// Planar 16-bit color map: R values, then G, then B.
			tColorMap: []uint16{0xffff, 0x0000, 0x0000, 0xffff, 0x0000, 0x0000},
		},
		// PackBits literal run: 4 bytes follow.
		pixels: []byte{3, 0, 1, 1, 0},
	}

	return buildMultiPage(enc, page0, page1, page2), []image.Image{want0, want1, want2}
}

// TestMultiPageMixedCompression tests iterating and randomly accessing a
// multi-page TIFF whose pages differ in size, compression and photometric
// interpretation.
func TestMultiPageMixedCompression(t *testing.T) {
	data, want := mixedPages(t)

	dec, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	n, err := dec.NumPages()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("NumPages: got %d, want 3", n)
	}

	// Metadata differs per page.
	wantSizes := [][2]int{{4, 3}, {2, 5}, {4, 1}}
	for i, sz := range wantSizes {
		cfg, err := dec.PageConfig(i)
		if err != nil {
			t.Fatalf("PageConfig(%d): %v", i, err)
		}
		if cfg.Width != sz[0] || cfg.Height != sz[1] {
			t.Errorf("PageConfig(%d): got %dx%d, want %dx%d", i, cfg.Width, cfg.Height, sz[0], sz[1])
		}
	}

	// Decode pages by index, in order.
	for i, w := range want {
		img, err := dec.DecodePage(i)
		if err != nil {
			t.Fatalf("DecodePage(%d): %v", i, err)
		}
		compare(t, w, img)
	}

	// Repeated and out-of-order reads are stable and independent.
	again, err := dec.DecodePage(1)
	if err != nil {
		t.Fatal(err)
	}
	compare(t, want[1], again)
	first, err := dec.DecodePage(0)
	if err != nil {
		t.Fatal(err)
	}
	compare(t, want[0], first)

	// Sequential iteration returns the same pages, then io.EOF.
	dec2, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range want {
		img, err := dec2.NextPage()
		if err != nil {
			t.Fatalf("NextPage(%d): %v", i, err)
		}
		compare(t, w, img)
	}
	if _, err := dec2.NextPage(); err != io.EOF {
		t.Fatalf("NextPage past end: got %v, want io.EOF", err)
	}

	// Single-page Decode of the same file decodes the first page.
	img0, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	compare(t, want[0], img0)
}

// TestMultiPageNonSeekable tests that multi-page decoding works when the
// input is a stream without a ReadAt method.
func TestMultiPageNonSeekable(t *testing.T) {
	data, want := mixedPages(t)

	dec, err := NewDecoder(ioReader{bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	n, err := dec.NumPages()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("NumPages: got %d, want 3", n)
	}
	// Read out of order and repeatedly.
	for _, i := range []int{2, 0, 1, 2} {
		img, err := dec.DecodePage(i)
		if err != nil {
			t.Fatalf("DecodePage(%d): %v", i, err)
		}
		compare(t, want[i], img)
	}
}

// countingReader counts the number of bytes read from the underlying reader.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestMultiPageLazy tests that creating a Decoder and querying the page
// count do not read (let alone decode) the whole input.
func TestMultiPageLazy(t *testing.T) {
	data, _ := mixedPages(t)
	// Append unreferenced trailing data: walking the IFD chain must not
	// require reading it.
	data = append(data, make([]byte, 4096)...)

	cr := &countingReader{r: bytes.NewReader(data)}
	dec, err := NewDecoder(cr)
	if err != nil {
		t.Fatal(err)
	}
	if cr.n > 8 {
		t.Fatalf("NewDecoder read %d bytes, want at most the 8-byte header", cr.n)
	}
	if _, err := dec.NumPages(); err != nil {
		t.Fatal(err)
	}
	if cr.n >= len(data) {
		t.Fatalf("NumPages read %d bytes, want less than the %d-byte file", cr.n, len(data))
	}
}

// TestMultiPageCorruptMiddlePage tests that a corrupt page does not affect
// its neighbors and that its error carries the page index.
func TestMultiPageCorruptMiddlePage(t *testing.T) {
	_, want := mixedPages(t)
	enc := binary.LittleEndian
	rgb := make([]byte, 4*3*3)
	for i := range rgb {
		rgb[i] = byte(7*i + 3)
	}
	// Corrupt page: zero-size image (an "empty page").
	corrupt := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(0),
			tImageLength:               uint32(5),
			tBitsPerSample:             uint16(8),
			tCompression:               uint16(cNone),
			tPhotometricInterpretation: uint16(pBlackIsZero),
		},
	}

	p0 := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(4),
			tImageLength:               uint32(3),
			tBitsPerSample:             []uint16{8, 8, 8},
			tCompression:               uint16(cNone),
			tPhotometricInterpretation: uint16(pRGB),
		},
		pixels: rgb,
	}
	gray := make([]byte, 2*5)
	for i := range gray {
		gray[i] = byte(255 - 17*i)
	}
	p1 := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(2),
			tImageLength:               uint32(5),
			tBitsPerSample:             uint16(8),
			tCompression:               uint16(cDeflate),
			tPhotometricInterpretation: uint16(pBlackIsZero),
		},
		pixels: deflateBytes(t, gray),
	}
	p2 := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(4),
			tImageLength:               uint32(1),
			tBitsPerSample:             uint16(8),
			tCompression:               uint16(cPackBits),
			tPhotometricInterpretation: uint16(pPaletted),
			tColorMap:                  []uint16{0xffff, 0x0000, 0x0000, 0xffff, 0x0000, 0x0000},
		},
		pixels: []byte{3, 0, 1, 1, 0},
	}
	corruptData := buildMultiPage(enc, p0, corrupt, p1, p2)

	dec, err := NewDecoder(bytes.NewReader(corruptData))
	if err != nil {
		t.Fatal(err)
	}
	n, err := dec.NumPages()
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("NumPages: got %d, want 4", n)
	}

	// The corrupt page fails with page context.
	_, err = dec.DecodePage(1)
	if err == nil {
		t.Fatal("DecodePage(1) on corrupt page: got nil error, want non-nil")
	}
	var pe *PageError
	if !errors.As(err, &pe) {
		t.Fatalf("DecodePage(1) error %v is not a PageError", err)
	}
	if pe.Page != 1 {
		t.Fatalf("PageError.Page: got %d, want 1", pe.Page)
	}
	if !strings.Contains(err.Error(), "page 1") {
		t.Fatalf("error %q does not mention the page number", err)
	}

	// The neighbors decode fine and match the single-page results.
	img0, err := dec.DecodePage(0)
	if err != nil {
		t.Fatal(err)
	}
	compare(t, want[0], img0)
	img2, err := dec.DecodePage(2)
	if err != nil {
		t.Fatal(err)
	}
	compare(t, want[1], img2)
	img3, err := dec.DecodePage(3)
	if err != nil {
		t.Fatal(err)
	}
	compare(t, want[2], img3)

	// Sequential iteration skips past the bad page.
	dec2, err := NewDecoder(bytes.NewReader(corruptData))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dec2.NextPage(); err != nil {
		t.Fatalf("NextPage page 0: %v", err)
	}
	if _, err := dec2.NextPage(); err == nil {
		t.Fatal("NextPage page 1: got nil error, want non-nil")
	}
	img, err := dec2.NextPage()
	if err != nil {
		t.Fatalf("NextPage page 2: %v", err)
	}
	compare(t, want[1], img)
	if _, err := dec2.NextPage(); err != nil {
		t.Fatalf("NextPage page 3: %v", err)
	}
	if _, err := dec2.NextPage(); err != io.EOF {
		t.Fatalf("NextPage past end: got %v, want io.EOF", err)
	}
}

// TestMultiPageBadChainOffset tests that an out-of-bounds IFD offset in the
// chain produces an error with page context instead of a panic, and that
// pages before the break still decode.
func TestMultiPageBadChainOffset(t *testing.T) {
	data, want := mixedPages(t)
	enc := binary.LittleEndian

	// Point page 0's next-IFD field out of bounds. Page 0's IFD offset is in
	// the header; its next-IFD field follows the entry count and entries.
	ifd0 := int64(enc.Uint32(data[4:8]))
	numItems := int64(enc.Uint16(data[ifd0:]))
	nextPos := ifd0 + 2 + ifdLen*numItems
	enc.PutUint32(data[nextPos:], 0x7fffffff)

	dec, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dec.NumPages(); err == nil {
		t.Fatal("NumPages: got nil error, want non-nil")
	} else {
		var pe *PageError
		if !errors.As(err, &pe) {
			t.Fatalf("NumPages error %v is not a PageError", err)
		}
		if pe.Page != 1 {
			t.Fatalf("PageError.Page: got %d, want 1", pe.Page)
		}
	}
	if _, err := dec.DecodePage(1); err == nil {
		t.Fatal("DecodePage(1): got nil error, want non-nil")
	}
	img0, err := dec.DecodePage(0)
	if err != nil {
		t.Fatal(err)
	}
	compare(t, want[0], img0)
}

// TestMultiPageErrorContext tests that palette and CCITT decoding errors on
// a later page are reported with that page's index.
func TestMultiPageErrorContext(t *testing.T) {
	enc := binary.LittleEndian
	good := testPage{
		entries: map[uint16]interface{}{
			tImageWidth:                uint32(1),
			tImageLength:               uint32(1),
			tBitsPerSample:             uint16(8),
			tCompression:               uint16(cNone),
			tPhotometricInterpretation: uint16(pBlackIsZero),
		},
		pixels: []byte{0x80},
	}

	t.Run("palette", func(t *testing.T) {
		badPalette := testPage{
			entries: map[uint16]interface{}{
				tImageWidth:                uint32(2),
				tImageLength:               uint32(1),
				tBitsPerSample:             uint16(8),
				tCompression:               uint16(cNone),
				tPhotometricInterpretation: uint16(pPaletted),
				tColorMap:                  []uint16{0xffff, 0x0000, 0x0000, 0xffff, 0x0000, 0x0000},
			},
			// Color index 5 is out of range for a 2-color palette.
			pixels: []byte{5, 0},
		}
		data := buildMultiPage(enc, good, badPalette)
		dec, err := NewDecoder(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		_, err = dec.DecodePage(1)
		if err == nil {
			t.Fatal("DecodePage(1): got nil error, want non-nil")
		}
		var pe *PageError
		if !errors.As(err, &pe) || pe.Page != 1 {
			t.Fatalf("error %v: want PageError with Page 1", err)
		}
		var fe FormatError
		if !errors.As(err, &fe) {
			t.Fatalf("error %v does not wrap a FormatError", err)
		}
	})

	t.Run("ccitt", func(t *testing.T) {
		badCCITT := testPage{
			entries: map[uint16]interface{}{
				tImageWidth:                uint32(64),
				tImageLength:               uint32(64),
				tBitsPerSample:             uint16(1),
				tCompression:               uint16(cG4),
				tPhotometricInterpretation: uint16(pWhiteIsZero),
			},
			// Far too little data for a 64x64 Group 4 image.
			pixels: []byte{0xde, 0xad, 0xbe, 0xef},
		}
		data := buildMultiPage(enc, good, badCCITT)
		dec, err := NewDecoder(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		_, err = dec.DecodePage(1)
		if err == nil {
			t.Fatal("DecodePage(1): got nil error, want non-nil")
		}
		var pe *PageError
		if !errors.As(err, &pe) || pe.Page != 1 {
			t.Fatalf("error %v: want PageError with Page 1", err)
		}
	})
}

// TestMultiPageCyclicIFD tests that a cyclic IFD chain is reported as an
// error instead of looping forever.
func TestMultiPageCyclicIFD(t *testing.T) {
	enc := binary.LittleEndian
	page := func(v byte) testPage {
		return testPage{
			entries: map[uint16]interface{}{
				tImageWidth:                uint32(1),
				tImageLength:               uint32(1),
				tBitsPerSample:             uint16(8),
				tCompression:               uint16(cNone),
				tPhotometricInterpretation: uint16(pBlackIsZero),
			},
			pixels: []byte{v},
		}
	}

	// Build the two pages, then patch page 1's next-IFD pointer back to
	// page 0's IFD, creating a cycle.
	data := buildMultiPage(enc, page(0x11), page(0x22))
	ifd0 := enc.Uint32(data[4:8])
	ifd1 := int64(0)
	{
		// Find page 1's IFD via page 0's next pointer.
		numItems := int64(enc.Uint16(data[ifd0:]))
		nextPos := int64(ifd0) + 2 + ifdLen*numItems
		ifd1 = int64(enc.Uint32(data[nextPos:]))
		// Patch page 1's next pointer to point back to page 0.
		numItems1 := int64(enc.Uint16(data[ifd1:]))
		nextPos1 := ifd1 + 2 + ifdLen*numItems1
		enc.PutUint32(data[nextPos1:], ifd0)
	}

	dec, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dec.NumPages(); err == nil {
		t.Fatal("NumPages on cyclic chain: got nil error, want non-nil")
	} else if !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("NumPages error: got %q, want mention of cyclic chain", err)
	}

	// Iteration yields the two pages, then fails instead of looping.
	dec2, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := dec2.NextPage(); err != nil {
			t.Fatalf("NextPage(%d): %v", i, err)
		}
	}
	if _, err := dec2.NextPage(); err == nil {
		t.Fatal("NextPage on cyclic chain: got nil error, want non-nil")
	}
}

// TestMultiPageTooManyPages tests that an IFD chain longer than the page
// limit is rejected instead of being walked without bound.
func TestMultiPageTooManyPages(t *testing.T) {
	enc := binary.LittleEndian
	b := newTIFF(enc)
	linkPos := 4
	for i := 0; i <= maxPages; i++ {
		b, linkPos = appendIFDAt(b, enc, map[uint16]interface{}{}, linkPos)
	}
	dec, err := NewDecoder(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dec.NumPages(); err == nil {
		t.Fatal("NumPages: got nil error, want non-nil")
	} else if !strings.Contains(err.Error(), "too many pages") {
		t.Fatalf("NumPages error: got %q, want mention of too many pages", err)
	}
}

// TestMultiPageEmpty tests a file whose header points to no IFD at all.
func TestMultiPageEmpty(t *testing.T) {
	data := newTIFF(binary.LittleEndian) // First IFD offset is 0.
	dec, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	n, err := dec.NumPages()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("NumPages: got %d, want 0", n)
	}
	if _, err := dec.NextPage(); err != io.EOF {
		t.Fatalf("NextPage: got %v, want io.EOF", err)
	}
	if _, err := dec.DecodePage(0); err == nil {
		t.Fatal("DecodePage(0): got nil error, want non-nil")
	}
	if _, err := dec.DecodePage(-1); err == nil {
		t.Fatal("DecodePage(-1): got nil error, want non-nil")
	}
	if _, err := dec.PageConfig(0); err == nil {
		t.Fatal("PageConfig(0): got nil error, want non-nil")
	}
}

// TestMultiPageBigEndian tests multi-page decoding of a big-endian file.
func TestMultiPageBigEndian(t *testing.T) {
	enc := binary.BigEndian
	page := func(w, h uint32, v byte) testPage {
		pix := bytes.Repeat([]byte{v}, int(w*h))
		return testPage{
			entries: map[uint16]interface{}{
				tImageWidth:                w,
				tImageLength:               h,
				tBitsPerSample:             uint16(8),
				tCompression:               uint16(cNone),
				tPhotometricInterpretation: uint16(pBlackIsZero),
			},
			pixels: pix,
		}
	}
	data := buildMultiPage(enc, page(3, 2, 0x42), page(1, 4, 0x99))

	dec, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	n, err := dec.NumPages()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("NumPages: got %d, want 2", n)
	}
	for i, want := range []struct {
		w, h int
		v    byte
	}{{3, 2, 0x42}, {1, 4, 0x99}} {
		img, err := dec.DecodePage(i)
		if err != nil {
			t.Fatalf("DecodePage(%d): %v", i, err)
		}
		g, ok := img.(*image.Gray)
		if !ok {
			t.Fatalf("page %d: got %T, want *image.Gray", i, img)
		}
		if b := g.Bounds(); b.Dx() != want.w || b.Dy() != want.h {
			t.Fatalf("page %d: got size %v, want %dx%d", i, b, want.w, want.h)
		}
		for _, p := range g.Pix {
			if p != want.v {
				t.Fatalf("page %d: got pixel %#x, want %#x", i, p, want.v)
			}
		}
	}
}

// TestMultiPageDecodeMatchesSinglePage tests that DecodePage(0) agrees with
// the classic single-page Decode on every existing single-page test file.
func TestMultiPageDecodeMatchesSinglePage(t *testing.T) {
	files := []string{
		"bw-uncompressed.tiff",
		"bw-deflate.tiff",
		"bw-packbits.tiff",
		"video-001.tiff",
		"video-001-16bit.tiff",
	}
	for _, name := range files {
		data, err := os.ReadFile(testdataDir + name)
		if err != nil {
			t.Fatal(err)
		}
		img0, err := Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: Decode: %v", name, err)
		}
		dec, err := NewDecoder(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: NewDecoder: %v", name, err)
		}
		if n, err := dec.NumPages(); err != nil || n != 1 {
			t.Fatalf("%s: NumPages = %d, %v; want 1, nil", name, n, err)
		}
		img1, err := dec.DecodePage(0)
		if err != nil {
			t.Fatalf("%s: DecodePage(0): %v", name, err)
		}
		compare(t, img0, img1)
		cfg1, err := dec.PageConfig(0)
		if err != nil {
			t.Fatalf("%s: PageConfig(0): %v", name, err)
		}
		cfg0, err := DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: DecodeConfig: %v", name, err)
		}
		if fmt.Sprint(cfg0) != fmt.Sprint(cfg1) {
			t.Fatalf("%s: PageConfig(0) = %v, DecodeConfig = %v", name, cfg1, cfg0)
		}
	}
}
