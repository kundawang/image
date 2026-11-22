// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bmp

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"math/bits"
	"os"
	"testing"
)

// packRows assembles BMP pixel data for a width x height image, calling pix
// for each pixel. pix returns a palette index for bpp < 16, a raw 16-bit
// value for 16 bpp, R<<16|G<<8|B for 24 bpp and R<<24|G<<16|B<<8|A for
// 32 bpp. Rows are emitted in file order: bottom-up unless topDown.
func packRows(width, height int, topDown bool, bpp int, pix func(x, y int) uint32) []byte {
	rowLen := rowBytes(width, bpp)
	data := make([]byte, 0, rowLen*height)
	for fy := 0; fy < height; fy++ {
		y := fy
		if !topDown {
			y = height - 1 - fy
		}
		row := make([]byte, rowLen)
		switch bpp {
		case 1, 2, 4, 8:
			pixelsPerByte := 8 / bpp
			for x := 0; x < width; x++ {
				bitIndex := 8 - (x%pixelsPerByte)*bpp - bpp
				row[x/pixelsPerByte] |= byte(pix(x, y)) << uint(bitIndex)
			}
		case 16:
			for x := 0; x < width; x++ {
				v := pix(x, y)
				row[2*x+0] = byte(v)
				row[2*x+1] = byte(v >> 8)
			}
		case 24:
			for x := 0; x < width; x++ {
				v := pix(x, y)
				row[3*x+0] = byte(v >> 0)  // B
				row[3*x+1] = byte(v >> 8)  // G
				row[3*x+2] = byte(v >> 16) // R
			}
		case 32:
			for x := 0; x < width; x++ {
				v := pix(x, y)
				row[4*x+0] = byte(v >> 8)  // B
				row[4*x+1] = byte(v >> 16) // G
				row[4*x+2] = byte(v >> 24) // R
				row[4*x+3] = byte(v >> 0)  // A
			}
		}
		data = append(data, row...)
	}
	return data
}

// makePalette returns n distinct palette entries. Note that bmpBuilder
// writes the struct fields in order, so the decoded color of entry e is
// color.RGBA{e.b, e.g, e.r, 0xff}: the first byte written is the BMP blue
// channel.
func makePalette(n int) []colorTableEntry {
	p := make([]colorTableEntry, n)
	for i := range p {
		p[i] = colorTableEntry{byte(3 + i*7), byte(5 + i*17), byte(11 + i*37)}
	}
	return p
}

func paletteColor(e colorTableEntry) color.RGBA {
	return color.RGBA{R: e.b, G: e.g, B: e.r, A: 0xff}
}

// scaleMask mirrors the decoder's 16 bpp channel scaling, independently
// written out for the tests.
func scaleMask(v, mask uint32) uint8 {
	shift := uint(bits.TrailingZeros32(mask))
	return uint8(((v & mask) >> shift) * 0xff / (mask >> shift))
}

type regionCase struct {
	name        string
	width       int
	height      int
	bpp         uint16
	compression uint32
	extra       []byte
	colorsUsed  uint32
	palette     []colorTableEntry
	pix         func(x, y int) uint32
	want        func(x, y int) color.Color
}

func (c *regionCase) build(topDown bool) []byte {
	h := c.height
	if topDown {
		h = -h
	}
	return bmpBuilder{
		width:        int32(c.width),
		height:       int32(h),
		planes:       1,
		bitsPerPixel: c.bpp,
		compression:  c.compression,
		colorsUsed:   c.colorsUsed,
		colorTable:   c.palette,
		extra:        c.extra,
		data:         packRows(c.width, c.height, topDown, int(c.bpp), c.pix),
	}.Bytes()
}

func regionTestCases() []regionCase {
	pal2 := makePalette(2)
	pal16 := makePalette(16)
	pal256 := makePalette(256)
	masks565 := []byte{
		0x00, 0xf8, 0x00, 0x00, // R
		0xe0, 0x07, 0x00, 0x00, // G
		0x1f, 0x00, 0x00, 0x00, // B
	}
	masks5551 := []byte{
		0x00, 0x7c, 0x00, 0x00, // R
		0xe0, 0x03, 0x00, 0x00, // G
		0x1f, 0x00, 0x00, 0x00, // B
		0x00, 0x80, 0x00, 0x00, // A
	}
	return []regionCase{{
		name:    "1bpp",
		width:   9,
		height:  7,
		bpp:     1,
		palette: pal2,
		pix:     func(x, y int) uint32 { return uint32((x + y) % 2) },
		want: func(x, y int) color.Color {
			return paletteColor(pal2[(x+y)%2])
		},
	}, {
		name:    "4bpp",
		width:   9,
		height:  7,
		bpp:     4,
		palette: pal16,
		pix:     func(x, y int) uint32 { return uint32((x*5 + y*3) % 16) },
		want: func(x, y int) color.Color {
			return paletteColor(pal16[(x*5+y*3)%16])
		},
	}, {
		name:    "8bpp",
		width:   9,
		height:  7,
		bpp:     8,
		palette: pal256,
		pix:     func(x, y int) uint32 { return uint32((x*37 + y*11) % 256) },
		want: func(x, y int) color.Color {
			return paletteColor(pal256[(x*37+y*11)%256])
		},
	}, {
		name:   "16bpp-555",
		width:  9,
		height: 7,
		bpp:    16,
		pix: func(x, y int) uint32 {
			return uint32((x*3+y*5)&0x1f)<<10 | uint32((x*7+y*11)&0x1f)<<5 | uint32((x*13+y*17)&0x1f)
		},
		want: func(x, y int) color.Color {
			v := uint32((x*3+y*5)&0x1f)<<10 | uint32((x*7+y*11)&0x1f)<<5 | uint32((x*13+y*17)&0x1f)
			return color.RGBA{scaleMask(v, 0x7c00), scaleMask(v, 0x03e0), scaleMask(v, 0x001f), 0xff}
		},
	}, {
		name:        "16bpp-565-bitfields",
		width:       9,
		height:      7,
		bpp:         16,
		compression: 3,
		extra:       masks565,
		pix: func(x, y int) uint32 {
			return uint32((x*3+y*5)&0x1f)<<11 | uint32((x*7+y*11)&0x3f)<<5 | uint32((x*13+y*17)&0x1f)
		},
		want: func(x, y int) color.Color {
			v := uint32((x*3+y*5)&0x1f)<<11 | uint32((x*7+y*11)&0x3f)<<5 | uint32((x*13+y*17)&0x1f)
			return color.RGBA{scaleMask(v, 0xf800), scaleMask(v, 0x07e0), scaleMask(v, 0x001f), 0xff}
		},
	}, {
		name:        "16bpp-5551-alpha-bitfields",
		width:       9,
		height:      7,
		bpp:         16,
		compression: 3,
		extra:       masks5551,
		pix: func(x, y int) uint32 {
			return uint32((x*3+y*5)&0x1f)<<10 | uint32((x*7+y*11)&0x1f)<<5 |
				uint32((x*13+y*17)&0x1f) | uint32((x+y)&1)<<15
		},
		want: func(x, y int) color.Color {
			v := uint32((x*3+y*5)&0x1f)<<10 | uint32((x*7+y*11)&0x1f)<<5 |
				uint32((x*13+y*17)&0x1f) | uint32((x+y)&1)<<15
			return color.NRGBA{scaleMask(v, 0x7c00), scaleMask(v, 0x03e0), scaleMask(v, 0x001f), scaleMask(v, 0x8000)}
		},
	}, {
		name:   "24bpp",
		width:  9,
		height: 7,
		bpp:    24,
		pix: func(x, y int) uint32 {
			return uint32((x*40+y*3+1)&0xff)<<16 | uint32((x*5+y*60+2)&0xff)<<8 | uint32((x*23+y*23+3)&0xff)
		},
		want: func(x, y int) color.Color {
			return color.RGBA{byte(x*40 + y*3 + 1), byte(x*5 + y*60 + 2), byte(x*23 + y*23 + 3), 0xff}
		},
	}, {
		name:   "32bpp",
		width:  9,
		height: 7,
		bpp:    32,
		pix: func(x, y int) uint32 {
			return uint32((x*40+y*3+1)&0xff)<<24 | uint32((x*5+y*60+2)&0xff)<<16 |
				uint32((x*23+y*23+3)&0xff)<<8 | uint32((x*7+y*9)&0xff)
		},
		// A BITMAPINFOHEADER 32 bpp image ignores the alpha channel.
		want: func(x, y int) color.Color {
			return color.NRGBA{byte(x*40 + y*3 + 1), byte(x*5 + y*60 + 2), byte(x*23 + y*23 + 3), 0xff}
		},
	}}
}

// testRects returns the regions to exercise for a width x height image.
func testRects(width, height int) []image.Rectangle {
	return []image.Rectangle{
		image.Rect(0, 0, width, height),                      // full image
		image.Rect(0, 0, 1, 1),                               // single corner pixel
		image.Rect(1, 1, width-1, height-1),                  // interior
		image.Rect(2, 1, width-2, height-3),                  // non-zero origin interior
		image.Rect(-3, -2, 2, 2),                             // clips top-left
		image.Rect(width-2, height-2, width+5, height+7),     // clips bottom-right
		image.Rect(width+10, height+10, width+20, height+20), // fully outside
		image.Rect(1, 2, 1, 4),                               // empty (zero width)
		image.Rect(width-1, height-1, 1, 1),                  // inverted corners
	}
}

func checkRegionPixels(t *testing.T, img image.Image, want func(x, y int) color.Color, r image.Rectangle) {
	t.Helper()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			g := img.At(x, y)
			w := want(x, y)
			gr, gg, gb, ga := g.RGBA()
			wr, wg, wb, wa := w.RGBA()
			if gr != wr || gg != wg || gb != wb || ga != wa {
				t.Fatalf("pixel (%d, %d): got %v, want %v", x, y, g, w)
			}
		}
	}
}

func TestDecodeRegionConstructed(t *testing.T) {
	for _, tc := range regionTestCases() {
		for _, topDown := range []bool{false, true} {
			if topDown && tc.compression != 0 {
				continue // top-down bitfields images are not realistic
			}
			name := tc.name
			if topDown {
				name += "-topdown"
			}
			t.Run(name, func(t *testing.T) {
				b := tc.build(topDown)
				bounds := image.Rect(0, 0, tc.width, tc.height)

				// The full Decode output is the reference, and must
				// itself match the expected pixels.
				ref, err := Decode(bytes.NewReader(b))
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				checkRegionPixels(t, ref, tc.want, bounds)

				for _, rect := range testRects(tc.width, tc.height) {
					want := rect.Canon().Intersect(bounds)
					got, err := DecodeRegion(bytes.NewReader(b), rect)
					if err != nil {
						t.Errorf("DecodeRegion(%v): %v", rect, err)
						continue
					}
					if !got.Bounds().Eq(want) {
						t.Errorf("DecodeRegion(%v): bounds %v, want %v", rect, got.Bounds(), want)
						continue
					}
					if want.Empty() {
						switch img := got.(type) {
						case *image.Paletted:
							if len(img.Pix) != 0 {
								t.Errorf("DecodeRegion(%v): %d pix bytes for empty region", rect, len(img.Pix))
							}
						case *image.RGBA:
							if len(img.Pix) != 0 {
								t.Errorf("DecodeRegion(%v): %d pix bytes for empty region", rect, len(img.Pix))
							}
						case *image.NRGBA:
							if len(img.Pix) != 0 {
								t.Errorf("DecodeRegion(%v): %d pix bytes for empty region", rect, len(img.Pix))
							}
						}
						continue
					}
					checkRegionPixels(t, got, tc.want, want)
				}
			})
		}
	}
}

func TestDecodeRegionFuncConstructed(t *testing.T) {
	for _, tc := range regionTestCases() {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.build(false)
			bounds := image.Rect(0, 0, tc.width, tc.height)
			for _, rect := range testRects(tc.width, tc.height) {
				want := rect.Canon().Intersect(bounds)
				var ys []int
				rows := map[int]image.Image{}
				err := DecodeRegionFunc(bytes.NewReader(b), rect, func(y int, row image.Image) error {
					ys = append(ys, y)
					rows[y] = row
					return nil
				})
				if err != nil {
					t.Errorf("DecodeRegionFunc(%v): %v", rect, err)
					continue
				}
				if len(ys) != want.Dy() {
					t.Errorf("DecodeRegionFunc(%v): got %d rows, want %d", rect, len(ys), want.Dy())
					continue
				}
				for i, y := range ys {
					if y != want.Min.Y+i {
						t.Errorf("DecodeRegionFunc(%v): row %d is y=%d, want %d", rect, i, y, want.Min.Y+i)
						continue
					}
					row := rows[y]
					if !row.Bounds().Eq(image.Rect(want.Min.X, y, want.Max.X, y+1)) {
						t.Errorf("DecodeRegionFunc(%v): row %d bounds %v", rect, y, row.Bounds())
						continue
					}
					checkRegionPixels(t, row, tc.want, row.Bounds())
				}
			}
		})
	}
}

func TestDecodeRegionFuncError(t *testing.T) {
	tc := regionTestCases()[6] // 24bpp
	b := tc.build(false)
	errFoo := errors.New("foo")
	calls := 0
	err := DecodeRegionFunc(bytes.NewReader(b), image.Rect(0, 0, tc.width, tc.height), func(y int, row image.Image) error {
		calls++
		return errFoo
	})
	if err != errFoo {
		t.Errorf("DecodeRegionFunc: got %v, want %v", err, errFoo)
	}
	if calls != 1 {
		t.Errorf("DecodeRegionFunc: callback called %d times, want 1", calls)
	}
}

func TestDecodeRegionErrors(t *testing.T) {
	pix24 := func(x, y int) uint32 { return uint32(x*40+y*3)<<16 | uint32(x*5+y*60)<<8 | uint32(x*23+y*23) }

	t.Run("truncated row", func(t *testing.T) {
		full := bmpBuilder{
			width: 4, height: 4, planes: 1, bitsPerPixel: 24,
			data: packRows(4, 4, false, 24, pix24),
		}.Bytes()
		// Keep two and a half of the four rows.
		rowLen := rowBytes(4, 24)
		trunc := full[:54+2*rowLen+rowLen/2]
		_, err := DecodeRegion(bytes.NewReader(trunc), image.Rect(0, 0, 4, 4))
		var re *RegionError
		if !errors.As(err, &re) {
			t.Fatalf("DecodeRegion error %v (%T), want *RegionError", err, err)
		}
		// Bottom-up: image row 0 lives in the last (missing) file row.
		if re.Row != 0 || re.Offset != 54+3*int64(rowLen) {
			t.Errorf("RegionError row=%d offset=%d, want row=0 offset=%d", re.Row, re.Offset, 54+3*rowLen)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("DecodeRegion error %v, want io.ErrUnexpectedEOF", err)
		}
		// Decode keeps its historical bare error.
		if _, err := Decode(bytes.NewReader(trunc)); err != io.ErrUnexpectedEOF {
			t.Errorf("Decode error %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("invalid palette index", func(t *testing.T) {
		palette := makePalette(2)
		bad := func(x, y int) uint32 {
			if y == 1 {
				return 3 // only 2 palette entries
			}
			return uint32((x + y) % 2)
		}
		b := bmpBuilder{
			width: 4, height: 4, planes: 1, bitsPerPixel: 8,
			colorsUsed: 2, colorTable: palette,
			data: packRows(4, 4, false, 8, bad),
		}.Bytes()
		dataOffset := int64(54 + 2*4)
		rowLen := int64(rowBytes(4, 8))

		// A region covering row 1 must report the failing row and offset.
		_, err := DecodeRegion(bytes.NewReader(b), image.Rect(0, 0, 4, 4))
		var re *RegionError
		if !errors.As(err, &re) {
			t.Fatalf("DecodeRegion error %v (%T), want *RegionError", err, err)
		}
		if !errors.Is(err, errInvalidPaletteIndex) {
			t.Errorf("DecodeRegion error %v, want errInvalidPaletteIndex", err)
		}
		// Bottom-up: image row 1 is file row 2; x=0 is the first bad pixel.
		wantOff := dataOffset + 2*rowLen
		if re.Row != 1 || re.Offset != wantOff {
			t.Errorf("RegionError row=%d offset=%d, want row=1 offset=%d", re.Row, re.Offset, wantOff)
		}

		// A region avoiding row 1 never reads the bad pixels.
		img, err := DecodeRegion(bytes.NewReader(b), image.Rect(0, 2, 4, 4))
		if err != nil {
			t.Errorf("DecodeRegion(rows 2-3): %v", err)
		} else if !img.Bounds().Eq(image.Rect(0, 2, 4, 4)) {
			t.Errorf("DecodeRegion(rows 2-3): bounds %v", img.Bounds())
		}

		// Decode keeps its historical bare error.
		if _, err := Decode(bytes.NewReader(b)); err != errInvalidPaletteIndex {
			t.Errorf("Decode error %v, want errInvalidPaletteIndex", err)
		}
	})

	t.Run("invalid bitfield mask", func(t *testing.T) {
		masks := []byte{
			0x00, 0x7c, 0x00, 0x00, // R = 0x7c00
			0xe1, 0x03, 0x00, 0x00, // G = 0x03e1, non-contiguous
			0x1f, 0x00, 0x00, 0x00, // B = 0x001f
		}
		b := bmpBuilder{
			width: 2, height: 2, planes: 1, bitsPerPixel: 16,
			compression: 3, extra: masks,
			data: make([]byte, 2*rowBytes(2, 16)),
		}.Bytes()
		_, err := DecodeRegion(bytes.NewReader(b), image.Rect(0, 0, 2, 2))
		var re *RegionError
		if !errors.As(err, &re) {
			t.Fatalf("DecodeRegion error %v (%T), want *RegionError", err, err)
		}
		if !errors.Is(err, errInvalidBitfieldMask) {
			t.Errorf("DecodeRegion error %v, want errInvalidBitfieldMask", err)
		}
		// The G mask sits at offset 14+40+4 in the file.
		if re.Row != -1 || re.Offset != 14+40+4 {
			t.Errorf("RegionError row=%d offset=%d, want row=-1 offset=%d", re.Row, re.Offset, 14+40+4)
		}
		// Decode keeps its historical bare error.
		if _, err := Decode(bytes.NewReader(b)); err != errInvalidBitfieldMask {
			t.Errorf("Decode error %v, want errInvalidBitfieldMask", err)
		}
	})

	t.Run("excessive dimensions", func(t *testing.T) {
		b := bmpBuilder{
			width: 2147483647, height: 2147483647, planes: 1, bitsPerPixel: 24,
		}.Bytes()
		_, err := DecodeRegion(bytes.NewReader(b), image.Rect(0, 0, 1, 1))
		var re *RegionError
		if !errors.As(err, &re) {
			t.Fatalf("DecodeRegion error %v (%T), want *RegionError", err, err)
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("DecodeRegion error %v, want ErrUnsupported", err)
		}
		if re.Row != -1 || re.Offset != 18 {
			t.Errorf("RegionError row=%d offset=%d, want row=-1 offset=18", re.Row, re.Offset)
		}
		if _, err := Decode(bytes.NewReader(b)); err != ErrUnsupported {
			t.Errorf("Decode error %v, want ErrUnsupported", err)
		}
	})
}

// TestDecode16bppValues spot-checks absolute decoded values for 16 bpp.
func TestDecode16bppValues(t *testing.T) {
	// RGB565 via bitfields: pure red, pure green, pure blue, white.
	masks565 := []byte{
		0x00, 0xf8, 0x00, 0x00,
		0xe0, 0x07, 0x00, 0x00,
		0x1f, 0x00, 0x00, 0x00,
	}
	vals := []uint32{0xf800, 0x07e0, 0x001f, 0xffff}
	b := bmpBuilder{
		width: 4, height: 1, planes: 1, bitsPerPixel: 16,
		compression: 3, extra: masks565,
		data: packRows(4, 1, false, 16, func(x, y int) uint32 { return vals[x] }),
	}.Bytes()
	img, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []color.RGBA{
		{0xff, 0x00, 0x00, 0xff},
		{0x00, 0xff, 0x00, 0xff},
		{0x00, 0x00, 0xff, 0xff},
		{0xff, 0xff, 0xff, 0xff},
	}
	for x, w := range want {
		if g := img.At(x, 0); g != w {
			t.Errorf("pixel (%d, 0): got %v, want %v", x, g, w)
		}
	}

	// Default (BI_RGB) 16 bpp is RGB555: 0x7fff is white.
	b = bmpBuilder{
		width: 1, height: 1, planes: 1, bitsPerPixel: 16,
		data: packRows(1, 1, false, 16, func(x, y int) uint32 { return 0x7fff }),
	}.Bytes()
	img, err = Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if g, w := img.At(0, 0), (color.RGBA{0xff, 0xff, 0xff, 0xff}); g != w {
		t.Errorf("pixel (0, 0): got %v, want %v", g, w)
	}
}

// sparseReader is an io.ReadSeeker over a virtual file whose header comes
// from header and whose remaining bytes are a deterministic function of
// their offset. It records how much was actually read.
type sparseReader struct {
	header    []byte
	size      int64
	off       int64
	bytesRead int64
	reads     int
}

func (s *sparseReader) Read(p []byte) (int, error) {
	if s.off >= s.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if s.off+n > s.size {
		n = s.size - s.off
	}
	for i := int64(0); i < n; i++ {
		o := s.off + i
		if o < int64(len(s.header)) {
			p[i] = s.header[o]
		} else {
			p[i] = byte(o*13 ^ (o >> 3))
		}
	}
	s.off += n
	s.bytesRead += n
	s.reads++
	return int(n), nil
}

func (s *sparseReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.off = offset
	case io.SeekCurrent:
		s.off += offset
	case io.SeekEnd:
		s.off = s.size + offset
	}
	return s.off, nil
}

// TestDecodeRegionMemory checks that decoding a small region of a huge
// image neither reads nor allocates for the unrequested pixels.
func TestDecodeRegionMemory(t *testing.T) {
	const (
		width  = 30000
		height = 10000
	)
	header := bmpBuilder{
		width: width, height: height, planes: 1, bitsPerPixel: 24,
	}.Bytes() // no pixel data; only the 54-byte header
	rowLen := rowBytes(width, 24)
	sr := &sparseReader{
		header: header,
		size:   int64(len(header)) + int64(height)*int64(rowLen),
	}
	rect := image.Rect(12345, 6789, 12345+64, 6789+32)
	img, err := DecodeRegion(sr, rect)
	if err != nil {
		t.Fatalf("DecodeRegion: %v", err)
	}
	if !img.Bounds().Eq(rect) {
		t.Fatalf("bounds %v, want %v", img.Bounds(), rect)
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		t.Fatalf("DecodeRegion returned %T, want *image.RGBA", img)
	}
	if got, want := len(rgba.Pix), 64*32*4; got != want {
		t.Errorf("len(Pix) = %d, want %d", got, want)
	}
	// Only the header and the requested column range of the requested
	// rows may be read: 54 header bytes plus 32 rows of 64*3 bytes.
	wantRead := int64(54 + 32*64*3)
	if sr.bytesRead != wantRead {
		t.Errorf("bytes read = %d, want %d", sr.bytesRead, wantRead)
	}
	if sr.reads > 40 {
		t.Errorf("read calls = %d, want at most 40", sr.reads)
	}
	// Spot-check pixels against the sparse file pattern.
	pat := func(o int64) byte { return byte(o*13 ^ (o >> 3)) }
	for _, p := range []image.Point{{12345, 6789}, {12345 + 63, 6789 + 31}, {12345 + 17, 6789 + 9}} {
		fileRow := int64(height - 1 - p.Y)
		off := int64(len(header)) + fileRow*int64(rowLen) + int64(p.X)*3
		want := color.RGBA{pat(off + 2), pat(off + 1), pat(off + 0), 0xff}
		if got := rgba.At(p.X, p.Y); got != want {
			t.Errorf("pixel %v: got %v, want %v", p, got, want)
		}
	}
}

// TestDecodeRegionTestdata compares region decoding against full decoding
// for the packaged test images.
func TestDecodeRegionTestdata(t *testing.T) {
	testCases := []string{
		"colormap",
		"colormap-0",
		"colormap-251",
		"video-001",
		"yellow_rose-small",
		"yellow_rose-small-v5",
		"bmp_1bpp",
		"bmp_4bpp",
		"bmp_8bpp",
	}
	for _, tc := range testCases {
		t.Run(tc, func(t *testing.T) {
			f0, err := os.Open(testdataDir + tc + ".bmp")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer f0.Close()
			ref, err := Decode(f0)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			bounds := ref.Bounds()

			rects := []image.Rectangle{bounds}
			if bounds.Dx() >= 4 && bounds.Dy() >= 4 {
				rects = append(rects,
					image.Rect(1, 1, bounds.Dx()-1, bounds.Dy()-1),
					image.Rect(bounds.Dx()/4, bounds.Dy()/4, 3*bounds.Dx()/4, 3*bounds.Dy()/4),
					image.Rect(-5, -5, bounds.Dx()/2, bounds.Dy()/2),
				)
			}
			for _, rect := range rects {
				if _, err := f0.Seek(0, io.SeekStart); err != nil {
					t.Fatalf("Seek: %v", err)
				}
				got, err := DecodeRegion(f0, rect)
				if err != nil {
					t.Errorf("DecodeRegion(%v): %v", rect, err)
					continue
				}
				want := rect.Canon().Intersect(bounds)
				if !got.Bounds().Eq(want) {
					t.Errorf("DecodeRegion(%v): bounds %v, want %v", rect, got.Bounds(), want)
					continue
				}
				for y := want.Min.Y; y < want.Max.Y; y++ {
					for x := want.Min.X; x < want.Max.X; x++ {
						c0 := ref.At(x, y)
						c1 := got.At(x, y)
						r0, g0, b0, a0 := c0.RGBA()
						r1, g1, b1, a1 := c1.RGBA()
						if r0 != r1 || g0 != g1 || b0 != b1 || a0 != a1 {
							t.Fatalf("DecodeRegion(%v): pixel (%d, %d): got %v, want %v", rect, x, y, c1, c0)
						}
					}
				}
			}
		})
	}
}
