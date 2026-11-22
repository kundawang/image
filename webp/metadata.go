// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webp

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"

	"golang.org/x/image/riff"
	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
)

// DefaultMaxMetadataSize is the default limit, in bytes, on the payload size
// of any single metadata chunk and on the total payload size of all of them.
// See MetadataOptions.MaxSize.
const DefaultMaxMetadataSize = 32 << 20

// DefaultMaxMetadataChunks is the default limit on the number of metadata
// chunks reported by DecodeConfigWithMetadata. See MetadataOptions.MaxChunks.
const DefaultMaxMetadataChunks = 1024

var (
	// ErrMetadataTooLarge is returned when a metadata chunk, or the metadata
	// in aggregate, exceeds the size limit configured in MetadataOptions.
	ErrMetadataTooLarge = errors.New("metadata too large")

	// ErrTooManyMetadataChunks is returned when a WebP file contains more
	// metadata chunks than MetadataOptions.MaxChunks allows.
	ErrTooManyMetadataChunks = errors.New("too many metadata chunks")

	// ErrDuplicateMetadata is returned when a second ICC profile, EXIF or XMP
	// chunk is found and MetadataOptions.RejectDuplicateMetadata is true.
	ErrDuplicateMetadata = errors.New("duplicate metadata chunk")
)

// ChunkType is a four-character code that identifies a RIFF chunk type.
type ChunkType [4]byte

// String returns c interpreted as a string.
func (c ChunkType) String() string {
	return string(c[:])
}

// The well-known WebP metadata chunk types.
var (
	ChunkICCP = ChunkType{'I', 'C', 'C', 'P'} // ICC color profile.
	ChunkEXIF = ChunkType{'E', 'X', 'I', 'F'} // EXIF metadata.
	ChunkXMP  = ChunkType{'X', 'M', 'P', ' '} // XMP metadata.
)

// fccANIM and fccANMF are animation chunks. They are image data rather than
// metadata, so they are not reported by DecodeConfigWithMetadata.
var (
	fccANIM = riff.FourCC{'A', 'N', 'I', 'M'}
	fccANMF = riff.FourCC{'A', 'N', 'M', 'F'}
)

// MetadataKind classifies a metadata chunk. Chunk types other than ICCP, EXIF
// and "XMP " are reported as KindUnknown with their four bytes preserved in
// MetadataChunk.Type.
type MetadataKind uint8

const (
	// KindUnknown is a chunk that is neither an ICC profile, EXIF nor XMP.
	KindUnknown MetadataKind = iota
	// KindICC is an ICC color profile chunk ("ICCP").
	KindICC
	// KindEXIF is an EXIF metadata chunk ("EXIF").
	KindEXIF
	// KindXMP is an XMP metadata chunk ("XMP ").
	KindXMP
)

// Kind returns the MetadataKind of c.
func (c ChunkType) Kind() MetadataKind {
	switch c {
	case ChunkICCP:
		return KindICC
	case ChunkEXIF:
		return KindEXIF
	case ChunkXMP:
		return KindXMP
	}
	return KindUnknown
}

// MetadataChunk describes one non-image RIFF chunk.
type MetadataChunk struct {
	// Type is the chunk's four-character code. Any four bytes are allowed;
	// chunks that are not ICCP, EXIF or "XMP " have KindUnknown.
	Type ChunkType

	// Offset is the byte offset, from the start of the RIFF stream, of the
	// first byte of the chunk's payload (that is, just past the 8-byte chunk
	// header and past the padding byte of the preceding chunk).
	//
	// Offsets are counted, not sought to, so they are also available when the
	// input is not an io.Seeker. In the slice returned by
	// DecodeConfigWithMetadata, offsets increase monotonically in file order.
	Offset int64

	// Length is the payload length in bytes. It excludes the padding byte that
	// follows the payload of an odd-length chunk. Length is meaningful even
	// when Data is nil.
	Length uint32

	// Data is a private copy of the chunk's payload, or nil if payload data
	// was omitted (see MetadataOptions.OmitData). A non-nil Data of length
	// zero represents an empty chunk.
	//
	// Data does not alias the input stream, another chunk's Data or a decoded
	// image, so the caller may modify it without affecting anything else, and
	// a malformed payload cannot corrupt image decoding.
	Data []byte

	// Duplicate reports whether an earlier chunk with the same non-zero
	// MetadataKind was already reported. Duplicates are reported rather than
	// rejected by default, so that a malformed file can still be audited;
	// set MetadataOptions.RejectDuplicateMetadata to treat them as an error.
	Duplicate bool
}

// Metadata is the ordered metadata found in a WebP file.
//
// Chunks are reported in the order they appear in the file. The ALPH, VP8,
// VP8L and VP8X chunks describe the image itself and are not included.
//
// This package does not interpret payloads; EXIF, XMP and ICC bytes are
// returned verbatim. Calling code is responsible for parsing them.
type Metadata struct {
	Chunks []MetadataChunk
}

// Chunk returns a copy of the first chunk of the given kind, or nil if there
// is none. The copy's Data is itself copied, so the caller may modify either
// without affecting m or another caller.
func (m *Metadata) Chunk(kind MetadataKind) *MetadataChunk {
	i := m.chunkIndex(kind)
	if i < 0 {
		return nil
	}
	c := m.Chunks[i]
	c.Data = append([]byte(nil), m.Chunks[i].Data...)
	if m.Chunks[i].Data == nil {
		c.Data = nil
	}
	return &c
}

func (m *Metadata) chunkIndex(kind MetadataKind) int {
	if m == nil {
		return -1
	}
	for i := range m.Chunks {
		if m.Chunks[i].Type.Kind() == kind {
			return i
		}
	}
	return -1
}

func (m *Metadata) data(kind MetadataKind) []byte {
	i := m.chunkIndex(kind)
	if i < 0 || m.Chunks[i].Data == nil {
		return nil
	}
	// Return a copy: the getters are read-only queries and must not hand out
	// a buffer that is shared with the Metadata or with another caller.
	return append([]byte(nil), m.Chunks[i].Data...)
}

// ICCProfile returns a copy of the payload of the first ICC color profile
// chunk, or nil if there is none or payload data was omitted.
func (m *Metadata) ICCProfile() []byte { return m.data(KindICC) }

// EXIF returns a copy of the payload of the first EXIF chunk, or nil if there
// is none or payload data was omitted. The bytes are returned exactly as
// stored in the file; this package does not parse the EXIF/TIFF structure.
func (m *Metadata) EXIF() []byte { return m.data(KindEXIF) }

// XMP returns a copy of the payload of the first XMP chunk, or nil if there is
// none or payload data was omitted. The bytes are returned exactly as stored
// in the file; this package does not parse the XML.
func (m *Metadata) XMP() []byte { return m.data(KindXMP) }

// MetadataOptions configures DecodeConfigWithMetadata. The zero value is
// valid: it keeps payload data, enforces the default limits and reports, but
// does not reject, duplicate metadata chunks.
type MetadataOptions struct {
	// OmitData, if true, records each metadata chunk's type, offset and
	// length but does not copy its payload into MetadataChunk.Data. The
	// chunk is still read and validated exactly once, so no extra scan of the
	// input is needed to obtain the payload later from a seekable stream.
	OmitData bool

	// MaxSize limits, in bytes, the payload of any single metadata chunk and
	// the total payload of all metadata chunks. Zero means
	// DefaultMaxMetadataSize. A negative value means no limit.
	//
	// The limit is checked against the lengths declared in the RIFF headers,
	// before any payload is allocated, and applies whether or not OmitData is
	// set, so an oversized chunk cannot be used to force a large allocation
	// or to overflow a counter.
	MaxSize int

	// MaxChunks limits the number of metadata chunks reported. Zero means
	// DefaultMaxMetadataChunks. A negative value means no limit.
	//
	// Many zero-length chunks can be described by few input bytes, so this
	// bounds the memory used for a maliciously crafted file.
	MaxChunks int

	// RejectDuplicateMetadata, if true, makes a second ICCP, EXIF or "XMP "
	// chunk an error (ErrDuplicateMetadata). By default duplicates are
	// reported in file order and MetadataChunk.Duplicate is set on all but
	// the first of each kind, so that a caller auditing an untrusted file can
	// still see every occurrence. The WebP container specification permits at
	// most one chunk of each of these kinds.
	//
	// Unknown chunk types may always repeat.
	RejectDuplicateMetadata bool
}

// countingReader counts the bytes read from r. Counting, rather than seeking,
// lets chunk offsets be reported for non-seekable readers.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func chunkError(id ChunkType, offset int64, err error) error {
	return fmt.Errorf("webp: chunk %q at offset %d: %w", id[:], offset, err)
}

// maxInt returns the largest value of type int as an int64.
func maxInt() int64 {
	return int64(^uint(0) >> 1)
}

// DecodeConfigWithMetadata returns the color model and dimensions of a WebP
// image together with the file's metadata chunks, without decoding pixel
// data. It reads r exactly once and never calls Seek, so r may be a
// non-seekable stream.
//
// The returned image.Config equals the value DecodeConfig returns for the
// same input. Because this function must scan past the image chunks to reach
// trailing metadata, it also validates them the way Decode does (for example,
// that a VP8X canvas matches the VP8/VP8L dimensions). Consequently it can
// reject input that DecodeConfig alone, which stops at the first VP8X or image
// chunk, would accept. Image chunks that appear after the first image data
// chunk are ignored, as DecodeConfig would already have returned.
//
// Metadata chunks are reported in file order, including chunk types this
// package does not recognize. Their payloads are copied unless opts.OmitData
// is set.
//
// Neither EXIF, XMP nor ICC content is interpreted, and no pixel data is
// decoded, so a malformed metadata payload cannot change the returned config
// or affect a later Decode of the same bytes: Decode ignores metadata chunks.
//
// The RIFF container, the image chunks, the metadata limits in opts, and (when
// RejectDuplicateMetadata is set) duplicate metadata chunks are all validated.
// Any error identifies the chunk type and byte offset, and errors.Is can test
// for ErrMetadataTooLarge, ErrTooManyMetadataChunks and ErrDuplicateMetadata.
//
// If opts is nil, the zero MetadataOptions is used.
func DecodeConfigWithMetadata(r io.Reader, opts *MetadataOptions) (image.Config, *Metadata, error) {
	o := MetadataOptions{}
	if opts != nil {
		o = *opts
	}
	maxSize := int64(o.MaxSize)
	if maxSize == 0 {
		maxSize = DefaultMaxMetadataSize
	}
	maxChunks := int64(o.MaxChunks)
	if maxChunks == 0 {
		maxChunks = DefaultMaxMetadataChunks
	}

	cr := &countingReader{r: r}
	formType, riffReader, err := riff.NewReader(cr)
	if err != nil {
		return image.Config{}, nil, err
	}
	if formType != fccWEBP {
		return image.Config{}, nil, errInvalidFormat
	}

	var (
		md Metadata

		cfg        image.Config
		haveConfig bool

		seenVP8X     bool
		seenImage    bool
		wantAlpha    bool
		alphaSeen    bool
		canvasWidth  uint64
		canvasHeight uint64

		total    int64
		seenKind [KindXMP + 1]bool

		// prevID and prevOffset identify the chunk whose payload was most
		// recently consumed. They provide error context when riff.Reader
		// reports a problem while draining that chunk or its padding.
		prevID     ChunkType
		prevOffset int64
		havePrev   bool
	)

	for {
		chunkID, chunkLen, chunkData, err := riffReader.Next()
		if err == io.EOF {
			if !haveConfig {
				// Neither a VP8X chunk nor an image data chunk was present.
				return image.Config{}, nil, errInvalidFormat
			}
			return cfg, &md, nil
		}
		if err != nil {
			if havePrev {
				return image.Config{}, nil, fmt.Errorf(
					"webp: RIFF scan at offset %d, after chunk %q at offset %d: %w",
					cr.n, prevID[:], prevOffset, err)
			}
			return image.Config{}, nil, fmt.Errorf("webp: RIFF scan at offset %d: %w", cr.n, err)
		}

		// riff.Reader has consumed this chunk's 8-byte header and, if the
		// previous chunk had odd length, its padding byte. cr.n is therefore
		// the offset of this chunk's payload.
		id := ChunkType(chunkID)
		offset := cr.n
		prevID, prevOffset, havePrev = id, offset, true

		switch chunkID {
		case fccVP8X:
			if seenImage {
				// A VP8X chunk that follows image data is out of order. It
				// cannot affect the configuration: DecodeConfig would already
				// have returned from the image data chunk. Skip it, keeping
				// the metadata scan going.
				continue
			}
			if seenVP8X {
				return image.Config{}, nil, errInvalidFormat
			}
			seenVP8X = true
			if chunkLen != 10 {
				return image.Config{}, nil, errInvalidFormat
			}
			var buf [10]byte
			if _, err := io.ReadFull(chunkData, buf[:]); err != nil {
				return image.Config{}, nil, chunkError(id, offset, err)
			}
			const alphaBit = 1 << 4
			wantAlpha = (buf[0] & alphaBit) != 0
			canvasWidth = uint64(uint32(buf[4])|uint32(buf[5])<<8|uint32(buf[6])<<16) + 1
			canvasHeight = uint64(uint32(buf[7])|uint32(buf[8])<<8|uint32(buf[9])<<16) + 1
			if canvasWidth*canvasHeight > 1<<31-1 {
				// The product of canvas width and height must fit in an int.
				return image.Config{}, nil, errInvalidFormat
			}
			if !haveConfig {
				cm := color.YCbCrModel
				if wantAlpha {
					cm = color.NYCbCrAModel
				}
				cfg = image.Config{
					ColorModel: cm,
					Width:      int(canvasWidth),
					Height:     int(canvasHeight),
				}
				haveConfig = true
			}

		case fccVP8:
			if seenImage {
				// A second image data chunk. DecodeConfig stops at the first,
				// so it grants this one no further validation.
				continue
			}
			if wantAlpha || int32(chunkLen) < 0 {
				return image.Config{}, nil, errInvalidFormat
			}
			d := vp8.NewDecoder()
			d.Init(chunkData, int(chunkLen))
			fh, err := d.DecodeFrameHeader()
			if err != nil {
				return image.Config{}, nil, chunkError(id, offset, err)
			}
			if seenVP8X {
				if uint64(fh.Width) != canvasWidth || uint64(fh.Height) != canvasHeight {
					return image.Config{}, nil, errInvalidFormat
				}
			}
			seenImage = true
			if !haveConfig {
				cfg = image.Config{
					ColorModel: color.YCbCrModel,
					Width:      fh.Width,
					Height:     fh.Height,
				}
				haveConfig = true
			}

		case fccVP8L:
			if seenImage {
				continue
			}
			if alphaSeen {
				return image.Config{}, nil, errInvalidFormat
			}
			c, err := vp8l.DecodeConfig(chunkData)
			if err != nil {
				return image.Config{}, nil, chunkError(id, offset, err)
			}
			if seenVP8X {
				if uint64(c.Width) != canvasWidth || uint64(c.Height) != canvasHeight {
					return image.Config{}, nil, errInvalidFormat
				}
			}
			seenImage = true
			if !haveConfig {
				cfg = c
				haveConfig = true
			}

		case fccALPH:
			if seenImage {
				continue
			}
			if !wantAlpha {
				return image.Config{}, nil, errInvalidFormat
			}
			wantAlpha = false
			alphaSeen = true
			// The payload is not needed to determine the configuration. The
			// next call to Next discards any remainder.

		case fccANIM, fccANMF:
			// Animation chunks describe image frames, not metadata. Skip them
			// as the next call to Next will drain the payload.

		default:
			if maxSize >= 0 {
				if int64(chunkLen) > maxSize {
					return image.Config{}, nil, chunkError(id, offset, fmt.Errorf(
						"%w: chunk length %d exceeds limit %d",
						ErrMetadataTooLarge, chunkLen, maxSize))
				}
				if total > maxSize-int64(chunkLen) {
					return image.Config{}, nil, chunkError(id, offset, fmt.Errorf(
						"%w: total length exceeds limit %d",
						ErrMetadataTooLarge, maxSize))
				}
			} else if total > math.MaxInt64-int64(chunkLen) {
				return image.Config{}, nil, chunkError(id, offset, fmt.Errorf(
					"%w: total length overflows int64", ErrMetadataTooLarge))
			}
			if maxChunks >= 0 && int64(len(md.Chunks)) >= maxChunks {
				return image.Config{}, nil, chunkError(id, offset, fmt.Errorf(
					"%w: limit %d", ErrTooManyMetadataChunks, maxChunks))
			}
			kind := id.Kind()
			duplicate := false
			if kind != KindUnknown {
				duplicate = seenKind[kind]
				if duplicate && o.RejectDuplicateMetadata {
					return image.Config{}, nil, chunkError(id, offset, ErrDuplicateMetadata)
				}
				seenKind[kind] = true
			}
			total += int64(chunkLen)
			var data []byte
			if !o.OmitData {
				if int64(chunkLen) > maxInt() {
					return image.Config{}, nil, chunkError(id, offset, fmt.Errorf(
						"%w: chunk length %d overflows int",
						ErrMetadataTooLarge, chunkLen))
				}
				data = make([]byte, chunkLen)
				if _, err := io.ReadFull(chunkData, data); err != nil {
					return image.Config{}, nil, chunkError(id, offset, err)
				}
			} else if _, err := io.Copy(io.Discard, chunkData); err != nil {
				return image.Config{}, nil, chunkError(id, offset, err)
			}
			md.Chunks = append(md.Chunks, MetadataChunk{
				Type:      id,
				Offset:    offset,
				Length:    chunkLen,
				Data:      data,
				Duplicate: duplicate,
			})
		}
	}
}
