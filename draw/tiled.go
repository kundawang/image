// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package draw

import (
	"context"
	"image"
	"math"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
)

const (
	// DefaultTileSize is the default edge length, in destination pixels, of a
	// tile drawn by DrawTiled, DrawMaskTiled or DrawTiledWith. A tile is a
	// rectangle whose width and height are at most DefaultTileSize, clipped to
	// the destination image's bounds.
	DefaultTileSize = 256

	// MaxTileWorkers is the maximum number of goroutines that a single tiled
	// draw will start. A larger Workers value in a TileOptions is silently
	// clamped to MaxTileWorkers.
	MaxTileWorkers = 256
)

// TileOptions are optional parameters to DrawTiled, DrawMaskTiled and
// DrawTiledWith.
//
// A nil *TileOptions means to use the default (zero) values of each field.
type TileOptions struct {
	// TileSize is the width and height, in destination pixels, of the
	// rectangles that a draw is split into. Values <= 0 mean DefaultTileSize.
	//
	// The effective tiles are the intersection of this grid with the
	// destination rectangle after it has been clipped to the dst, src and mask
	// bounds. Tiles never overlap, so no two goroutines write the same
	// destination pixel.
	TileSize int

	// Workers is the number of goroutines that draw tiles in parallel. Values
	// <= 0 mean runtime.GOMAXPROCS(0), i.e. one goroutine per available CPU.
	// Values greater than MaxTileWorkers are clamped to MaxTileWorkers, and
	// values greater than the number of tiles are clamped to the number of
	// tiles.
	//
	// Workers == 1 draws every tile in the calling goroutine.
	Workers int

	// Sequential forces the draw to run entirely in the calling goroutine,
	// regardless of Workers, in top-to-bottom, left-to-right tile order. It is
	// equivalent to Workers == 1 but is also useful to make the intent clear.
	//
	// Sequential does not change the resulting pixels; it only changes how the
	// work is scheduled.
	Sequential bool

	// Context, if non-nil, can cancel the draw. Cancellation is checked before
	// the draw starts and before each tile is drawn. When the context is done,
	// no further tiles are started and the context's error is returned.
	//
	// A canceled draw may leave the destination partially updated: some tiles
	// may have been drawn and others not. Any individual tile is either drawn
	// completely or not at all. If the context was already done when the call
	// started, the destination is not modified at all.
	//
	// A draw that this package cannot safely split into tiles (see DrawTiled)
	// runs as a single call. Cancellation is checked before and after that
	// call, which itself cannot be interrupted.
	Context context.Context
}

// resolve returns the effective tile size, worker count and context.
func (o *TileOptions) resolve() (tileSize, workers int, ctx context.Context) {
	tileSize = DefaultTileSize
	workers = runtime.GOMAXPROCS(0)
	ctx = context.Background()
	if o == nil {
		return tileSize, clampTileWorkers(workers), ctx
	}
	if o.TileSize > 0 {
		tileSize = o.TileSize
	}
	if o.Workers > 0 {
		workers = o.Workers
	}
	if o.Context != nil {
		ctx = o.Context
	}
	if o.Sequential {
		workers = 1
	}
	return tileSize, clampTileWorkers(workers), ctx
}

func clampTileWorkers(workers int) int {
	if workers < 1 {
		return 1
	}
	if workers > MaxTileWorkers {
		return MaxTileWorkers
	}
	return workers
}

// DrawTiled is like Draw but splits the drawing work into tiles that can be
// drawn concurrently.
//
// The resulting pixels are identical to Draw(dst, r, src, sp, op): drawing the
// tiles of a rectangle in any order gives the same result as drawing the whole
// rectangle. A nil opts uses the default TileOptions values.
//
// Tiles are only drawn concurrently when this package can prove that they do
// not alias. That is possible when dst and src are standard library image
// types (*image.RGBA, *image.NRGBA, *image.RGBA64, *image.NRGBA64,
// *image.Gray, *image.Gray16, *image.Alpha, *image.Alpha16, *image.CMYK,
// *image.Paletted and *image.YCbCr) or when src is an *image.Uniform, and dst
// and src do not share pixel storage. Otherwise, including when src and dst
// alias (such as drawing an image onto itself with an offset), the draw still
// produces exactly what Draw produces, but it is not split into tiles: it runs
// as a single sequential call. This is what keeps aliased draws correct, since
// the standard library draws overlapping rectangles in a particular order.
//
// A dst of type *image.YCbCr or *image.NYCbCrA is also drawn as a single call,
// because neighbouring destination pixels share chroma bytes and so are not
// independent.
//
// If the context in opts is done before the draw completes, DrawTiled returns
// the context's error after stopping all of its goroutines. The destination
// may then be partially updated. DrawTiled never leaks goroutines.
func DrawTiled(dst Image, r image.Rectangle, src image.Image, sp image.Point, op Op, opts *TileOptions) error {
	return DrawMaskTiled(dst, r, src, sp, nil, image.Point{}, op, opts)
}

// DrawMaskTiled is like DrawMask but splits the drawing work into tiles that
// can be drawn concurrently.
//
// The resulting pixels are identical to
// DrawMask(dst, r, src, sp, mask, mp, op). See DrawTiled for the conditions
// under which the tiles are actually drawn concurrently, and for the
// cancellation behavior.
//
// A mask that shares pixel storage with dst is detected, and the draw then runs
// as a single sequential call, so that no goroutine reads a mask pixel that
// another goroutine is concurrently overwriting.
func DrawMaskTiled(dst Image, r image.Rectangle, src image.Image, sp image.Point, mask image.Image, mp image.Point, op Op, opts *TileOptions) error {
	tileSize, workers, ctx := opts.resolve()
	return runTiled(ctx, tileSize, workers, dst, r, src, sp, mask, mp,
		func(t image.Rectangle) {
			// A tile is a destination rectangle, so map src and mask points the
			// same way the whole-rectangle call maps them: r.Min is aligned
			// with sp (and mp), and t.Min is an offset from r.Min.
			d := t.Min.Sub(r.Min)
			DrawMask(dst, t, src, sp.Add(d), mask, mp.Add(d), op)
		},
		func() {
			// r is untouched here, including an empty or uncanonical r, so this
			// is exactly the call that Draw would make.
			DrawMask(dst, r, src, sp, mask, mp, op)
		})
}

// DrawTiledWith is like DrawTiled but for a Drawer, such as FloydSteinberg or
// an Op.
//
// Only drawers whose Draw method visits destination pixels independently and
// does not read dst can be split into tiles. This package knows that Op
// satisfies that; FloydSteinberg does not, because its error diffusion carries
// state from one pixel to its neighbours. Any other Drawer is invoked exactly
// once, for the whole rectangle, just as calling its Draw method directly
// would.
//
// The resulting pixels are identical to d.Draw(dst, r, src, sp), except that a
// canceled context stops a tiled Op draw early (see TileOptions.Context).
func DrawTiledWith(d Drawer, dst Image, r image.Rectangle, src image.Image, sp image.Point, opts *TileOptions) error {
	tileSize, workers, ctx := opts.resolve()
	if err := ctx.Err(); err != nil {
		return err
	}

	switch d.(type) {
	case Op:
		return runTiled(ctx, tileSize, workers, dst, r, src, sp, nil, image.Point{},
			func(t image.Rectangle) {
				d.Draw(dst, t, src, sp.Add(t.Min.Sub(r.Min)))
			},
			func() {
				d.Draw(dst, r, src, sp)
			})
	}

	// Any other Drawer, including FloydSteinberg, may read dst or carry state
	// from one row or pixel to the next. Calling it once, exactly as its Draw
	// method would be called, is the only way to reproduce its output.
	d.Draw(dst, r, src, sp)
	return ctx.Err()
}

// runTiled is the common driver for the tiled draws.
//
// tile draws one tile, which is a destination rectangle inside the clipped
// argument rectangle. whole draws the unmodified argument rectangle. r, src,
// sp, mask and mp are exactly the arguments of the corresponding un-tiled
// call, used to clip and to decide whether tiling is safe.
func runTiled(ctx context.Context, tileSize, workers int, dst Image, r image.Rectangle, src image.Image, sp image.Point, mask image.Image, mp image.Point, tile func(image.Rectangle), whole func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if !tileSafe(dst, src, mask) {
		// Tiling could change the pixels (src or mask aliases dst) or we cannot
		// see the pixel storage involved. Do exactly what the old API does.
		whole()
		return ctx.Err()
	}

	cr := clippedDrawRect(dst, r, src, sp, mask, mp)
	if cr.Empty() {
		// The whole-rectangle call would be a no-op too.
		return nil
	}

	if workers <= 1 {
		return runTilesSequential(ctx, cr, tileSize, tile)
	}
	return runTilesParallel(ctx, cr, tileSize, workers, tile)
}

// clippedDrawRect clips r against the dst, src and mask bounds like
// image/draw's clip function, and returns the rectangle in dst coordinates. If
// the intersection is empty, image.Rectangle{} is returned.
func clippedDrawRect(dst Image, r image.Rectangle, src image.Image, sp image.Point, mask image.Image, mp image.Point) image.Rectangle {
	orig := r.Min
	r = r.Intersect(dst.Bounds())
	r = r.Intersect(src.Bounds().Add(orig.Sub(sp)))
	if mask != nil {
		r = r.Intersect(mask.Bounds().Add(orig.Sub(mp)))
	}
	if r.Empty() {
		return image.Rectangle{}
	}
	return r
}

// tileGrid lays out a grid of non-overlapping tiles covering r.
type tileGrid struct {
	r    image.Rectangle
	size int
	cols int64
	rows int64
}

func newTileGrid(r image.Rectangle, size int) tileGrid {
	if size <= 0 {
		size = DefaultTileSize
	}
	// r is non-empty, so Dx and Dy are positive.
	dx, dy := int64(r.Dx()), int64(r.Dy())
	return tileGrid{
		r:    r,
		size: size,
		cols: (dx + int64(size) - 1) / int64(size),
		rows: (dy + int64(size) - 1) / int64(size),
	}
}

// count returns the number of tiles, saturating instead of overflowing.
func (g tileGrid) count() int64 {
	if g.rows != 0 && g.cols > math.MaxInt64/g.rows {
		return math.MaxInt64
	}
	return g.cols * g.rows
}

// tileAt returns the i'th tile, numbering tiles row by row.
func (g tileGrid) tileAt(i int64) image.Rectangle {
	col := i % g.cols
	row := i / g.cols
	x0 := int(int64(g.r.Min.X) + col*int64(g.size))
	y0 := int(int64(g.r.Min.Y) + row*int64(g.size))
	x1, y1 := x0+g.size, y0+g.size
	// The "x1 < x0" comparisons guard against int overflow for pathological
	// rectangles whose coordinates are near the ends of the int range.
	if x1 > g.r.Max.X || x1 < x0 {
		x1 = g.r.Max.X
	}
	if y1 > g.r.Max.Y || y1 < y0 {
		y1 = g.r.Max.Y
	}
	return image.Rect(x0, y0, x1, y1)
}

// runTilesSequential draws every tile in the calling goroutine, in row-major
// order.
func runTilesSequential(ctx context.Context, r image.Rectangle, tileSize int, fn func(image.Rectangle)) error {
	g := newTileGrid(r, tileSize)
	n := g.count()
	for i := int64(0); i < n; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		fn(g.tileAt(i))
	}
	return nil
}

// runTilesParallel draws tiles with a fixed pool of workers. It returns the
// context's error if the draw stopped before every tile was drawn. Workers stop
// as soon as the context is done, so no goroutine outlives this call.
//
// Tiles are disjoint and each tile is drawn by exactly one worker, so the
// result does not depend on scheduling.
func runTilesParallel(ctx context.Context, r image.Rectangle, tileSize, workers int, fn func(image.Rectangle)) error {
	g := newTileGrid(r, tileSize)
	n := g.count()
	workers = clampTileWorkers(workers)
	if workers == 1 || n <= 1 {
		return runTilesSequential(ctx, r, tileSize, fn)
	}
	if int64(workers) > n {
		workers = int(n)
	}

	var (
		next atomic.Int64 // the next tile index to claim, plus one
		done atomic.Int64 // the number of tiles completely drawn
		wg   sync.WaitGroup
	)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				// Check for cancellation before claiming a tile. A claimed tile
				// is always drawn to completion, so cancellation never leaves a
				// partially drawn tile behind.
				if ctx.Err() != nil {
					return
				}
				i := next.Add(1) - 1
				if i >= n {
					return
				}
				fn(g.tileAt(i))
				done.Add(1)
			}
		}()
	}
	wg.Wait()

	if done.Load() == n {
		return nil
	}
	// Workers only stop early when the context is done.
	return ctx.Err()
}

// tileSafe reports whether a draw may be split into tiles that are drawn
// concurrently. It is deliberately conservative: if it cannot identify the
// pixel storage of an image, or finds that src or mask may alias dst, it
// reports false and the caller draws the whole rectangle in one call.
func tileSafe(dst Image, src, mask image.Image) bool {
	dstPlanes, ok := dstPixPlanes(dst)
	if !ok {
		return false
	}
	if src != nil && !disjointPix(dstPlanes, src) {
		return false
	}
	if mask != nil && !disjointPix(dstPlanes, mask) {
		return false
	}
	return true
}

// dstPixPlanes returns the byte slices that hold dst's pixels, for image types
// that are safe to write concurrently on disjoint rectangles. ok is false for
// everything else, including *image.YCbCr and *image.NYCbCrA, whose
// neighbouring pixels share chroma bytes.
func dstPixPlanes(m Image) ([][]byte, bool) {
	switch m := m.(type) {
	case *image.RGBA:
		return [][]byte{m.Pix}, true
	case *image.RGBA64:
		return [][]byte{m.Pix}, true
	case *image.NRGBA:
		return [][]byte{m.Pix}, true
	case *image.NRGBA64:
		return [][]byte{m.Pix}, true
	case *image.Alpha:
		return [][]byte{m.Pix}, true
	case *image.Alpha16:
		return [][]byte{m.Pix}, true
	case *image.Gray:
		return [][]byte{m.Pix}, true
	case *image.Gray16:
		return [][]byte{m.Pix}, true
	case *image.CMYK:
		return [][]byte{m.Pix}, true
	case *image.Paletted:
		return [][]byte{m.Pix}, true
	}
	return nil, false
}

// disjointPix reports whether m's pixel storage is provably disjoint from
// dstPlanes. An unrecognized image type is treated as possibly aliasable.
func disjointPix(dstPlanes [][]byte, m image.Image) bool {
	planes, ok := imagePixPlanes(m)
	if !ok {
		return false
	}
	for _, dp := range dstPlanes {
		for _, p := range planes {
			if slicesOverlap(dp, p) {
				return false
			}
		}
	}
	return true
}

// imagePixPlanes returns the byte slices that hold m's pixels. ok is false for
// image types whose storage this package does not know how to inspect, in
// which case callers must assume that m may share storage with dst.
//
// The returned slices may extend beyond the rectangle that m actually uses
// (for example, a SubImage's Pix extends to the end of its parent). That makes
// overlap detection conservative, never wrong.
func imagePixPlanes(m image.Image) (planes [][]byte, ok bool) {
	switch m := m.(type) {
	case *image.RGBA:
		return [][]byte{m.Pix}, true
	case *image.RGBA64:
		return [][]byte{m.Pix}, true
	case *image.NRGBA:
		return [][]byte{m.Pix}, true
	case *image.NRGBA64:
		return [][]byte{m.Pix}, true
	case *image.Alpha:
		return [][]byte{m.Pix}, true
	case *image.Alpha16:
		return [][]byte{m.Pix}, true
	case *image.Gray:
		return [][]byte{m.Pix}, true
	case *image.Gray16:
		return [][]byte{m.Pix}, true
	case *image.CMYK:
		return [][]byte{m.Pix}, true
	case *image.Paletted:
		return [][]byte{m.Pix}, true
	case *image.YCbCr:
		return [][]byte{m.Y, m.Cb, m.Cr}, true
	case *image.NYCbCrA:
		return [][]byte{m.Y, m.Cb, m.Cr, m.A}, true
	case *image.Uniform:
		// A Uniform has no pixel storage: it returns the same color for every
		// coordinate, so it can never share writable storage with dst.
		return nil, true
	}
	return nil, false
}

// slicesOverlap reports whether a and b share at least one byte. The
// comparisons are written so that they cannot overflow even for addresses near
// the top of the uintptr range.
func slicesOverlap(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	a0 := reflect.ValueOf(a).Pointer()
	b0 := reflect.ValueOf(b).Pointer()
	if a0 <= b0 {
		return b0-a0 < uintptr(len(a))
	}
	return a0-b0 < uintptr(len(b))
}
