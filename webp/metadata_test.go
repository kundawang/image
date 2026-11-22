// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rawChunk is a chunk as found by the independent RIFF parser below.
type rawChunk struct {
	id     [4]byte
	offset int64 // Payload offset from the start of the file.
	length uint32
	data   []byte
}

// parseRIFF is a deliberately simple, independent RIFF parser used to check
// the offsets and lengths reported by DecodeConfigWithMetadata. It does not
// use the riff package.
func parseRIFF(t *testing.T, b []byte) []rawChunk {
	t.Helper()
	if len(b) < 12 {
		t.Fatalf("file is only %d bytes", len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		t.Fatalf("not a WEBP RIFF file: % x", b[:12])
	}
	riffSize := int64(binary.LittleEndian.Uint32(b[4:8]))
	end := 8 + riffSize
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	var chunks []rawChunk
	pos := int64(12)
	for pos+8 <= end {
		var c rawChunk
		copy(c.id[:], b[pos:pos+4])
		c.length = binary.LittleEndian.Uint32(b[pos+4 : pos+8])
		c.offset = pos + 8
		dataEnd := c.offset + int64(c.length)
		if dataEnd > int64(len(b)) {
			t.Fatalf("chunk %q at offset %d: declared length %d overruns file (%d)",
				c.id[:], c.offset, c.length, len(b))
		}
		c.data = b[c.offset:dataEnd]
		chunks = append(chunks, c)
		pos = dataEnd + int64(c.length&1)
	}
	return chunks
}

func isImageChunk(id [4]byte) bool {
	return id == [4]byte{'V', 'P', '8', ' '} ||
		id == [4]byte{'V', 'P', '8', 'L'} ||
		id == [4]byte{'V', 'P', '8', 'X'} ||
		id == [4]byte{'A', 'L', 'P', 'H'} ||
		id == [4]byte{'A', 'N', 'I', 'M'} ||
		id == [4]byte{'A', 'N', 'M', 'F'}
}

// TestDecodeConfigWithMetadataAgainstDecodeConfig checks every real fixture:
// the returned config must equal DecodeConfig's, and the metadata inventory
// must match an independent parse of the same bytes, in file order, with
// payloads that are exact copies.
func TestDecodeConfigWithMetadataAgainstDecodeConfig(t *testing.T) {
	filenames, err := filepath.Glob("../testdata/*.webp")
	if err != nil {
		t.Fatal(err)
	}
	if len(filenames) == 0 {
		t.Fatal("no testdata fixtures found")
	}
	for _, filename := range filenames {
		t.Run(filepath.Base(filename), func(t *testing.T) {
			b, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}

			wantCfg, wantErr := DecodeConfig(bytes.NewReader(b))
			gotCfg, md, gotErr := DecodeConfigWithMetadata(bytes.NewReader(b), nil)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("DecodeConfig error %v, DecodeConfigWithMetadata error %v", wantErr, gotErr)
			}
			if wantErr == nil && gotCfg != wantCfg {
				t.Fatalf("config: got %+v, want %+v", gotCfg, wantCfg)
			}
			if gotErr != nil {
				return
			}
			if md == nil {
				t.Fatal("nil Metadata with nil error")
			}

			var want []rawChunk
			for _, c := range parseRIFF(t, b) {
				if !isImageChunk(c.id) {
					want = append(want, c)
				}
			}
			if len(md.Chunks) != len(want) {
				t.Fatalf("got %d metadata chunks, want %d: %+v", len(md.Chunks), len(want), md.Chunks)
			}
			for i, w := range want {
				g := md.Chunks[i]
				if g.Type != ChunkType(w.id) {
					t.Errorf("chunk %d: type got %q, want %q", i, g.Type, w.id[:])
				}
				if g.Offset != w.offset {
					t.Errorf("chunk %d (%q): offset got %d, want %d", i, g.Type, g.Offset, w.offset)
				}
				if g.Length != w.length {
					t.Errorf("chunk %d (%q): length got %d, want %d", i, g.Type, g.Length, w.length)
				}
				if !bytes.Equal(g.Data, w.data) {
					t.Errorf("chunk %d (%q): data mismatch", i, g.Type)
				}
				if i > 0 && g.Offset <= md.Chunks[i-1].Offset {
					t.Errorf("chunk %d (%q): offset %d is not after previous %d",
						i, g.Type, g.Offset, md.Chunks[i-1].Offset)
				}
			}
		})
	}
}

// TestMetadataRealICCP checks the ICC profile of a real fixture that has one.
func TestMetadataRealICCP(t *testing.T) {
	b, err := os.ReadFile("../testdata/gopher-doc.with-alpha.lossless.webp")
	if err != nil {
		t.Fatal(err)
	}
	_, md, err := DecodeConfigWithMetadata(bytes.NewReader(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	icc := md.Chunk(KindICC)
	if icc == nil {
		t.Fatal("no ICC chunk reported")
	}
	if icc.Type != ChunkICCP {
		t.Errorf("type: got %q, want ICCP", icc.Type)
	}
	want := 672
	if len(icc.Data) != want || int(icc.Length) != want {
		t.Errorf("ICC profile length: got (%d, %d), want %d", len(icc.Data), icc.Length, want)
	}
	if !bytes.Equal(md.ICCProfile(), icc.Data) {
		t.Error("ICCProfile does not match the reported chunk data")
	}
	if got := md.EXIF(); got != nil {
		t.Errorf("EXIF: got %d bytes, want nil", len(got))
	}
	if got := md.XMP(); got != nil {
		t.Errorf("XMP: got %d bytes, want nil", len(got))
	}
}

// chunkSpec describes a chunk when building a synthetic WebP file.
type chunkSpec struct {
	id   [4]byte
	data []byte
	// declared, if true, overrides the length written in the chunk header.
	declared *uint32
}

func u32ptr(v uint32) *uint32 { return &v }

// buildWebP assembles a RIFF/WEBP file from chunks. If riffSize is non-nil it
// overrides the RIFF size field.
func buildWebP(chunks []chunkSpec, riffSize *uint32) []byte {
	var body bytes.Buffer
	body.WriteString("WEBP")
	for _, c := range chunks {
		body.Write(c.id[:])
		n := uint32(len(c.data))
		if c.declared != nil {
			n = *c.declared
		}
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], n)
		body.Write(lenBuf[:])
		body.Write(c.data)
		if len(c.data)&1 == 1 {
			body.WriteByte(0)
		}
	}
	var sizeBuf [4]byte
	if riffSize != nil {
		binary.LittleEndian.PutUint32(sizeBuf[:], *riffSize)
	} else {
		binary.LittleEndian.PutUint32(sizeBuf[:], uint32(body.Len()))
	}
	out := append([]byte("RIFF"), sizeBuf[:]...)
	return append(out, body.Bytes()...)
}

// vp8xChunk returns a VP8X chunk with the given flags and 1x1 canvas.
func vp8xChunk(flags byte) chunkSpec {
	return chunkSpec{
		id: [4]byte{'V', 'P', '8', 'X'},
		data: []byte{flags, 0, 0, 0,
			0, 0, 0, // Canvas width minus one.
			0, 0, 0, // Canvas height minus one.
		},
	}
}

// TestMetadataMixedChunks exercises odd-length padding, repeated unknown
// types, chunk ordering and offsets in one scan.
func TestMetadataMixedChunks(t *testing.T) {
	exif := []byte("EXIF-odd")  // 8 bytes, even.
	xmp := []byte("<x>odd</x>") // 10 bytes.
	icc := []byte("icc")        // 3 bytes, odd: exercises padding.
	unknownA := []byte{0x01}    // 1 byte, odd.
	unknownB := []byte("ng")    // 2 bytes.
	exif2 := []byte("second")   // 6 bytes.

	chunks := []chunkSpec{
		vp8xChunk(0),
		{id: ChunkICCP, data: icc},
		{id: ChunkEXIF, data: exif},
		{id: [4]byte{0x00, 0xff, 'A', ' '}, data: unknownA},
		{id: ChunkXMP, data: xmp},
		{id: [4]byte{0x00, 0xff, 'A', ' '}, data: unknownB},
		{id: ChunkEXIF, data: exif2},
	}
	file := buildWebP(chunks, nil)

	cfg, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 1 || cfg.Height != 1 {
		t.Errorf("config: got %dx%d, want 1x1", cfg.Width, cfg.Height)
	}
	if got, want := len(md.Chunks), 6; got != want {
		t.Fatalf("got %d chunks, want %d", got, want)
	}

	// Cross-check offsets and lengths against the independent parser.
	var want []rawChunk
	for _, c := range parseRIFF(t, file) {
		if !isImageChunk(c.id) {
			want = append(want, c)
		}
	}
	for i := range want {
		g := md.Chunks[i]
		if g.Type != ChunkType(want[i].id) || g.Offset != want[i].offset ||
			g.Length != want[i].length || !bytes.Equal(g.Data, want[i].data) {
			t.Fatalf("chunk %d: got {%q %d %d %d bytes}, want {%q %d %d %d bytes}",
				i, g.Type, g.Offset, g.Length, len(g.Data),
				want[i].id[:], want[i].offset, want[i].length, len(want[i].data))
		}
	}

	// The getters return the first chunk of each kind.
	if !bytes.Equal(md.EXIF(), exif) {
		t.Errorf("EXIF: got %q, want %q", md.EXIF(), exif)
	}
	if !bytes.Equal(md.XMP(), xmp) {
		t.Errorf("XMP: got %q, want %q", md.XMP(), xmp)
	}
	if !bytes.Equal(md.ICCProfile(), icc) {
		t.Errorf("ICC: got %q, want %q", md.ICCProfile(), icc)
	}

	// Duplicates are flagged, and the unknown type repeats without error.
	if md.Chunks[1].Duplicate {
		t.Error("first EXIF marked duplicate")
	}
	if !md.Chunks[5].Duplicate {
		t.Error("second EXIF not marked duplicate")
	}
	for i, c := range md.Chunks {
		if c.Type.Kind() == KindUnknown && c.Duplicate {
			t.Errorf("chunk %d (%q): unknown chunk marked duplicate", i, c.Type)
		}
	}
	if got, want := md.Chunks[2].Type.String(), string([]byte{0x00, 0xff, 'A', ' '}); got != want {
		t.Errorf("unknown type string: got %q, want %q", got, want)
	}
}

// TestMetadataDuplicateRejected checks the default policy for duplicates of
// the chunks the container spec allows at most once.
func TestMetadataDuplicateRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   [4]byte
	}{
		{"ICCP", ChunkICCP},
		{"EXIF", ChunkEXIF},
		{"XMP", ChunkXMP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := buildWebP([]chunkSpec{
				vp8xChunk(0),
				{id: tc.id, data: []byte("a")},
				{id: tc.id, data: []byte("b")},
			}, nil)
			_, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{
				RejectDuplicateMetadata: true,
			})
			if !errors.Is(err, ErrDuplicateMetadata) {
				t.Fatalf("got error %v, want ErrDuplicateMetadata", err)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error %q does not name the chunk type %q", err, tc.name)
			}
		})
	}
}

// TestMetadataRepeatedUnknownChunk checks that unknown chunks may repeat and
// that all of them are reported.
func TestMetadataRepeatedUnknownChunk(t *testing.T) {
	id := [4]byte{'Z', 'Z', 'Z', 'Z'}
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: id, data: []byte("1")},
		{id: id, data: []byte("22")},
		{id: id, data: []byte("333")},
	}, nil)
	_, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(md.Chunks), 3; got != want {
		t.Fatalf("got %d unknown chunks, want %d", got, want)
	}
	for i, c := range md.Chunks {
		if c.Duplicate {
			t.Errorf("chunk %d: unknown chunk marked duplicate", i)
		}
	}
}

// TestMetadataSizeLimits checks the per-chunk and cumulative limits, including
// that they are enforced against declared lengths before allocation.
func TestMetadataSizeLimits(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		file := buildWebP([]chunkSpec{
			vp8xChunk(0),
			{id: ChunkEXIF, data: bytes.Repeat([]byte{'x'}, 100)},
		}, nil)
		_, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxSize: 99})
		if !errors.Is(err, ErrMetadataTooLarge) {
			t.Fatalf("got error %v, want ErrMetadataTooLarge", err)
		}
		if !strings.Contains(err.Error(), "EXIF") || !strings.Contains(err.Error(), "offset") {
			t.Errorf("error %q lacks chunk/offset context", err)
		}
		// The declared length is checked before allocation, so OmitData does
		// not bypass the limit.
		_, _, err = DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{
			MaxSize:  99,
			OmitData: true,
		})
		if !errors.Is(err, ErrMetadataTooLarge) {
			t.Fatalf("OmitData: got error %v, want ErrMetadataTooLarge", err)
		}
	})

	t.Run("cumulative", func(t *testing.T) {
		file := buildWebP([]chunkSpec{
			vp8xChunk(0),
			{id: ChunkEXIF, data: bytes.Repeat([]byte{'x'}, 60)},
			{id: ChunkXMP, data: bytes.Repeat([]byte{'y'}, 60)},
		}, nil)
		// Each chunk fits under 100, but the total does not.
		_, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxSize: 100})
		if !errors.Is(err, ErrMetadataTooLarge) {
			t.Fatalf("got error %v, want ErrMetadataTooLarge", err)
		}
		if !strings.Contains(err.Error(), "XMP") {
			t.Errorf("error %q does not name the offending chunk", err)
		}
	})

	t.Run("exact", func(t *testing.T) {
		file := buildWebP([]chunkSpec{
			vp8xChunk(0),
			{id: ChunkEXIF, data: bytes.Repeat([]byte{'x'}, 100)},
		}, nil)
		if _, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxSize: 100}); err != nil {
			t.Fatalf("exactly at the limit: %v", err)
		}
	})

	t.Run("unlimited", func(t *testing.T) {
		file := buildWebP([]chunkSpec{
			vp8xChunk(0),
			{id: ChunkEXIF, data: bytes.Repeat([]byte{'x'}, 100)},
		}, nil)
		if _, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxSize: -1}); err != nil {
			t.Fatalf("negative MaxSize should be unlimited: %v", err)
		}
	})

	t.Run("declared oversized", func(t *testing.T) {
		// A chunk header that declares 0xffffffff bytes. The RIFF container
		// rejects it before any metadata limit applies, but the check must be
		// made on the declared length, not on the data actually read.
		file := buildWebP([]chunkSpec{
			vp8xChunk(0),
			{id: ChunkEXIF, data: []byte("x"), declared: u32ptr(0xf0000000)},
		}, nil)
		_, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxSize: 1 << 20})
		if err == nil {
			t.Fatal("got nil error for an oversized declared chunk")
		}
	})
}

// TestMetadataMaxChunks checks the chunk-count limit, which bounds bookkeeping
// for files full of zero-length chunks.
func TestMetadataMaxChunks(t *testing.T) {
	var chunks []chunkSpec
	chunks = append(chunks, vp8xChunk(0))
	for i := 0; i < 10; i++ {
		chunks = append(chunks, chunkSpec{id: [4]byte{'Z', 'Z', 'Z', 'Z'}})
	}
	file := buildWebP(chunks, nil)

	if _, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxChunks: 10}); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	_, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxChunks: 9})
	if !errors.Is(err, ErrTooManyMetadataChunks) {
		t.Fatalf("got error %v, want ErrTooManyMetadataChunks", err)
	}
	if _, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxChunks: -1}); err != nil {
		t.Fatalf("negative MaxChunks should be unlimited: %v", err)
	}
}

// TestMetadataOmitData checks that payload retention can be turned off while
// offsets and lengths remain exact.
func TestMetadataOmitData(t *testing.T) {
	exif := []byte("EXIF payload")
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: exif},
	}, nil)
	_, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{OmitData: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(md.Chunks), 1; got != want {
		t.Fatalf("got %d chunks, want %d", got, want)
	}
	c := md.Chunks[0]
	if c.Data != nil {
		t.Errorf("Data is non-nil (%d bytes) with OmitData", len(c.Data))
	}
	if int(c.Length) != len(exif) {
		t.Errorf("Length: got %d, want %d", c.Length, len(exif))
	}
	if c.Offset != 20+10+8 {
		t.Errorf("Offset: got %d, want %d", c.Offset, 20+10+8)
	}
	if md.EXIF() != nil {
		t.Error("EXIF returned data with OmitData")
	}
	// Type remains available for inventorying the file.
	if c.Type != ChunkEXIF || c.Type.Kind() != KindEXIF {
		t.Errorf("type: got %q (kind %d), want EXIF", c.Type, c.Type.Kind())
	}
}

// onlyReader hides any Seeker/WriterTo/ReaderFrom implementation of its
// underlying reader, so the test proves that no seeking or re-scanning is
// involved.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// TestMetadataNonSeekable checks offsets on a stream that cannot seek and
// confirms the input is consumed once, in order.
func TestMetadataNonSeekable(t *testing.T) {
	icc := []byte("abc") // Odd, so the next chunk needs a padding byte.
	xmp := []byte("XMP data")
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkICCP, data: icc},
		{id: ChunkXMP, data: xmp},
	}, nil)

	_, wantMD, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	br := &byteCountingReader{r: bytes.NewReader(file)}
	_, gotMD, err := DecodeConfigWithMetadata(onlyReader{br}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if br.n != len(file) {
		t.Errorf("read %d bytes, want exactly the %d-byte file (no re-scan)", br.n, len(file))
	}
	if len(gotMD.Chunks) != len(wantMD.Chunks) {
		t.Fatalf("got %d chunks, want %d", len(gotMD.Chunks), len(wantMD.Chunks))
	}
	for i := range gotMD.Chunks {
		g, w := gotMD.Chunks[i], wantMD.Chunks[i]
		if g.Offset != w.Offset || g.Length != w.Length || !bytes.Equal(g.Data, w.Data) {
			t.Errorf("chunk %d: got {%d %d}, want {%d %d}", i, g.Offset, g.Length, w.Offset, w.Length)
		}
	}
	if gotMD.Chunks[1].Offset <= gotMD.Chunks[0].Offset {
		t.Error("offsets are not increasing")
	}
	// Explicit odd-padding check: ICC is 3 bytes, so XMP starts 1 byte later
	// than the end of the ICC payload.
	if want := gotMD.Chunks[0].Offset + int64(gotMD.Chunks[0].Length) + 1 + 8; gotMD.Chunks[1].Offset != want {
		t.Errorf("padding not skipped: XMP offset got %d, want %d", gotMD.Chunks[1].Offset, want)
	}
}

type byteCountingReader struct {
	r io.Reader
	n int
}

func (b *byteCountingReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += n
	return n, err
}

// TestMetadataTruncated checks that a truncated payload or header produces an
// error naming the chunk and its offset.
func TestMetadataTruncated(t *testing.T) {
	exif := bytes.Repeat([]byte{'e'}, 20)
	full := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: exif},
		{id: ChunkXMP, data: []byte("after")},
	}, nil)

	t.Run("payload", func(t *testing.T) {
		// Cut the XMP chunk's payload short.
		truncated := full[:len(full)-3]
		_, _, err := DecodeConfigWithMetadata(bytes.NewReader(truncated), nil)
		if err == nil {
			t.Fatal("got nil error for a truncated file")
		}
		if !strings.Contains(err.Error(), "XMP") {
			t.Errorf("error %q does not name the truncated chunk", err)
		}
	})

	t.Run("header", func(t *testing.T) {
		// Cut the final chunk's header in half.
		truncated := full[:len(full)-8-6-3]
		_, _, err := DecodeConfigWithMetadata(bytes.NewReader(truncated), nil)
		if err == nil {
			t.Fatal("got nil error for a truncated header")
		}
		if !strings.Contains(err.Error(), "EXIF") {
			t.Errorf("error %q does not name the chunk before the truncation", err)
		}
	})
}

// TestMetadataNoImageChunk checks that metadata alone is not a usable image.
func TestMetadataNoImageChunk(t *testing.T) {
	file := buildWebP([]chunkSpec{
		{id: ChunkEXIF, data: []byte("no image here")},
	}, nil)
	_, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != errInvalidFormat {
		t.Fatalf("got error %v, want errInvalidFormat", err)
	}
}

// TestMetadataGetDoesNotShareBuffers checks that the query methods hand out
// copies: mutating a returned slice must not change the Metadata or a
// subsequently returned slice.
func TestMetadataGetDoesNotShareBuffers(t *testing.T) {
	exif := []byte("EXIF-payload")
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: exif},
	}, nil)
	_, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := md.Chunk(KindEXIF); c == nil {
		t.Fatal("Chunk(KindEXIF) is nil")
	} else if c.Type != ChunkEXIF || int(c.Length) != len(exif) {
		t.Errorf("Chunk(KindEXIF): got type %q length %d", c.Type, c.Length)
	}
	a := md.EXIF()
	for i := range a {
		a[i] = 'X'
	}
	if !bytes.Equal(md.EXIF(), exif) {
		t.Errorf("mutating a returned slice changed the Metadata: got %q, want %q", md.EXIF(), exif)
	}
	if !bytes.Equal(md.Chunks[0].Data, exif) {
		t.Errorf("mutating a returned slice changed Chunks[0].Data: got %q, want %q", md.Chunks[0].Data, exif)
	}
}

// TestDecodeUnaffectedByMetadata checks that garbage metadata does not change
// what Decode returns for the same pixels.
func TestDecodeUnaffectedByMetadata(t *testing.T) {
	base, err := os.ReadFile("../testdata/blue-purple-pink.lossy.webp")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Decode(bytes.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}

	// Splice in EXIF and XMP chunks containing garbage, after the image data.
	imageChunk := parseRIFF(t, base)[0]
	for _, tc := range []struct {
		name string
		id   [4]byte
		data []byte
	}{
		{"exif", ChunkEXIF, []byte("\x00\x01not a valid TIFF\xff\xff")},
		{"xmp", ChunkXMP, []byte("<not-xml")},
		{"unknown", [4]byte{0, 1, 2, 3}, []byte("\xff\xfe")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withMeta := buildWebP([]chunkSpec{
				{id: imageChunk.id, data: imageChunk.data},
				{id: tc.id, data: tc.data},
			}, nil)
			got, err := Decode(bytes.NewReader(withMeta))
			if err != nil {
				t.Fatalf("Decode with %s metadata: %v", tc.name, err)
			}
			wantYCbCr, ok := want.(*image.YCbCr)
			if !ok {
				t.Fatalf("base image is %T, want *image.YCbCr", want)
			}
			gotYCbCr, ok := got.(*image.YCbCr)
			if !ok {
				t.Fatalf("image with %s metadata is %T, want *image.YCbCr", tc.name, got)
			}
			if !bytes.Equal(gotYCbCr.Y, wantYCbCr.Y) ||
				!bytes.Equal(gotYCbCr.Cb, wantYCbCr.Cb) ||
				!bytes.Equal(gotYCbCr.Cr, wantYCbCr.Cr) {
				t.Errorf("decoded pixels differ with %s metadata", tc.name)
			}

			// The metadata reader also reports the chunk, and its config
			// agrees with DecodeConfig.
			cfg, md, err := DecodeConfigWithMetadata(bytes.NewReader(withMeta), nil)
			if err != nil {
				t.Fatalf("DecodeConfigWithMetadata: %v", err)
			}
			wantCfg, err := DecodeConfig(bytes.NewReader(withMeta))
			if err != nil {
				t.Fatal(err)
			}
			if cfg != wantCfg {
				t.Errorf("config: got %+v, want %+v", cfg, wantCfg)
			}
			if len(md.Chunks) != 1 || md.Chunks[0].Type != ChunkType(tc.id) {
				t.Errorf("metadata: got %+v, want one %q chunk", md.Chunks, tc.id[:])
			}
		})
	}
}

// TestMetadataNilOptions checks that nil options behave like the zero value.
func TestMetadataNilOptions(t *testing.T) {
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: []byte("data")},
	}, nil)
	_, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := md.EXIF(); !bytes.Equal(got, []byte("data")) {
		t.Errorf("EXIF with nil options: got %q, want %q", got, "data")
	}
}

// TestMetadataAfterImageData checks that metadata is found even when it
// follows the VP8/VP8L chunk, which is where encoders commonly put EXIF/XMP.
func TestMetadataAfterImageData(t *testing.T) {
	imageChunk := parseRIFF(t, mustReadFile(t, "../testdata/blue-purple-pink.lossless.webp"))[0]
	exif := []byte("late EXIF")
	file := buildWebP([]chunkSpec{
		{id: imageChunk.id, data: imageChunk.data},
		{id: ChunkEXIF, data: exif},
	}, nil)
	cfg, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantCfg, err := DecodeConfig(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != wantCfg {
		t.Errorf("config: got %+v, want %+v", cfg, wantCfg)
	}
	if got := md.EXIF(); !bytes.Equal(got, exif) {
		t.Errorf("EXIF: got %q, want %q", got, exif)
	}
	// Offsets beyond the image chunk must still be right.
	wantOffset := int64(12+8) + int64(imageChunk.length) + int64(imageChunk.length&1) + 8
	if md.Chunks[0].Offset != wantOffset {
		t.Errorf("EXIF offset: got %d, want %d", md.Chunks[0].Offset, wantOffset)
	}
}

func mustReadFile(t *testing.T, filename string) []byte {
	t.Helper()
	b, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMetadataChunkOffsetForEmptyChunk checks chunks with no payload: their
// offset is the position just past the header, and a following chunk is still
// located correctly.
func TestMetadataChunkOffsetForEmptyChunk(t *testing.T) {
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF},
		{id: ChunkXMP, data: []byte("x")},
	}, nil)
	_, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(md.Chunks))
	}
	if md.Chunks[0].Length != 0 || md.Chunks[0].Data == nil || len(md.Chunks[0].Data) != 0 {
		t.Errorf("empty chunk: got length %d, data %v", md.Chunks[0].Length, md.Chunks[0].Data)
	}
	if md.Chunks[1].Offset != md.Chunks[0].Offset+8 {
		t.Errorf("offset after empty chunk: got %d, want %d",
			md.Chunks[1].Offset, md.Chunks[0].Offset+8)
	}
}

// TestMetadataFormatError checks that a non-WEBP RIFF and a non-RIFF stream
// are rejected.
func TestMetadataFormatError(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"not riff", []byte("not a riff file at all")},
		{"not webp", []byte("RIFF\x04\x00\x00\x00WAVE")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := DecodeConfigWithMetadata(bytes.NewReader(tc.data), nil); err == nil {
				t.Fatal("got nil error, want non-nil")
			}
		})
	}
}

// TestMetadataErrorFormat spot-checks the shape of the error context.
func TestMetadataErrorFormat(t *testing.T) {
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: bytes.Repeat([]byte{'x'}, 16)},
	}, nil)
	_, _, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{MaxSize: 8})
	if err == nil {
		t.Fatal("got nil error")
	}
	msg := err.Error()
	for _, want := range []string{"webp:", "EXIF", "offset", "38", "exceeds limit 8"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if !errors.Is(err, ErrMetadataTooLarge) {
		t.Errorf("errors.Is(err, ErrMetadataTooLarge) is false for %v", err)
	}
}

func ExampleDecodeConfigWithMetadata() {
	// A minimal WebP file: a 1x1 VP8X header, an EXIF chunk and no pixels.
	file := []byte{
		'R', 'I', 'F', 'F', 34, 0, 0, 0, 'W', 'E', 'B', 'P',
		'V', 'P', '8', 'X', 10, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		'E', 'X', 'I', 'F', 4, 0, 0, 0, 'd', 'a', 't', 'a',
	}
	cfg, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), &MetadataOptions{OmitData: true})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(cfg.Width, cfg.Height)
	fmt.Printf("%q %d %d %v\n", md.Chunks[0].Type, md.Chunks[0].Length, md.Chunks[0].Offset, md.Chunks[0].Data == nil)
	// Output:
	// 1 1
	// "EXIF" 4 38 true
}

// TestMetadataConfigMatchesDecodeConfigForLayouts builds the same image data
// in several container layouts (with metadata before or after the image, with
// odd-length chunks and with a VP8X header) and checks that the config and any
// error agree with DecodeConfig in every layout.
func TestMetadataConfigMatchesDecodeConfigForLayouts(t *testing.T) {
	vp8 := parseRIFF(t, mustReadFile(t, "../testdata/blue-purple-pink.lossy.webp"))[0]
	vp8l := parseRIFF(t, mustReadFile(t, "../testdata/blue-purple-pink.lossless.webp"))[0]
	alpha := parseRIFF(t, mustReadFile(t, "../testdata/yellow_rose.lossy-with-alpha.webp"))
	var alph chunkSpec
	for _, c := range alpha {
		if [4]byte{'A', 'L', 'P', 'H'} == c.id {
			alph = chunkSpec{id: c.id, data: c.data}
		}
	}
	// A VP8X header whose canvas matches blue-purple-pink (VP8 and VP8L agree
	// on its dimensions) but which advertises no alpha.
	cfgWant, err := DecodeConfig(bytes.NewReader(mustReadFile(t, "../testdata/blue-purple-pink.lossy.webp")))
	if err != nil {
		t.Fatal(err)
	}
	mkVP8X := func(flags byte, w, h int) chunkSpec {
		return chunkSpec{
			id: [4]byte{'V', 'P', '8', 'X'},
			data: []byte{flags, 0, 0, 0,
				byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16),
				byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)},
		}
	}
	exif := []byte("odd") // 3 bytes, so padding is exercised.
	xmp := []byte("XMP!")

	for _, tc := range []struct {
		name   string
		chunks []chunkSpec
	}{
		{"vp8 then metadata", []chunkSpec{
			{id: vp8.id, data: vp8.data}, {id: ChunkEXIF, data: exif}, {id: ChunkXMP, data: xmp},
		}},
		{"metadata then vp8", []chunkSpec{
			{id: ChunkEXIF, data: exif}, {id: ChunkXMP, data: xmp}, {id: vp8.id, data: vp8.data},
		}},
		{"vp8l then metadata", []chunkSpec{
			{id: vp8l.id, data: vp8l.data}, {id: ChunkEXIF, data: exif},
		}},
		{"vp8x vp8 metadata", []chunkSpec{
			mkVP8X(0, cfgWant.Width, cfgWant.Height), {id: vp8.id, data: vp8.data},
			{id: ChunkXMP, data: xmp},
		}},
		{"vp8x alph vp8 metadata", []chunkSpec{
			mkVP8X(1<<4, cfgWant.Width, cfgWant.Height), alph, {id: vp8.id, data: vp8.data},
			{id: ChunkEXIF, data: exif},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := buildWebP(tc.chunks, nil)
			wantCfg, wantErr := DecodeConfig(bytes.NewReader(file))
			gotCfg, md, gotErr := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("DecodeConfig error %v, DecodeConfigWithMetadata error %v", wantErr, gotErr)
			}
			if wantErr != nil {
				return
			}
			if gotCfg != wantCfg {
				t.Errorf("config: got %+v, want %+v", gotCfg, wantCfg)
			}
			// Offsets must still match the independent parser.
			var want []rawChunk
			for _, c := range parseRIFF(t, file) {
				if !isImageChunk(c.id) {
					want = append(want, c)
				}
			}
			if len(md.Chunks) != len(want) {
				t.Fatalf("got %d metadata chunks, want %d", len(md.Chunks), len(want))
			}
			for i := range want {
				if md.Chunks[i].Offset != want[i].offset || md.Chunks[i].Type != ChunkType(want[i].id) {
					t.Errorf("chunk %d: got {%q %d}, want {%q %d}", i,
						md.Chunks[i].Type, md.Chunks[i].Offset, want[i].id[:], want[i].offset)
				}
			}
		})
	}
}

// TestMetadataPayloadNotAliased checks that retained payloads are independent
// of each other and of the source bytes: mutating the source after the call
// does not change any Data.
func TestMetadataPayloadNotAliased(t *testing.T) {
	exif := []byte("EXIF-data")
	xmp := []byte("XMP-data")
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: exif},
		{id: ChunkXMP, data: xmp},
	}, nil)
	_, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range file {
		file[i] = 0
	}
	if got := md.EXIF(); !bytes.Equal(got, exif) {
		t.Errorf("EXIF aliases the source: got %q, want %q", got, exif)
	}
	if got := md.XMP(); !bytes.Equal(got, xmp) {
		t.Errorf("XMP aliases the source: got %q, want %q", got, xmp)
	}
	if md.Chunks[0].Data == nil || md.Chunks[1].Data == nil ||
		&md.Chunks[0].Data[0] == &md.Chunks[1].Data[0] {
		t.Error("chunk payloads share a buffer")
	}
}

// TestMetadataEmptyFile checks the degenerate inputs.
func TestMetadataEmptyFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short", []byte("RIFF")},
		{"header only", []byte("RIFF\x04\x00\x00\x00WEBP")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, md, err := DecodeConfigWithMetadata(bytes.NewReader(tc.data), nil)
			if err == nil {
				t.Fatal("got nil error, want non-nil")
			}
			if md != nil {
				t.Errorf("got non-nil Metadata (%+v) with an error", md)
			}
		})
	}
}

// TestMetadataTrailingImageChunks checks that image chunks after the first
// image data chunk do not add validation errors that DecodeConfig would not
// raise, since DecodeConfig stops at the first such chunk.
func TestMetadataTrailingImageChunks(t *testing.T) {
	vp8 := parseRIFF(t, mustReadFile(t, "../testdata/blue-purple-pink.lossy.webp"))[0]
	exif := []byte("trailing EXIF")
	for _, tc := range []struct {
		name   string
		chunks []chunkSpec
	}{
		{"second vp8", []chunkSpec{
			{id: vp8.id, data: vp8.data},
			{id: vp8.id, data: []byte("not a vp8 frame")},
			{id: ChunkEXIF, data: exif},
		}},
		{"vp8x after image", []chunkSpec{
			{id: vp8.id, data: vp8.data},
			vp8xChunk(0),
			{id: ChunkEXIF, data: exif},
		}},
		{"alph after image", []chunkSpec{
			{id: vp8.id, data: vp8.data},
			{id: [4]byte{'A', 'L', 'P', 'H'}, data: []byte("bogus")},
			{id: ChunkEXIF, data: exif},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := buildWebP(tc.chunks, nil)
			wantCfg, wantErr := DecodeConfig(bytes.NewReader(file))
			if wantErr != nil {
				t.Fatalf("DecodeConfig: %v", wantErr)
			}
			gotCfg, md, gotErr := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
			if gotErr != nil {
				t.Fatalf("DecodeConfigWithMetadata: %v", gotErr)
			}
			if gotCfg != wantCfg {
				t.Errorf("config: got %+v, want %+v", gotCfg, wantCfg)
			}
			if got := md.EXIF(); !bytes.Equal(got, exif) {
				t.Errorf("EXIF after trailing image chunk: got %q, want %q", got, exif)
			}
		})
	}
}

// TestMetadataVP8XOnly checks a file with a VP8X header and no image data
// chunk, which DecodeConfig accepts. Metadata after the VP8X chunk must still
// be reported.
func TestMetadataVP8XOnly(t *testing.T) {
	exif := []byte("no pixels")
	file := buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: exif},
	}, nil)
	wantCfg, wantErr := DecodeConfig(bytes.NewReader(file))
	if wantErr != nil {
		t.Fatalf("DecodeConfig: %v", wantErr)
	}
	gotCfg, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotCfg != wantCfg {
		t.Errorf("config: got %+v, want %+v", gotCfg, wantCfg)
	}
	if got := md.EXIF(); !bytes.Equal(got, exif) {
		t.Errorf("EXIF: got %q, want %q", got, exif)
	}
}

// TestMetadataWithoutVP8X checks a bare VP8 file, the common case for a plain
// lossy image with an appended EXIF chunk.
func TestMetadataWithoutVP8X(t *testing.T) {
	b, err := os.ReadFile("../testdata/blue-purple-pink.lossy.webp")
	if err != nil {
		t.Fatal(err)
	}
	// Find the file's existing single chunk and rebuild it with EXIF appended.
	c := parseRIFF(t, b)[0]
	exif := []byte("EXIF after the image")
	file := buildWebP([]chunkSpec{
		{id: c.id, data: c.data},
		{id: ChunkEXIF, data: exif},
	}, nil)
	gotCfg, md, err := DecodeConfigWithMetadata(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantCfg, err := DecodeConfig(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if gotCfg != wantCfg {
		t.Errorf("config: got %+v, want %+v", gotCfg, wantCfg)
	}
	if got := md.EXIF(); !bytes.Equal(got, exif) {
		t.Errorf("EXIF: got %q, want %q", got, exif)
	}
}

// FuzzDecodeConfigWithMetadata checks that arbitrary input never panics and
// that, whenever the call succeeds, the reported offsets, lengths and data are
// internally consistent.
func FuzzDecodeConfigWithMetadata(f *testing.F) {
	seeds, _ := filepath.Glob("../testdata/*.webp")
	for _, name := range seeds {
		if b, err := os.ReadFile(name); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("RIFF\x0a\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00"))
	f.Add(buildWebP([]chunkSpec{
		vp8xChunk(0),
		{id: ChunkEXIF, data: []byte("abc")},
	}, nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, md, err := DecodeConfigWithMetadata(bytes.NewReader(data), nil)
		if err != nil {
			return
		}
		if md == nil {
			t.Fatal("nil Metadata with nil error")
		}
		if cfg.Width < 0 || cfg.Height < 0 {
			t.Fatalf("negative config dimensions: %+v", cfg)
		}
		var prev int64 = -1
		for i, c := range md.Chunks {
			if c.Offset <= prev {
				t.Fatalf("chunk %d (%q): offset %d not after %d", i, c.Type, c.Offset, prev)
			}
			prev = c.Offset
			if int64(c.Length) != int64(len(c.Data)) {
				t.Fatalf("chunk %d (%q): length %d, data %d", i, c.Type, c.Length, len(c.Data))
			}
			if c.Offset+int64(c.Length) > int64(len(data)) {
				t.Fatalf("chunk %d (%q): range [%d, %d) past end of %d-byte input",
					i, c.Type, c.Offset, c.Offset+int64(c.Length), len(data))
			}
			if !bytes.Equal(c.Data, data[c.Offset:c.Offset+int64(c.Length)]) {
				t.Fatalf("chunk %d (%q): data does not match input at offset", i, c.Type)
			}
		}
	})
}
