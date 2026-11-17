// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"io"
	"testing"
)

// rgba32TestImage returns an NRGBA image with the given size and a deterministic
// pattern including fully transparent, semi-transparent and opaque pixels.
func rgba32TestImage(w, h int) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*m.Stride + 4*x
			m.Pix[i+0] = uint8(0x10 + 0x20*x + 0x40*y)
			m.Pix[i+1] = uint8(0x30 + 0x11*x + 0x22*y)
			m.Pix[i+2] = uint8(0x50 + 0x07*x + 0x03*y)
			switch (x + y) % 3 {
			case 0:
				m.Pix[i+3] = 0x00 // fully transparent, color must survive
			case 1:
				m.Pix[i+3] = 0x80 // semi-transparent
			case 2:
				m.Pix[i+3] = 0xff // opaque
			}
		}
	}
	return m
}

// TestEncodeRGBA32Header checks every field of the BITMAPFILEHEADER and
// BITMAPV4HEADER written by EncodeWithOptions with RGBA32 set.
func TestEncodeRGBA32Header(t *testing.T) {
	const w, h = 3, 2 // odd width: no row padding at 32 bpp
	m := rgba32TestImage(w, h)
	buf := new(bytes.Buffer)
	if err := EncodeWithOptions(buf, m, &Options{RGBA32: true}); err != nil {
		t.Fatalf("EncodeWithOptions: %v", err)
	}
	b := buf.Bytes()

	const pixelOffset = 14 + 108
	wantSize := uint32(pixelOffset + 4*w*h)
	if len(b) != int(wantSize) {
		t.Fatalf("len(output) = %d, want %d", len(b), wantSize)
	}

	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

	got := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"signature", uint32(b[0]) | uint32(b[1])<<8, 'B' | 'M'<<8},
		{"file size", u32(2), wantSize},
		{"reserved 0", u32(6), 0},
		{"pixel offset", u32(10), pixelOffset},
		{"DIB header size", u32(14), 108},
		{"width", u32(18), w},
		{"height", u32(22), h},
		{"planes", uint32(u16(26)), 1},
		{"bits per pixel", uint32(u16(28)), 32},
		{"compression", u32(30), 3}, // BI_BITFIELDS
		{"image size", u32(34), 4 * w * h},
		{"x pixels per meter", u32(38), 0},
		{"y pixels per meter", u32(42), 0},
		{"colors used", u32(46), 0},
		{"colors important", u32(50), 0},
		{"red mask", u32(54), 0x00ff0000},
		{"green mask", u32(58), 0x0000ff00},
		{"blue mask", u32(62), 0x000000ff},
		{"alpha mask", u32(66), 0xff000000},
		{"color space", u32(70), 0x73524742}, // LCS_sRGB
	}
	for _, tc := range got {
		if tc.got != tc.want {
			t.Errorf("%s = %#x, want %#x", tc.name, tc.got, tc.want)
		}
	}
	for i := 74; i < 122; i++ {
		if b[i] != 0 {
			t.Errorf("DIB byte %d = %#x, want 0", i, b[i])
			break
		}
	}

	// Rows are stored bottom-up, BGR(A) and, for the default masks, in
	// little-endian 0xAARRGGBB form.
	for row := 0; row < h; row++ {
		y := h - 1 - row
		for x := 0; x < w; x++ {
			i := 4*x + y*m.Stride
			want := uint32(m.Pix[i+3])<<24 | uint32(m.Pix[i+0])<<16 |
				uint32(m.Pix[i+1])<<8 | uint32(m.Pix[i+2])
			off := pixelOffset + 4*(row*w+x)
			if got := u32(off); got != want {
				t.Errorf("pixel (%d, %d) = %#08x, want %#08x", x, y, got, want)
			}
		}
	}
}

// TestEncodeRGBA32RoundTrip checks that a compatible decoder (this package's
// own Decode, which accepts the default V4 masks) recovers exact
// non-premultiplied colors and alpha, including odd widths and semi-transparent
// and fully transparent pixels.
func TestEncodeRGBA32RoundTrip(t *testing.T) {
	for _, size := range []image.Point{{1, 1}, {3, 1}, {5, 2}, {7, 3}, {4, 4}} {
		m := rgba32TestImage(size.X, size.Y)
		buf := new(bytes.Buffer)
		if err := EncodeWithOptions(buf, m, &Options{RGBA32: true}); err != nil {
			t.Fatalf("%v: EncodeWithOptions: %v", size, err)
		}
		got, err := Decode(buf)
		if err != nil {
			t.Fatalf("%v: Decode: %v", size, err)
		}
		dm, ok := got.(*image.NRGBA)
		if !ok {
			t.Fatalf("%v: Decode returned %T, want *image.NRGBA", size, got)
		}
		if !dm.Bounds().Size().Eq(size) {
			t.Fatalf("%v: decoded size = %v", size, dm.Bounds().Size())
		}
		// Compare raw bytes, not At(...).RGBA(), so that the color of a
		// fully transparent pixel is actually checked.
		for y := 0; y < size.Y; y++ {
			for x := 0; x < size.X; x++ {
				i0 := y*m.Stride + 4*x
				i1 := y*dm.Stride + 4*x
				gotPx := dm.Pix[i1 : i1+4]
				wantPx := m.Pix[i0 : i0+4]
				if !bytes.Equal(gotPx, wantPx) {
					t.Errorf("%v: pixel (%d, %d) = %v, want %v", size, x, y, gotPx, wantPx)
				}
			}
		}
	}
}

// TestEncodeRGBA32Premultiplied checks that *image.RGBA input is written
// non-premultiplied.
func TestEncodeRGBA32Premultiplied(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	// Semi-transparent premultiplied red: (128, 0, 0, 128).
	src.Pix[0], src.Pix[1], src.Pix[2], src.Pix[3] = 0x80, 0, 0, 0x80
	// Fully transparent black.
	src.Pix[4], src.Pix[5], src.Pix[6], src.Pix[7] = 0, 0, 0, 0

	buf := new(bytes.Buffer)
	if err := EncodeWithOptions(buf, src, &Options{RGBA32: true}); err != nil {
		t.Fatalf("EncodeWithOptions: %v", err)
	}
	b := buf.Bytes()
	px := b[122:]
	// One bottom-up row, left to right.
	// Round trip through RGBA gives back approximately (0x80, 0, 0, 0x80):
	// 0x80 premultiplied by alpha 0x80 becomes 0xff non-premultiplied.
	if got, want := binary.LittleEndian.Uint32(px[0:]), uint32(0x80ff0000); got != want {
		t.Errorf("semi-transparent pixel = %#08x, want %#08x", got, want)
	}
	if got, want := binary.LittleEndian.Uint32(px[4:]), uint32(0x00000000); got != want {
		t.Errorf("transparent pixel = %#08x, want %#08x", got, want)
	}
}

// TestEncodeRGBA32GenericImage checks that images that are neither *image.NRGBA
// nor *image.RGBA are converted to non-premultiplied RGBA.
func TestEncodeRGBA32GenericImage(t *testing.T) {
	m := image.NewGray(image.Rect(0, 0, 2, 1))
	m.Pix[0], m.Pix[1] = 0x12, 0xfe

	buf := new(bytes.Buffer)
	if err := EncodeWithOptions(buf, m, &Options{RGBA32: true}); err != nil {
		t.Fatalf("EncodeWithOptions: %v", err)
	}
	b := buf.Bytes()
	px := b[122:]
	// One bottom-up row, left to right: 0x12 then 0xfe, both opaque.
	if got, want := binary.LittleEndian.Uint32(px[0:]), uint32(0xff121212); got != want {
		t.Errorf("first pixel = %#08x, want %#08x", got, want)
	}
	if got, want := binary.LittleEndian.Uint32(px[4:]), uint32(0xfffefefe); got != want {
		t.Errorf("second pixel = %#08x, want %#08x", got, want)
	}

	// Also check a type whose At method returns non-premultiplied colors.
	pal := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{
		color.NRGBA{R: 0x11, G: 0x22, B: 0x33, A: 0x44},
	})
	buf.Reset()
	if err := EncodeWithOptions(buf, pal, &Options{RGBA32: true}); err != nil {
		t.Fatalf("EncodeWithOptions paletted: %v", err)
	}
	if got, want := binary.LittleEndian.Uint32(buf.Bytes()[122:]), uint32(0x44112233); got != want {
		t.Errorf("paletted pixel = %#08x, want %#08x", got, want)
	}
}

// TestEncodeRGBA32CustomMasks checks a non-default channel order.
func TestEncodeRGBA32CustomMasks(t *testing.T) {
	opt := &Options{
		RGBA32:    true,
		RedMask:   0x000000ff,
		GreenMask: 0x0000ff00,
		BlueMask:  0x00ff0000,
		AlphaMask: 0xff000000,
	}
	m := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	m.Pix[0], m.Pix[1], m.Pix[2], m.Pix[3] = 0x11, 0x22, 0x33, 0x44
	buf := new(bytes.Buffer)
	if err := EncodeWithOptions(buf, m, opt); err != nil {
		t.Fatalf("EncodeWithOptions: %v", err)
	}
	b := buf.Bytes()
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
	if got, want := u32(54), opt.RedMask; got != want {
		t.Errorf("red mask = %#08x, want %#08x", got, want)
	}
	if got, want := u32(58), opt.GreenMask; got != want {
		t.Errorf("green mask = %#08x, want %#08x", got, want)
	}
	if got, want := u32(62), opt.BlueMask; got != want {
		t.Errorf("blue mask = %#08x, want %#08x", got, want)
	}
	if got, want := u32(66), opt.AlphaMask; got != want {
		t.Errorf("alpha mask = %#08x, want %#08x", got, want)
	}
	// ABGR in memory: alpha 0x44, blue 0x33, green 0x22, red 0x11.
	if got, want := u32(122), uint32(0x44332211); got != want {
		t.Errorf("pixel = %#08x, want %#08x", got, want)
	}
}

// TestEncodeRGBA32ZeroMasks checks that zero masks select the default layout.
func TestEncodeRGBA32ZeroMasks(t *testing.T) {
	m := rgba32TestImage(2, 2)
	withZero := new(bytes.Buffer)
	if err := EncodeWithOptions(withZero, m, &Options{RGBA32: true}); err != nil {
		t.Fatalf("EncodeWithOptions: %v", err)
	}
	explicit := new(bytes.Buffer)
	if err := EncodeWithOptions(explicit, m, &Options{
		RGBA32:    true,
		RedMask:   0x00ff0000,
		GreenMask: 0x0000ff00,
		BlueMask:  0x000000ff,
		AlphaMask: 0xff000000,
	}); err != nil {
		t.Fatalf("EncodeWithOptions explicit: %v", err)
	}
	if !bytes.Equal(withZero.Bytes(), explicit.Bytes()) {
		t.Error("zero masks and explicit default masks produced different output")
	}
}

// TestEncodeRGBA32InvalidMasks checks mask validation.
func TestEncodeRGBA32InvalidMasks(t *testing.T) {
	m := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	for _, tc := range []struct {
		name string
		opt  Options
	}{
		{"overlap", Options{RGBA32: true, RedMask: 0x00ff0000, GreenMask: 0x00ff0000, BlueMask: 0x000000ff, AlphaMask: 0xff000000}},
		{"non-contiguous", Options{RGBA32: true, RedMask: 0x00000011, GreenMask: 0x0000ff00, BlueMask: 0x00ff0000, AlphaMask: 0xff000000}},
		{"not 8 bits", Options{RGBA32: true, RedMask: 0x0000ff00 | 0x00000001, GreenMask: 0x00ff0000, BlueMask: 0x000000fe, AlphaMask: 0xff000000}},
		{"one zero", Options{RGBA32: true, RedMask: 0x00ff0000, GreenMask: 0x0000ff00, BlueMask: 0x000000ff}},
		{"not covering", Options{RGBA32: true, RedMask: 0x0000ff00, GreenMask: 0x000000ff, BlueMask: 0x00000000 | 0xff000000, AlphaMask: 0}},
		{"masks without RGBA32", Options{RedMask: 0x00ff0000}},
	} {
		err := EncodeWithOptions(io.Discard, m, &tc.opt)
		if !errors.Is(err, ErrInvalidMasks) {
			t.Errorf("%s: error = %v, want ErrInvalidMasks", tc.name, err)
		}
	}
}

// TestEncodeRGBA32Empty checks zero-sized images still produce a valid header.
func TestEncodeRGBA32Empty(t *testing.T) {
	for _, size := range []image.Point{{0, 0}, {0, 5}, {5, 0}} {
		m := image.NewNRGBA(image.Rect(0, 0, size.X, size.Y))
		buf := new(bytes.Buffer)
		if err := EncodeWithOptions(buf, m, &Options{RGBA32: true}); err != nil {
			t.Fatalf("%v: EncodeWithOptions: %v", size, err)
		}
		b := buf.Bytes()
		if len(b) != 122 {
			t.Errorf("%v: len = %d, want 122", size, len(b))
		}
		if got, want := binary.LittleEndian.Uint32(b[2:]), uint32(122); got != want {
			t.Errorf("%v: file size = %d, want %d", size, got, want)
		}
		if got, want := binary.LittleEndian.Uint32(b[34:]), uint32(0); got != want {
			t.Errorf("%v: image size = %d, want %d", size, got, want)
		}
		if got := int32(binary.LittleEndian.Uint32(b[18:])); got != int32(size.X) {
			t.Errorf("%v: width = %d", size, got)
		}
		if got := int32(binary.LittleEndian.Uint32(b[22:])); got != int32(size.Y) {
			t.Errorf("%v: height = %d", size, got)
		}
	}
}

// bigImage reports huge bounds without allocating pixel storage.
type bigImage struct{ image.Point }

func (b bigImage) ColorModel() color.Model { return color.NRGBAModel }
func (b bigImage) Bounds() image.Rectangle { return image.Rect(0, 0, b.X, b.Y) }
func (b bigImage) At(x, y int) color.Color { return color.NRGBA{} }

// TestEncodeTooLarge checks that dimensions or file sizes that do not fit in a
// BMP header are rejected instead of overflowing.
func TestEncodeTooLarge(t *testing.T) {
	for _, m := range []image.Image{
		bigImage{image.Point{1 << 20, 1 << 20}}, // 4 GiB of pixel data
		bigImage{image.Point{1 << 30, 1 << 30}}, // overflows int multiply
		bigImage{image.Point{maxInt32Const, maxInt32Const}},
		bigImage{image.Point{maxInt32Const + 1, 0}}, // width does not fit
		bigImage{image.Point{0, maxInt32Const + 1}}, // height does not fit
	} {
		if err := EncodeWithOptions(io.Discard, m, &Options{RGBA32: true}); !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%v: RGBA32 error = %v, want ErrImageTooLarge", m.Bounds().Size(), err)
		}
		if err := Encode(io.Discard, m); !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%v: Encode error = %v, want ErrImageTooLarge", m.Bounds().Size(), err)
		}
	}
}

const maxInt32Const = 1<<31 - 1

// shortWriter accepts at most n bytes and then fails with a short write.
type shortWriter struct{ n int }

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, nil
	}
	if len(p) > w.n {
		n := w.n
		w.n = 0
		return n, nil
	}
	w.n -= len(p)
	return len(p), nil
}

// TestEncodeShortWrite checks that a short write is reported as
// io.ErrShortWrite for both header and pixel data.
func TestEncodeShortWrite(t *testing.T) {
	m := rgba32TestImage(4, 4)
	for _, opt := range []*Options{nil, {RGBA32: true}} {
		// Fail while writing the header.
		if err := EncodeWithOptions(&shortWriter{n: 3}, m, opt); !errors.Is(err, io.ErrShortWrite) {
			t.Errorf("opt=%v: header short write error = %v, want io.ErrShortWrite", opt, err)
		}
		// Fail while writing pixel data (not the header).
		if err := EncodeWithOptions(&shortWriter{n: 100}, m, opt); !errors.Is(err, io.ErrShortWrite) {
			t.Errorf("opt=%v: pixel short write error = %v, want io.ErrShortWrite", opt, err)
		}
	}
}

// TestEncodeDefaultUnchanged pins the default Encode output for a small opaque
// image so that the option plumbing cannot silently change the byte format.
func TestEncodeDefaultUnchanged(t *testing.T) {
	m := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	m.Pix[0], m.Pix[1], m.Pix[2], m.Pix[3] = 0x11, 0x22, 0x33, 0xff

	buf := new(bytes.Buffer)
	if err := Encode(buf, m); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []byte{
		'B', 'M',
		58, 0, 0, 0, // file size
		0, 0, 0, 0, // reserved
		54, 0, 0, 0, // pixel offset
		40, 0, 0, 0, // DIB header size
		1, 0, 0, 0, // width
		1, 0, 0, 0, // height
		1, 0, // planes
		24, 0, // bits per pixel
		0, 0, 0, 0, // BI_RGB
		4, 0, 0, 0, // image size (one padded 4-byte row)
		0, 0, 0, 0, // x pixels per meter
		0, 0, 0, 0, // y pixels per meter
		0, 0, 0, 0, // colors used
		0, 0, 0, 0, // colors important
		0x33, 0x22, 0x11, 0, // BGR + padding
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("Encode output = % x\nwant            % x", buf.Bytes(), want)
	}

	// A nil Options must behave exactly like Encode.
	buf2 := new(bytes.Buffer)
	if err := EncodeWithOptions(buf2, m, nil); err != nil {
		t.Fatalf("EncodeWithOptions: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Error("Encode and EncodeWithOptions(nil) differ")
	}
}
