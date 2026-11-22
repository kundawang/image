// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bmp

import (
	"fmt"
	"image"
	"image/color"
	"io"
	"math/bits"
)

// RegionError describes an error encountered while decoding a region of a
// BMP image. It records where in the image and where in the file the
// failure occurred.
type RegionError struct {
	Op     string // the operation that failed: "parse", "seek" or "read"
	Row    int    // the image row (y coordinate) being decoded, or -1 if not row-specific
	Offset int64  // the file offset being processed, or -1 if unknown
	Err    error  // the underlying error
}

func (e *RegionError) Error() string {
	s := "bmp: "
	if e.Op != "" {
		s += e.Op + " "
	}
	if e.Row >= 0 {
		s += fmt.Sprintf("row %d: ", e.Row)
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf("offset %d: ", e.Offset)
	}
	return s + e.Err.Error()
}

func (e *RegionError) Unwrap() error { return e.Err }

// DecodeRegion reads a BMP image from rs and decodes only the pixels that
// lie within rect, expressed in image coordinates with (0, 0) at the
// top-left corner. It seeks past unrequested rows and columns instead of
// reading them, and never allocates pixel data outside the requested
// region, so it is suitable for extracting thumbnails or tiles from very
// large images. rs must be positioned at the start of the BMP file.
//
// rect is canonicalized first (see image.Rectangle.Canon), so rectangles
// with inverted corners are accepted. The bounds of the returned image
// equal rect intersected with the image bounds: a non-zero rect.Min is
// preserved, and if the intersection is empty (including when rect itself
// is empty) the returned image has empty bounds and no pixel data, and the
// error is nil.
//
// The concrete type of the returned image matches what Decode would return
// for the same file: *image.Paletted for 1, 2, 4 and 8 bits per pixel,
// *image.RGBA for 16 (without an alpha mask) and 24 bits per pixel, and
// *image.NRGBA otherwise.
//
// Errors are returned as *RegionError, recording the image row and file
// offset being processed when the failure occurred.
//
// Limitation: The file must be 1, 2, 4, 8, 16, 24 or 32 bits per pixel.
func DecodeRegion(rs io.ReadSeeker, rect image.Rectangle) (image.Image, error) {
	h, err := decodeConfig(rs)
	if err != nil {
		return nil, err
	}
	r := rect.Canon().Intersect(image.Rect(0, 0, h.config.Width, h.config.Height))
	img := h.newImage(r)
	if r.Empty() {
		return img, nil
	}
	rr := &regionReader{rs: rs, h: h, buf: make([]byte, h.rowBytes)}
	conv := h.converter()
	b0, b1 := h.columnBytes(r.Min.X, r.Max.X)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		row, off, err := rr.readRow(y, b0, b1)
		if err != nil {
			return nil, err
		}
		if err := conv(y, off, h.rowPix(img, y), row, r.Min.X, r.Max.X); err != nil {
			return nil, err
		}
	}
	return img, nil
}

// DecodeRegionFunc reads a BMP image from rs like DecodeRegion, but instead
// of assembling a single image it calls f once for each row of the
// requested region, in increasing y order, with a 1-pixel-high image whose
// bounds are image.Rect(x0, y, x1, y+1). The row image is freshly allocated
// for each call, so f may retain it. If f returns an error, decoding stops
// and that error is returned.
//
// Rows and columns outside the image bounds are never read; if rect does
// not intersect the image, f is not called at all and the error is nil.
//
// Limitation: The file must be 1, 2, 4, 8, 16, 24 or 32 bits per pixel.
func DecodeRegionFunc(rs io.ReadSeeker, rect image.Rectangle, f func(y int, row image.Image) error) error {
	h, err := decodeConfig(rs)
	if err != nil {
		return err
	}
	r := rect.Canon().Intersect(image.Rect(0, 0, h.config.Width, h.config.Height))
	if r.Empty() {
		return nil
	}
	rr := &regionReader{rs: rs, h: h, buf: make([]byte, h.rowBytes)}
	conv := h.converter()
	b0, b1 := h.columnBytes(r.Min.X, r.Max.X)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		rowImg := h.newImage(image.Rect(r.Min.X, y, r.Max.X, y+1))
		row, off, err := rr.readRow(y, b0, b1)
		if err != nil {
			return err
		}
		if err := conv(y, off, h.rowPix(rowImg, y), row, r.Min.X, r.Max.X); err != nil {
			return err
		}
		if err := f(y, rowImg); err != nil {
			return err
		}
	}
	return nil
}

// regionReader seeks to and reads individual rows (or the needed column
// range of them) of a BMP image's pixel data.
type regionReader struct {
	rs  io.ReadSeeker
	h   *bmpHeader
	buf []byte
}

// readRow reads the bytes [b0, b1) of the file row holding image row y into
// rr.buf, returning the buffer and the file offset of rr.buf[b0].
func (rr *regionReader) readRow(y, b0, b1 int) ([]byte, int64, error) {
	fileRow := y
	if !rr.h.topDown {
		fileRow = rr.h.config.Height - 1 - y
	}
	off := rr.h.dataOffset + int64(fileRow)*int64(rr.h.rowBytes) + int64(b0)
	if _, err := rr.rs.Seek(off, io.SeekStart); err != nil {
		return nil, off, &RegionError{Op: "seek", Row: y, Offset: off, Err: err}
	}
	if _, err := io.ReadFull(rr.rs, rr.buf[b0:b1]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, off, &RegionError{Op: "read", Row: y, Offset: off, Err: err}
	}
	return rr.buf, off - int64(b0), nil
}

// newImage returns an image with the given bounds and the concrete type
// that Decode produces for this file.
func (h *bmpHeader) newImage(r image.Rectangle) image.Image {
	switch h.bpp {
	case 1, 2, 4, 8:
		return image.NewPaletted(r, h.config.ColorModel.(color.Palette))
	case 16:
		if h.hasAlphaMask {
			return image.NewNRGBA(r)
		}
		return image.NewRGBA(r)
	case 24:
		return image.NewRGBA(r)
	case 32:
		return image.NewNRGBA(r)
	}
	panic("unreachable")
}

// rowPix returns the pixel slice of img holding image row y, where img was
// created by h.newImage with bounds containing y.
func (h *bmpHeader) rowPix(img image.Image, y int) []byte {
	switch img := img.(type) {
	case *image.Paletted:
		return img.Pix[(y-img.Rect.Min.Y)*img.Stride:]
	case *image.RGBA:
		return img.Pix[(y-img.Rect.Min.Y)*img.Stride:]
	case *image.NRGBA:
		return img.Pix[(y-img.Rect.Min.Y)*img.Stride:]
	}
	panic("unreachable")
}

// columnBytes returns the byte range within one (padded) file row that
// holds the pixels in columns [x0, x1).
func (h *bmpHeader) columnBytes(x0, x1 int) (int, int) {
	if h.bpp < 8 {
		pixelsPerByte := 8 / h.bpp
		return x0 / pixelsPerByte, (x1 + pixelsPerByte - 1) / pixelsPerByte
	}
	s := h.bpp / 8
	return x0 * s, x1 * s
}

// rowConverter converts the pixels in columns [x0, x1) of a raw BMP row
// (src, the padded file row) into dst, the destination pixel slice for that
// row of the decoded (sub-)image. y and off give the image row and the file
// offset of src, for error reporting.
type rowConverter func(y int, off int64, dst, src []byte, x0, x1 int) error

// converter returns the rowConverter for this file's pixel format.
func (h *bmpHeader) converter() rowConverter {
	switch h.bpp {
	case 1, 2, 4, 8:
		palette := h.config.ColorModel.(color.Palette)
		pixelsPerByte := 8 / h.bpp
		mask := byte(1<<h.bpp) - 1
		return func(y int, off int64, dst, src []byte, x0, x1 int) error {
			byteIndex := x0 / pixelsPerByte
			bitIndex := 8 - (x0%pixelsPerByte)*h.bpp
			for i := 0; x0 < x1; x0, i = x0+1, i+1 {
				bitIndex -= h.bpp
				paletteIndex := src[byteIndex] >> uint(bitIndex) & mask
				if int(paletteIndex) >= len(palette) {
					return &RegionError{Op: "parse", Row: y, Offset: off + int64(byteIndex), Err: errInvalidPaletteIndex}
				}
				dst[i] = paletteIndex
				if bitIndex == 0 {
					byteIndex++
					bitIndex = 8
				}
			}
			return nil
		}
	case 16:
		rf, gf, bf := newBitfield(h.masks[0]), newBitfield(h.masks[1]), newBitfield(h.masks[2])
		var af bitfield
		if h.hasAlphaMask {
			af = newBitfield(h.masks[3])
		}
		allowAlpha := h.hasAlphaMask
		return func(y int, off int64, dst, src []byte, x0, x1 int) error {
			j := x0 * 2
			for i := 0; i < (x1-x0)*4; i, j = i+4, j+2 {
				// BMP stores 16 bpp pixels little-endian.
				v := uint32(src[j]) | uint32(src[j+1])<<8
				dst[i+0] = rf.scale(v)
				dst[i+1] = gf.scale(v)
				dst[i+2] = bf.scale(v)
				if allowAlpha {
					dst[i+3] = af.scale(v)
				} else {
					dst[i+3] = 0xFF
				}
			}
			return nil
		}
	case 24:
		return func(y int, off int64, dst, src []byte, x0, x1 int) error {
			j := x0 * 3
			for i := 0; i < (x1-x0)*4; i, j = i+4, j+3 {
				// BMP images are stored in BGR order rather than RGB order.
				dst[i+0] = src[j+2]
				dst[i+1] = src[j+1]
				dst[i+2] = src[j+0]
				dst[i+3] = 0xFF
			}
			return nil
		}
	case 32:
		allowAlpha := h.allowAlpha
		return func(y int, off int64, dst, src []byte, x0, x1 int) error {
			j := x0 * 4
			for i := 0; i < (x1-x0)*4; i, j = i+4, j+4 {
				// BMP images are stored in BGRA order rather than RGBA order.
				dst[i+0] = src[j+2]
				dst[i+1] = src[j+1]
				dst[i+2] = src[j+0]
				if allowAlpha {
					dst[i+3] = src[j+3]
				} else {
					dst[i+3] = 0xFF
				}
			}
			return nil
		}
	}
	panic("unreachable")
}

// bitfield extracts one color channel from a 16 bpp pixel value.
type bitfield struct {
	mask  uint32
	shift uint
	max   uint32
}

func newBitfield(mask uint32) bitfield {
	shift := uint(bits.TrailingZeros32(mask))
	return bitfield{mask: mask, shift: shift, max: mask >> shift}
}

// scale extracts the channel value from v and scales it to 8 bits.
func (bf bitfield) scale(v uint32) uint8 {
	return uint8(((v & bf.mask) >> bf.shift) * 0xff / bf.max)
}

// checkBitfieldMasks validates the R, G, B and (if hasAlpha) A bitfield
// masks of a 16 bpp image. offset is the file offset of masks[0], used for
// error reporting.
func checkBitfieldMasks(masks [4]uint32, hasAlpha bool, offset int64) error {
	used := uint32(0)
	for i, m := range masks {
		if i == 3 && !hasAlpha {
			continue
		}
		// Each mask must be non-zero, fit within 16 bits, be contiguous
		// and not overlap any other mask.
		shifted := uint32(0)
		if m != 0 {
			shifted = m >> bits.TrailingZeros32(m)
		}
		if m == 0 || m&^0xffff != 0 || shifted&(shifted+1) != 0 || used&m != 0 {
			return &RegionError{Op: "parse", Row: -1, Offset: offset + 4*int64(i), Err: errInvalidBitfieldMask}
		}
		used |= m
	}
	return nil
}
