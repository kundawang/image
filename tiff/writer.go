// Copyright 2012 The Go Authors. All rights reserved.
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
	"io"
	"math"
	"sort"

	"golang.org/x/image/internal/safemath"
)

// The TIFF format allows to choose the order of the different elements freely.
// The basic structure of a TIFF file written by this package is:
//
//   1. Header (8 bytes).
//   2. Image data.
//   3. Image File Directory (IFD).
//   4. "Pointer area" for larger entries in the IFD.

// We only write little-endian TIFF files.
var enc = binary.LittleEndian

// An ifdEntry is a single entry in an Image File Directory.
// A value of type dtRational is composed of two 32-bit values,
// thus data contains two uints (numerator and denominator) for a single number.
type ifdEntry struct {
	tag      int
	datatype int
	data     []uint32
}

func (e ifdEntry) putData(p []byte) {
	for _, d := range e.data {
		switch e.datatype {
		case dtByte, dtASCII:
			p[0] = byte(d)
			p = p[1:]
		case dtShort:
			enc.PutUint16(p, uint16(d))
			p = p[2:]
		case dtLong, dtRational:
			enc.PutUint32(p, uint32(d))
			p = p[4:]
		}
	}
}

type byTag []ifdEntry

func (d byTag) Len() int           { return len(d) }
func (d byTag) Less(i, j int) bool { return d[i].tag < d[j].tag }
func (d byTag) Swap(i, j int)      { d[i], d[j] = d[j], d[i] }

// checkedWriter wraps an io.Writer and turns a short write (one that accepts
// fewer bytes than offered without returning an error) into an
// io.ErrShortWrite. Every byte of a TIFF file is written through a
// checkedWriter, so a truncated destination is always reported instead of
// silently producing a corrupt file.
//
// The optional buf field avoids an interface-method indirection on the very
// common *bytes.Buffer destination.
type checkedWriter struct {
	w   io.Writer
	buf *bytes.Buffer
}

func (c checkedWriter) Write(p []byte) (int, error) {
	if c.buf != nil {
		n, err := c.buf.Write(p)
		if err == nil && n < len(p) {
			err = io.ErrShortWrite
		}
		return n, err
	}
	n, err := c.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// newCheckedWriter wraps w, using the direct *bytes.Buffer path when
// applicable.
func newCheckedWriter(w io.Writer) checkedWriter {
	cw := checkedWriter{w: w}
	if b, ok := w.(*bytes.Buffer); ok {
		cw.buf = b
	}
	return cw
}

// rowLen returns the on-disk length in bytes of one image row, or an error
// if the multiplication would overflow the platform int.
func rowLen(dx, channels, bytesPerSample int) (int, error) {
	// Each factor is non-negative; Mul3 rejects overflow and negatives.
	n, ok := safemath.Mul3(dx, channels, bytesPerSample)
	if !ok {
		return 0, errRowTooLarge
	}
	return n, nil
}

// pixEnd returns the exclusive end offset of dy rows of rowLen bytes laid
// out in pix at off with the given stride. The boolean is false if the
// offset arithmetic overflows int. A caller still has to compare the result
// against len(pix).
func pixEnd(dy, stride, off, rowLength int) (int, bool) {
	if dy <= 0 {
		return off, true
	}
	span, ok := safemath.Mul3(dy-1, stride, 1)
	if !ok {
		return 0, false
	}
	end := off + span
	if end < off {
		return 0, false
	}
	end += rowLength
	if end < off+span {
		return 0, false
	}
	return end, true
}

// writeRows writes dy verbatim rows. It takes the single-write fast path
// used by the old encoder when the rows are tightly packed, and falls back
// to one write per row when stride includes padding or the image starts at
// a non-zero Pix offset.
func writeRows(w io.Writer, pix []byte, dy, stride, off, rowLength int) error {
	if off == 0 && stride == rowLength {
		if _, err := w.Write(pix[:dy*rowLength]); err != nil {
			return err
		}
		return nil
	}
	for y := 0; y < dy; y++ {
		min := off + y*stride
		if _, err := w.Write(pix[min : min+rowLength]); err != nil {
			return err
		}
	}
	return nil
}

// encodeGray writes an 8-bit, single-channel image (Gray or Paletted).
//
// Without a predictor the row is written verbatim, one byte per sample.
// With the horizontal predictor, sample x stores uint8(sample[x]-sample[x-1])
// and the first sample of each row is stored verbatim.
func encodeGray(w io.Writer, pix []uint8, dx, dy, stride, off int, pred Predictor) error {
	row, err := rowLen(dx, 1, 1)
	if err != nil {
		return err
	}
	end, ok := pixEnd(dy, stride, off, row)
	if !ok || off < 0 || end > len(pix) {
		return errNoPixels
	}
	if pred != PredictorHorizontal {
		return writeRows(w, pix, dy, stride, off, row)
	}
	buf := make([]byte, row)
	for y := 0; y < dy; y++ {
		min := off + y*stride
		line := pix[min : min+dx]
		var v0 uint8
		for i := 0; i < dx; i++ {
			v1 := line[i]
			buf[i] = v1 - v0
			v0 = v1
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

// encodeGray16 writes a 16-bit, single-channel image.
//
// In-memory samples are big-endian (image.Gray16 layout); they are serialized
// little-endian on disk. The horizontal predictor differences uint16 sample
// values before serialization; the first sample of each row is verbatim.
func encodeGray16(w io.Writer, pix []uint8, dx, dy, stride, off int, pred Predictor) error {
	row, err := rowLen(dx, 1, 2)
	if err != nil {
		return err
	}
	end, ok := pixEnd(dy, stride, off, row)
	if !ok || off < 0 || end > len(pix) {
		return errNoPixels
	}
	if pred != PredictorHorizontal {
		// Still convert big-endian Pix to the little-endian file layout.
		buf := make([]byte, row)
		for y := 0; y < dy; y++ {
			min := off + y*stride
			line := pix[min : min+row]
			o := 0
			for i := 0; i < dx; i++ {
				v1 := uint16(line[2*i])<<8 | uint16(line[2*i+1])
				buf[o+0] = byte(v1)
				buf[o+1] = byte(v1 >> 8)
				o += 2
			}
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
		return nil
	}
	buf := make([]byte, row)
	for y := 0; y < dy; y++ {
		min := off + y*stride
		line := pix[min : min+row]
		o := 0
		var v0 uint16
		for i := 0; i < dx; i++ {
			// An image.Gray16's Pix is in big-endian order.
			v1 := uint16(line[2*i])<<8 | uint16(line[2*i+1])
			v0, v1 = v1, v1-v0
			// We only write little-endian TIFF files.
			buf[o+0] = byte(v1)
			buf[o+1] = byte(v1 >> 8)
			o += 2
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

// encodeRGBA writes an 8-bit, four-channel image in RGBA order
// (image.RGBA or image.NRGBA).
//
// Without a predictor each row is written verbatim. With the horizontal
// predictor each of the four channels is differenced against its own
// left neighbor; the alpha channel is handled independently of R, G and B.
func encodeRGBA(w io.Writer, pix []uint8, dx, dy, stride, off int, pred Predictor) error {
	row, err := rowLen(dx, 4, 1)
	if err != nil {
		return err
	}
	end, ok := pixEnd(dy, stride, off, row)
	if !ok || off < 0 || end > len(pix) {
		return errNoPixels
	}
	if pred != PredictorHorizontal {
		return writeRows(w, pix, dy, stride, off, row)
	}
	buf := make([]byte, row)
	for y := 0; y < dy; y++ {
		min := off + y*stride
		line := pix[min : min+row]
		o := 0
		var r0, g0, b0, a0 uint8
		for i := 0; i < dx; i++ {
			r1, g1, b1, a1 := line[4*i+0], line[4*i+1], line[4*i+2], line[4*i+3]
			buf[o+0] = r1 - r0
			buf[o+1] = g1 - g0
			buf[o+2] = b1 - b0
			buf[o+3] = a1 - a0
			o += 4
			r0, g0, b0, a0 = r1, g1, b1, a1
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

// encodeRGBA64 writes a 16-bit, four-channel image in RGBA order
// (image.RGBA64 or image.NRGBA64).
//
// In-memory samples are big-endian; they are serialized little-endian on
// disk. With the horizontal predictor every channel is differenced against
// its own left neighbor as a uint16, including the alpha channel.
func encodeRGBA64(w io.Writer, pix []uint8, dx, dy, stride, off int, pred Predictor) error {
	row, err := rowLen(dx, 4, 2)
	if err != nil {
		return err
	}
	end, ok := pixEnd(dy, stride, off, row)
	if !ok || off < 0 || end > len(pix) {
		return errNoPixels
	}
	buf := make([]byte, row)
	for y := 0; y < dy; y++ {
		min := off + y*stride
		line := pix[min : min+row]
		o := 0
		var r0, g0, b0, a0 uint16
		for i := 0; i < dx; i++ {
			j := 8 * i
			// An image.RGBA64's Pix is in big-endian order.
			r1 := uint16(line[j+0])<<8 | uint16(line[j+1])
			g1 := uint16(line[j+2])<<8 | uint16(line[j+3])
			b1 := uint16(line[j+4])<<8 | uint16(line[j+5])
			a1 := uint16(line[j+6])<<8 | uint16(line[j+7])
			if pred == PredictorHorizontal {
				r0, r1 = r1, r1-r0
				g0, g1 = g1, g1-g0
				b0, b1 = b1, b1-b0
				a0, a1 = a1, a1-a0
			}
			// We only write little-endian TIFF files.
			buf[o+0] = byte(r1)
			buf[o+1] = byte(r1 >> 8)
			buf[o+2] = byte(g1)
			buf[o+3] = byte(g1 >> 8)
			buf[o+4] = byte(b1)
			buf[o+5] = byte(b1 >> 8)
			buf[o+6] = byte(a1)
			buf[o+7] = byte(a1 >> 8)
			o += 8
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

// encode is the generic fallback for image types whose in-memory layout does
// not match a TIFF pixel type directly. It always writes 8-bit RGBA, sampled
// through image.Image.At, and therefore only supports PredictorNone.
func encode(w io.Writer, m image.Image, pred Predictor) error {
	bounds := m.Bounds()
	dx := bounds.Dx()
	row, err := rowLen(dx, 4, 1)
	if err != nil {
		return err
	}
	buf := make([]byte, row)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		off := 0
		if pred == PredictorHorizontal {
			var r0, g0, b0, a0 uint8
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				r, g, b, a := m.At(x, y).RGBA()
				r1 := uint8(r >> 8)
				g1 := uint8(g >> 8)
				b1 := uint8(b >> 8)
				a1 := uint8(a >> 8)
				buf[off+0] = r1 - r0
				buf[off+1] = g1 - g0
				buf[off+2] = b1 - b0
				buf[off+3] = a1 - a0
				off += 4
				r0, g0, b0, a0 = r1, g1, b1, a1
			}
		} else {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				r, g, b, a := m.At(x, y).RGBA()
				buf[off+0] = uint8(r >> 8)
				buf[off+1] = uint8(g >> 8)
				buf[off+2] = uint8(b >> 8)
				buf[off+3] = uint8(a >> 8)
				off += 4
			}
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

func writeIFD(w io.Writer, ifdOffset int, d []ifdEntry) error {
	var buf [ifdLen]byte
	// Make space for "pointer area" containing IFD entry data
	// longer than 4 bytes.
	parea := make([]byte, 1024)
	pstart := ifdOffset + ifdLen*len(d) + 6
	var o int // Current offset in parea.

	// The IFD has to be written with the tags in ascending order.
	sort.Sort(byTag(d))

	// Write the number of entries in this IFD.
	if err := binary.Write(w, enc, uint16(len(d))); err != nil {
		return err
	}
	for _, ent := range d {
		enc.PutUint16(buf[0:2], uint16(ent.tag))
		enc.PutUint16(buf[2:4], uint16(ent.datatype))
		count := uint32(len(ent.data))
		if ent.datatype == dtRational {
			count /= 2
		}
		enc.PutUint32(buf[4:8], count)
		datalen := int(count * lengths[ent.datatype])
		if datalen <= 4 {
			ent.putData(buf[8:12])
		} else {
			if (o + datalen) > len(parea) {
				newlen := len(parea) + 1024
				for (o + datalen) > newlen {
					newlen += 1024
				}
				newarea := make([]byte, newlen)
				copy(newarea, parea)
				parea = newarea
			}
			ent.putData(parea[o : o+datalen])
			enc.PutUint32(buf[8:12], uint32(pstart+o))
			o += datalen
		}
		if _, err := w.Write(buf[:]); err != nil {
			return err
		}
	}
	// The IFD ends with the offset of the next IFD in the file,
	// or zero if it is the last one (page 14).
	if err := binary.Write(w, enc, uint32(0)); err != nil {
		return err
	}
	_, err := w.Write(parea[:o])
	return err
}

// Options are the encoding parameters.
type Options struct {
	// Compression is the type of compression used.
	Compression CompressionType

	// Predictor determines whether a differencing predictor is used;
	// if true, instead of each pixel's color, the color difference to the
	// preceding one is saved. This improves the compression for certain
	// types of images and compressors. For example, it works well for
	// photos with Deflate compression.
	//
	// For backwards compatibility this field only takes effect with the
	// LZW compression type, matching the behavior of earlier versions.
	// New code should use PredictorSelector instead.
	Predictor bool

	// PredictorSelector selects the predictor for each row and channel of
	// the image. It works with every supported compression, including
	// uncompressed files. If nil, no horizontal differencing is applied
	// unless the legacy Predictor field requests it for LZW.
	//
	// The selected predictor is recorded verbatim in the TIFF Predictor
	// tag, so the encoding parameters stay auditable in the output file.
	// See [PredictorSelector] for the conflict rules.
	PredictorSelector PredictorSelector
}

// Encode writes the image m to w. opt determines the options used for
// encoding, such as the compression type. If opt is nil, an uncompressed
// image is written.
func Encode(w io.Writer, m image.Image, opt *Options) error {
	cw := newCheckedWriter(w)
	bounds := m.Bounds()
	d := bounds.Size()

	if d.X == 0 || d.Y == 0 {
		return errors.New("tiff: zero-size image")
	}

	compression := uint32(cNone)
	pred := PredictorNone
	if opt != nil {
		compression = opt.Compression.specValue()
		// The predictor field is only used with LZW. See page 64 of the spec.
		// Preserve the historical, LZW-gated semantics of the boolean field.
		if opt.Predictor && compression == cLZW {
			pred = PredictorHorizontal
		}
	}

	// Resolve the new selector before anything is written so that invalid
	// or conflicting requests never produce a partial file and never touch
	// the input image.
	if opt != nil && opt.PredictorSelector != nil {
		pt, ok := pixelTypeOf(m)
		if !ok {
			return UnsupportedError(fmt.Sprintf("predictor for image type %T", m))
		}
		sel, err := resolvePredictor(opt.PredictorSelector, pt, d.X, d.Y)
		if err != nil {
			return err
		}
		if pred == PredictorHorizontal && sel != PredictorHorizontal {
			return errPredictorConflict
		}
		pred = sel
	}

	if _, err := io.WriteString(cw, leHeader); err != nil {
		return err
	}

	// Compressed data is written into a buffer first, so that we
	// know the compressed size.
	var buf bytes.Buffer
	// dst holds the destination for the pixel data of the image --
	// either cw or a checked writer to buf.
	var dst io.Writer
	// closer flushes and closes the pixel-data writer for compressed
	// output; it stays nil when the data is written uncompressed.
	var closer io.Closer
	// imageLen is the length of the pixel data in bytes.
	// The offset of the IFD is imageLen + 8 header bytes.
	var (
		imageLen int
		ok       bool
	)

	switch compression {
	case cNone:
		dst = cw
		// Write IFD offset before outputting pixel data.
		var bytesPerPixel int
		switch m.(type) {
		case *image.Paletted:
			bytesPerPixel = 1
		case *image.Gray:
			bytesPerPixel = 1
		case *image.Gray16:
			bytesPerPixel = 2
		case *image.RGBA64:
			bytesPerPixel = 8
		case *image.NRGBA64:
			bytesPerPixel = 8
		default:
			bytesPerPixel = 4
		}
		imageLen, ok = safemath.Mul3(d.X, d.Y, bytesPerPixel)
		// The IFD stores strip offsets and counts as uint32. The uint64
		// comparison stays valid on 32-bit platforms, where MaxUint32 does
		// not fit in an int.
		if !ok || uint64(imageLen)+8 > math.MaxUint32 {
			return errImageTooLarge
		}
		if err := binary.Write(cw, enc, uint32(imageLen+8)); err != nil {
			return err
		}
	case cDeflate:
		zw := zlib.NewWriter(&buf)
		closer = zw
		dst = checkedWriter{w: zw}
	default:
		return errors.New("tiff: unsupported compression")
	}

	pr := uint32(prNone)
	photometricInterpretation := uint32(pRGB)
	samplesPerPixel := uint32(4)
	bitsPerSample := []uint32{8, 8, 8, 8}
	extraSamples := uint32(0)
	colorMap := []uint32{}

	if pred == PredictorHorizontal {
		pr = prHorizontal
	}

	var err error
	switch m := m.(type) {
	case *image.Paletted:
		photometricInterpretation = pPaletted
		samplesPerPixel = 1
		bitsPerSample = []uint32{8}
		colorMap = make([]uint32, 256*3)
		for i := 0; i < 256 && i < len(m.Palette); i++ {
			r, g, b, _ := m.Palette[i].RGBA()
			colorMap[i+0*256] = uint32(r)
			colorMap[i+1*256] = uint32(g)
			colorMap[i+2*256] = uint32(b)
		}
		err = encodeGray(dst, m.Pix, d.X, d.Y, m.Stride, m.PixOffset(bounds.Min.X, bounds.Min.Y), pred)
	case *image.Gray:
		photometricInterpretation = pBlackIsZero
		samplesPerPixel = 1
		bitsPerSample = []uint32{8}
		err = encodeGray(dst, m.Pix, d.X, d.Y, m.Stride, m.PixOffset(bounds.Min.X, bounds.Min.Y), pred)
	case *image.Gray16:
		photometricInterpretation = pBlackIsZero
		samplesPerPixel = 1
		bitsPerSample = []uint32{16}
		err = encodeGray16(dst, m.Pix, d.X, d.Y, m.Stride, m.PixOffset(bounds.Min.X, bounds.Min.Y), pred)
	case *image.NRGBA:
		extraSamples = 2 // Unassociated alpha.
		err = encodeRGBA(dst, m.Pix, d.X, d.Y, m.Stride, m.PixOffset(bounds.Min.X, bounds.Min.Y), pred)
	case *image.NRGBA64:
		extraSamples = 2 // Unassociated alpha.
		bitsPerSample = []uint32{16, 16, 16, 16}
		err = encodeRGBA64(dst, m.Pix, d.X, d.Y, m.Stride, m.PixOffset(bounds.Min.X, bounds.Min.Y), pred)
	case *image.RGBA:
		extraSamples = 1 // Associated alpha.
		err = encodeRGBA(dst, m.Pix, d.X, d.Y, m.Stride, m.PixOffset(bounds.Min.X, bounds.Min.Y), pred)
	case *image.RGBA64:
		extraSamples = 1 // Associated alpha.
		bitsPerSample = []uint32{16, 16, 16, 16}
		err = encodeRGBA64(dst, m.Pix, d.X, d.Y, m.Stride, m.PixOffset(bounds.Min.X, bounds.Min.Y), pred)
	default:
		extraSamples = 1 // Associated alpha.
		err = encode(dst, m, pred)
	}
	if err != nil {
		return err
	}

	if closer != nil {
		if err = closer.Close(); err != nil {
			return fmt.Errorf("tiff: compression failed: %w", err)
		}
		imageLen = buf.Len()
		if uint64(imageLen)+8 > math.MaxUint32 {
			return errImageTooLarge
		}
		if err = binary.Write(cw, enc, uint32(imageLen+8)); err != nil {
			return err
		}
		if _, err = cw.Write(buf.Bytes()); err != nil {
			return err
		}
	}

	ifd := []ifdEntry{
		{tImageWidth, dtShort, []uint32{uint32(d.X)}},
		{tImageLength, dtShort, []uint32{uint32(d.Y)}},
		{tBitsPerSample, dtShort, bitsPerSample},
		{tCompression, dtShort, []uint32{compression}},
		{tPhotometricInterpretation, dtShort, []uint32{photometricInterpretation}},
		{tStripOffsets, dtLong, []uint32{8}},
		{tSamplesPerPixel, dtShort, []uint32{samplesPerPixel}},
		{tRowsPerStrip, dtShort, []uint32{uint32(d.Y)}},
		{tStripByteCounts, dtLong, []uint32{uint32(imageLen)}},
		// There is currently no support for storing the image
		// resolution, so give a bogus value of 72x72 dpi.
		{tXResolution, dtRational, []uint32{72, 1}},
		{tYResolution, dtRational, []uint32{72, 1}},
		{tResolutionUnit, dtShort, []uint32{resPerInch}},
	}
	if pr != prNone {
		ifd = append(ifd, ifdEntry{tPredictor, dtShort, []uint32{pr}})
	}
	if len(colorMap) != 0 {
		ifd = append(ifd, ifdEntry{tColorMap, dtShort, colorMap})
	}
	if extraSamples > 0 {
		ifd = append(ifd, ifdEntry{tExtraSamples, dtShort, []uint32{extraSamples}})
	}

	return writeIFD(cw, imageLen+8, ifd)
}
