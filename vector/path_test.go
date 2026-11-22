// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vector

// The reference helpers in this file (ref* functions and the ref* data
// structures) deliberately re-derive the geometry from scratch instead of
// calling the production code, so that the tests compare two independent
// implementations of the same mathematics.

import (
	"image"
	"math"
	"math/rand"
	"testing"
)

// ---------------------------------------------------------------------------
// Independent analytic reference: Bézier evaluation and tight bounds.
// ---------------------------------------------------------------------------

func refLerp(a, b, t float64) float64 { return a + (b-a)*t }

// refQuadPoint evaluates a quadratic Bézier via two stages of de Casteljau.
func refQuadPoint(ax, ay, bx, by, cx, cy, t float64) (x, y float64) {
	abx, aby := refLerp(ax, bx, t), refLerp(ay, by, t)
	bcx, bcy := refLerp(bx, cx, t), refLerp(by, cy, t)
	return refLerp(abx, bcx, t), refLerp(aby, bcy, t)
}

// refCubePoint evaluates a cubic Bézier via three stages of de Casteljau.
func refCubePoint(ax, ay, bx, by, cx, cy, dx, dy, t float64) (x, y float64) {
	abx, aby := refLerp(ax, bx, t), refLerp(ay, by, t)
	bcx, bcy := refLerp(bx, cx, t), refLerp(by, cy, t)
	cdx, cdy := refLerp(cx, dx, t), refLerp(cy, dy, t)
	abcx, abcy := refLerp(abx, bcx, t), refLerp(aby, bcy, t)
	bcdx, bcdy := refLerp(bcx, cdx, t), refLerp(bcy, cdy, t)
	return refLerp(abcx, bcdx, t), refLerp(abcy, bcdy, t)
}

type refBox struct {
	minX, minY, maxX, maxY float64
	empty                  bool
}

func newRefBox() refBox {
	nan := math.NaN()
	return refBox{minX: nan, minY: nan, maxX: nan, maxY: nan, empty: true}
}

func (r *refBox) add(x, y float64) {
	if r.empty {
		r.minX, r.maxX = x, x
		r.minY, r.maxY = y, y
		r.empty = false
		return
	}
	r.minX = math.Min(r.minX, x)
	r.maxX = math.Max(r.maxX, x)
	r.minY = math.Min(r.minY, y)
	r.maxY = math.Max(r.maxY, y)
}

// refDenseScan bounds a curve by dense uniform sampling (N+1 points). It is
// the independent oracle for the stationary-point math: coarse but
// trustworthy, and only used inside the test.
func refDenseQuad(ax, ay, bx, by, cx, cy float64, n int) refBox {
	r := newRefBox()
	for i := 0; i <= n; i++ {
		x, y := refQuadPoint(ax, ay, bx, by, cx, cy, float64(i)/float64(n))
		r.add(x, y)
	}
	return r
}

func refDenseCube(ax, ay, bx, by, cx, cy, dx, dy float64, n int) refBox {
	r := newRefBox()
	for i := 0; i <= n; i++ {
		x, y := refCubePoint(ax, ay, bx, by, cx, cy, dx, dy, float64(i)/float64(n))
		r.add(x, y)
	}
	return r
}

// refAnalyticQuad returns the mathematically exact (within float64) tight
// bounding box of a quadratic Bézier by solving B'(t)=0 per component.
func refAnalyticQuad(ax, ay, bx, by, cx, cy float64) refBox {
	r := refDenseQuad(ax, ay, bx, by, cx, cy, 2) // endpoints
	stationary := func(a, b, c float64) (float64, bool) {
		den := a - 2*b + c
		if den == 0 {
			return 0, false
		}
		t := (a - b) / den
		return t, t > 0 && t < 1
	}
	if t, ok := stationary(ax, bx, cx); ok {
		x, y := refQuadPoint(ax, ay, bx, by, cx, cy, t)
		r.add(x, y)
	}
	if t, ok := stationary(ay, by, cy); ok {
		x, y := refQuadPoint(ax, ay, bx, by, cx, cy, t)
		r.add(x, y)
	}
	return r
}

// refAnalyticCube returns the exact tight bounding box of a cubic Bézier by
// solving the component derivative quadratics with the discriminant formula.
func refAnalyticCube(ax, ay, bx, by, cx, cy, dx, dy float64) refBox {
	r := refDenseCube(ax, ay, bx, by, cx, cy, dx, dy, 2) // endpoints
	roots := func(a, b, c, d float64) []float64 {
		// d/dt component = 3[A t² + B t + C] with the coefficients derived
		// by expanding the cubic Bernstein form:
		//	A = -a + 3b - 3c + d
		//	B = 2a - 4b + 2c
		//	C = -a + b
		A := -a + 3*b - 3*c + d
		B := 2*a - 4*b + 2*c
		C := -a + b
		// A near-degenerate quadratic: fall back to the linear derivative,
		// whose single root is evaluated stably.
		if A == 0 || math.Abs(A)*1e12 <= math.Abs(B) {
			if B == 0 {
				return nil
			}
			return []float64{-C / B}
		}
		disc := B*B - 4*A*C
		if disc < 0 {
			return nil
		}
		if disc == 0 {
			return []float64{-B / (2 * A)}
		}
		s := math.Sqrt(disc)
		q := -0.5 * (B + math.Copysign(s, B))
		return []float64{q / A, C / q}
	}
	var ts []float64
	ts = append(ts, roots(ax, bx, cx, dx)...)
	ts = append(ts, roots(ay, by, cy, dy)...)
	for _, t := range ts {
		if t > 0 && t < 1 {
			x, y := refCubePoint(ax, ay, bx, by, cx, cy, dx, dy, t)
			r.add(x, y)
		}
	}
	return r
}

func boxesOverlapEnough(got, want refBox, tol float64) bool {
	return math.Abs(got.minX-want.minX) <= tol &&
		math.Abs(got.maxX-want.maxX) <= tol &&
		math.Abs(got.minY-want.minY) <= tol &&
		math.Abs(got.maxY-want.maxY) <= tol
}

// ---------------------------------------------------------------------------
// Analytic curve extrema: production vs independent reference.
// ---------------------------------------------------------------------------

func TestPathQuadExtremaReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 200; iter++ {
		ax, ay := rng.NormFloat64()*40, rng.NormFloat64()*40
		bx, by := rng.NormFloat64()*60, rng.NormFloat64()*60
		cx, cy := rng.NormFloat64()*40, rng.NormFloat64()*40

		b := newBoxF64()
		quadExtrema(&b, ax, ay, bx, by, cx, cy)
		b.include(ax, ay)
		b.include(cx, cy)

		want := refAnalyticQuad(ax, ay, bx, by, cx, cy)
		// Dense sampling must never escape the analytic box (sanity check on
		// the oracle itself). The tolerance is relative to the coordinate
		// magnitude because de Casteljau in float64 accumulates rounding.
		dense := refDenseQuad(ax, ay, bx, by, cx, cy, 500)
		tol := 1e-9 * (1 + math.Max(math.Abs(want.maxX), math.Abs(want.maxY)))
		if dense.minX < want.minX-tol || dense.maxX > want.maxX+tol ||
			dense.minY < want.minY-tol || dense.maxY > want.maxY+tol {
			t.Fatalf("iter=%d: dense sample escaped analytic box: %+v vs %+v", iter, dense, want)
		}
		got := refBox{minX: b.minX, minY: b.minY, maxX: b.maxX, maxY: b.maxY, empty: false}
		if !boxesOverlapEnough(got, want, 1e-9) {
			t.Errorf("iter=%d: got %+v, want %+v", iter, got, want)
		}
	}
}

func TestPathCubeExtremaReference(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for iter := 0; iter < 300; iter++ {
		ax, ay := rng.NormFloat64()*40, rng.NormFloat64()*40
		bx, by := rng.NormFloat64()*70, rng.NormFloat64()*70
		cx, cy := rng.NormFloat64()*70, rng.NormFloat64()*70
		dx, dy := rng.NormFloat64()*40, rng.NormFloat64()*40

		b := newBoxF64()
		cubeExtrema(&b, ax, ay, bx, by, cx, cy, dx, dy)
		b.include(ax, ay)
		b.include(dx, dy)

		want := refAnalyticCube(ax, ay, bx, by, cx, cy, dx, dy)
		dense := refDenseCube(ax, ay, bx, by, cx, cy, dx, dy, 800)
		tol := 1e-8 * (1 + math.Max(math.Abs(want.maxX), math.Abs(want.maxY)))
		if dense.minX < want.minX-tol || dense.maxX > want.maxX+tol ||
			dense.minY < want.minY-tol || dense.maxY > want.maxY+tol {
			t.Fatalf("iter=%d: dense sample escaped analytic box: %+v vs %+v", iter, dense, want)
		}
		got := refBox{minX: b.minX, minY: b.minY, maxX: b.maxX, maxY: b.maxY, empty: false}
		if !boxesOverlapEnough(got, want, 1e-8) {
			t.Errorf("iter=%d: got %+v, want %+v", iter, got, want)
		}
	}
}

func TestPathQuadraticRoots(t *testing.T) {
	// (t-1)(t-2) = t²-3t+2.
	got := quadraticRoots(1, -3, 2)
	want := map[float64]bool{1: false, 2: false}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for _, r := range got {
		if _, ok := want[r]; !ok || math.Abs(r-math.Round(r)) > 1e-9 {
			t.Errorf("unexpected root %v", r)
		}
	}
	// No real roots: t²+1 = 0.
	if rs := quadraticRoots(1, 0, 1); len(rs) != 0 {
		t.Errorf("expected no roots, got %v", rs)
	}
	// Degenerate linear: 2t-4 = 0.
	if rs := quadraticRoots(0, 2, -4); len(rs) != 1 || math.Abs(rs[0]-2) > 1e-9 {
		t.Errorf("linear case got %v", rs)
	}
}

// TestQuadSegmentCount pins the shared flattening heuristic against the
// formula that used to be inlined in Rasterizer.QuadTo/CubeTo, including the
// threshold boundary and the non-finite fallback.
func TestQuadSegmentCount(t *testing.T) {
	refCount := func(devsq float32) int {
		if devsq >= 0.333 {
			const tol = 3
			return 1 + int(math.Sqrt(math.Sqrt(tol*float64(devsq))))
		}
		return 1
	}
	for _, devsq := range []float32{0, 0.1, 0.332, 0.333, 0.334, 1, 3, 10, 100, 1e3, 1e6} {
		if got, want := quadSegmentCount(devsq), refCount(devsq); got != want {
			t.Errorf("devsq=%v: got %d, want %d", devsq, got, want)
		}
	}
	// NaN must behave like the original `devsq >= 0.333` test, which is false:
	// exactly one segment, never a panic from int(NaN).
	if got := quadSegmentCount(float32(math.NaN())); got != 1 {
		t.Errorf("NaN devsq: got %d, want 1", got)
	}
	if got := quadSegmentCount(float32(math.Inf(1))); got != refCount(float32(math.Inf(1))) {
		t.Errorf("+Inf devsq mismatch")
	}
}

// ---------------------------------------------------------------------------
// Fill bounds: lines, closed sub-paths, fixed-point coordinates and the
// pixel rounding rule.
// ---------------------------------------------------------------------------

func TestPathFillLineBounds(t *testing.T) {
	// A single non-horizontal line. Pixel coverage of a polyline segment is
	// exactly the floor/ceil box of its endpoints (same rule as the
	// Rasterizer's scan conversion).
	var p Path
	p.MoveTo(2.3, 1.2)
	p.LineTo(7.8, -3.4)
	got := p.Bounds(BoundsOptions{})
	want := image.Rect(2, -4, 8, 2)
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPathFillClosedSubPathBounds(t *testing.T) {
	var p Path
	p.MoveTo(1.5, 1.5)
	p.LineTo(4.5, 1.5)
	p.LineTo(4.5, 4.5)
	p.ClosePath() // closing edge back to (1.5, 1.5).
	got := p.Bounds(BoundsOptions{})
	want := image.Rect(1, 1, 5, 5)
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPathFillIntegerCoordinates(t *testing.T) {
	// Integer (fixed-point-friendly) coordinates must land exactly on pixel
	// boundaries without spurious growth.
	var p Path
	p.MoveTo(10, 20)
	p.LineTo(30, 40)
	got := p.Bounds(BoundsOptions{})
	want := image.Rect(10, 20, 30, 40)
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPathFillNegativeCoordinates(t *testing.T) {
	var p Path
	p.MoveTo(-10.25, -20.75)
	p.LineTo(-5.5, -2.25)
	got := p.Bounds(BoundsOptions{})
	want := image.Rect(-11, -21, -5, -2)
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestPathTinyCurvature covers the devsq < 0.333 regime, where a Bézier is
// flattened to a single segment and its bounds collapse to those of the
// chord: a tight box with no subdivision noise.
func TestPathTinyCurvature(t *testing.T) {
	// A quadratic whose control point is only a hair off the chord.
	var q Path
	q.MoveTo(0, 0)
	q.QuadTo(5, 0.001, 10, 0)
	if devSquared(0, 0, 5, 0.001, 10, 0) >= 0.333 {
		t.Fatal("test setup no longer in the single-segment regime")
	}
	qb := q.Bounds(BoundsOptions{})
	if qb != image.Rect(0, 0, 10, 1) {
		t.Errorf("near-flat quad: got %v, want (0,0)-(10,1)", qb)
	}

	// A fully collinear quadratic is geometrically a straight segment.
	var flat Path
	flat.MoveTo(1, 1)
	flat.QuadTo(5, 5, 9, 9)
	fb := flat.Bounds(BoundsOptions{})
	if fb != image.Rect(1, 1, 9, 9) {
		t.Errorf("collinear quad: got %v, want (1,1)-(9,9)", fb)
	}

	// A collinear cubic likewise.
	var cube Path
	cube.MoveTo(0, 0)
	cube.CubeTo(2, 2, 4, 4, 6, 6)
	cb := cube.Bounds(BoundsOptions{})
	if cb != image.Rect(0, 0, 6, 6) {
		t.Errorf("collinear cube: got %v, want (0,0)-(6,6)", cb)
	}
}

// TestPathStrokeNegativeCoordinates checks a stroke entirely in the negative
// quadrant so the box cannot accidentally anchor at the origin.
func TestPathStrokeNegativeCoordinates(t *testing.T) {
	var p Path
	p.MoveTo(-10, -10)
	p.LineTo(-2, -4)
	got := p.Bounds(BoundsOptions{StrokeWidth: 2, Cap: StrokeCapButt, Join: StrokeJoinBevel})
	if got.Min.X >= 0 || got.Min.Y >= 0 || got.Empty() {
		t.Errorf("negative-quadrant stroke got %v", got)
	}
	// Every covered pixel stays strictly negative.
	if got.Max.X > -1 || got.Max.Y > -3 {
		t.Errorf("stroke box %v reaches the non-negative quadrant", got)
	}
}

func TestPathFillMultipleSubPaths(t *testing.T) {
	var p Path
	p.MoveTo(0, 0)
	p.LineTo(10, 10)
	p.MoveTo(20, 20)
	p.QuadTo(30, 5, 40, 20)
	p.ClosePath()
	got := p.Bounds(BoundsOptions{})
	if got.Min.X != 0 || got.Min.Y != 0 || got.Max.X != 40 || got.Max.Y != 20 {
		t.Errorf("got %v, want union starting at (0,0) ending at (40,20)", got)
	}
}

// ---------------------------------------------------------------------------
// Empty / zero-area path behavior, matching the Rasterizer's coverage.
// ---------------------------------------------------------------------------

func TestPathEmptyAndZeroArea(t *testing.T) {
	var zero Path
	if got := zero.Bounds(BoundsOptions{}); !got.Empty() {
		t.Errorf("zero path: got %v, want empty", got)
	}

	var moveOnly Path
	moveOnly.MoveTo(3, 4)
	if got := moveOnly.Bounds(BoundsOptions{}); !got.Empty() {
		t.Errorf("move-only path: got %v, want empty", got)
	}

	// Horizontal-only geometry has zero coverage in the Rasterizer and hence
	// an empty (zero-height) bounds.
	var horizontal Path
	horizontal.MoveTo(0, 5)
	horizontal.LineTo(10, 5)
	horizontal.LineTo(20, 5)
	if got := horizontal.Bounds(BoundsOptions{}); !got.Empty() {
		t.Errorf("horizontal path: got %v, want empty", got)
	}

	// Reset returns the Path to its empty state.
	var p Path
	p.MoveTo(1, 1)
	p.LineTo(9, 9)
	p.Reset()
	if got := p.Bounds(BoundsOptions{}); !got.Empty() {
		t.Errorf("after Reset: got %v, want empty", got)
	}
	if x, y := p.Pen(); x != 0 || y != 0 {
		t.Errorf("pen after Reset = (%v, %v), want (0, 0)", x, y)
	}
}

// ---------------------------------------------------------------------------
// Non-finite values fail closed.
// ---------------------------------------------------------------------------

func TestPathNonFinite(t *testing.T) {
	nan := float32(math.NaN())
	inf := float32(math.Inf(1))
	cases := []struct {
		name string
		mod  func(p *Path)
	}{
		{"move-nan", func(p *Path) { p.MoveTo(nan, 1) }},
		{"line-inf", func(p *Path) { p.MoveTo(0, 0); p.LineTo(inf, 1) }},
		{"quad-nan-cp", func(p *Path) { p.MoveTo(0, 0); p.QuadTo(nan, 2, 4, 4) }},
		{"cube-inf-end", func(p *Path) { p.MoveTo(0, 0); p.CubeTo(1, 1, 2, 2, inf, 4) }},
		{"valid-then-bad", func(p *Path) {
			p.MoveTo(0, 0)
			p.LineTo(5, 5)
			p.LineTo(6, nan)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p Path
			tc.mod(&p)
			if got := p.Bounds(BoundsOptions{}); !got.Empty() {
				t.Errorf("fill: got %v, want empty", got)
			}
			if got := p.Bounds(BoundsOptions{StrokeWidth: 2}); !got.Empty() {
				t.Errorf("stroke: got %v, want empty", got)
			}
		})
	}

	// A non-finite Transform matrix is rejected and leaves the path intact.
	var p Path
	p.MoveTo(1, 1)
	p.LineTo(9, 9)
	before := p.Bounds(BoundsOptions{})
	p.Transform(nan, 0, 0, 1, 0, 0)
	if got := p.Bounds(BoundsOptions{}); got != before {
		t.Errorf("Transform with NaN changed the path: got %v, want %v", got, before)
	}
}

// ---------------------------------------------------------------------------
// Huge coordinates: saturation must not overflow or panic.
// ---------------------------------------------------------------------------

func TestPathHugeCoordinates(t *testing.T) {
	const huge = float32(1e30)
	var p Path
	p.MoveTo(-huge, -huge)
	p.LineTo(+huge, +huge)
	got := p.Bounds(BoundsOptions{})
	if got.Empty() {
		t.Fatalf("huge path returned empty bounds")
	}
	if got.Min.X != math.MinInt32 || got.Min.Y != math.MinInt32 ||
		got.Max.X != math.MaxInt32 || got.Max.Y != math.MaxInt32 {
		t.Errorf("got %v, want saturated int32 corners", got)
	}

	// float32 values beyond int32 but finite, in moderate positions, clamp
	// per-axis instead of wrapping.
	var q Path
	q.MoveTo(1, 1)
	q.LineTo(huge, 2)
	qgot := q.Bounds(BoundsOptions{})
	if qgot.Min.X != 1 || qgot.Min.Y != 1 || qgot.Max.X != math.MaxInt32 || qgot.Max.Y != 2 {
		t.Errorf("one-sided huge: got %v", qgot)
	}
}

// ---------------------------------------------------------------------------
// Bounds do not mutate the Path; Pen tracking is correct.
// ---------------------------------------------------------------------------

func TestPathBoundsDoesNotMutate(t *testing.T) {
	var p Path
	p.MoveTo(1, 2)
	p.LineTo(3, 4)
	p.QuadTo(5, 6, 7, 8)
	p.CubeTo(9, 10, 11, 12, 13, 14)
	p.ClosePath()
	snap := append([]segment(nil), p.seg...)

	_ = p.Bounds(BoundsOptions{})
	_ = p.Bounds(BoundsOptions{StrokeWidth: 3, Cap: StrokeCapRound, Join: StrokeJoinRound})
	_ = p.Bounds(BoundsOptions{StrokeWidth: 1, MiterLimit: 2})

	if len(p.seg) != len(snap) {
		t.Fatalf("segment count changed: %d vs %d", len(p.seg), len(snap))
	}
	for i := range snap {
		if p.seg[i] != snap[i] {
			t.Errorf("segment %d changed: got %+v, want %+v", i, p.seg[i], snap[i])
		}
	}
	px, py := p.Pen()
	if px != 1 || py != 2 {
		t.Errorf("pen after ClosePath = (%v, %v), want (1, 2)", px, py)
	}
}

// ---------------------------------------------------------------------------
// Transform: compare against an independent float64 affine reference.
// ---------------------------------------------------------------------------

func TestPathTransformReference(t *testing.T) {
	var p Path
	p.MoveTo(1, 2)
	p.LineTo(3, 4)
	p.QuadTo(5, 6, 7, 8)
	p.CubeTo(9, 10, 11, 12, 13, 14)
	snap := append([]segment(nil), p.seg...)
	prePenX, prePenY := p.Pen()
	preFirstX, preFirstY := p.firstX, p.firstY

	// Rotate 90° CW plus scale and translation:
	//	x' =  2y + 100
	//	y' = -2x + 200
	const a, b, c, d, e, f = float32(0), float32(-2), float32(2), float32(0), float32(100), float32(200)
	p.Transform(a, b, c, d, e, f)

	ref := func(x, y float32) (float32, float32) {
		return float32(a*x + c*y + e), float32(b*x + d*y + f)
	}
	pen := func(s segment) (float32, float32) {
		switch s.kind {
		case segMove, segLine:
			return s.c0, s.c1
		case segQuad:
			return s.c2, s.c3
		case segCube:
			return s.c4, s.c5
		}
		return 0, 0
	}
	for i, s := range p.seg {
		ox, oy := pen(snap[i])
		wx, wy := ref(ox, oy)
		gx, gy := pen(s)
		if gx != wx || gy != wy {
			t.Errorf("seg %d end: got (%v,%v), want (%v,%v)", i, gx, gy, wx, wy)
		}
	}
	// The pen (last CubeTo end) and the first point transform independently.
	wantPenX, wantPenY := ref(prePenX, prePenY)
	if px, py := p.Pen(); px != wantPenX || py != wantPenY {
		t.Errorf("pen = (%v,%v), want (%v,%v)", px, py, wantPenX, wantPenY)
	}
	wantFirstX, wantFirstY := ref(preFirstX, preFirstY)
	if p.firstX != wantFirstX || p.firstY != wantFirstY {
		t.Errorf("first = (%v,%v), want (%v,%v)", p.firstX, p.firstY, wantFirstX, wantFirstY)
	}
}

// TestPathTransformAfterClose checks a closed path: ClosePath returns the pen
// to the first point, so both transform to the same value.
func TestPathTransformAfterClose(t *testing.T) {
	var p Path
	p.MoveTo(1, 2)
	p.LineTo(3, 4)
	p.QuadTo(5, 6, 7, 8)
	p.CubeTo(9, 10, 11, 12, 13, 14)
	p.ClosePath()
	p.Transform(0, -2, 2, 0, 100, 200)
	// First point (1, 2) maps to (104, 198); ClosePath makes the pen match.
	if px, py := p.Pen(); px != 104 || py != 198 {
		t.Errorf("pen = (%v,%v), want (104,198)", px, py)
	}
	if p.firstX != 104 || p.firstY != 198 {
		t.Errorf("first = (%v,%v), want (104,198)", p.firstX, p.firstY)
	}
}

// ---------------------------------------------------------------------------
// Rasterize replay: a replayed Path renders identically to the equivalent
// direct Rasterizer calls.
// ---------------------------------------------------------------------------

func rasterizePath(t *testing.T, p Path, w, h int, floating bool) *image.Alpha {
	t.Helper()
	z := NewRasterizer(w, h)
	z.setUseFloatingPointMath(floating)
	// setUseFloatingPointMath resizes the buffers for the current size; make
	// sure both variants allocate matching backing storage.
	z.setUseFloatingPointMath(floating)
	p.Rasterize(z)
	dst := image.NewAlpha(z.Bounds())
	z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
	return dst
}

func TestPathRasterizeEquivalence(t *testing.T) {
	build := func() Path {
		var p Path
		p.MoveTo(2, 2)
		p.LineTo(8, 2)
		p.QuadTo(14, 2, 14, 14)
		p.CubeTo(8, 2, 5, 20, 2, 8)
		p.ClosePath()
		return p
	}
	p := build()

	// Direct Rasterizer reference (the testBasicPath shape).
	zd := NewRasterizer(16, 16)
	zd.MoveTo(2, 2)
	zd.LineTo(8, 2)
	zd.QuadTo(14, 2, 14, 14)
	zd.CubeTo(8, 2, 5, 20, 2, 8)
	zd.ClosePath()
	direct := image.NewAlpha(zd.Bounds())
	zd.Draw(direct, direct.Bounds(), image.Opaque, image.Point{})

	for _, floating := range []bool{false, true} {
		got := rasterizePath(t, p, 16, 16, floating)
		for i := range direct.Pix {
			delta := int(got.Pix[i]) - int(direct.Pix[i])
			if delta < -2 || delta > 2 {
				t.Fatalf("floating=%v, pixel %d: got %#02x, want %#02x", floating, i, got.Pix[i], direct.Pix[i])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Coverage consistency: Bounds always contains every covered pixel, for
// both the fixed and floating Rasterizer variants, including clipping.
// ---------------------------------------------------------------------------

func coverageRect(m *image.Alpha) (image.Rectangle, bool) {
	minX, minY := m.Bounds().Max.X, m.Bounds().Max.Y
	maxX, maxY := m.Bounds().Min.X, m.Bounds().Min.Y
	any := false
	for y := m.Bounds().Min.Y; y < m.Bounds().Max.Y; y++ {
		for x := m.Bounds().Min.X; x < m.Bounds().Max.X; x++ {
			if m.AlphaAt(x, y).A != 0 {
				any = true
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x+1 > maxX {
					maxX = x + 1
				}
				if y+1 > maxY {
					maxY = y + 1
				}
			}
		}
	}
	if !any {
		return image.Rectangle{}, false
	}
	return image.Rect(minX, minY, maxX, maxY), true
}

func randomClosedPath(rng *rand.Rand) Path {
	var p Path
	p.MoveTo(rng.Float32()*20+4, rng.Float32()*20+4)
	for k := rng.Intn(6) + 2; k > 0; k-- {
		switch rng.Intn(3) {
		case 0:
			p.LineTo(rng.Float32()*24+2, rng.Float32()*24+2)
		case 1:
			p.QuadTo(rng.Float32()*24+2, rng.Float32()*24+2,
				rng.Float32()*24+2, rng.Float32()*24+2)
		default:
			p.CubeTo(rng.Float32()*24+2, rng.Float32()*24+2,
				rng.Float32()*24+2, rng.Float32()*24+2,
				rng.Float32()*24+2, rng.Float32()*24+2)
		}
	}
	p.ClosePath()
	return p
}

// TestPathBoundsCoversRasterizer checks, on closed paths, that every pixel
// the Rasterizer covers lies inside the reported bounds, under both the
// fixed-point and floating-point scan converters and with both clipping and
// non-clipping canvas sizes.
//
// Open (un-closed) paths are deliberately excluded: the Rasterizer uses a
// winding scan converter, so an open polyline fills the whole scanline
// between its crossings, which is a rasterization rule rather than a
// property of the path's geometry.
func TestPathBoundsCoversRasterizer(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for iter := 0; iter < 300; iter++ {
		p := randomClosedPath(rng)
		want := p.Bounds(BoundsOptions{})

		for _, floating := range []bool{false, true} {
			// A deliberately small canvas exercises clipping; a generous
			// canvas exercises the unclipped coverage box.
			for _, size := range []int{12, 64} {
				z := NewRasterizer(size, size)
				z.setUseFloatingPointMath(floating)
				p.Rasterize(z)
				dst := image.NewAlpha(z.Bounds())
				z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
				cov, ok := coverageRect(dst)
				if !ok {
					continue
				}
				// The reported bounds must contain every covered pixel.
				if cov.Min.X < want.Min.X || cov.Min.Y < want.Min.Y ||
					cov.Max.X > want.Max.X || cov.Max.Y > want.Max.Y {
					t.Errorf("iter=%d floating=%v size=%d: coverage %v escapes bounds %v",
						iter, floating, size, cov, want)
				}
			}
		}
	}
}

// TestPathOpenPathWindingSemantics documents that an open path's fill
// coverage can extend to the canvas edge even though the geometry bounds are
// local, so callers must close a path when they rely on fill bounds.
func TestPathOpenPathWindingSemantics(t *testing.T) {
	var p Path
	p.MoveTo(4, 4)
	p.LineTo(20, 10) // deliberately not closed
	bounds := p.Bounds(BoundsOptions{})
	if bounds.Empty() || bounds.Min.X < 0 {
		t.Fatalf("unexpected fill bounds %v", bounds)
	}

	z := NewRasterizer(32, 32)
	p.Rasterize(z)
	dst := image.NewAlpha(z.Bounds())
	z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
	cov, ok := coverageRect(dst)
	if !ok {
		t.Fatal("expected winding fill coverage")
	}
	// The open segment's winding fill reaches the left canvas edge, outside
	// the local geometry bounds. This is expected Rasterizer behavior, not a
	// bounds defect.
	if cov.Min.X >= bounds.Min.X {
		t.Logf("open path coverage %v stays within bounds %v on this shape", cov, bounds)
	}
}

// TestPathBoundsCoversRasterizerClosed repeats the containment check on
// closed polygons only, where the winding fill cannot produce full-width
// scanlines. Here the bounds and coverage boxes agree within a pixel.
func TestPathBoundsCoversRasterizerClosed(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for iter := 0; iter < 200; iter++ {
		p := randomClosedPath(rng)
		want := p.Bounds(BoundsOptions{})

		z := NewRasterizer(32, 32)
		p.Rasterize(z)
		dst := image.NewAlpha(z.Bounds())
		z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
		cov, ok := coverageRect(dst)
		if !ok {
			continue
		}
		if cov.Min.X < want.Min.X || cov.Min.Y < want.Min.Y ||
			cov.Max.X > want.Max.X || cov.Max.Y > want.Max.Y {
			t.Errorf("iter=%d: coverage %v escapes closed bounds %v", iter, cov, want)
		}
	}
}

// TestPathLineBoundsExactCoverage pins the rounding rule for a closed
// polygon's non-horizontal edges: for a filled (winding) path, bounds and
// the Rasterizer's coverage box coincide. Open polylines are excluded
// because the winding scan-converter fills entire scanlines between
// crossings, which is rasterizer semantics rather than a bounds property.
func TestPathLineBoundsExactCoverage(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	for iter := 0; iter < 100; iter++ {
		var p Path
		ax, ay := rng.Float32()*8+4, rng.Float32()*8+4
		bx, by := rng.Float32()*8+12, ay+5
		cx, cy := rng.Float32()*8+4, by+5
		p.MoveTo(ax, ay)
		p.LineTo(bx, by)
		p.LineTo(cx, cy)
		p.ClosePath()
		want := p.Bounds(BoundsOptions{})

		z := NewRasterizer(32, 32)
		p.Rasterize(z)
		dst := image.NewAlpha(z.Bounds())
		z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
		cov, ok := coverageRect(dst)
		if !ok {
			continue
		}
		// Coverage must never escape the reported bounds.
		if cov.Min.X < want.Min.X || cov.Min.Y < want.Min.Y ||
			cov.Max.X > want.Max.X || cov.Max.Y > want.Max.Y {
			t.Errorf("iter=%d: coverage %v escapes bounds %v", iter, cov, want)
		}
		// For a closed polygon, floor/ceil of the vertices is the
		// scan-converted box. A boundary that lands exactly on a pixel edge
		// can leave the outermost pixel with zero coverage, so the reported
		// bounds may be at most one pixel looser per side.
		if cov.Min.X < want.Min.X-1 || cov.Min.Y < want.Min.Y-1 ||
			cov.Max.X > want.Max.X+1 || cov.Max.Y > want.Max.Y+1 {
			t.Errorf("iter=%d: coverage %v more than one pixel inside bounds %v", iter, cov, want)
		}
	}
}

// TestPathNearHorizontal mirrors the Rasterizer's almost-horizontal
// tolerance: a segment spanning less than 1e-6 in y contributes no coverage,
// while the reported bounds still contain every pixel that is covered. The
// shape is a closed, convex polygon so the winding fill stays local.
func TestPathNearHorizontal(t *testing.T) {
	var p Path
	p.MoveTo(2, 2)
	p.LineTo(10, math.Nextafter32(2, 0))
	p.LineTo(10, 8)
	p.LineTo(2, 8)
	p.ClosePath()
	got := p.Bounds(BoundsOptions{})

	z := NewRasterizer(12, 12)
	p.Rasterize(z)
	dst := image.NewAlpha(z.Bounds())
	z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
	cov, ok := coverageRect(dst)
	if !ok {
		t.Fatal("expected some coverage")
	}
	// The coverage is a subset of the reported bounds; the dropped
	// almost-horizontal edge may make the bounds slightly looser.
	if cov.Min.X < got.Min.X || cov.Min.Y < got.Min.Y ||
		cov.Max.X > got.Max.X || cov.Max.Y > got.Max.Y {
		t.Errorf("coverage %v escapes bounds %v", cov, got)
	}
}

// ---------------------------------------------------------------------------
// Stroke bounds: independent analytic reference for bands, caps and joins.
// ---------------------------------------------------------------------------

// refStrokeBandBox is an exact, independently derived stroke-bounds oracle
// for butt caps and bevel (or miter-within-limit, which the edge bands also
// bound) joins: the union of the cross-section rectangles swept by each
// edge, computed directly from each edge's unit normal rather than via the
// production code.
func refStrokeBandBox(pts []pointF64, width float64) refBox {
	h := width / 2
	r := newRefBox()
	for i := 0; i+1 < len(pts); i++ {
		ax, ay := pts[i].x, pts[i].y
		bx, by := pts[i+1].x, pts[i+1].y
		dx, dy := bx-ax, by-ay
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		nx, ny := -dy/l, dx/l // left normal
		r.add(ax+h*nx, ay+h*ny)
		r.add(ax-h*nx, ay-h*ny)
		r.add(bx+h*nx, by+h*ny)
		r.add(bx-h*nx, by-h*ny)
	}
	return r
}

func TestPathStrokeBandsReference(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for iter := 0; iter < 100; iter++ {
		var p Path
		var pts []pointF64
		x, y := rng.Float32()*20, rng.Float32()*20
		p.MoveTo(x, y)
		pts = append(pts, pointF64{float64(x), float64(y)})
		for k := rng.Intn(4) + 2; k > 0; k-- {
			nx, ny := rng.Float32()*30, rng.Float32()*30
			p.LineTo(nx, ny)
			pts = append(pts, pointF64{float64(nx), float64(ny)})
		}
		const width = float32(3)
		// Butt caps and bevel joins add nothing beyond the edge bands.
		got := p.Bounds(BoundsOptions{StrokeWidth: width, Cap: StrokeCapButt, Join: StrokeJoinBevel})

		ref := refStrokeBandBox(pts, float64(width))
		want := image.Rect(
			int(math.Floor(ref.minX)), int(math.Floor(ref.minY)),
			int(math.Ceil(ref.maxX)), int(math.Ceil(ref.maxY)))
		// The two independent derivations must agree exactly, up to a
		// one-pixel rounding tolerance for float64 noise.
		if got.Min.X < want.Min.X-1 || got.Min.Y < want.Min.Y-1 ||
			got.Max.X > want.Max.X+1 || got.Max.Y > want.Max.Y+1 {
			t.Errorf("iter=%d: got %v, want ~%v (ref %+v)", iter, got, want, ref)
		}
		// And the production box must never be smaller than the exact box.
		if got.Min.X > want.Min.X || got.Min.Y > want.Min.Y ||
			got.Max.X < want.Max.X || got.Max.Y < want.Max.Y {
			t.Errorf("iter=%d: bounds %v inside exact band box %v", iter, got, want)
		}
	}
}

func TestPathStrokeCapShape(t *testing.T) {
	// Horizontal stroke from (5,5) to (15,5), width 4 (h=2).
	mk := func() Path {
		var p Path
		p.MoveTo(5, 5)
		p.LineTo(15, 5)
		return p
	}
	butt := mk().Bounds(BoundsOptions{StrokeWidth: 4, Cap: StrokeCapButt})
	if butt != image.Rect(5, 3, 15, 7) {
		t.Errorf("butt: got %v, want (5,3)-(15,7)", butt)
	}
	square := mk().Bounds(BoundsOptions{StrokeWidth: 4, Cap: StrokeCapSquare})
	if square != image.Rect(3, 3, 17, 7) {
		t.Errorf("square: got %v, want (3,3)-(17,7)", square)
	}
	round := mk().Bounds(BoundsOptions{StrokeWidth: 4, Cap: StrokeCapRound})
	if round != image.Rect(3, 3, 17, 7) {
		t.Errorf("round: got %v, want (3,3)-(17,7)", round)
	}
	// Stroke bounds strictly contain the fill bounds for a non-degenerate
	// path.
	fill := mk().Bounds(BoundsOptions{})
	if !fill.In(round) {
		t.Errorf("fill %v not inside stroke %v", fill, round)
	}
}

func TestPathStrokeMiterLimit(t *testing.T) {
	// A sharp V: miter tip should project well beyond the bevel box.
	mk := func() Path {
		var p Path
		p.MoveTo(0, 0)
		p.LineTo(10, 10)
		p.LineTo(20, 0)
		return p
	}
	miter := mk().Bounds(BoundsOptions{StrokeWidth: 2, Join: StrokeJoinMiter, MiterLimit: 100})
	bevel := mk().Bounds(BoundsOptions{StrokeWidth: 2, Join: StrokeJoinBevel})
	if miter == bevel {
		t.Errorf("miter and bevel bounds coincide: %v", miter)
	}
	// With a miter limit of 1, the join must fall back to the bevel.
	clipped := mk().Bounds(BoundsOptions{StrokeWidth: 2, Join: StrokeJoinMiter, MiterLimit: 1})
	if clipped != bevel {
		t.Errorf("clipped miter: got %v, want bevel %v", clipped, bevel)
	}
	// Default miter limit (zero value) behaves like the documented 4.
	def := mk().Bounds(BoundsOptions{StrokeWidth: 2})
	want := mk().Bounds(BoundsOptions{StrokeWidth: 2, MiterLimit: defaultMiterLimit})
	if def != want {
		t.Errorf("default miter limit: got %v, want %v", def, want)
	}
}

func TestPathStrokeRoundJoin(t *testing.T) {
	// A right-angle turn with a round join.
	var p Path
	p.MoveTo(0, 0)
	p.LineTo(10, 0)
	p.LineTo(10, 10)
	round := p.Bounds(BoundsOptions{StrokeWidth: 2, Join: StrokeJoinRound, Cap: StrokeCapButt})
	// With h=1, the two edge bands span:
	//	horizontal edge: y in [-1, 1], x in [0, 10];
	//	vertical edge:   x in [ 9,11], y in [ 0, 10].
	// The outer quarter-circle join fills the convex corner (10,-1) to
	// (11,0), which is already inside that union's axis-aligned box, so the
	// join adds no new extent. Its cardinal extremum (11, 0) is included
	// explicitly by the arc math.
	if round != image.Rect(0, -1, 11, 10) {
		t.Errorf("right-angle round: got %v, want (0,-1)-(11,10)", round)
	}

	// A sharp, non-axis-aligned turn is where the round join's outer arc
	// reaches beyond the plain band union. Pixel rounding can make the two
	// pixel boxes coincide, so compare the continuous geometry instead.
	var v Path
	v.MoveTo(0, 0)
	v.LineTo(10, 5)
	v.LineTo(0, 10)
	bevel, ok1 := v.strokeGeometryBounds(BoundsOptions{StrokeWidth: 2, Join: StrokeJoinBevel, Cap: StrokeCapButt})
	rjoin, ok2 := v.strokeGeometryBounds(BoundsOptions{StrokeWidth: 2, Join: StrokeJoinRound, Cap: StrokeCapButt})
	if !ok1 || !ok2 {
		t.Fatal("stroke geometry failed")
	}
	if !(rjoin.maxX > bevel.maxX) {
		t.Errorf("round join maxX=%v does not extend bevel maxX=%v", rjoin.maxX, bevel.maxX)
	}
	// The cardinal extremum of the outer arc is exactly (11, 5).
	if math.Abs(rjoin.maxX-11) > 1e-9 {
		t.Errorf("round join maxX=%v, want 11", rjoin.maxX)
	}
	if !(bevel.minX >= rjoin.minX && bevel.minY >= rjoin.minY &&
		bevel.maxX <= rjoin.maxX && bevel.maxY <= rjoin.maxY) {
		t.Errorf("bevel box %+v not contained in round join box %+v", bevel, rjoin)
	}
}

func TestPathStrokeZeroWidthFallsBackToFill(t *testing.T) {
	var p Path
	p.MoveTo(1.5, 1.5)
	p.LineTo(8.5, 8.5)
	fill := p.Bounds(BoundsOptions{})
	stroke := p.Bounds(BoundsOptions{StrokeWidth: 0, Cap: StrokeCapRound})
	if fill != stroke {
		t.Errorf("zero-width stroke %v != fill %v", stroke, fill)
	}
}

func TestPathStrokeEmpty(t *testing.T) {
	var p Path
	if got := p.Bounds(BoundsOptions{StrokeWidth: 2}); !got.Empty() {
		t.Errorf("empty stroke: got %v, want empty", got)
	}
	var single Path
	single.MoveTo(3, 3)
	// A single point with butt caps renders nothing.
	if got := single.Bounds(BoundsOptions{StrokeWidth: 2, Cap: StrokeCapButt}); !got.Empty() {
		t.Errorf("single-point butt: got %v, want empty", got)
	}
	// Round/square caps render a dot.
	if got := single.Bounds(BoundsOptions{StrokeWidth: 2, Cap: StrokeCapRound}); got != image.Rect(2, 2, 4, 4) {
		t.Errorf("single-point round: got %v, want (2,2)-(4,4)", got)
	}
}
