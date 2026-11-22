// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ccitt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

const (
	checkpointTestWidth  = 153
	checkpointTestHeight = 55
)

// readCheckpointRows decodes all rows from r, one Read call per row, taking a
// Checkpoint at every row boundary (including before the first row and after
// the last one). It returns the per-row output and the checkpoints.
func readCheckpointRows(t *testing.T, r *Reader, rowBytes int) ([][]byte, []Checkpoint) {
	t.Helper()

	rows := [][]byte(nil)
	cps := []Checkpoint(nil)

	cp, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("initial Checkpoint: %v", err)
	}
	cps = append(cps, cp)

	for {
		row := make([]byte, rowBytes)
		n, err := r.Read(row)
		if err == io.EOF {
			if n != 0 {
				t.Fatalf("final Read: got (%d, EOF), want (0, EOF)", n)
			}
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if n != rowBytes {
			t.Fatalf("Read: got %d bytes, want %d", n, rowBytes)
		}
		rows = append(rows, row)

		cp, err := r.Checkpoint()
		if err != nil {
			t.Fatalf("Checkpoint after row %d: %v", len(rows), err)
		}
		cps = append(cps, cp)
	}
	return rows, cps
}

// testCheckpointSplit decodes the first k rows with a fresh Reader, takes a
// checkpoint, resumes from it on a fresh copy of the stream, and checks that
// the resumed Reader reproduces rows[k:] (from an uninterrupted decode) and
// the same per-row consumed-byte offsets. It also checks that the original
// Reader is undisturbed by the checkpoint and the resumed Reader.
func testCheckpointSplit(t *testing.T, data []byte, order Order, sf SubFormat, width, height int, opts *Options, rows [][]byte, cps []Checkpoint, k int) {
	t.Helper()
	rowBytes := (width + 7) / 8

	r1 := NewCheckpointReader(bytes.NewReader(data), order, sf, width, height, opts)
	for y := 0; y < k; y++ {
		row := make([]byte, rowBytes)
		if n, err := r1.Read(row); n != rowBytes || err != nil {
			t.Fatalf("r1.Read row %d: got (%d, %v), want (%d, nil)", y, n, err, rowBytes)
		}
		if !bytes.Equal(row, rows[y]) {
			t.Fatalf("r1 row %d: got % 02X, want % 02X", y, row, rows[y])
		}
	}

	cp, err := r1.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if cp.Row() != k {
		t.Fatalf("Checkpoint.Row: got %d, want %d", cp.Row(), k)
	}
	if cp.Offset() != cps[k].Offset() {
		t.Fatalf("Checkpoint.Offset: got %d, want %d", cp.Offset(), cps[k].Offset())
	}

	// Resume from the checkpoint on a fresh copy of the stream. The resumed
	// reader must reproduce the remaining rows and consume the same number
	// of bytes as the uninterrupted reader.
	r2, err := cp.Resume(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	for y := k; y < len(rows); y++ {
		row := make([]byte, rowBytes)
		if n, err := r2.Read(row); n != rowBytes || err != nil {
			t.Fatalf("r2.Read row %d: got (%d, %v), want (%d, nil)", y, n, err, rowBytes)
		}
		if !bytes.Equal(row, rows[y]) {
			t.Fatalf("r2 row %d: got % 02X, want % 02X", y, row, rows[y])
		}
		cp2, err := r2.Checkpoint()
		if err != nil {
			t.Fatalf("r2.Checkpoint after row %d: %v", y, err)
		}
		if cp2.Row() != y+1 {
			t.Fatalf("r2.Checkpoint after row %d: got Row %d, want %d", y, cp2.Row(), y+1)
		}
		if cp2.Offset() != cps[y+1].Offset() {
			t.Fatalf("r2.Checkpoint after row %d: got Offset %d, want %d",
				y, cp2.Offset(), cps[y+1].Offset())
		}
	}
	if n, err := r2.Read(make([]byte, rowBytes)); n != 0 || err != io.EOF {
		t.Fatalf("r2 final Read: got (%d, %v), want (0, EOF)", n, err)
	}

	// The original reader must be unaffected by taking the checkpoint and by
	// the resumed reader's decoding: no mutable buffers may be shared.
	for y := k; y < len(rows); y++ {
		row := make([]byte, rowBytes)
		if n, err := r1.Read(row); n != rowBytes || err != nil {
			t.Fatalf("r1.Read row %d after resume: got (%d, %v), want (%d, nil)", y, n, err, rowBytes)
		}
		if !bytes.Equal(row, rows[y]) {
			t.Fatalf("r1 row %d after resume: got % 02X, want % 02X", y, row, rows[y])
		}
	}
	if n, err := r1.Read(make([]byte, rowBytes)); n != 0 || err != io.EOF {
		t.Fatalf("r1 final Read: got (%d, %v), want (0, EOF)", n, err)
	}
}

func testCheckpointFile(t *testing.T, fileName string, sf SubFormat, align, invert, truncated bool) {
	t.Helper()

	data, err := os.ReadFile(fileName)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	opts := &Options{
		Align:  align,
		Invert: invert,
	}
	const rowBytes = (checkpointTestWidth + 7) / 8

	// The reference output comes from the original NewReader API, exactly as
	// in TestRead.
	wantFlat, err := io.ReadAll(NewReader(bytes.NewReader(data), MSB, sf,
		checkpointTestWidth, checkpointTestHeight, opts))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(wantFlat) != checkpointTestHeight*rowBytes {
		t.Fatalf("len(wantFlat): got %d, want %d", len(wantFlat), checkpointTestHeight*rowBytes)
	}

	// Decode the whole stream row by row, recording checkpoints.
	rows, cps := readCheckpointRows(t, NewCheckpointReader(bytes.NewReader(data),
		MSB, sf, checkpointTestWidth, checkpointTestHeight, opts), rowBytes)
	if len(rows) != checkpointTestHeight {
		t.Fatalf("got %d rows, want %d", len(rows), checkpointTestHeight)
	}
	if flat := bytes.Join(rows, nil); !bytes.Equal(flat, wantFlat) {
		t.Fatalf("continuous row-by-row output differs from NewReader output")
	}
	for i, cp := range cps {
		if cp.Row() != i {
			t.Fatalf("cps[%d].Row: got %d, want %d", i, cp.Row(), i)
		}
		if cp.Offset() < 0 || cp.Offset() > int64(len(data)) {
			t.Fatalf("cps[%d].Offset: got %d, want within [0, %d]", i, cp.Offset(), len(data))
		}
		if i > 0 && cp.Offset() < cps[i-1].Offset() {
			t.Fatalf("cps[%d].Offset: got %d, want >= %d", i, cp.Offset(), cps[i-1].Offset())
		}
	}

	// Decode in two segments: rows [:k] from one reader, rows [k:] from a
	// reader resumed from the checkpoint at row k.
	splits := []int{0, 1, 2, 7, checkpointTestHeight / 2,
		checkpointTestHeight - 2, checkpointTestHeight - 1, checkpointTestHeight}
	for _, k := range splits {
		t.Run(fmt.Sprintf("split=%d", k), func(t *testing.T) {
			testCheckpointSplit(t, data, MSB, sf, checkpointTestWidth, checkpointTestHeight,
				opts, rows, cps, k)
		})
	}

	// Passing AutoDetectHeight should behave the same, including across a
	// checkpoint/resume, provided that the input hasn't truncated the
	// trailing sequence of consecutive EOL's.
	if !truncated {
		adhRows, adhCps := readCheckpointRows(t, NewCheckpointReader(bytes.NewReader(data),
			MSB, sf, checkpointTestWidth, AutoDetectHeight, opts), rowBytes)
		if len(adhRows) != len(rows) {
			t.Fatalf("AutoDetectHeight: got %d rows, want %d", len(adhRows), len(rows))
		}
		for y := range rows {
			if !bytes.Equal(adhRows[y], rows[y]) {
				t.Fatalf("AutoDetectHeight row %d differs", y)
			}
		}
		for _, k := range []int{0, 7, checkpointTestHeight / 2, checkpointTestHeight} {
			t.Run(fmt.Sprintf("autodetect/split=%d", k), func(t *testing.T) {
				testCheckpointSplit(t, data, MSB, sf, checkpointTestWidth, AutoDetectHeight,
					opts, adhRows, adhCps, k)
			})
		}
	}
}

func TestCheckpoint(t *testing.T) {
	for _, fileName := range []string{
		"testdata/bw-gopher.ccitt_group3",
		"testdata/bw-gopher-aligned.ccitt_group3",
		"testdata/bw-gopher-inverted.ccitt_group3",
		"testdata/bw-gopher-inverted-aligned.ccitt_group3",
		"testdata/bw-gopher.ccitt_group4",
		"testdata/bw-gopher-aligned.ccitt_group4",
		"testdata/bw-gopher-inverted.ccitt_group4",
		"testdata/bw-gopher-inverted-aligned.ccitt_group4",
		"testdata/bw-gopher-truncated0.ccitt_group3",
		"testdata/bw-gopher-truncated0.ccitt_group4",
		"testdata/bw-gopher-truncated1.ccitt_group3",
		"testdata/bw-gopher-truncated1.ccitt_group4",
	} {
		subFormat := Group3
		if strings.HasSuffix(fileName, "group4") {
			subFormat = Group4
		}
		align := strings.Contains(fileName, "aligned")
		invert := strings.Contains(fileName, "inverted")
		truncated := strings.Contains(fileName, "truncated")
		t.Run(fileName, func(t *testing.T) {
			testCheckpointFile(t, fileName, subFormat, align, invert, truncated)
		})
	}
}

// packBitsLSB packs a string of '0' and '1' characters into bytes, first bit
// in the least significant bit of the first byte.
func packBitsLSB(s string) []byte {
	out := []byte(nil)
	cur := byte(0)
	n := uint(0)
	for _, c := range s {
		if c == '1' {
			cur |= 1 << n
		}
		n++
		if n == 8 {
			out = append(out, cur)
			cur = 0
			n = 0
		}
	}
	if n > 0 {
		out = append(out, cur)
	}
	return out
}

func testCheckpointLSB(t *testing.T, sf SubFormat, width, height int, data []byte, wantRow byte, opts *Options) {
	t.Helper()
	rowBytes := (width + 7) / 8

	rows, cps := readCheckpointRows(t,
		NewCheckpointReader(bytes.NewReader(data), LSB, sf, width, height, opts), rowBytes)
	if len(rows) != height {
		t.Fatalf("got %d rows, want %d", len(rows), height)
	}
	for y, row := range rows {
		want := bytes.Repeat([]byte{wantRow}, rowBytes)
		if !bytes.Equal(row, want) {
			t.Fatalf("row %d: got % 02X, want % 02X", y, row, want)
		}
	}
	for k := 0; k <= height; k++ {
		testCheckpointSplit(t, data, LSB, sf, width, height, opts, rows, cps, k)
	}
}

func TestCheckpointLSBGroup4(t *testing.T) {
	// An all-white 8x3 image, Group 4, LSB-first: one modeV0 code ("1") per
	// row, then the two-EOL return-to-control.
	const width, height = 8, 3
	data := packBitsLSB(strings.Repeat("1", height) + strings.Repeat("000000000001", 2))

	t.Run("plain", func(t *testing.T) {
		testCheckpointLSB(t, Group4, width, height, data, 0xFF, nil)
	})
	t.Run("invert", func(t *testing.T) {
		testCheckpointLSB(t, Group4, width, height, data, 0x00, &Options{Invert: true})
	})
}

func TestCheckpointLSBGroup3(t *testing.T) {
	// An all-white 8x3 image, Group 3 (1D), LSB-first: an EOL, then a white
	// run-length code and an EOL per row, then the six-EOL return-to-control
	// (the last row's EOL counts as the first of the six).
	const width, height = 8, 3
	const eol = "000000000001"
	white8 := ""
	for _, c := range whiteCodes {
		if c.val == width {
			white8 = c.str
			break
		}
	}
	if white8 == "" {
		t.Fatalf("no white run-length code for %d", width)
	}
	data := packBitsLSB(eol + strings.Repeat(white8+eol, height) + strings.Repeat(eol, 5))

	t.Run("plain", func(t *testing.T) {
		testCheckpointLSB(t, Group3, width, height, data, 0xFF, nil)
	})
	t.Run("invert", func(t *testing.T) {
		testCheckpointLSB(t, Group3, width, height, data, 0x00, &Options{Invert: true})
	})
}

// errSeeker is an io.ReadSeeker whose Seek always fails.
type errSeeker struct {
	io.Reader
}

func (errSeeker) Seek(int64, int) (int64, error) {
	return 0, errors.New("test: seek failed")
}

// checkpointAtRow returns a checkpoint taken after decoding n rows of the
// given CCITT stream.
func checkpointAtRow(t *testing.T, data []byte, sf SubFormat, n int) Checkpoint {
	t.Helper()
	const rowBytes = (checkpointTestWidth + 7) / 8
	r := NewCheckpointReader(bytes.NewReader(data), MSB, sf,
		checkpointTestWidth, checkpointTestHeight, nil)
	for y := 0; y < n; y++ {
		if _, err := r.Read(make([]byte, rowBytes)); err != nil {
			t.Fatalf("Read row %d: %v", y, err)
		}
	}
	cp, err := r.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return cp
}

func TestCheckpointErrors(t *testing.T) {
	data, err := os.ReadFile("testdata/bw-gopher.ccitt_group4")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	cp := checkpointAtRow(t, data, Group4, 2)

	// A version mismatch is rejected.
	bad := cp
	bad.version = checkpointVersion + 1
	if _, err := bad.Resume(bytes.NewReader(data)); !errors.Is(err, errCheckpointVersion) {
		t.Fatalf("version mismatch: got %v, want %v", err, errCheckpointVersion)
	}

	// A zero-value Checkpoint has an unsupported version.
	if _, err := (&Checkpoint{}).Resume(bytes.NewReader(data)); !errors.Is(err, errCheckpointVersion) {
		t.Fatalf("zero Checkpoint: got %v, want %v", err, errCheckpointVersion)
	}

	// Invalid row numbers are rejected.
	bad = cp
	bad.row = -1
	if _, err := bad.Resume(bytes.NewReader(data)); !errors.Is(err, errInvalidRowNumber) {
		t.Fatalf("row -1: got %v, want %v", err, errInvalidRowNumber)
	}
	bad = cp
	bad.row = checkpointTestHeight + 1
	if _, err := bad.Resume(bytes.NewReader(data)); !errors.Is(err, errInvalidRowNumber) {
		t.Fatalf("row > height: got %v, want %v", err, errInvalidRowNumber)
	}

	// Oversized state is rejected.
	bad = cp
	bad.width = maxWidth + 1
	bad.prev = make([]byte, maxWidth+1)
	if _, err := bad.Resume(bytes.NewReader(data)); !errors.Is(err, errCheckpointTooLarge) {
		t.Fatalf("oversized state: got %v, want %v", err, errCheckpointTooLarge)
	}

	// Inconsistent state is rejected.
	bad = cp
	bad.prev = make([]byte, cp.width+1)
	if _, err := bad.Resume(bytes.NewReader(data)); !errors.Is(err, errInvalidCheckpoint) {
		t.Fatalf("reference row / width mismatch: got %v, want %v", err, errInvalidCheckpoint)
	}
	bad = cp
	bad.order = Order(42)
	if _, err := bad.Resume(bytes.NewReader(data)); !errors.Is(err, errInvalidCheckpoint) {
		t.Fatalf("invalid order: got %v, want %v", err, errInvalidCheckpoint)
	}

	// A nil Checkpoint is rejected.
	var nilCP *Checkpoint
	if _, err := nilCP.Resume(bytes.NewReader(data)); !errors.Is(err, errInvalidCheckpoint) {
		t.Fatalf("nil Checkpoint: got %v, want %v", err, errInvalidCheckpoint)
	}

	// A seek failure is reported, and does not consume the checkpoint.
	if _, err := cp.Resume(errSeeker{bytes.NewReader(data)}); err == nil {
		t.Fatalf("errSeeker: got nil error")
	}

	// Resuming the same checkpoint twice is rejected.
	if _, err := cp.Resume(bytes.NewReader(data)); err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	if _, err := cp.Resume(bytes.NewReader(data)); !errors.Is(err, errCheckpointReused) {
		t.Fatalf("second Resume: got %v, want %v", err, errCheckpointReused)
	}

	// Taking a checkpoint in the middle of a row is rejected.
	const rowBytes = (checkpointTestWidth + 7) / 8
	r := NewCheckpointReader(bytes.NewReader(data), MSB, Group4,
		checkpointTestWidth, checkpointTestHeight, nil)
	if n, err := r.Read(make([]byte, rowBytes-1)); n != rowBytes-1 || err != nil {
		t.Fatalf("partial Read: got (%d, %v), want (%d, nil)", n, err, rowBytes-1)
	}
	if _, err := r.Checkpoint(); !errors.Is(err, errNotAtRowBoundary) {
		t.Fatalf("mid-row Checkpoint: got %v, want %v", err, errNotAtRowBoundary)
	}
}

func TestCheckpointTruncatedStream(t *testing.T) {
	for _, tt := range []struct {
		fileName string
		sf       SubFormat
	}{
		{"testdata/bw-gopher.ccitt_group3", Group3},
		{"testdata/bw-gopher.ccitt_group4", Group4},
	} {
		t.Run(tt.fileName, func(t *testing.T) {
			data, err := os.ReadFile(tt.fileName)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			// Checkpoint at row 20 of 55, then resume with the stream cut
			// off at the checkpoint's offset. Decoding the remaining 35
			// rows must run out of bits in the middle of a code.
			cp := checkpointAtRow(t, data, tt.sf, 20)
			r, err := cp.Resume(bytes.NewReader(data[:cp.Offset()]))
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if _, err := io.ReadAll(r); !errors.Is(err, errIncompleteCode) {
				t.Fatalf("truncated stream: got %v, want %v", err, errIncompleteCode)
			}
		})
	}
}
