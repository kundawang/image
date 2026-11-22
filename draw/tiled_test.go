// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package draw

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"
)

// checkerboard returns an NRGBA image with half-transparent and fully opaque
// squares, so that both Over and Src exercise alpha handling.
func checkerboard(r image.Rectangle, alpha uint8) *image.NRGBA {
	m := image.NewNRGBA(r)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := color.NRGBA{R: 0xff, G: 0x00, B: 0x00, A: alpha}
			if (x/3+y/3)%2 == 0 {
				c = color.NRGBA{R: 0x00, G: 0x80, B: 0xff, A: alpha}
			}
			m.SetNRGBA(x, y, c)
		}
	}
	return m
}

// transparent returns an image where most pixels are fully transparent and a
// few are not, which is the case that a mask-free Over draw must skip.
func transparent(r image.Rectangle) *image.NRGBA {
	m := image.NewNRGBA(r)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			a := uint8(0)
			if x%7 == 0 && y%5 == 0 {
				a = uint8((x*13 + y*29) & 0xff)
			}
			m.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0x99, A: a})
		}
	}
	return m
}

func alphaMask(r image.Rectangle) *image.Alpha {
	m := image.NewAlpha(r)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			m.SetAlpha(x, y, color.Alpha{A: uint8((x*31 + y*17) & 0xff)})
		}
	}
	return m
}

// newDst returns a fresh destination image of the given kind, filled with a
// deterministic pattern so that Over draws have something visible to blend
// with.
func newDst(kind string, r image.Rectangle) Image {
	var dst Image
	switch kind {
	case "RGBA":
		dst = image.NewRGBA(r)
	case "NRGBA":
		dst = image.NewNRGBA(r)
	case "RGBA64":
		dst = image.NewRGBA64(r)
	case "NRGBA64":
		dst = image.NewNRGBA64(r)
	case "Gray":
		dst = image.NewGray(r)
	case "Gray16":
		dst = image.NewGray16(r)
	case "Alpha":
		dst = image.NewAlpha(r)
	case "Alpha16":
		dst = image.NewAlpha16(r)
	case "CMYK":
		dst = image.NewCMYK(r)
	case "Paletted":
		pal := color.Palette{
			color.RGBA{0x00, 0x00, 0x00, 0xff},
			color.RGBA{0xff, 0xff, 0xff, 0xff},
			color.RGBA{0x20, 0x80, 0xc0, 0xff},
			color.RGBA{0xff, 0x00, 0x00, 0x80},
		}
		dst = image.NewPaletted(r, pal)
	default:
		panic("unknown dst kind: " + kind)
	}
	fillImage(dst, r)
	return dst
}

func fillImage(dst Image, r image.Rectangle) {
	nrgba := image.NewNRGBA(r)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			nrgba.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 7),
				G: uint8(y * 11),
				B: uint8(x ^ y),
				A: uint8(0x40 + (x*3+y*5)&0xbf),
			})
		}
	}
	Draw(dst, r, nrgba, r.Min, Src)
}

func dstPix(dst Image) []byte {
	switch d := dst.(type) {
	case *image.RGBA:
		return d.Pix
	case *image.NRGBA:
		return d.Pix
	case *image.RGBA64:
		return d.Pix
	case *image.NRGBA64:
		return d.Pix
	case *image.Gray:
		return d.Pix
	case *image.Gray16:
		return d.Pix
	case *image.Alpha:
		return d.Pix
	case *image.Alpha16:
		return d.Pix
	case *image.CMYK:
		return d.Pix
	case *image.Paletted:
		return d.Pix
	}
	panic("unknown dst")
}

func srcSets(bounds image.Rectangle) map[string]image.Image {
	return map[string]image.Image{
		"checkerboard": checkerboard(bounds, 0x80),
		"transparent":  transparent(bounds),
		"opaque":       checkerboard(bounds, 0xff),
		"uniform":      image.NewUniform(color.RGBA{0x11, 0x22, 0x44, 0x7f}),
		"rgba":         func() image.Image { m, _ := srcRGBA64(bounds); return m }(),
		"gray":         func() image.Image { m, _ := srcGray(bounds); return m }(),
		"ycbcr":        func() image.Image { m, _ := srcYCbCr(bounds); return m }(),
	}
}

// TestDrawTiledMatchesDraw checks that every combination of destination,
// source, mask, op, tile size and worker count produces byte-identical output
// to the un-tiled API.
func TestDrawTiledMatchesDraw(t *testing.T) {
	srcBounds := image.Rect(-4, -3, 30, 27)
	srcs := srcSets(srcBounds)
	srcNames := []string{"checkerboard", "transparent", "opaque", "uniform", "rgba", "gray", "ycbcr"}
	mask := alphaMask(srcBounds)
	maskOffset := image.Pt(3, -2)

	dstKinds := []string{"RGBA", "NRGBA", "RGBA64", "NRGBA64", "Gray", "Gray16", "Alpha", "Alpha16", "CMYK", "Paletted"}
	ops := []struct {
		name string
		op   Op
	}{{"Over", Over}, {"Src", Src}}
	rs := []image.Rectangle{
		image.Rect(0, 0, 24, 20),     // The dst bounds.
		image.Rect(3, 4, 17, 12),     // A strict subset.
		image.Rect(-6, -5, 8, 9),     // Offset into a nonzero-bounds src.
		image.Rect(2, -3, 22, 30),    // Hangs off two edges.
		image.Rect(-20, -20, -5, -5), // Entirely out of bounds.
		image.Rect(5, 5, 5, 9),       // Empty.
		image.Rect(9, 5, 5, 9),       // Empty and uncanonical.
	}
	sp := image.Pt(3, 2)
	tileSizes := []int{1, 2, 3, 7, 16, 256, 1 << 20}
	workerCounts := []int{-1, 1, 2, 5, 1000}

	for _, dstKind := range dstKinds {
		for _, srcName := range srcNames {
			src := srcs[srcName]
			for _, useMask := range []bool{false, true} {
				for _, o := range ops {
					for _, r := range rs {
						ref := newDst(dstKind, image.Rect(0, 0, 24, 20))
						if useMask {
							DrawMask(ref, r, src, sp, mask, maskOffset, o.op)
						} else {
							Draw(ref, r, src, sp, o.op)
						}

						for _, tileSize := range tileSizes {
							for _, workers := range workerCounts {
								got := newDst(dstKind, image.Rect(0, 0, 24, 20))
								opts := &TileOptions{TileSize: tileSize, Workers: workers}
								var err error
								if useMask {
									err = DrawMaskTiled(got, r, src, sp, mask, maskOffset, o.op, opts)
								} else {
									err = DrawTiled(got, r, src, sp, o.op, opts)
								}
								if err != nil {
									t.Fatalf("dst=%s src=%s mask=%t op=%s r=%v tile=%d workers=%d: unexpected error: %v",
										dstKind, srcName, useMask, o.name, r, tileSize, workers, err)
								}
								if !bytes.Equal(dstPix(ref), dstPix(got)) {
									t.Fatalf("dst=%s src=%s mask=%t op=%s r=%v tile=%d workers=%d: pixels differ from Draw/DrawMask",
										dstKind, srcName, useMask, o.name, r, tileSize, workers)
								}

								// Sequential mode must agree too.
								got2 := newDst(dstKind, image.Rect(0, 0, 24, 20))
								opts2 := &TileOptions{TileSize: tileSize, Workers: workers, Sequential: true}
								if useMask {
									err = DrawMaskTiled(got2, r, src, sp, mask, maskOffset, o.op, opts2)
								} else {
									err = DrawTiled(got2, r, src, sp, o.op, opts2)
								}
								if err != nil {
									t.Fatalf("sequential: dst=%s src=%s mask=%t op=%s r=%v tile=%d workers=%d: unexpected error: %v",
										dstKind, srcName, useMask, o.name, r, tileSize, workers, err)
								}
								if !bytes.Equal(dstPix(ref), dstPix(got2)) {
									t.Fatalf("sequential: dst=%s src=%s mask=%t op=%s r=%v tile=%d workers=%d: pixels differ from Draw/DrawMask",
										dstKind, srcName, useMask, o.name, r, tileSize, workers)
								}
							}
						}
					}
				}
			}
		}
	}
}

// TestDrawTiledNilOptions checks the default options path.
func TestDrawTiledNilOptions(t *testing.T) {
	dst0 := image.NewRGBA(image.Rect(0, 0, 600, 500))
	dst1 := image.NewRGBA(image.Rect(0, 0, 600, 500))
	src := checkerboard(image.Rect(0, 0, 700, 700), 0xc0)
	Draw(dst0, dst0.Bounds(), src, image.Pt(17, 23), Over)
	if err := DrawTiled(dst1, dst1.Bounds(), src, image.Pt(17, 23), Over, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst0.Pix, dst1.Pix) {
		t.Fatal("nil options: pixels differ")
	}
}

// TestDrawTiledNonZeroBounds checks a destination whose Bounds do not start at
// the origin and a source with negative coordinates.
func TestDrawTiledNonZeroBounds(t *testing.T) {
	dstRect := image.Rect(-30, -20, 30, 20)
	srcRect := image.Rect(-50, -50, 50, 50)
	src := checkerboard(srcRect, 0x60)
	r := image.Rect(-27, -13, 25, 18)
	sp := image.Pt(-40, -33)
	dr := newDst("RGBA", dstRect)
	Draw(dr, r, src, sp, Over)
	got := newDst("RGBA", dstRect)
	if err := DrawTiled(got, r, src, sp, Over, &TileOptions{TileSize: 4, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dstPix(dr), dstPix(got)) {
		t.Fatal("nonzero bounds: pixels differ")
	}
}

// TestDrawTiledAliasing checks that drawing an image onto itself (with an
// offset and with overlap) matches the sequential API, in both directions.
func TestDrawTiledAliasing(t *testing.T) {
	for _, op := range []Op{Over, Src} {
		for _, sp := range []image.Point{
			{0, 0},
			{3, 4},   // Down-right: forward copy must not overwrite unread pixels.
			{-3, -4}, // Up-left: the standard library walks backward.
			{0, 5},
			{5, 0},
		} {
			const w, h = 40, 33
			ref := image.NewRGBA(image.Rect(0, 0, w, h))
			fillImage(ref, ref.Bounds())
			Draw(ref, ref.Bounds(), ref, sp, op)

			got := image.NewRGBA(image.Rect(0, 0, w, h))
			fillImage(got, got.Bounds())
			if err := DrawTiled(got, got.Bounds(), got, sp, op, &TileOptions{TileSize: 5, Workers: 8}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(ref.Pix, got.Pix) {
				t.Fatalf("alias sp=%v op=%v: pixels differ", sp, op)
			}
		}
	}

	// A SubImage of dst aliases dst's backing array: it must not be tiled.
	base := image.NewRGBA(image.Rect(0, 0, 32, 32))
	fillImage(base, base.Bounds())
	ref := image.NewRGBA(base.Rect)
	copy(ref.Pix, base.Pix)
	sub := ref.SubImage(image.Rect(4, 4, 20, 20)).(*image.RGBA)
	Draw(ref, image.Rect(2, 3, 25, 22), sub, image.Pt(6, 5), Over)

	gotBase := image.NewRGBA(base.Rect)
	copy(gotBase.Pix, base.Pix)
	gotSub := gotBase.SubImage(image.Rect(4, 4, 20, 20)).(*image.RGBA)
	if err := DrawTiled(gotBase, image.Rect(2, 3, 25, 22), gotSub, image.Pt(6, 5), Over, &TileOptions{TileSize: 4, Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ref.Pix, gotBase.Pix) {
		t.Fatal("dst SubImage alias: pixels differ")
	}

	// A mask that aliases dst must also fall back to a single call, so that it
	// sees dst's pixels as they were before the draw, not as they are mutated.
	// Build the reference and the tiled draw from independent copies of the
	// same starting image.
	start := image.NewRGBA(image.Rect(0, 0, 32, 32))
	fillImage(start, start.Bounds())
	maskRef := image.NewRGBA(start.Rect)
	copy(maskRef.Pix, start.Pix)
	maskGot := image.NewRGBA(start.Rect)
	copy(maskGot.Pix, start.Pix)
	maskSrc := checkerboard(image.Rect(0, 0, 40, 40), 0x80)
	maskRect := image.Rect(3, 3, 28, 28)
	DrawMask(maskRef, maskRect, maskSrc, image.Pt(2, 1), maskRef, image.Pt(5, 5), Over)
	if err := DrawMaskTiled(maskGot, maskRect, maskSrc, image.Pt(2, 1), maskGot, image.Pt(5, 5), Over, &TileOptions{TileSize: 4, Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(maskRef.Pix, maskGot.Pix) {
		t.Fatal("mask alias: pixels differ")
	}
}

// unknownSrc hides a concrete image so that this package cannot inspect its
// pixel storage.
type unknownSrc struct{ image.Image }

// unknownDst is an Image of a type that this package cannot inspect.
type unknownDst struct{ *image.RGBA }

// TestDrawTiledUnknownImage checks that an image type whose storage cannot be
// inspected still produces identical output.
func TestDrawTiledUnknownImage(t *testing.T) {
	src := unknownSrc{checkerboard(image.Rect(0, 0, 30, 30), 0x80)}
	dst0 := image.NewRGBA(image.Rect(0, 0, 20, 20))
	dst1 := image.NewRGBA(image.Rect(0, 0, 20, 20))
	fillImage(dst0, dst0.Bounds())
	fillImage(dst1, dst1.Bounds())
	r := image.Rect(2, 2, 18, 17)
	Draw(dst0, r, src, image.Pt(3, 4), Over)
	if err := DrawTiled(dst1, r, src, image.Pt(3, 4), Over, &TileOptions{TileSize: 3, Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst0.Pix, dst1.Pix) {
		t.Fatal("unknown src: pixels differ")
	}

	// An unknown destination is also handled by a single call, which preserves
	// the standard library's backwards iteration for an aliased draw.
	backing := image.NewRGBA(image.Rect(0, 0, 20, 20))
	fillImage(backing, backing.Bounds())
	refBacking := image.NewRGBA(backing.Rect)
	copy(refBacking.Pix, backing.Pix)
	Draw(unknownDst{refBacking}, image.Rect(1, 1, 19, 19), refBacking, image.Pt(2, 3), Src)

	gotBacking := image.NewRGBA(backing.Rect)
	copy(gotBacking.Pix, backing.Pix)
	if err := DrawTiled(unknownDst{gotBacking}, image.Rect(1, 1, 19, 19), gotBacking, image.Pt(2, 3), Src, &TileOptions{TileSize: 4, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(refBacking.Pix, gotBacking.Pix) {
		t.Fatal("unknown dst: pixels differ")
	}
}

// TestDrawTiledFloydSteinberg checks that Floyd-Steinberg, which cannot be
// tiled, produces the same output as the old API.
func TestDrawTiledFloydSteinberg(t *testing.T) {
	src := checkerboard(image.Rect(0, 0, 64, 48), 0xff)
	dst0 := image.NewPaletted(image.Rect(0, 0, 64, 48), color.Palette{
		color.RGBA{0, 0, 0, 0xff},
		color.RGBA{0xff, 0xff, 0xff, 0xff},
		color.RGBA{0xff, 0, 0, 0xff},
	})
	dst1 := image.NewPaletted(image.Rect(0, 0, 64, 48), color.Palette{
		color.RGBA{0, 0, 0, 0xff},
		color.RGBA{0xff, 0xff, 0xff, 0xff},
		color.RGBA{0xff, 0, 0, 0xff},
	})
	Draw(dst0, dst0.Bounds(), image.NewUniform(color.Black), image.Point{}, Src)
	Draw(dst1, dst1.Bounds(), image.NewUniform(color.Black), image.Point{}, Src)
	FloydSteinberg.Draw(dst0, dst0.Bounds(), src, image.Point{})
	if err := DrawTiledWith(FloydSteinberg, dst1, dst1.Bounds(), src, image.Point{}, &TileOptions{TileSize: 8, Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst0.Pix, dst1.Pix) {
		t.Fatal("FloydSteinberg: pixels differ")
	}
}

// TestDrawTiledWithOp checks that an Op used as a Drawer matches DrawTiled.
func TestDrawTiledWithOp(t *testing.T) {
	src := checkerboard(image.Rect(0, 0, 64, 48), 0x80)
	for _, op := range []Op{Over, Src} {
		dst0 := image.NewRGBA(image.Rect(0, 0, 64, 48))
		dst1 := image.NewRGBA(image.Rect(0, 0, 64, 48))
		fillImage(dst0, dst0.Bounds())
		fillImage(dst1, dst1.Bounds())
		r := image.Rect(3, 4, 60, 40)
		Draw(dst0, r, src, image.Pt(5, 6), op)
		if err := DrawTiledWith(op, dst1, r, src, image.Pt(5, 6), &TileOptions{TileSize: 7, Workers: 4}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dst0.Pix, dst1.Pix) {
			t.Fatalf("op=%v: pixels differ", op)
		}
	}
}

// TestDrawTiledCanceled checks the pre-canceled case: no pixels change and the
// context error is returned.
func TestDrawTiledCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := newDst("RGBA", image.Rect(0, 0, 32, 32))
	before := append([]byte(nil), dstPix(dst)...)
	err := DrawTiled(dst, dst.Bounds(), checkerboard(image.Rect(0, 0, 32, 32), 0xff), image.Point{}, Src, &TileOptions{Context: ctx})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want context.Canceled", err)
	}
	if !bytes.Equal(before, dstPix(dst)) {
		t.Fatal("pre-canceled draw modified the destination")
	}

	// Also for a draw that cannot be tiled.
	err = DrawTiledWith(FloydSteinberg, dst, dst.Bounds(), checkerboard(image.Rect(0, 0, 32, 32), 0xff), image.Point{}, &TileOptions{Context: ctx})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FloydSteinberg: got error %v, want context.Canceled", err)
	}
	if !bytes.Equal(before, dstPix(dst)) {
		t.Fatal("pre-canceled FloydSteinberg draw modified the destination")
	}
}

// TestDrawTiledCancelDuringWholeCall checks that a context canceled while a
// non-tileable draw is running is reported afterwards.
func TestDrawTiledCancelDuringWholeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dst := image.NewRGBA(image.Rect(0, 0, 16, 16))
	cancelAfter := &countingImage{Image: checkerboard(image.Rect(0, 0, 16, 16), 0xff), cancel: cancel, after: 4}
	// The source type is unknown, so the draw is a single call.
	if err := DrawTiled(dst, dst.Bounds(), cancelAfter, image.Point{}, Src, &TileOptions{Context: ctx}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want context.Canceled", err)
	}
}

type countingImage struct {
	image.Image
	cancel context.CancelFunc
	after  int
	n      int
}

func (m *countingImage) At(x, y int) color.Color {
	// The draw calls At once per pixel for this unrecognized source type. Count
	// calls without a mutex; the draw is sequential here.
	m.n++
	if m.n == m.after {
		m.cancel()
	}
	return m.Image.At(x, y)
}

// TestTiledSchedulerCancel checks the internal scheduler directly: a
// cancellation after k tiles stops the remaining tiles, every tile that was
// started is completed, and no goroutine outlives the call.
func TestTiledSchedulerCancel(t *testing.T) {
	base := runtime.NumGoroutine()
	r := image.Rect(0, 0, 128, 128)
	for _, workers := range []int{1, 2, 4, 16} {
		ctx, cancel := context.WithCancel(context.Background())
		var mu sync.Mutex
		var started, finished int
		fn := func(tile image.Rectangle) {
			mu.Lock()
			started++
			if started == 5 {
				cancel()
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			finished++
			mu.Unlock()
		}
		err := runTilesParallel(ctx, r, 8, workers, fn)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("workers=%d: got error %v, want context.Canceled", workers, err)
		}
		// Every started tile must finish, and we must not have drawn all 256.
		if started != finished {
			t.Fatalf("workers=%d: started %d tiles but finished %d", workers, started, finished)
		}
		if finished >= 256 {
			t.Fatalf("workers=%d: cancellation did not stop the draw", workers)
		}
	}

	// Allow any lingering goroutines to exit before checking for leaks. There
	// should be none, because runTilesParallel waits for its workers.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("goroutine leak: %d now, %d before", n, base)
	}
}

// TestTiledSchedulerParallel checks that tiles really are drawn concurrently,
// and that no more than the requested number of workers runs at once.
func TestTiledSchedulerParallel(t *testing.T) {
	const workers = 4
	var (
		mu                    sync.Mutex
		inFlight, maxInFlight int
	)
	fn := func(image.Rectangle) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}
	// 16 tiles over a worker pool of 4.
	if err := runTilesParallel(context.Background(), image.Rect(0, 0, 128, 128), 32, workers, fn); err != nil {
		t.Fatal(err)
	}
	if maxInFlight < 2 {
		t.Fatalf("tiles never ran concurrently: max in flight %d", maxInFlight)
	}
	if maxInFlight > workers {
		t.Fatalf("%d tiles in flight, want at most %d", maxInFlight, workers)
	}
	if GOMAXPROCS := runtime.GOMAXPROCS(0); GOMAXPROCS == 1 && maxInFlight != 1 {
		t.Fatalf("with GOMAXPROCS=1, got %d tiles in flight", maxInFlight)
	}
}

// TestTiledSchedulerBounds checks the grid layout, including tiny tiles, huge
// tile sizes, and worker counts above the tile count and the global limit.
func TestTiledSchedulerBounds(t *testing.T) {
	r := image.Rect(-7, 3, 13, 9) // 20x6.
	for _, tc := range []struct {
		size     int
		wantCols int64
		wantRows int64
	}{
		{1, 20, 6},
		{2, 10, 3},
		{3, 7, 2},
		{5, 4, 2},
		{1000, 1, 1},
		{0, (20 + DefaultTileSize - 1) / DefaultTileSize, 1}, // DefaultTileSize.
	} {
		g := newTileGrid(r, tc.size)
		got := g.count()
		if g.cols != tc.wantCols || g.rows != tc.wantRows {
			t.Fatalf("size=%d: grid is %dx%d, want %dx%d", tc.size, g.cols, g.rows, tc.wantCols, tc.wantRows)
		}
		if got != tc.wantCols*tc.wantRows {
			t.Fatalf("size=%d: count is %d, want %d", tc.size, got, tc.wantCols*tc.wantRows)
		}
		// The tiles must exactly partition r.
		area := 0
		for i := int64(0); i < got; i++ {
			tile := g.tileAt(i)
			if tile.Empty() || !tile.In(r) {
				t.Fatalf("size=%d: tile %d is %v, not inside %v", tc.size, i, tile, r)
			}
			area += tile.Dx() * tile.Dy()
		}
		if area != r.Dx()*r.Dy() {
			t.Fatalf("size=%d: tiles cover %d pixels, want %d", tc.size, area, r.Dx()*r.Dy())
		}
	}

	// Workers above the tile count and above MaxTileWorkers must still run.
	runs := 0
	var mu sync.Mutex
	if err := runTilesParallel(context.Background(), image.Rect(0, 0, 4, 4), 2, 1<<30, func(image.Rectangle) {
		mu.Lock()
		runs++
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	if runs != 4 {
		t.Fatalf("ran %d tiles, want 4", runs)
	}
}

// TestDrawTiledHugeRect checks rectangles whose coordinates are near the ends
// of the int range. These must not overflow the tile arithmetic.
func TestDrawTiledHugeRect(t *testing.T) {
	dst := image.NewRGBA(image.Rect(0, 0, 8, 8))
	fillImage(dst, dst.Bounds())
	// A rectangle far larger than dst. Because it extends the uniform source's
	// bounds by the same amount, the whole-rectangle call is itself a no-op, and
	// the tiled call must agree with it rather than overflow.
	const huge = 1 << 28
	ref := image.NewRGBA(image.Rect(0, 0, 8, 8))
	fillImage(ref, ref.Bounds())
	Draw(ref, image.Rect(-huge, -huge, huge, huge), image.NewUniform(color.White), image.Point{}, Src)
	if err := DrawTiled(dst, image.Rect(-huge, -huge, huge, huge), image.NewUniform(color.White), image.Point{}, Src, &TileOptions{TileSize: 1, Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ref.Pix, dst.Pix) {
		t.Fatal("huge rect: pixels differ from Draw")
	}

	// A huge rectangle that does cover dst.
	ref2 := image.NewRGBA(image.Rect(0, 0, 8, 8))
	fillImage(ref2, ref2.Bounds())
	Draw(ref2, image.Rect(-huge, -huge, huge, huge), image.NewUniform(color.White), image.Pt(-20, -20), Src)
	dst2 := image.NewRGBA(image.Rect(0, 0, 8, 8))
	fillImage(dst2, dst2.Bounds())
	if err := DrawTiled(dst2, image.Rect(-huge, -huge, huge, huge), image.NewUniform(color.White), image.Pt(-20, -20), Src, &TileOptions{TileSize: 3, Workers: 8}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ref2.Pix, dst2.Pix) {
		t.Fatal("huge covering rect: pixels differ from Draw")
	}

	// Tile coordinates near the end of the int range must not overflow.
	maxInt := int(^uint(0) >> 1)
	r := image.Rect(maxInt-20, maxInt-20, maxInt, maxInt)
	g := newTileGrid(r, 7)
	area := 0
	for i := int64(0); i < g.count(); i++ {
		tile := g.tileAt(i)
		if tile.Empty() || !tile.In(r) {
			t.Fatalf("tile %d is %v, not inside %v", i, tile, r)
		}
		area += tile.Dx() * tile.Dy()
	}
	if area != 400 {
		t.Fatalf("tiles cover %d pixels, want 400", area)
	}
}

// TestDrawTiledEmptyIntersection checks that empty intersections do not start
// any work and do not error.
func TestDrawTiledEmptyIntersection(t *testing.T) {
	src := checkerboard(image.Rect(0, 0, 16, 16), 0x80)
	for _, r := range []image.Rectangle{
		image.Rect(0, 0, 0, 0),
		image.Rect(5, 5, 5, 10),
		image.Rect(100, 100, 200, 200),
		// An uncanonical empty rectangle; image.Rect would canonicalize it.
		{Min: image.Pt(9, 9), Max: image.Pt(4, 4)},
	} {
		dst := image.NewRGBA(image.Rect(0, 0, 8, 8))
		fillImage(dst, dst.Bounds())
		before := append([]byte(nil), dst.Pix...)
		if err := DrawTiled(dst, r, src, image.Point{}, Src, &TileOptions{TileSize: 1, Workers: 8}); err != nil {
			t.Fatalf("r=%v: %v", r, err)
		}
		if !bytes.Equal(before, dst.Pix) {
			t.Fatalf("r=%v: empty intersection modified dst", r)
		}
	}
}

// TestTileSafeAliasing checks the detection logic directly.
func TestTileSafeAliasing(t *testing.T) {
	a := image.NewRGBA(image.Rect(0, 0, 16, 16))
	b := image.NewRGBA(image.Rect(0, 0, 16, 16))
	if !tileSafe(a, b, nil) {
		t.Fatal("distinct images reported as aliased")
	}
	if tileSafe(a, a, nil) {
		t.Fatal("identical image not reported as aliased")
	}
	if tileSafe(a, a.SubImage(image.Rect(2, 2, 6, 6)), nil) {
		t.Fatal("SubImage not reported as aliased")
	}
	if !tileSafe(a, image.NewUniform(color.White), nil) {
		t.Fatal("Uniform reported as aliased; it has no pixel storage")
	}
	if tileSafe(a, nil, a) {
		t.Fatal("mask aliasing not reported")
	}
	// Zero-length Pix must not be considered overlapping.
	empty := &image.RGBA{}
	if !tileSafe(a, empty, nil) {
		t.Fatal("empty image reported as aliased")
	}
}

func BenchmarkDraw(b *testing.B)      { benchmarkDraw(b, false) }
func BenchmarkDrawTiled(b *testing.B) { benchmarkDraw(b, true) }

func benchmarkDraw(b *testing.B, tiled bool) {
	const w, h = 2048, 2048
	src := checkerboard(image.Rect(0, 0, w, h), 0x80)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	opts := &TileOptions{TileSize: 256, Workers: runtime.GOMAXPROCS(0)}
	b.ReportAllocs()
	b.SetBytes(int64(w * h * 4))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if tiled {
			if err := DrawTiled(dst, dst.Bounds(), src, image.Point{}, Over, opts); err != nil {
				b.Fatal(err)
			}
		} else {
			Draw(dst, dst.Bounds(), src, image.Point{}, Over)
		}
	}
}

// TestDrawTiledConcurrentReads checks that two tiled draws can run at once
// when they write to different destinations and read the same source, which is
// the editor's "several large images at once" case.
func TestDrawTiledConcurrentReads(t *testing.T) {
	src := checkerboard(image.Rect(0, 0, 512, 512), 0x80)
	run := func(dst Image) error {
		return DrawTiled(dst, dst.Bounds(), src, image.Point{}, Over, &TileOptions{TileSize: 64, Workers: 4})
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		dst := image.NewRGBA(image.Rect(0, 0, 512, 512))
		fillImage(dst, dst.Bounds())
		wg.Add(1)
		go func(i int, dst Image) {
			defer wg.Done()
			errs[i] = run(dst)
		}(i, dst)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("draw %d: %v", i, err)
		}
	}
}

// TestDrawTiledRandomStress compares tiled and serial drawing on random
// rectangles, sources and masks.
func TestDrawTiledRandomStress(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	dstBounds := image.Rect(-8, -5, 40, 33)
	for i := 0; i < 200; i++ {
		srcBounds := image.Rect(-20, -20, 60, 60)
		src := checkerboard(srcBounds, uint8(rnd.Intn(256)))
		mask := alphaMask(srcBounds)
		r := randomRect(rnd, dstBounds)
		sp := image.Pt(rnd.Intn(80)-20, rnd.Intn(80)-20)
		mp := image.Pt(rnd.Intn(80)-20, rnd.Intn(80)-20)
		var op Op
		if rnd.Intn(2) == 0 {
			op = Over
		} else {
			op = Src
		}
		useMask := rnd.Intn(2) == 0
		tileSize := 1 + rnd.Intn(20)
		workers := 1 + rnd.Intn(12)
		if rnd.Intn(4) == 0 {
			workers = -1
		}

		ref := newDst("RGBA", dstBounds)
		got := newDst("RGBA", dstBounds)
		if useMask {
			DrawMask(ref, r, src, sp, mask, mp, op)
			if err := DrawMaskTiled(got, r, src, sp, mask, mp, op, &TileOptions{TileSize: tileSize, Workers: workers}); err != nil {
				t.Fatal(err)
			}
		} else {
			Draw(ref, r, src, sp, op)
			if err := DrawTiled(got, r, src, sp, op, &TileOptions{TileSize: tileSize, Workers: workers}); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(dstPix(ref), dstPix(got)) {
			t.Fatalf("iteration %d: r=%v sp=%v mp=%v op=%v mask=%t tile=%d workers=%d: pixels differ",
				i, r, sp, mp, op, useMask, tileSize, workers)
		}
	}
}

func randomRect(rnd *rand.Rand, bounds image.Rectangle) image.Rectangle {
	x0 := bounds.Min.X - 10 + rnd.Intn(bounds.Dx()+20)
	x1 := bounds.Min.X - 10 + rnd.Intn(bounds.Dx()+20)
	y0 := bounds.Min.Y - 10 + rnd.Intn(bounds.Dy()+20)
	y1 := bounds.Min.Y - 10 + rnd.Intn(bounds.Dy()+20)
	return image.Rect(min(x0, x1), min(y0, y1), max(x0, x1), max(y0, y1))
}

// TestTiledOptionsResolve checks the option clamps.
func TestTiledOptionsResolve(t *testing.T) {
	for _, tc := range []struct {
		opts    *TileOptions
		tile    int
		workers int
	}{
		{nil, DefaultTileSize, clampTileWorkers(runtime.GOMAXPROCS(0))},
		{&TileOptions{}, DefaultTileSize, clampTileWorkers(runtime.GOMAXPROCS(0))},
		{&TileOptions{TileSize: -5, Workers: -5}, DefaultTileSize, clampTileWorkers(runtime.GOMAXPROCS(0))},
		{&TileOptions{TileSize: 3, Workers: 2}, 3, 2},
		{&TileOptions{Workers: MaxTileWorkers + 1000}, DefaultTileSize, MaxTileWorkers},
		{&TileOptions{Workers: 8, Sequential: true}, DefaultTileSize, 1},
	} {
		tile, workers, ctx := tc.opts.resolve()
		if tile != tc.tile || workers != tc.workers {
			t.Errorf("opts=%+v: got tile=%d workers=%d, want tile=%d workers=%d",
				tc.opts, tile, workers, tc.tile, tc.workers)
		}
		if ctx == nil {
			t.Errorf("opts=%+v: nil context", tc.opts)
		}
	}
}
