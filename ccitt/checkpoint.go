// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccitt

import (
	"errors"
	"fmt"
	"io"
)

var (
	errCheckpointReused   = errors.New("ccitt: checkpoint was already resumed")
	errCheckpointTooLarge = errors.New("ccitt: checkpoint state too large")
	errCheckpointVersion  = errors.New("ccitt: checkpoint version mismatch")
	errInvalidCheckpoint  = errors.New("ccitt: invalid checkpoint")
	errInvalidRowNumber   = errors.New("ccitt: invalid row number")
	errNotAtRowBoundary   = errors.New("ccitt: cannot checkpoint in the middle of a row")
)

// checkpointVersion is the version of the decoder state held by a Checkpoint.
// A Checkpoint can only be resumed by a package that understands its version.
const checkpointVersion = 1

// A Checkpoint is a read-only snapshot of a Reader's state, taken at a row
// boundary. It captures everything needed to resume decoding so that the
// remaining rows decode to the same output as an uninterrupted reader: the
// bit order, the two-dimensional reference row, the EOL/RTC (end-of-line /
// return-to-control) state and the offset of the unread portion of the
// underlying stream.
//
// A Checkpoint is single-use: after a successful Resume, resuming it again
// returns an error. A Checkpoint holds its own copy of the reference row; it
// shares no mutable buffers with the Reader it came from or with any resumed
// Reader.
type Checkpoint struct {
	version uint32
	used    bool

	// order and subFormat are copied from the Reader, so that a resumed
	// Reader decodes the same stream the same way.
	order     Order
	subFormat SubFormat

	// align and invert are copied from the Reader's Options.
	align  bool
	invert bool

	// width is the image width in pixels. height is the image height in
	// pixels, or negative (such as AutoDetectHeight) if the height was not
	// known in advance. row is the number of rows already decoded.
	width  int
	height int
	row    int

	// offset is the byte offset of the unread portion of the underlying
	// stream. bits and nBits hold pending bits that were read from bytes
	// before offset but not yet consumed.
	offset int64
	bits   uint64
	nBits  uint32

	// prev is a copy of the reference row (the most recently decoded row),
	// one byte per pixel, or nil if the next row is decoded against the
	// implicit all-white row above the first row.
	prev []byte

	// seenStartOfImage and truncated capture the EOL/RTC state: whether the
	// start-of-image codes were decoded, and whether the stream is missing
	// its trailing consecutive EOL's.
	seenStartOfImage bool
	truncated        bool
}

// Row returns the number of rows that were fully decoded before the
// checkpoint was taken. Resuming from the checkpoint produces rows starting
// at this row number.
func (cp *Checkpoint) Row() int {
	return cp.row
}

// Offset returns the byte offset into the underlying stream of the first
// byte that had not been consumed when the checkpoint was taken.
func (cp *Checkpoint) Offset() int64 {
	return cp.offset
}

// Resume returns a Reader that continues decoding from the checkpoint, as if
// the original Reader had never stopped. rs must be the same underlying
// stream that the checkpointed Reader was decoding; Resume seeks it to the
// checkpoint's offset.
//
// The resumed Reader preserves the original Reader's bit order, sub-format,
// byte-alignment and inversion options and (remaining) image height, so its
// output matches an uninterrupted reader row for row.
//
// Resume returns a contextual error if the checkpoint's version is not
// supported, its row number is invalid, its state is implausibly large, it
// was already resumed, or the seek fails. A truncated or otherwise corrupt
// stream is reported by the resumed Reader's Read method, not by Resume.
func (cp *Checkpoint) Resume(rs io.ReadSeeker) (*Reader, error) {
	if cp == nil {
		return nil, errInvalidCheckpoint
	}
	if cp.version != checkpointVersion {
		return nil, fmt.Errorf("%w: got version %d, this package supports version %d",
			errCheckpointVersion, cp.version, checkpointVersion)
	}
	if cp.used {
		return nil, errCheckpointReused
	}
	if (cp.order != LSB && cp.order != MSB) ||
		(cp.subFormat != Group3 && cp.subFormat != Group4) ||
		(cp.offset < 0) || (cp.nBits > 64) ||
		(len(cp.prev) != 0 && len(cp.prev) != cp.width) {
		return nil, errInvalidCheckpoint
	}
	if cp.width < 0 {
		return nil, errInvalidBounds
	}
	if (cp.width > maxWidth) || (len(cp.prev) > maxWidth) {
		return nil, fmt.Errorf("%w: width %d, reference row %d bytes",
			errCheckpointTooLarge, cp.width, len(cp.prev))
	}
	if (cp.row < 0) || (cp.height >= 0 && cp.row > cp.height) {
		return nil, fmt.Errorf("%w: row %d, height %d",
			errInvalidRowNumber, cp.row, cp.height)
	}
	if _, err := rs.Seek(cp.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("ccitt: cannot seek to checkpoint offset %d: %w", cp.offset, err)
	}

	// Copy the reference row, so that the resumed Reader shares no mutable
	// buffer with the Checkpoint (or with anything the Checkpoint was
	// derived from).
	var prev []byte
	if cp.prev != nil {
		prev = make([]byte, len(cp.prev))
		copy(prev, cp.prev)
	}

	rowsRemaining := cp.height
	if rowsRemaining >= 0 {
		rowsRemaining -= cp.row
	}

	cp.used = true
	return &Reader{z: reader{
		br: bitReader{
			r:         rs,
			order:     cp.order,
			bits:      cp.bits,
			nBits:     cp.nBits,
			bytesRead: cp.offset,
		},
		subFormat:        cp.subFormat,
		width:            cp.width,
		rowsRemaining:    rowsRemaining,
		rowsDecoded:      cp.row,
		prev:             prev,
		align:            cp.align,
		invert:           cp.invert,
		atStartOfRow:     true,
		seenStartOfImage: cp.seenStartOfImage,
		truncated:        cp.truncated,
	}}, nil
}

// A Reader decodes CCITT-formatted data like the io.Reader returned by
// NewReader, and can additionally export its state at row boundaries as a
// Checkpoint, from which decoding can later be resumed (see
// Checkpoint.Resume).
type Reader struct {
	z reader
}

// NewCheckpointReader returns a Reader that decodes the CCITT-formatted data
// in r. Its parameters and its decoded output are exactly those of NewReader.
func NewCheckpointReader(r io.Reader, order Order, sf SubFormat, width int, height int, opts *Options) *Reader {
	readErr := error(nil)
	if width < 0 {
		readErr = errInvalidBounds
	} else if width > maxWidth {
		readErr = errUnsupportedWidth
	}

	return &Reader{z: reader{
		br:            bitReader{r: r, order: order},
		subFormat:     sf,
		align:         (opts != nil) && opts.Align,
		invert:        (opts != nil) && opts.Invert,
		width:         width,
		rowsRemaining: height,
		readErr:       readErr,
	}}
}

// Read decodes the next bytes of the image, one bit per pixel (MSB first),
// each row byte-aligned, exactly like the io.Reader returned by NewReader.
func (r *Reader) Read(p []byte) (int, error) {
	return r.z.Read(p)
}

// Checkpoint returns a snapshot of the Reader's state, from which decoding
// can later be resumed with the same output. It must be called at a row
// boundary: after a multiple of ((width+7)/8) bytes have been read (or before
// anything has been read), and before the Reader has returned an error.
// Otherwise, Checkpoint returns an error.
//
// The returned Checkpoint is read-only and shares no mutable state with the
// Reader; the Reader may keep decoding whether or not the Checkpoint is ever
// resumed.
func (r *Reader) Checkpoint() (Checkpoint, error) {
	z := &r.z
	if z.readErr != nil {
		return Checkpoint{}, fmt.Errorf("ccitt: cannot checkpoint after read error: %w", z.readErr)
	}
	if (z.ri != 0) || (z.seenStartOfImage && !z.atStartOfRow) {
		return Checkpoint{}, errNotAtRowBoundary
	}

	// Copy the reference row, so that the Checkpoint shares no mutable
	// buffer with the Reader.
	var prev []byte
	if z.prev != nil {
		prev = make([]byte, len(z.prev))
		copy(prev, z.prev)
	}

	height := z.rowsRemaining
	if height >= 0 {
		height += z.rowsDecoded
	}

	return Checkpoint{
		version:          checkpointVersion,
		order:            z.br.order,
		subFormat:        z.subFormat,
		align:            z.align,
		invert:           z.invert,
		width:            z.width,
		height:           height,
		row:              z.rowsDecoded,
		offset:           z.br.bytesRead - int64(z.br.bw-z.br.br),
		bits:             z.br.bits,
		nBits:            z.br.nBits,
		prev:             prev,
		seenStartOfImage: z.seenStartOfImage,
		truncated:        z.truncated,
	}, nil
}
