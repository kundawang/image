// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vector

// This file contains the path geometry core: a queryable Path type that can
// report the tight bounding box of its fills and strokes before any
// rasterization happens.
//
// The fill and the bounds computations share the same geometry core. In
// particular, the number of line segments that a Bézier curve is flattened
// into is computed by quadSegmentCount in both places, using the same
// flatness measure and the same float32 arithmetic, so the reported bounds
// are consistent with (that is, a superset of) the pixels the Rasterizer can
// cover. See raster_fixed.go and raster_floating.go.

import (
	"image"
	"math"
)

// Path segment kinds.
const (
	segMove uint8 = iota
	segLine
	segQuad
	segCube
	segClose
)

// segment is a single Path command. Unused coordinates are zero.
type segment struct {
	kind uint8
	c0   float32
	c1   float32
	c2   float32
	c3   float32
	c4   float32
	c5   float32
}

// StrokeCap describes the shape of the ends of an open stroked sub-path.
type StrokeCap uint8

const (
	// StrokeCapButt is the default: the stroke ends exactly at the end
	// point, with no projection beyond it.
	StrokeCapButt StrokeCap = iota

	// StrokeCapSquare projects the stroke beyond the end point by half of
	// the stroke width.
	StrokeCapSquare

	// StrokeCapRound projects the stroke beyond the end point with a
	// semicircle whose radius is half of the stroke width.
	StrokeCapRound
)

// StrokeJoin describes the shape of the join between two consecutive
// stroked segments.
type StrokeJoin uint8

const (
	// StrokeJoinMiter is the default: consecutive segments are joined by
	// extending their outer edges until they meet. If the miter would be
	// longer than the miter limit, StrokeJoinBevel is used instead.
	StrokeJoinMiter StrokeJoin = iota

	// StrokeJoinBevel joins consecutive segments with a straight edge
	// between the two outer corners.
	StrokeJoinBevel

	// StrokeJoinRound joins consecutive segments with a circular arc whose
	// radius is half of the stroke width.
	StrokeJoinRound
)

// defaultMiterLimit is used when BoundsOptions.MiterLimit is not positive,
// matching the SVG and Cairo conventions.
const defaultMiterLimit = 4

// BoundsOptions configures an optional stroke when computing a Path's
// bounds.
//
// The zero value computes the fill bounds: StrokeWidth is zero, so no
// stroke is applied.
type BoundsOptions struct {
	// StrokeWidth is the stroke width in pixels. If zero (or negative), the
	// fill bounds are computed instead of the stroke bounds.
	StrokeWidth float32

	// Cap selects the cap at the ends of open sub-paths. It is ignored for
	// fill bounds. The zero value is StrokeCapButt.
	Cap StrokeCap

	// Join selects the join between consecutive stroked segments. It is
	// ignored for fill bounds. The zero value is StrokeJoinMiter.
	Join StrokeJoin

	// MiterLimit is the maximum miter length, as a multiple of half the
	// stroke width. It only applies to StrokeJoinMiter joins. If zero (or
	// negative), the default miter limit of 4 is used.
	MiterLimit float32
}

// Path is a sequence of 2-D vector graphics sub-paths built from move, line,
// quadratic Bézier and cubic Bézier commands.
//
// Unlike Rasterizer, a Path only stores the geometry. It can report the
// tight bounding box that the path (or its stroke) covers before anything is
// rasterized, and it can be replayed onto one or more Rasterizers.
//
// The zero value is an empty Path, ready to use.
type Path struct {
	seg []segment

	penX   float32
	penY   float32
	firstX float32
	firstY float32
}

// Reset empties the path and returns the pen to (0, 0).
func (p *Path) Reset() {
	p.seg = p.seg[:0]
	p.penX, p.penY = 0, 0
	p.firstX, p.firstY = 0, 0
}

// Pen returns the location of the path-drawing pen: the last argument to the
// most recent XxxTo call.
func (p Path) Pen() (x, y float32) {
	return p.penX, p.penY
}

// MoveTo starts a new sub-path and moves the pen to (x, y).
func (p *Path) MoveTo(x, y float32) {
	p.seg = append(p.seg, segment{kind: segMove, c0: x, c1: y})
	p.firstX, p.firstY = x, y
	p.penX, p.penY = x, y
}

// LineTo adds a line segment from the pen to (x, y) and moves the pen to
// (x, y).
func (p *Path) LineTo(x, y float32) {
	p.seg = append(p.seg, segment{kind: segLine, c0: x, c1: y})
	p.penX, p.penY = x, y
}

// QuadTo adds a quadratic Bézier segment from the pen, via (cx, cy), to
// (x, y), and moves the pen to (x, y).
func (p *Path) QuadTo(cx, cy, x, y float32) {
	p.seg = append(p.seg, segment{
		kind: segQuad,
		c0:   cx, c1: cy,
		c2: x, c3: y,
	})
	p.penX, p.penY = x, y
}

// CubeTo adds a cubic Bézier segment from the pen, via (c1x, c1y) and
// (c2x, c2y), to (x, y), and moves the pen to (x, y).
func (p *Path) CubeTo(c1x, c1y, c2x, c2y, x, y float32) {
	p.seg = append(p.seg, segment{
		kind: segCube,
		c0:   c1x, c1: c1y,
		c2: c2x, c3: c2y,
		c4: x, c5: y,
	})
	p.penX, p.penY = x, y
}

// ClosePath closes the current sub-path by connecting the pen back to the
// sub-path's starting point.
func (p *Path) ClosePath() {
	p.seg = append(p.seg, segment{kind: segClose})
	p.penX, p.penY = p.firstX, p.firstY
}

// Transform applies the affine transformation
//
//	x' = Ax + Cy + E
//	y' = Bx + Dy + F
//
// to every point in the path, in place. If any of the six matrix elements is
// not finite, the path is left unchanged.
//
// The arithmetic is float32 (with FMA explicitly disabled by the explicit
// float32 conversions, matching the Rasterizer's bit-exactness conventions)
// so that transforming and then querying the bounds is deterministic across
// GOARCHes.
func (p *Path) Transform(a, b, c, d, e, f float32) {
	if !finiteF32(a) || !finiteF32(b) || !finiteF32(c) ||
		!finiteF32(d) || !finiteF32(e) || !finiteF32(f) {
		return
	}
	for i := range p.seg {
		s := &p.seg[i]
		switch s.kind {
		case segMove, segLine:
			x, y := s.c0, s.c1
			s.c0 = float32(a*x + c*y + e)
			s.c1 = float32(b*x + d*y + f)
		case segQuad:
			x0, y0 := s.c0, s.c1
			x1, y1 := s.c2, s.c3
			s.c0 = float32(a*x0 + c*y0 + e)
			s.c1 = float32(b*x0 + d*y0 + f)
			s.c2 = float32(a*x1 + c*y1 + e)
			s.c3 = float32(b*x1 + d*y1 + f)
		case segCube:
			x0, y0 := s.c0, s.c1
			x1, y1 := s.c2, s.c3
			x2, y2 := s.c4, s.c5
			s.c0 = float32(a*x0 + c*y0 + e)
			s.c1 = float32(b*x0 + d*y0 + f)
			s.c2 = float32(a*x1 + c*y1 + e)
			s.c3 = float32(b*x1 + d*y1 + f)
			s.c4 = float32(a*x2 + c*y2 + e)
			s.c5 = float32(b*x2 + d*y2 + f)
		}
	}
	penX, penY := p.penX, p.penY
	firstX, firstY := p.firstX, p.firstY
	// Both components are computed from the saved pre-transform
	// coordinates. Assign each field on its own line so the result cannot
	// depend on the evaluation order of a multi-value assignment.
	p.penX = float32(a*penX + c*penY + e)
	p.penY = float32(b*penX + d*penY + f)
	p.firstX = float32(a*firstX + c*firstY + e)
	p.firstY = float32(b*firstX + d*firstY + f)
}

// Rasterize replays the path's commands onto z. It does not modify the path
// or reset z; callers typically call z.Reset first.
func (p Path) Rasterize(z *Rasterizer) {
	for _, s := range p.seg {
		switch s.kind {
		case segMove:
			z.MoveTo(s.c0, s.c1)
		case segLine:
			z.LineTo(s.c0, s.c1)
		case segQuad:
			z.QuadTo(s.c0, s.c1, s.c2, s.c3)
		case segCube:
			z.CubeTo(s.c0, s.c1, s.c2, s.c3, s.c4, s.c5)
		case segClose:
			z.ClosePath()
		}
	}
}

// Bounds returns the pixel-aligned bounding rectangle of the area that the
// path can cover, before rasterization.
//
// With the zero BoundsOptions, the fill bounds are returned: the tight
// bounding box of the path's geometry, expanded outwards (floor on the
// minimums and ceil on the maximums) to integer pixel boundaries. Passing a
// BoundsOptions with a positive StrokeWidth returns the bounds of the
// stroked path instead, taking the cap, join and miter limit into account.
//
// The returned rectangle is a superset of the pixels a Rasterizer can cover
// when replaying this path (clipping aside): every finite segment is
// flattened with the same flatness heuristic that the Rasterizer uses, and
// the analytic extrema of each Bézier curve are also considered, so the
// bounds are tight rather than a mere control-point box.
//
// An empty path, or a path containing a non-finite (NaN or Inf) coordinate,
// returns an empty rectangle. A zero-area path (for example a single point,
// or one made only of horizontal segments to which the Rasterizer itself
// gives no coverage) likewise returns an empty rectangle.
//
// Bounds takes a value receiver and does not modify the path.
func (p Path) Bounds(o BoundsOptions) image.Rectangle {
	b := newBoxF64()
	var ok bool
	if o.StrokeWidth > 0 {
		b, ok = p.strokeGeometryBounds(o)
	} else {
		b, ok = p.fillGeometryBounds()
	}
	if !ok || !b.valid() || b.empty() {
		return image.Rectangle{}
	}
	// A fill box with zero width or zero height gives the Rasterizer no
	// coverage and is reported as empty. A stroked box always has positive
	// area once initialized (the pen's cross-section has non-zero width), so
	// only the fill case applies the zero-area rule.
	if o.StrokeWidth <= 0 && (b.minX == b.maxX || b.minY == b.maxY) {
		return image.Rectangle{}
	}
	return b.pixelRect()
}

// boxF64 is an axis-aligned bounding box in continuous (float64)
// coordinates.
//
// The minX field doubles as the initialization flag: it is NaN for a box
// that has never seen a point, and it stays NaN if a non-finite point is
// included, so that callers fail closed instead of silently dropping
// geometry. A zero-valued boxF64 is therefore a valid empty box rather than
// a box anchored at the origin.
type boxF64 struct {
	minX, minY, maxX, maxY float64
	poisoned               bool
}

func newBoxF64() boxF64 {
	nan := math.NaN()
	return boxF64{minX: nan, minY: nan, maxX: nan, maxY: nan}
}

func (b *boxF64) include(x, y float64) {
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		b.poisoned = true
		return
	}
	if math.IsNaN(b.minX) {
		b.minX, b.maxX = x, x
		b.minY, b.maxY = y, y
		return
	}
	if x < b.minX {
		b.minX = x
	}
	if x > b.maxX {
		b.maxX = x
	}
	if y < b.minY {
		b.minY = y
	}
	if y > b.maxY {
		b.maxY = y
	}
}

// empty reports whether the box has never received a finite point.
func (b boxF64) empty() bool {
	return math.IsNaN(b.minX)
}

// valid reports whether every point included so far was finite.
func (b boxF64) valid() bool {
	return !b.poisoned
}

// pixelRect expands the continuous box outwards to the integer pixel
// boundaries, saturating to the int32 range instead of overflowing on
// extremely large coordinates.
func (b boxF64) pixelRect() image.Rectangle {
	return image.Rectangle{
		Min: image.Point{X: int(saturatingFloor(b.minX)), Y: int(saturatingFloor(b.minY))},
		Max: image.Point{X: int(saturatingCeil(b.maxX)), Y: int(saturatingCeil(b.maxY))},
	}
}

func saturatingFloor(x float64) int32 {
	x = math.Floor(x)
	switch {
	case x >= math.MaxInt32:
		return math.MaxInt32
	case x <= math.MinInt32:
		return math.MinInt32
	default:
		return int32(x)
	}
}

func saturatingCeil(x float64) int32 {
	x = math.Ceil(x)
	switch {
	case x >= math.MaxInt32:
		return math.MaxInt32
	case x <= math.MinInt32:
		return math.MinInt32
	default:
		return int32(x)
	}
}

func finiteF32(x float32) bool {
	v := float64(x)
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// fillGeometryBounds walks the path once, flattening every Bézier with the
// same heuristic as the Rasterizer and additionally including each curve's
// analytic extrema. The second result is false if any coordinate is not
// finite.
func (p Path) fillGeometryBounds() (boxF64, bool) {
	b := newBoxF64()
	var penX, penY, firstX, firstY float32
	havePen := false
	for _, s := range p.seg {
		switch s.kind {
		case segMove:
			if !finiteF32(s.c0) || !finiteF32(s.c1) {
				return b, false
			}
			penX, penY = s.c0, s.c1
			firstX, firstY = penX, penY
			havePen = true
			b.include(float64(penX), float64(penY))
		case segLine:
			if !havePen || !finiteF32(s.c0) || !finiteF32(s.c1) {
				return b, false
			}
			b.include(float64(penX), float64(penY))
			b.include(float64(s.c0), float64(s.c1))
			penX, penY = s.c0, s.c1
		case segQuad:
			if !havePen || !finiteF32(s.c0) || !finiteF32(s.c1) ||
				!finiteF32(s.c2) || !finiteF32(s.c3) {
				return b, false
			}
			// The flattened sample points use the exact same float32
			// arithmetic and segment count as Rasterizer.QuadTo.
			n := quadSegmentCount(devSquared(penX, penY, s.c0, s.c1, s.c2, s.c3))
			if n > 1 {
				t, nInv := float32(0), 1/float32(n)
				for i := 0; i < n-1; i++ {
					t += nInv
					abx, aby := lerp(t, penX, penY, s.c0, s.c1)
					bcx, bcy := lerp(t, s.c0, s.c1, s.c2, s.c3)
					qx, qy := lerp(t, abx, aby, bcx, bcy)
					b.include(float64(qx), float64(qy))
				}
			}
			// Analytic extrema tighten the box beyond the flattening.
			quadExtrema(&b,
				float64(penX), float64(penY),
				float64(s.c0), float64(s.c1),
				float64(s.c2), float64(s.c3))
			b.include(float64(s.c2), float64(s.c3))
			penX, penY = s.c2, s.c3
		case segCube:
			if !havePen || !finiteF32(s.c0) || !finiteF32(s.c1) ||
				!finiteF32(s.c2) || !finiteF32(s.c3) ||
				!finiteF32(s.c4) || !finiteF32(s.c5) {
				return b, false
			}
			devsq := devSquared(penX, penY, s.c0, s.c1, s.c4, s.c5)
			if devsqAlt := devSquared(penX, penY, s.c2, s.c3, s.c4, s.c5); devsq < devsqAlt {
				devsq = devsqAlt
			}
			n := quadSegmentCount(devsq)
			if n > 1 {
				t, nInv := float32(0), 1/float32(n)
				for i := 0; i < n-1; i++ {
					t += nInv
					abx, aby := lerp(t, penX, penY, s.c0, s.c1)
					bcx, bcy := lerp(t, s.c0, s.c1, s.c2, s.c3)
					cdx, cdy := lerp(t, s.c2, s.c3, s.c4, s.c5)
					abcx, abcy := lerp(t, abx, aby, bcx, bcy)
					bcdx, bcdy := lerp(t, bcx, bcy, cdx, cdy)
					cx, cy := lerp(t, abcx, abcy, bcdx, bcdy)
					b.include(float64(cx), float64(cy))
				}
			}
			cubeExtrema(&b,
				float64(penX), float64(penY),
				float64(s.c0), float64(s.c1),
				float64(s.c2), float64(s.c3),
				float64(s.c4), float64(s.c5))
			b.include(float64(s.c4), float64(s.c5))
			penX, penY = s.c4, s.c5
		case segClose:
			if havePen {
				b.include(float64(firstX), float64(firstY))
				penX, penY = firstX, firstY
			}
		}
		if !b.valid() {
			return b, false
		}
	}
	return b, true
}

// quadExtrema includes the stationary points (where a component's derivative
// is zero) of the quadratic Bézier from A to C via B.
func quadExtrema(b *boxF64, ax, ay, bx, by, cx, cy float64) {
	// B(t) = (1-t)²A + 2(1-t)tB + t²C; B'(t) = 2((1-t)(B-A) + t(C-B)).
	// A component is stationary at t = (A-B) / (A-2B+C).
	if d := ax - 2*bx + cx; d != 0 {
		if t := (ax - bx) / d; t > 0 && t < 1 {
			b.include(quadEval(ax, bx, cx, t), quadEval(ay, by, cy, t))
		}
	}
	if d := ay - 2*by + cy; d != 0 {
		if t := (ay - by) / d; t > 0 && t < 1 {
			b.include(quadEval(ax, bx, cx, t), quadEval(ay, by, cy, t))
		}
	}
}

func quadEval(a, b, c, t float64) float64 {
	mt := 1 - t
	return mt*mt*a + 2*mt*t*b + t*t*c
}

// cubeExtrema includes the stationary points of each component of the cubic
// Bézier from A to D via B and C, evaluating the full curve point at each
// interior root.
func cubeExtrema(b *boxF64, ax, ay, bx, by, cx, cy, dx, dy float64) {
	for _, t := range cubeStationaryTs(ax, bx, cx, dx) {
		if t > 0 && t < 1 {
			b.include(cubeEval(ax, bx, cx, dx, t), cubeEval(ay, by, cy, dy, t))
		}
	}
	for _, t := range cubeStationaryTs(ay, by, cy, dy) {
		if t > 0 && t < 1 {
			b.include(cubeEval(ax, bx, cx, dx, t), cubeEval(ay, by, cy, dy, t))
		}
	}
}

// cubeStationaryTs returns the interior-relevant parameters at which one
// component of a cubic Bézier is stationary. Expanding the component
//
//	P(t) = (1-t)³a + 3(1-t)²t·b + 3(1-t)t²·c + t³d
//
// and differentiating gives, up to a factor of 3, the quadratic
//
//	αt² + βt + γ with
//		α = -a + 3b - 3c + d
//		β =  2a - 4b + 2c
//		γ = -a + b
func cubeStationaryTs(a, b, c, d float64) []float64 {
	alpha := -a + 3*b - 3*c + d
	beta := 2*a - 4*b + 2*c
	gamma := -a + b
	return quadraticRoots(alpha, beta, gamma)
}

func cubeEval(a, b, c, d, t float64) float64 {
	mt := 1 - t
	return mt*mt*mt*a + 3*mt*mt*t*b + 3*mt*t*t*c + t*t*t*d
}

// quadraticRoots returns the real roots of αt² + βt + γ = 0.
func quadraticRoots(alpha, beta, gamma float64) []float64 {
	const eps = 1e-12
	if math.Abs(alpha) <= eps*(1+math.Abs(beta)+math.Abs(gamma)) {
		if beta == 0 {
			return nil
		}
		return []float64{-gamma / beta}
	}
	disc := beta*beta - 4*alpha*gamma
	if disc < 0 {
		return nil
	}
	if disc == 0 {
		return []float64{-beta / (2 * alpha)}
	}
	// Numerically stable form: pull the sign out of the -β term.
	s := math.Sqrt(disc)
	q := -0.5 * (beta + math.Copysign(s, beta))
	return []float64{q / alpha, gamma / q}
}

// strokeGeometryBounds computes the continuous bounds of the stroked path.
func (p Path) strokeGeometryBounds(o BoundsOptions) (boxF64, bool) {
	b := newBoxF64()
	h := float64(o.StrokeWidth) / 2
	if h <= 0 {
		return b, true
	}
	limit := float64(o.MiterLimit)
	if limit <= 0 {
		limit = defaultMiterLimit
	}

	var pts []pointF64
	closed := false
	flush := func() bool {
		if len(pts) == 0 {
			return true
		}
		if !strokeSubPathBounds(&b, pts, closed, h, o.Cap, o.Join, limit) {
			return false
		}
		pts = pts[:0]
		return true
	}

	var penX, penY, firstX, firstY float32
	havePen := false
	for _, s := range p.seg {
		switch s.kind {
		case segMove:
			if !finiteF32(s.c0) || !finiteF32(s.c1) {
				return b, false
			}
			if !flush() {
				return b, false
			}
			penX, penY = s.c0, s.c1
			firstX, firstY = penX, penY
			havePen = true
			closed = false
			pts = append(pts, pointF64{float64(penX), float64(penY)})
		case segLine:
			if !havePen || !finiteF32(s.c0) || !finiteF32(s.c1) {
				return b, false
			}
			penX, penY = s.c0, s.c1
			pts = append(pts, pointF64{float64(penX), float64(penY)})
		case segQuad:
			if !havePen || !finiteF32(s.c0) || !finiteF32(s.c1) ||
				!finiteF32(s.c2) || !finiteF32(s.c3) {
				return b, false
			}
			n := quadSegmentCount(devSquared(penX, penY, s.c0, s.c1, s.c2, s.c3))
			if n > 1 {
				t, nInv := float32(0), 1/float32(n)
				for i := 0; i < n-1; i++ {
					t += nInv
					abx, aby := lerp(t, penX, penY, s.c0, s.c1)
					bcx, bcy := lerp(t, s.c0, s.c1, s.c2, s.c3)
					qx, qy := lerp(t, abx, aby, bcx, bcy)
					pts = append(pts, pointF64{float64(qx), float64(qy)})
				}
			}
			penX, penY = s.c2, s.c3
			pts = append(pts, pointF64{float64(penX), float64(penY)})
		case segCube:
			if !havePen || !finiteF32(s.c0) || !finiteF32(s.c1) ||
				!finiteF32(s.c2) || !finiteF32(s.c3) ||
				!finiteF32(s.c4) || !finiteF32(s.c5) {
				return b, false
			}
			devsq := devSquared(penX, penY, s.c0, s.c1, s.c4, s.c5)
			if devsqAlt := devSquared(penX, penY, s.c2, s.c3, s.c4, s.c5); devsq < devsqAlt {
				devsq = devsqAlt
			}
			n := quadSegmentCount(devsq)
			if n > 1 {
				t, nInv := float32(0), 1/float32(n)
				for i := 0; i < n-1; i++ {
					t += nInv
					abx, aby := lerp(t, penX, penY, s.c0, s.c1)
					bcx, bcy := lerp(t, s.c0, s.c1, s.c2, s.c3)
					cdx, cdy := lerp(t, s.c2, s.c3, s.c4, s.c5)
					abcx, abcy := lerp(t, abx, aby, bcx, bcy)
					bcdx, bcdy := lerp(t, bcx, bcy, cdx, cdy)
					cx, cy := lerp(t, abcx, abcy, bcdx, bcdy)
					pts = append(pts, pointF64{float64(cx), float64(cy)})
				}
			}
			penX, penY = s.c4, s.c5
			pts = append(pts, pointF64{float64(penX), float64(penY)})
		case segClose:
			if havePen {
				if penX != firstX || penY != firstY {
					penX, penY = firstX, firstY
					pts = append(pts, pointF64{float64(penX), float64(penY)})
				}
				closed = true
			}
		}
		if !b.valid() {
			return b, false
		}
	}
	if !flush() {
		return b, false
	}
	return b, b.valid()
}

type pointF64 struct{ x, y float64 }

type edgeF64 struct {
	ux, uy float64 // unit direction
	nx, ny float64 // left unit normal
}

// strokeSubPathBounds unions the stroke band of every non-degenerate edge,
// the joins between them and the end caps (for open sub-paths) into b. It
// returns false if a non-finite value is encountered.
func strokeSubPathBounds(b *boxF64, pts []pointF64, closed bool, h float64, cap StrokeCap, join StrokeJoin, limit float64) bool {
	// With fewer than two distinct points there is no edge. A closed
	// sub-path renders nothing; an open one renders a dot only with the
	// round or square cap.
	if len(pts) < 2 {
		if !closed && len(pts) == 1 {
			switch cap {
			case StrokeCapRound, StrokeCapSquare:
				v := pts[0]
				b.include(v.x-h, v.y-h)
				b.include(v.x+h, v.y+h)
			}
		}
		return true
	}

	var edges []edgeF64
	edgeStart := make([]int, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		dx := pts[i].x - pts[i-1].x
		dy := pts[i].y - pts[i-1].y
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		e := edgeF64{ux: dx / l, uy: dy / l, nx: -dy / l, ny: dx / l}
		edges = append(edges, e)
		edgeStart = append(edgeStart, i-1)
		// Edge band: rectangle swept by the pen's cross-section, spanning
		// the two offset copies of the endpoint.
		ax, ay := pts[i-1].x, pts[i-1].y
		bx, by := pts[i].x, pts[i].y
		b.include(ax+h*e.nx, ay+h*e.ny)
		b.include(ax-h*e.nx, ay-h*e.ny)
		b.include(bx+h*e.nx, by+h*e.ny)
		b.include(bx-h*e.nx, by-h*e.ny)
	}
	if len(edges) == 0 {
		// All edges were zero-length: same as a single-point open sub-path.
		return strokeSubPathBounds(b, pts[:1], closed, h, cap, join, limit)
	}

	joinAt := func(v pointF64, e1, e2 edgeF64) {
		switch join {
		case StrokeJoinBevel:
			// The band union already covers the bevel triangle.
		case StrokeJoinRound:
			roundJoinBounds(b, v, h, e1, e2)
		default: // StrokeJoinMiter
			miterJoinBounds(b, v, h, limit, e1, e2)
		}
	}

	// Interior vertices: vertex k is the start point of edge k.
	for k := 1; k < len(edges); k++ {
		joinAt(pts[edgeStart[k]], edges[k-1], edges[k])
	}
	if closed {
		// The closing vertex is the first point; edges wrap around.
		joinAt(pts[0], edges[len(edges)-1], edges[0])
	} else {
		// Caps project from the first/last surviving edge's endpoint.
		first, last := edges[0], edges[len(edges)-1]
		capBounds(b, pts[edgeStart[0]], h, first, cap, true)
		capBounds(b, pts[edgeStart[len(edges)-1]+1], h, last, cap, false)
	}
	return b.valid()
}

// miterJoinBounds intersects the two offset edge lines and, if the miter is
// within the limit, includes both miter tips. A too-long (or
// near-parallel/opposite) miter falls back to the bevel behavior, which the
// edge bands already cover.
func miterJoinBounds(b *boxF64, v pointF64, h, limit float64, e1, e2 edgeF64) {
	p1x, p1y := v.x+h*e1.nx, v.y+h*e1.ny
	p2x, p2y := v.x+h*e2.nx, v.y+h*e2.ny
	cross := e1.ux*e2.uy - e1.uy*e2.ux
	if math.Abs(cross) < 1e-9 {
		return // Bevel fallback; bands cover it.
	}
	// Solve P1 + t·u1 = P2 + s·u2 for t.
	dpx, dpy := p2x-p1x, p2y-p1y
	t := (dpx*e2.uy - dpy*e2.ux) / cross
	mx, my := p1x+t*e1.ux, p1y+t*e1.uy
	if math.Hypot(mx-v.x, my-v.y) > h*limit {
		return // Bevel fallback.
	}
	b.include(mx, my)
	b.include(2*v.x-mx, 2*v.y-my)
}

// roundJoinBounds includes the cardinal-direction extremal points of the
// outer circular arc of a round join, when those angles lie in the swept
// sector.
func roundJoinBounds(b *boxF64, v pointF64, h float64, e1, e2 edgeF64) {
	cross := e1.ux*e2.uy - e1.uy*e2.ux
	dot := e1.ux*e2.ux + e1.uy*e2.uy
	alpha := math.Atan2(math.Abs(cross), dot) // turn angle, in [0, π].
	if alpha == 0 {
		return
	}
	var lo, hi float64
	if cross > 0 {
		// Counter-clockwise turn: the outer side is the right normal, -n.
		base := math.Atan2(-e1.ny, -e1.nx)
		lo, hi = base, base+alpha
	} else {
		// Clockwise turn: outer side is the left normal, n.
		base := math.Atan2(e1.ny, e1.nx)
		lo, hi = base-alpha, base
	}
	includeArcCardinals(b, v, h, lo, hi)
}

// capBounds adds the projection of an open sub-path's end cap. start
// identifies the sub-path's first point (backward projection); false its
// last point (forward projection).
func capBounds(b *boxF64, p pointF64, h float64, e edgeF64, cap StrokeCap, start bool) {
	switch cap {
	case StrokeCapButt:
		// Nothing beyond the edge band's end cross-section.
	case StrokeCapSquare:
		sign := float64(1)
		if start {
			sign = -1
		}
		b.include(p.x+sign*h*e.ux+h*e.nx, p.y+sign*h*e.uy+h*e.ny)
		b.include(p.x+sign*h*e.ux-h*e.nx, p.y+sign*h*e.uy-h*e.ny)
	case StrokeCapRound:
		base := math.Atan2(e.ny, e.nx)
		if start {
			// Backward semicircle: n to -n through -u.
			includeArcCardinals(b, p, h, base, base+math.Pi)
		} else {
			// Forward semicircle: -n to n through +u.
			includeArcCardinals(b, p, h, base-math.Pi, base)
		}
	}
}

// cardinalAngles are the angles of the four axis-aligned extremal points of
// a circle.
var cardinalAngles = [4]float64{0, math.Pi / 2, math.Pi, -math.Pi / 2}

// includeArcCardinals includes the two arc endpoints and any of the four
// axis-aligned extremal points of the circle centered at v with radius h
// whose angles lie in [lo, hi], an interval no longer than π.
func includeArcCardinals(b *boxF64, v pointF64, h, lo, hi float64) {
	// The arc endpoints are extrema of the sector even when they are not
	// axis-aligned.
	b.include(v.x+h*math.Cos(lo), v.y+h*math.Sin(lo))
	b.include(v.x+h*math.Cos(hi), v.y+h*math.Sin(hi))
	for _, theta := range cardinalAngles {
		t := theta
		for t < lo {
			t += 2 * math.Pi
		}
		for t > hi {
			t -= 2 * math.Pi
		}
		if t >= lo && t <= hi {
			b.include(v.x+h*math.Cos(theta), v.y+h*math.Sin(theta))
		}
	}
}
