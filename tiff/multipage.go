// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tiff

import (
	"encoding/binary"
	"fmt"
	"image"
	"io"
)

// maxPages is the maximum number of pages (IFDs) a Decoder will walk in a
// single file. It bounds the amount of work a malicious or corrupt file can
// force us to perform while following the IFD chain.
const maxPages = 1 << 16

// A PageError reports an error that occurred while locating, parsing or
// decoding a specific page of a multi-page TIFF.
type PageError struct {
	Page int // The zero-based index of the page that caused the error.
	Err  error
}

func (e *PageError) Error() string {
	return fmt.Sprintf("tiff: page %d: %v", e.Page, e.Err)
}

func (e *PageError) Unwrap() error { return e.Err }

// A Decoder decodes the pages of a multi-page TIFF image one at a time.
//
// The pages are described by a chain of Image File Directories (IFDs). The
// Decoder walks the chain lazily: pages are located and decoded only when
// requested, and locating a page never decodes its pixel data. Each page is
// decoded with fresh per-page state, so pages may differ in size,
// compression and photometric interpretation, and a corrupt page does not
// affect the decoding of any other page.
//
// The input may be any io.Reader, including a non-seekable stream; it is
// buffered internally as needed. Reading pages in any order, re-reading a
// page, or interleaving NextPage and DecodePage calls yields stable
// results.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	ra        io.ReaderAt
	byteOrder binary.ByteOrder

	offsets []int64 // offsets[i] is the IFD offset of page i, discovered so far.
	eoc     bool    // The end of the IFD chain has been reached.
	next    int     // The index of the page the next NextPage call will return.
}

// NewDecoder returns a Decoder that reads the pages of the TIFF image in r.
// Only the TIFF header is read at this point.
func NewDecoder(r io.Reader) (*Decoder, error) {
	ra, byteOrder, ifdOffset, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	d := &Decoder{
		ra:        ra,
		byteOrder: byteOrder,
	}
	if ifdOffset == 0 {
		// No pages at all.
		d.eoc = true
	} else {
		d.offsets = []int64{ifdOffset}
	}
	return d, nil
}

// walk extends d.offsets until it contains the IFD offset of the given page
// or the end of the IFD chain is reached. It detects cyclic chains and
// enforces the maxPages limit.
func (d *Decoder) walk(page int) error {
	for !d.eoc && len(d.offsets) <= page {
		if len(d.offsets) >= maxPages {
			return FormatError(fmt.Sprintf("too many pages (limit %d)", maxPages))
		}
		next, err := d.nextIFDOffset(d.offsets[len(d.offsets)-1])
		if err != nil {
			return err
		}
		if next == 0 {
			d.eoc = true
			break
		}
		for _, prev := range d.offsets {
			if prev == next {
				return FormatError("cyclic IFD chain")
			}
		}
		d.offsets = append(d.offsets, next)
	}
	return nil
}

// nextIFDOffset reads the IFD at off and returns the offset of the next IFD
// in the chain, or 0 if there is none. It reads only the IFD entry count
// and the chain pointer, not the entries themselves.
func (d *Decoder) nextIFDOffset(off int64) (int64, error) {
	var p [2]byte
	if _, err := d.ra.ReadAt(p[:], off); err != nil {
		return 0, err
	}
	numItems := int64(d.byteOrder.Uint16(p[:]))
	var q [4]byte
	if _, err := d.ra.ReadAt(q[:], off+2+ifdLen*numItems); err != nil {
		return 0, err
	}
	return int64(d.byteOrder.Uint32(q[:])), nil
}

// walkError wraps a chain-walking error with the index of the page whose
// IFD was being examined when the walk failed.
func (d *Decoder) walkError(err error) error {
	page := len(d.offsets) - 1
	if page < 0 {
		page = 0
	}
	return &PageError{Page: page, Err: err}
}

// NumPages returns the number of pages in the image. It walks the IFD chain
// without decoding any pixel data. An error is returned, with page context,
// if the chain is cyclic, exceeds the page limit, or is truncated.
func (d *Decoder) NumPages() (int, error) {
	if err := d.walk(maxPages); err != nil {
		return 0, d.walkError(err)
	}
	return len(d.offsets), nil
}

// pageDecoder returns a decoder for the given zero-based page.
func (d *Decoder) pageDecoder(page int) (*decoder, error) {
	if page < 0 {
		return nil, &PageError{Page: page, Err: FormatError("negative page index")}
	}
	if err := d.walk(page); err != nil {
		return nil, d.walkError(err)
	}
	if page >= len(d.offsets) {
		return nil, &PageError{Page: page, Err: FormatError("page index out of range")}
	}
	dec, err := newDecoderAt(d.ra, d.byteOrder, d.offsets[page])
	if err != nil {
		return nil, &PageError{Page: page, Err: err}
	}
	return dec, nil
}

// PageConfig returns the color model and dimensions of the given zero-based
// page without decoding its pixel data.
func (d *Decoder) PageConfig(page int) (image.Config, error) {
	dec, err := d.pageDecoder(page)
	if err != nil {
		return image.Config{}, err
	}
	return dec.config, nil
}

// DecodePage decodes and returns the given zero-based page. Pages may be
// decoded in any order and more than once; each call returns an independent
// image.
func (d *Decoder) DecodePage(page int) (image.Image, error) {
	dec, err := d.pageDecoder(page)
	if err != nil {
		return nil, err
	}
	img, err := dec.decodeImage()
	if err != nil {
		return nil, &PageError{Page: page, Err: err}
	}
	return img, nil
}

// NextPage decodes and returns the next page in sequence, starting with
// page 0. It returns io.EOF when all pages have been returned. If a page
// fails to decode, NextPage returns the error (with page context) and
// advances past the bad page, so a subsequent call continues with the
// following page.
func (d *Decoder) NextPage() (image.Image, error) {
	page := d.next
	if err := d.walk(page); err != nil {
		return nil, d.walkError(err)
	}
	if page >= len(d.offsets) {
		return nil, io.EOF
	}
	d.next++
	return d.DecodePage(page)
}
