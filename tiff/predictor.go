// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"errors"
	"image"
)

// A Predictor selects the per-sample transformation applied to pixel data
// before it is written. The numeric values are exactly the values stored in
// the TIFF Predictor tag (tag 317), so the choice made at encode time stays
// auditable in the resulting file.
//
// Both predictors operate independently on each channel (sample) of a
// pixel and restart at the beginning of every row, so the first sample of
// each channel in a row is never relative to a sample from the row above.
type Predictor int

const (
	// PredictorNone writes each sample verbatim (tag value 1).
	//
	// Byte semantics: for an 8-bit channel the stored byte is the sample
	// itself; for a 16-bit channel the stored two bytes are the sample in
	// the file's byte order (this package writes little-endian files,
	// regardless of the big-endian layout of image.Gray16/image.RGBA64
	// Pix in memory).
	PredictorNone Predictor = 1

	// PredictorHorizontal writes horizontal (left-neighbor) differences
	// (tag value 2).
	//
	// Byte semantics: the first sample of each channel in a row is stored
	// verbatim; every later sample stores the modular difference to the
	// sample immediately to its left within the same channel. For 8-bit
	// channels the stored byte is uint8(a-b), for 16-bit channels it is
	// uint16(a-b) serialized in the file's byte order. Subtraction wraps
	// modulo 256 or 65536, so the decoder reconstructs the exact original
	// sample with modular addition -- no information is lost.
	//
	// Each channel is differenced independently: the red, green, blue and
	// alpha channels never reference one another.
	PredictorHorizontal Predictor = 2
)

// PixelType identifies the layout of the image whose rows are presented to a
// [PredictorSelector].
type PixelType int

const (
	// PixelGray is an 8-bit, single-channel image (image.Gray).
	PixelGray PixelType = iota
	// PixelGray16 is a 16-bit, single-channel image (image.Gray16).
	PixelGray16
	// PixelRGBA is an 8-bit, four-channel image in RGBA order
	// (image.RGBA and image.NRGBA).
	PixelRGBA
	// PixelRGBA64 is a 16-bit, four-channel image in RGBA order
	// (image.RGBA64 and image.NRGBA64).
	PixelRGBA64
)

// Channels reports the number of samples (channels) per pixel for t: 1 for
// the gray types and 4 for the RGBA types.
func (t PixelType) Channels() int {
	switch t {
	case PixelGray, PixelGray16:
		return 1
	case PixelRGBA, PixelRGBA64:
		return 4
	}
	return 0
}

// PredictorSelector chooses a [Predictor] for each sample of an image.
//
// Predictor is called once per row and channel before any pixel data is
// written. pt is the image's [PixelType], row is the zero-based row number
// (0 is the top row) and channel is the zero-based channel number: 0 for
// gray images, and 0=R, 1=G, 2=B, 3=A for RGBA images.
//
// The TIFF Predictor tag describes a whole image, not an individual sample,
// so every call for a given image must return the same [Predictor]. A
// selector that returns different predictors -- across rows or across
// channels -- makes Encode return an error describing the conflict and
// leaves the input image untouched.
type PredictorSelector interface {
	Predictor(pt PixelType, row, channel int) Predictor
}

// PredictorFunc adapts an ordinary function to the [PredictorSelector]
// interface.
type PredictorFunc func(pt PixelType, row, channel int) Predictor

// Predictor calls f(pt, row, channel).
func (f PredictorFunc) Predictor(pt PixelType, row, channel int) Predictor {
	return f(pt, row, channel)
}

// ConstantPredictor returns a [PredictorSelector] that always returns p. It
// is the simplest way to request horizontal differencing for every channel
// of an image.
func ConstantPredictor(p Predictor) PredictorSelector {
	return PredictorFunc(func(PixelType, int, int) Predictor {
		return p
	})
}

var (
	// errInvalidPredictor is returned when a PredictorSelector returns a
	// value other than PredictorNone or PredictorHorizontal.
	errInvalidPredictor = errors.New("tiff: invalid predictor value")

	// errPredictorConflict is returned when a PredictorSelector asks for
	// more than one predictor within the same image, which the single
	// TIFF Predictor tag cannot express.
	errPredictorConflict = errors.New("tiff: conflicting predictors within one image")

	// errRowTooLarge is returned when a single image row is too large to
	// encode without overflowing the platform's int type.
	errRowTooLarge = errors.New("tiff: image row is too large")

	// errImageTooLarge is returned when the whole image, or its encoded
	// pixel data, exceeds the addressable size of a TIFF file.
	errImageTooLarge = errors.New("tiff: image is too large")
)

// resolvePredictor queries sel for every row and channel of the image and
// returns the single, consistent [Predictor] to use. It returns an error
// without side effects if sel returns an invalid value or asks for
// conflicting predictors.
func resolvePredictor(sel PredictorSelector, pt PixelType, width, height int) (Predictor, error) {
	if sel == nil {
		return PredictorNone, nil
	}
	channels := pt.Channels()
	want := sel.Predictor(pt, 0, 0)
	if want != PredictorNone && want != PredictorHorizontal {
		return 0, errInvalidPredictor
	}
	for row := 0; row < height; row++ {
		for ch := 0; ch < channels; ch++ {
			got := sel.Predictor(pt, row, ch)
			if got != PredictorNone && got != PredictorHorizontal {
				return 0, errInvalidPredictor
			}
			if got != want {
				return 0, errPredictorConflict
			}
		}
	}
	return want, nil
}

// pixelTypeOf reports the [PixelType] of the concrete image types the
// predictor-aware encoder handles directly. The second result is false for
// image types that cannot be combined with a [PredictorSelector]: paletted
// images have no entry in [PixelType] (their bytes are palette indices, not
// color samples), and generic image.Image values have no fixed sample
// layout.
func pixelTypeOf(m image.Image) (PixelType, bool) {
	switch m.(type) {
	case *image.Gray:
		return PixelGray, true
	case *image.Gray16:
		return PixelGray16, true
	case *image.RGBA, *image.NRGBA:
		return PixelRGBA, true
	case *image.RGBA64, *image.NRGBA64:
		return PixelRGBA64, true
	default:
		// Covers *image.Paletted and arbitrary image.Image implementations.
		return 0, false
	}
}
