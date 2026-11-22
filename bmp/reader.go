// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package bmp implements a BMP image decoder and encoder.
//
// The BMP specification is at http://www.digicamsoft.com/bmp/bmp.html.
package bmp // import "golang.org/x/image/bmp"

import (
	"errors"
	"image"
	"image/color"
	"io"

	"golang.org/x/image/internal/safemath"
)

// ErrUnsupported means that the input BMP image uses a valid but unsupported
// feature.
var ErrUnsupported = errors.New("bmp: unsupported BMP image")

var (
	errInvalidPaletteIndex = errors.New("bmp: invalid palette index")
	errInvalidBitfieldMask = errors.New("bmp: invalid bitfield mask")
)

func readUint16(b []byte) uint16 {
	return uint16(b[0]) | uint16(b[1])<<8
}

func readUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// decodePaletted reads a 1, 2, 4 or 8 bit-per-pixel BMP image from r.
// If topDown is false, the image rows will be read bottom-up.
func decodePaletted(r io.Reader, c image.Config, topDown bool, bpp int) (image.Image, error) {
	palette := c.ColorModel.(color.Palette)
	paletted := image.NewPaletted(image.Rect(0, 0, c.Width, c.Height), palette)
	if c.Width == 0 || c.Height == 0 {
		return paletted, nil
	}
	y0, y1, yDelta := c.Height-1, -1, -1
	if topDown {
		y0, y1, yDelta = 0, c.Height, +1
	}

	pixelsPerByte := 8 / bpp
	// Pad up to ensure each row is 4-bytes aligned.
	bytesPerRow := ((c.Width+pixelsPerByte-1)/pixelsPerByte + 3) &^ 3
	b := make([]byte, bytesPerRow)

	for y := y0; y != y1; y += yDelta {
		p := paletted.Pix[y*paletted.Stride : y*paletted.Stride+c.Width]
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		byteIndex, bitIndex, mask := 0, 8, byte((1<<bpp)-1)
		for pixIndex := 0; pixIndex < c.Width; pixIndex++ {
			bitIndex -= bpp
			paletteIndex := (b[byteIndex]) >> bitIndex & mask
			if int(paletteIndex) >= len(palette) {
				return nil, errInvalidPaletteIndex
			}
			p[pixIndex] = paletteIndex
			if bitIndex == 0 {
				byteIndex++
				bitIndex = 8
			}
		}
	}
	return paletted, nil
}

// decodeRGB16 reads a 16 bit-per-pixel BMP image from r.
// If h.topDown is false, the image rows will be read bottom-up.
func decodeRGB16(r io.Reader, h *bmpHeader) (image.Image, error) {
	w, height := h.config.Width, h.config.Height
	img := h.newImage(image.Rect(0, 0, w, height))
	if w == 0 || height == 0 {
		return img, nil
	}
	var pix []byte
	var stride int
	switch img := img.(type) {
	case *image.RGBA:
		pix, stride = img.Pix, img.Stride
	case *image.NRGBA:
		pix, stride = img.Pix, img.Stride
	}
	buf := make([]byte, h.rowBytes)
	conv := h.converter()
	y0, y1, yDelta := height-1, -1, -1
	if h.topDown {
		y0, y1, yDelta = 0, height, +1
	}
	for y := y0; y != y1; y += yDelta {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		// The 16 bit-per-pixel converter never returns an error.
		_ = conv(y, -1, pix[y*stride:], buf, 0, w)
	}
	return img, nil
}

// decodeRGB reads a 24 bit-per-pixel BMP image from r.
// If topDown is false, the image rows will be read bottom-up.
func decodeRGB(r io.Reader, c image.Config, topDown bool) (image.Image, error) {
	rgba := image.NewRGBA(image.Rect(0, 0, c.Width, c.Height))
	if c.Width == 0 || c.Height == 0 {
		return rgba, nil
	}
	// There are 3 bytes per pixel, and each row is 4-byte aligned.
	b := make([]byte, (3*c.Width+3)&^3)
	y0, y1, yDelta := c.Height-1, -1, -1
	if topDown {
		y0, y1, yDelta = 0, c.Height, +1
	}
	for y := y0; y != y1; y += yDelta {
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		p := rgba.Pix[y*rgba.Stride : y*rgba.Stride+c.Width*4]
		for i, j := 0, 0; i < len(p); i, j = i+4, j+3 {
			// BMP images are stored in BGR order rather than RGB order.
			p[i+0] = b[j+2]
			p[i+1] = b[j+1]
			p[i+2] = b[j+0]
			p[i+3] = 0xFF
		}
	}
	return rgba, nil
}

// decodeNRGBA reads a 32 bit-per-pixel BMP image from r.
// If topDown is false, the image rows will be read bottom-up.
func decodeNRGBA(r io.Reader, c image.Config, topDown, allowAlpha bool) (image.Image, error) {
	rgba := image.NewNRGBA(image.Rect(0, 0, c.Width, c.Height))
	if c.Width == 0 || c.Height == 0 {
		return rgba, nil
	}
	y0, y1, yDelta := c.Height-1, -1, -1
	if topDown {
		y0, y1, yDelta = 0, c.Height, +1
	}
	for y := y0; y != y1; y += yDelta {
		p := rgba.Pix[y*rgba.Stride : y*rgba.Stride+c.Width*4]
		if _, err := io.ReadFull(r, p); err != nil {
			return nil, err
		}
		for i := 0; i < len(p); i += 4 {
			// BMP images are stored in BGRA order rather than RGBA order.
			p[i+0], p[i+2] = p[i+2], p[i+0]
			if !allowAlpha {
				p[i+3] = 0xFF
			}
		}
	}
	return rgba, nil
}

// Decode reads a BMP image from r and returns it as an image.Image.
// Limitation: The file must be 1, 2, 4, 8, 16, 24 or 32 bits per pixel.
func Decode(r io.Reader) (image.Image, error) {
	h, err := decodeConfig(r)
	if err != nil {
		return nil, bareError(err)
	}
	switch h.bpp {
	case 1, 2, 4, 8:
		return decodePaletted(r, h.config, h.topDown, h.bpp)
	case 16:
		return decodeRGB16(r, h)
	case 24:
		return decodeRGB(r, h.config, h.topDown)
	case 32:
		return decodeNRGBA(r, h.config, h.topDown, h.allowAlpha)
	}
	panic("unreachable")
}

// DecodeConfig returns the color model and dimensions of a BMP image without
// decoding the entire image.
// Limitation: The file must be 1, 2, 4, 8, 16, 24 or 32 bits per pixel.
func DecodeConfig(r io.Reader) (image.Config, error) {
	h, err := decodeConfig(r)
	if err != nil {
		return image.Config{}, bareError(err)
	}
	return h.config, nil
}

// bareError unwraps the *RegionError that decodeConfig attaches to header
// errors, preserving the historical error values returned by Decode and
// DecodeConfig.
func bareError(err error) error {
	if re, ok := err.(*RegionError); ok {
		return re.Err
	}
	return err
}

// bmpHeader describes the parsed headers of a BMP file.
type bmpHeader struct {
	config       image.Config
	bpp          int
	topDown      bool
	allowAlpha   bool
	dataOffset   int64     // file offset of the pixel data
	rowBytes     int       // size of one row of pixel data, including padding
	masks        [4]uint32 // R, G, B and A bitfield masks, for 16 bpp images
	hasAlphaMask bool
}

// rowBytes returns the size in bytes of one row of a BMP image with the
// given width and bits-per-pixel, including padding to a 4-byte boundary.
// The caller must have already checked that the image dimensions fit in
// memory, so the result cannot overflow.
func rowBytes(width, bpp int) int {
	n := width * (bpp / 8)
	if bpp < 8 {
		n = (width + 8/bpp - 1) / (8 / bpp)
	}
	return (n + 3) &^ 3
}

func decodeConfig(r io.Reader) (*bmpHeader, error) {
	// We only support those BMP images with one of the following DIB headers:
	// - BITMAPINFOHEADER (40 bytes)
	// - BITMAPV4HEADER (108 bytes)
	// - BITMAPV5HEADER (124 bytes)
	const (
		fileHeaderLen   = 14
		infoHeaderLen   = 40
		v4InfoHeaderLen = 108
		v5InfoHeaderLen = 124
	)
	headerErr := func(offset int64, err error) error {
		return &RegionError{Op: "parse", Row: -1, Offset: offset, Err: err}
	}
	var b [1024]byte
	if _, err := io.ReadFull(r, b[:fileHeaderLen+4]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, headerErr(0, err)
	}
	if string(b[:2]) != "BM" {
		return nil, headerErr(0, errors.New("bmp: invalid format"))
	}
	offset := readUint32(b[10:14])
	infoLen := readUint32(b[14:18])
	if infoLen != infoHeaderLen && infoLen != v4InfoHeaderLen && infoLen != v5InfoHeaderLen {
		return nil, headerErr(14, ErrUnsupported)
	}
	if _, err := io.ReadFull(r, b[fileHeaderLen+4:fileHeaderLen+infoLen]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, headerErr(fileHeaderLen+4, err)
	}
	width := int(int32(readUint32(b[18:22])))
	height := int(int32(readUint32(b[22:26])))
	topDown := false
	if height < 0 {
		height, topDown = -height, true
	}
	if width < 0 || height < 0 {
		return nil, headerErr(18, ErrUnsupported)
	}
	if (width == 0) != (height == 0) {
		// We'll take 0x0, but Nx0 or 0xN is suspicious.
		return nil, headerErr(18, ErrUnsupported)
	}
	// Check that the image fits in memory.
	// This conservatively assumes 4 bytes per pixel,
	// rather than using the actual pixel size.
	if _, ok := safemath.Mul3(width, height, 4); !ok {
		return nil, headerErr(18, ErrUnsupported)
	}
	// We only support 1 plane and no compression, except for 16 bpp
	// BI_BITFIELDS images.
	planes, bpp, compression := readUint16(b[26:28]), readUint16(b[28:30]), readUint32(b[30:34])
	// if compression is set to BI_BITFIELDS, but the bitmask is set to the default bitmask
	// that would be used if compression was set to 0, we can continue as if compression was 0
	if compression == 3 && infoLen > infoHeaderLen &&
		readUint32(b[54:58]) == 0xff0000 && readUint32(b[58:62]) == 0xff00 &&
		readUint32(b[62:66]) == 0xff && readUint32(b[66:70]) == 0xff000000 {
		compression = 0
	}
	if planes != 1 || (compression != 0 && !(compression == 3 && bpp == 16)) {
		return nil, headerErr(26, ErrUnsupported)
	}
	h := &bmpHeader{
		bpp:        int(bpp),
		topDown:    topDown,
		dataOffset: int64(offset),
		rowBytes:   rowBytes(width, int(bpp)),
	}
	switch bpp {
	case 1, 2, 4, 8:
		colorUsed := readUint32(b[46:50])

		if colorUsed == 0 {
			colorUsed = 1 << bpp
		} else if colorUsed > (1 << bpp) {
			return nil, headerErr(46, ErrUnsupported)
		}

		if offset != fileHeaderLen+infoLen+colorUsed*4 {
			return nil, headerErr(10, ErrUnsupported)
		}
		if _, err := io.ReadFull(r, b[:colorUsed*4]); err != nil {
			return nil, headerErr(int64(fileHeaderLen+infoLen), err)
		}
		pcm := make(color.Palette, colorUsed)
		for i := range pcm {
			// BMP images are stored in BGR order rather than RGB order.
			// Every 4th byte is padding.
			pcm[i] = color.RGBA{b[4*i+2], b[4*i+1], b[4*i+0], 0xFF}
		}
		h.config = image.Config{ColorModel: pcm, Width: width, Height: height}
	case 16:
		// The default (BI_RGB) 16 bpp layout is RGB555.
		h.masks = [4]uint32{0x7c00, 0x03e0, 0x001f, 0}
		dataOffset := int64(fileHeaderLen + infoLen)
		if compression == 3 {
			maskOffset := dataOffset
			if infoLen > infoHeaderLen {
				// BITMAPV4HEADER and later store the masks in the
				// info header itself.
				h.masks[0] = readUint32(b[54:58])
				h.masks[1] = readUint32(b[58:62])
				h.masks[2] = readUint32(b[62:66])
				h.masks[3] = readUint32(b[66:70])
			} else {
				// For a BITMAPINFOHEADER, the bitfield masks are stored
				// in the file immediately after the header: 12 bytes for
				// the R, G and B masks, plus an optional 4-byte alpha
				// mask.
				if _, err := io.ReadFull(r, b[:12]); err != nil {
					return nil, headerErr(dataOffset, err)
				}
				h.masks[0] = readUint32(b[0:4])
				h.masks[1] = readUint32(b[4:8])
				h.masks[2] = readUint32(b[8:12])
				dataOffset += 12
				if offset == uint32(dataOffset)+4 {
					if _, err := io.ReadFull(r, b[:4]); err != nil {
						return nil, headerErr(dataOffset, err)
					}
					h.masks[3] = readUint32(b[0:4])
					dataOffset += 4
				}
			}
			h.hasAlphaMask = h.masks[3] != 0
			if err := checkBitfieldMasks(h.masks, h.hasAlphaMask, maskOffset); err != nil {
				return nil, err
			}
		}
		if int64(offset) != dataOffset {
			return nil, headerErr(10, ErrUnsupported)
		}
		h.config = image.Config{ColorModel: color.RGBAModel, Width: width, Height: height}
	case 24:
		if offset != fileHeaderLen+infoLen {
			return nil, headerErr(10, ErrUnsupported)
		}
		h.config = image.Config{ColorModel: color.RGBAModel, Width: width, Height: height}
	case 32:
		if offset != fileHeaderLen+infoLen {
			return nil, headerErr(10, ErrUnsupported)
		}
		// 32 bits per pixel is possibly RGBX (X is padding) or RGBA (A is
		// alpha transparency). However, for BMP images, "Alpha is a
		// poorly-documented and inconsistently-used feature" says
		// https://source.chromium.org/chromium/chromium/src/+/bc0a792d7ebc587190d1a62ccddba10abeea274b:third_party/blink/renderer/platform/image-decoders/bmp/bmp_image_reader.cc;l=621
		//
		// That goes on to say "BITMAPV3HEADER+ have an alpha bitmask in the
		// info header... so we respect it at all times... [For earlier
		// (smaller) headers we] ignore alpha in Windows V3 BMPs except inside
		// ICO files".
		//
		// "Ignore" means to always set alpha to 0xFF (fully opaque):
		// https://source.chromium.org/chromium/chromium/src/+/bc0a792d7ebc587190d1a62ccddba10abeea274b:third_party/blink/renderer/platform/image-decoders/bmp/bmp_image_reader.h;l=272
		//
		// Confusingly, "Windows V3" does not correspond to BITMAPV3HEADER, but
		// instead corresponds to the earlier (smaller) BITMAPINFOHEADER:
		// https://source.chromium.org/chromium/chromium/src/+/bc0a792d7ebc587190d1a62ccddba10abeea274b:third_party/blink/renderer/platform/image-decoders/bmp/bmp_image_reader.cc;l=258
		//
		// This Go package does not support ICO files and the (infoLen >
		// infoHeaderLen) condition distinguishes BITMAPINFOHEADER (40 bytes)
		// vs later (larger) headers.
		h.allowAlpha = infoLen > infoHeaderLen
		h.config = image.Config{ColorModel: color.RGBAModel, Width: width, Height: height}
	default:
		return nil, headerErr(28, ErrUnsupported)
	}
	return h, nil
}

func init() {
	image.RegisterFormat("bmp", "BM????\x00\x00\x00\x00", Decode, DecodeConfig)
}
