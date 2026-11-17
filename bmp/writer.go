// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"math/bits"

	"golang.org/x/image/internal/safemath"
)

// ErrImageTooLarge is returned by EncodeWithOptions when the image dimensions
// or the resulting file size cannot be represented in a BMP file.
var ErrImageTooLarge = errors.New("bmp: image too large")

// ErrInvalidMasks is returned by EncodeWithOptions when the requested RGBA bit
// masks are not four non-overlapping, contiguous, 8-bit fields covering all
// 32 bits.
var ErrInvalidMasks = errors.New("bmp: invalid RGBA masks")

// Options configures EncodeWithOptions.
type Options struct {
	// RGBA32 requests a 32-bit-per-pixel BMP that uses BI_BITFIELDS
	// compression and explicit red, green, blue and alpha bit masks.
	//
	// Pixels are stored in non-premultiplied form. In particular, the color
	// components of a fully transparent pixel are preserved, and the alpha
	// channel is written verbatim for an *image.NRGBA source. Images that are
	// not *image.NRGBA or *image.RGBA are converted to non-premultiplied 8-bit
	// RGBA via color.NRGBAModel.
	RGBA32 bool

	// RedMask, GreenMask, BlueMask and AlphaMask optionally override the
	// channel masks used by RGBA32. If all four are zero, the conventional
	// masks 0x00ff0000, 0x0000ff00, 0x000000ff and 0xff000000 are used.
	//
	// Otherwise all four must be non-zero, contiguous, non-overlapping 8-bit
	// masks whose union covers all 32 bits. These fields must be zero unless
	// RGBA32 is true.
	RedMask   uint32
	GreenMask uint32
	BlueMask  uint32
	AlphaMask uint32
}

const (
	// biBitfields is the BMP compression type BI_BITFIELDS.
	biBitfields = 3

	// lcsSRGB is the BITMAPV4HEADER color space type LCS_sRGB ('sRGB').
	lcsSRGB = 0x73524742

	// rgba32DIBHeaderSize is the size of a BITMAPV4HEADER, the smallest DIB
	// header that can carry both RGBA bit masks and an alpha mask.
	rgba32DIBHeaderSize = 108

	// rgba32PixelOffset is the offset of the pixel data in a 32-bit
	// BI_BITFIELDS BMP written by this package: the 14-byte file header
	// followed by the 108-byte DIB header.
	rgba32PixelOffset = 14 + rgba32DIBHeaderSize
)

type rgba32Masks struct {
	red, green, blue, alpha                     uint32
	redShift, greenShift, blueShift, alphaShift uint
}

// masks returns the masks selected by o, after validating them.
func (o *Options) masks() (rgba32Masks, error) {
	m := rgba32Masks{
		red:   o.RedMask,
		green: o.GreenMask,
		blue:  o.BlueMask,
		alpha: o.AlphaMask,
	}
	if m.red|m.green|m.blue|m.alpha == 0 {
		m.red = 0x00ff0000
		m.green = 0x0000ff00
		m.blue = 0x000000ff
		m.alpha = 0xff000000
	}

	const eightBits = uint32(0xff)
	var seen uint32
	for _, f := range []struct {
		name string
		mask uint32
	}{
		{"red", m.red},
		{"green", m.green},
		{"blue", m.blue},
		{"alpha", m.alpha},
	} {
		if f.mask == 0 {
			return rgba32Masks{}, fmt.Errorf("%w: %s mask is zero", ErrInvalidMasks, f.name)
		}
		if n := bits.OnesCount32(f.mask); n != 8 {
			return rgba32Masks{}, fmt.Errorf("%w: %s mask %#08x has %d bits, want 8", ErrInvalidMasks, f.name, f.mask, n)
		}
		shift := uint(bits.TrailingZeros32(f.mask))
		if f.mask != eightBits<<shift {
			return rgba32Masks{}, fmt.Errorf("%w: %s mask %#08x is not contiguous", ErrInvalidMasks, f.name, f.mask)
		}
		if seen&f.mask != 0 {
			return rgba32Masks{}, fmt.Errorf("%w: %s mask %#08x overlaps another mask", ErrInvalidMasks, f.name, f.mask)
		}
		seen |= f.mask
	}
	if seen != math.MaxUint32 {
		return rgba32Masks{}, fmt.Errorf("%w: masks do not cover all 32 bits", ErrInvalidMasks)
	}

	m.redShift = uint(bits.TrailingZeros32(m.red))
	m.greenShift = uint(bits.TrailingZeros32(m.green))
	m.blueShift = uint(bits.TrailingZeros32(m.blue))
	m.alphaShift = uint(bits.TrailingZeros32(m.alpha))
	return m, nil
}

type header struct {
	sigBM           [2]byte
	fileSize        uint32
	resverved       [2]uint16
	pixOffset       uint32
	dibHeaderSize   uint32
	width           uint32
	height          uint32
	colorPlane      uint16
	bpp             uint16
	compression     uint32
	imageSize       uint32
	xPixelsPerMeter uint32
	yPixelsPerMeter uint32
	colorUse        uint32
	colorImportant  uint32
}

// rgba32Header is the part of a BITMAPV4HEADER that follows the 40-byte
// BITMAPINFOHEADER fields. Together they make up a 108-byte DIB header.
type rgba32Header struct {
	redMask    uint32
	greenMask  uint32
	blueMask   uint32
	alphaMask  uint32
	csType     uint32
	endpoints  [9]uint32 // CIE XYZ triples for red, green and blue.
	gammaRed   uint32
	gammaGreen uint32
	gammaBlue  uint32
}

// writeFull writes p to w and reports a short write as io.ErrShortWrite.
func writeFull(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// pack returns the little-endian 32-bit pixel value for the given
// non-premultiplied color components.
func (m *rgba32Masks) pack(r, g, b, a uint8) uint32 {
	return uint32(r)<<m.redShift |
		uint32(g)<<m.greenShift |
		uint32(b)<<m.blueShift |
		uint32(a)<<m.alphaShift
}

// unpremultiply converts an 8-bit premultiplied component to non-premultiplied.
func unpremultiply(c, a uint8) uint8 {
	if a == 0 {
		return 0
	}
	if a == 0xff {
		return c
	}
	// Saturate rather than wrap if the input is not actually premultiplied
	// (color.RGBA permits such values), so the stored color stays in range.
	v := uint32(c) * 0xffff / uint32(a)
	if v > 0xffff {
		return 0xff
	}
	return uint8(v >> 8)
}

// encodeRGBA32 writes the pixel rows of a 32-bit BI_BITFIELDS BMP, bottom-up.
// The file stores non-premultiplied color components.
func encodeRGBA32(w io.Writer, m image.Image, masks rgba32Masks) error {
	b := m.Bounds()
	dx, dy := b.Dx(), b.Dy()
	if dx == 0 || dy == 0 {
		return nil
	}
	// 32 bits per pixel is inherently 4-byte aligned, so there is no
	// per-row padding.
	buf := make([]byte, 4*dx)

	switch m := m.(type) {
	case *image.NRGBA:
		for y := b.Max.Y - 1; y >= b.Min.Y; y-- {
			i := m.PixOffset(b.Min.X, y)
			for off := 0; off < len(buf); off += 4 {
				// NRGBA is already non-premultiplied: copy every byte,
				// including the color of fully transparent pixels.
				binary.LittleEndian.PutUint32(buf[off:],
					masks.pack(m.Pix[i+0], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3]))
				i += 4
			}
			if err := writeFull(w, buf); err != nil {
				return err
			}
		}
	case *image.RGBA:
		for y := b.Max.Y - 1; y >= b.Min.Y; y-- {
			i := m.PixOffset(b.Min.X, y)
			for off := 0; off < len(buf); off += 4 {
				// RGBA is premultiplied; undo that so the file is
				// non-premultiplied, as BI_BITFIELDS RGBA expects.
				a := m.Pix[i+3]
				binary.LittleEndian.PutUint32(buf[off:], masks.pack(
					unpremultiply(m.Pix[i+0], a),
					unpremultiply(m.Pix[i+1], a),
					unpremultiply(m.Pix[i+2], a),
					a))
				i += 4
			}
			if err := writeFull(w, buf); err != nil {
				return err
			}
		}
	default:
		for y := b.Max.Y - 1; y >= b.Min.Y; y-- {
			off := 0
			for x := b.Min.X; x < b.Max.X; x++ {
				c := color.NRGBAModel.Convert(m.At(x, y)).(color.NRGBA)
				binary.LittleEndian.PutUint32(buf[off:], masks.pack(c.R, c.G, c.B, c.A))
				off += 4
			}
			if err := writeFull(w, buf); err != nil {
				return err
			}
		}
	}
	return nil
}

// rowBytes returns the 4-byte-aligned number of bytes in one row of an image
// with dx pixels and bytesPerPixel bytes per pixel.
func rowBytes(dx, bytesPerPixel int) (int, bool) {
	n, ok := safemath.Mul3(dx, bytesPerPixel, 1)
	if !ok || n > math.MaxInt-3 {
		return 0, false
	}
	return (n + 3) &^ 3, true
}

// dimensionsOK reports whether d fits in the signed 32-bit width and height
// fields of a BMP header. Encode always writes a positive (bottom-up) height.
func dimensionsOK(d image.Point) bool {
	return d.X <= math.MaxInt32 && d.Y <= math.MaxInt32
}

// imageBytes returns the size in bytes of dy rows of step bytes each, plus the
// pixel offset, checking that the BMP file size fits in a uint32.
func imageBytes(dy, step, offset int) (imageSize int, fileSize uint32, ok bool) {
	imageSize, ok = safemath.Mul3(dy, step, 1)
	if !ok {
		return 0, 0, false
	}
	total := uint64(offset) + uint64(imageSize)
	if total > math.MaxUint32 {
		return 0, 0, false
	}
	return imageSize, uint32(total), true
}

func encodePaletted(w io.Writer, pix []uint8, dx, dy, stride, step int) error {
	var padding []byte
	if dx < step {
		padding = make([]byte, step-dx)
	}
	for y := dy - 1; y >= 0; y-- {
		min := y*stride + 0
		max := y*stride + dx
		if err := writeFull(w, pix[min:max]); err != nil {
			return err
		}
		if padding != nil {
			if err := writeFull(w, padding); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeRGBA(w io.Writer, pix []uint8, dx, dy, stride, step int, opaque bool) error {
	buf := make([]byte, step)
	if opaque {
		for y := dy - 1; y >= 0; y-- {
			min := y*stride + 0
			max := y*stride + dx*4
			off := 0
			for i := min; i < max; i += 4 {
				buf[off+2] = pix[i+0]
				buf[off+1] = pix[i+1]
				buf[off+0] = pix[i+2]
				off += 3
			}
			if err := writeFull(w, buf); err != nil {
				return err
			}
		}
	} else {
		for y := dy - 1; y >= 0; y-- {
			min := y*stride + 0
			max := y*stride + dx*4
			off := 0
			for i := min; i < max; i += 4 {
				a := uint32(pix[i+3])
				if a == 0 {
					buf[off+2] = 0
					buf[off+1] = 0
					buf[off+0] = 0
					buf[off+3] = 0
					off += 4
					continue
				} else if a == 0xff {
					buf[off+2] = pix[i+0]
					buf[off+1] = pix[i+1]
					buf[off+0] = pix[i+2]
					buf[off+3] = 0xff
					off += 4
					continue
				}
				buf[off+2] = uint8(((uint32(pix[i+0]) * 0xffff) / a) >> 8)
				buf[off+1] = uint8(((uint32(pix[i+1]) * 0xffff) / a) >> 8)
				buf[off+0] = uint8(((uint32(pix[i+2]) * 0xffff) / a) >> 8)
				buf[off+3] = uint8(a)
				off += 4
			}
			if err := writeFull(w, buf); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeNRGBA(w io.Writer, pix []uint8, dx, dy, stride, step int, opaque bool) error {
	buf := make([]byte, step)
	if opaque {
		for y := dy - 1; y >= 0; y-- {
			min := y*stride + 0
			max := y*stride + dx*4
			off := 0
			for i := min; i < max; i += 4 {
				buf[off+2] = pix[i+0]
				buf[off+1] = pix[i+1]
				buf[off+0] = pix[i+2]
				off += 3
			}
			if err := writeFull(w, buf); err != nil {
				return err
			}
		}
	} else {
		for y := dy - 1; y >= 0; y-- {
			min := y*stride + 0
			max := y*stride + dx*4
			off := 0
			for i := min; i < max; i += 4 {
				buf[off+2] = pix[i+0]
				buf[off+1] = pix[i+1]
				buf[off+0] = pix[i+2]
				buf[off+3] = pix[i+3]
				off += 4
			}
			if err := writeFull(w, buf); err != nil {
				return err
			}
		}
	}
	return nil
}

func encode(w io.Writer, m image.Image, step int) error {
	b := m.Bounds()
	buf := make([]byte, step)
	for y := b.Max.Y - 1; y >= b.Min.Y; y-- {
		off := 0
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, b, _ := m.At(x, y).RGBA()
			buf[off+2] = byte(r >> 8)
			buf[off+1] = byte(g >> 8)
			buf[off+0] = byte(b >> 8)
			off += 3
		}
		if err := writeFull(w, buf); err != nil {
			return err
		}
	}
	return nil
}

// Encode writes the image m to w in BMP format.
//
// The image is written as an 8-bit paletted BMP for *image.Gray and
// *image.Paletted sources, and as a 24-bit or 32-bit BMP otherwise. Alpha in
// *image.RGBA and *image.NRGBA sources is preserved only when the image is not
// opaque; the 32-bit output uses BI_RGB compression.
//
// To force a 32-bit BI_BITFIELDS BMP with explicit RGBA bit masks, use
// EncodeWithOptions.
func Encode(w io.Writer, m image.Image) error {
	return EncodeWithOptions(w, m, nil)
}

// EncodeWithOptions writes the image m to w in BMP format, using opt to
// customize the output. A nil opt is equivalent to Encode.
func EncodeWithOptions(w io.Writer, m image.Image, opt *Options) error {
	if opt != nil {
		if !opt.RGBA32 && (opt.RedMask|opt.GreenMask|opt.BlueMask|opt.AlphaMask) != 0 {
			return fmt.Errorf("%w: masks require RGBA32", ErrInvalidMasks)
		}
		if opt.RGBA32 {
			return encodeRGBA32Options(w, m, opt)
		}
	}
	return encodeDefault(w, m)
}

// encodeRGBA32Options writes m as a 32-bit BI_BITFIELDS BMP with a
// BITMAPV4HEADER.
func encodeRGBA32Options(w io.Writer, m image.Image, opt *Options) error {
	masks, err := opt.masks()
	if err != nil {
		return err
	}

	// An image with zero width or height still gets a complete header with
	// zero dimensions, and no pixel data.
	d := m.Bounds().Size()
	if d.X < 0 || d.Y < 0 {
		return errors.New("bmp: negative bounds")
	}
	if !dimensionsOK(d) {
		return ErrImageTooLarge
	}
	step, ok := rowBytes(d.X, 4)
	if !ok {
		return ErrImageTooLarge
	}
	imageSize, fileSize, ok := imageBytes(d.Y, step, rgba32PixelOffset)
	if !ok {
		return ErrImageTooLarge
	}

	h := &header{
		sigBM:         [2]byte{'B', 'M'},
		fileSize:      fileSize,
		pixOffset:     rgba32PixelOffset,
		dibHeaderSize: rgba32DIBHeaderSize,
		width:         uint32(d.X),
		height:        uint32(d.Y),
		colorPlane:    1,
		bpp:           32,
		compression:   biBitfields,
		imageSize:     uint32(imageSize),
	}
	h4 := &rgba32Header{
		redMask:   masks.red,
		greenMask: masks.green,
		blueMask:  masks.blue,
		alphaMask: masks.alpha,
		csType:    lcsSRGB,
	}

	var buf [rgba32PixelOffset]byte
	var hdr bytes.Buffer
	if err := binary.Write(&hdr, binary.LittleEndian, h); err != nil {
		return err
	}
	if err := binary.Write(&hdr, binary.LittleEndian, h4); err != nil {
		return err
	}
	if n := copy(buf[:], hdr.Bytes()); n != len(buf) {
		return errors.New("bmp: internal error: wrong header size")
	}

	if err := writeFull(w, buf[:]); err != nil {
		return err
	}
	return encodeRGBA32(w, m, masks)
}

// encodeDefault implements Encode's original, option-free behavior.
func encodeDefault(w io.Writer, m image.Image) error {
	d := m.Bounds().Size()
	if d.X < 0 || d.Y < 0 {
		return errors.New("bmp: negative bounds")
	}
	if !dimensionsOK(d) {
		return ErrImageTooLarge
	}
	h := &header{
		sigBM:         [2]byte{'B', 'M'},
		fileSize:      14 + 40,
		pixOffset:     14 + 40,
		dibHeaderSize: 40,
		width:         uint32(d.X),
		height:        uint32(d.Y),
		colorPlane:    1,
	}

	var step int
	var palette []byte
	var opaque bool
	var ok bool
	switch m := m.(type) {
	case *image.Gray:
		step, ok = rowBytes(d.X, 1)
		palette = make([]byte, 1024)
		for i := 0; i < 256; i++ {
			palette[i*4+0] = uint8(i)
			palette[i*4+1] = uint8(i)
			palette[i*4+2] = uint8(i)
			palette[i*4+3] = 0xFF
		}
		h.bpp = 8

	case *image.Paletted:
		step, ok = rowBytes(d.X, 1)
		palette = make([]byte, 1024)
		for i := 0; i < len(m.Palette) && i < 256; i++ {
			r, g, b, _ := m.Palette[i].RGBA()
			palette[i*4+0] = uint8(b >> 8)
			palette[i*4+1] = uint8(g >> 8)
			palette[i*4+2] = uint8(r >> 8)
			palette[i*4+3] = 0xFF
		}
		h.bpp = 8
	case *image.RGBA:
		opaque = m.Opaque()
		if opaque {
			step, ok = rowBytes(d.X, 3)
			h.bpp = 24
		} else {
			step, ok = rowBytes(d.X, 4)
			h.bpp = 32
		}
	case *image.NRGBA:
		opaque = m.Opaque()
		if opaque {
			step, ok = rowBytes(d.X, 3)
			h.bpp = 24
		} else {
			step, ok = rowBytes(d.X, 4)
			h.bpp = 32
		}
	default:
		step, ok = rowBytes(d.X, 3)
		h.bpp = 24
	}
	if !ok {
		return ErrImageTooLarge
	}
	offset := 14 + 40 + len(palette)
	imageSize, fileSize, ok := imageBytes(d.Y, step, offset)
	if !ok {
		return ErrImageTooLarge
	}
	h.imageSize = uint32(imageSize)
	h.fileSize = fileSize
	h.pixOffset = uint32(offset)

	if err := binary.Write(w, binary.LittleEndian, h); err != nil {
		return err
	}
	if palette != nil {
		if err := binary.Write(w, binary.LittleEndian, palette); err != nil {
			return err
		}
	}

	if d.X == 0 || d.Y == 0 {
		return nil
	}

	switch m := m.(type) {
	case *image.Gray:
		return encodePaletted(w, m.Pix, d.X, d.Y, m.Stride, step)
	case *image.Paletted:
		return encodePaletted(w, m.Pix, d.X, d.Y, m.Stride, step)
	case *image.RGBA:
		return encodeRGBA(w, m.Pix, d.X, d.Y, m.Stride, step, opaque)
	case *image.NRGBA:
		return encodeNRGBA(w, m.Pix, d.X, d.Y, m.Stride, step, opaque)
	}
	return encode(w, m, step)
}
