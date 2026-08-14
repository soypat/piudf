package piupage

import (
	"io"

	"github.com/soypat/piudf/piulex"
)

// FillRule decides which regions of a path count as inside it when the path's
// subpaths overlap.
type FillRule uint8

const (
	// NonZero is PDF's default rule: a point is inside when the winding number
	// of the path around it is not zero.
	NonZero FillRule = iota
	// EvenOdd counts crossings instead, so overlapping subpaths punch holes.
	EvenOdd
)

// Path is a sequence of subpaths.
//
// Coordinates are in the canvas's current user space at the time it is painted,
// so a path drawn about the origin can be placed with [Canvas.Translate].
type Path struct {
	// ops holds one PDF operator byte per segment and pts the coordinates they
	// consume, so that recording a segment costs no more than a few appends.
	ops []byte
	pts []float64
	// cur is the current point and start the current subpath's first point,
	// which QuadTo and Close respectively need.
	curX, curY     float64
	startX, startY float64
	rule           FillRule
	bounded        bool
	err            error
}

// Reset empties the path and binds it to caller-owned buffers. ops holds one
// byte per segment and pts the coordinates, a move or line spends two, a cubic
// six, a rectangle four and a close none.
func (p *Path) Reset(ops []byte, pts []float64) {
	*p = Path{ops: ops[:0], pts: pts[:0], bounded: true}
}

// Clear empties the path, keeping its buffers and its fill rule.
func (p *Path) Clear() {
	p.ops = p.ops[:0]
	p.pts = p.pts[:0]
	p.curX, p.curY = 0, 0
	p.startX, p.startY = 0, 0
	p.err = nil
}

// Err reports the first overflow the path hit, or nil. Errors are sticky.
func (p *Path) Err() error { return p.err }

// IsEmpty reports whether the path has no segments.
func (p *Path) IsEmpty() bool { return len(p.ops) == 0 }

// Rule returns the path's fill rule, [NonZero] by default.
func (p *Path) Rule() FillRule { return p.rule }

// SetRule sets the rule deciding which regions the path encloses. It belongs to
// the path rather than to the paint call because it describes how the path's
// own subpaths combine.
func (p *Path) SetRule(r FillRule) { p.rule = r }

// MoveTo begins a new subpath at (x, y).
func (p *Path) MoveTo(x, y float64) {
	p.push('m', 2, [6]float64{x, y})
	p.curX, p.curY = x, y
	p.startX, p.startY = x, y
}

// LineTo extends the current subpath with a straight segment to (x, y).
func (p *Path) LineTo(x, y float64) {
	p.push('l', 2, [6]float64{x, y})
	p.curX, p.curY = x, y
}

// CubicTo extends the current subpath with a cubic bézier curve to (x3, y3),
// bending towards control points (x1, y1) and (x2, y2).
func (p *Path) CubicTo(x1, y1, x2, y2, x3, y3 float64) {
	p.push('c', 6, [6]float64{x1, y1, x2, y2, x3, y3})
	p.curX, p.curY = x3, y3
}

// QuadTo extends the current subpath with a quadratic bézier curve to (x2, y2)
// bending towards (x1, y1). PDF has no quadratic operator, so the curve is
// degree-elevated to the cubic describing the identical shape.
func (p *Path) QuadTo(x1, y1, x2, y2 float64) {
	c1x, c1y := p.curX+2.0/3.0*(x1-p.curX), p.curY+2.0/3.0*(y1-p.curY)
	c2x, c2y := x2+2.0/3.0*(x1-x2), y2+2.0/3.0*(y1-y2)
	p.CubicTo(c1x, c1y, c2x, c2y, x2, y2)
}

// Rect adds a closed rectangular subpath with its corner at (x, y). A negative
// w or h extends the rectangle the other way, as PDF's re operator does.
func (p *Path) Rect(x, y, w, h float64) {
	p.push('r', 4, [6]float64{x, y, w, h})
	p.curX, p.curY = x, y
	p.startX, p.startY = x, y
}

// Close closes the current subpath with a straight segment back to its first
// point. Closing matters to a stroke, which joins a closed subpath's ends
// instead of capping them.
func (p *Path) Close() {
	p.push('h', 0, [6]float64{})
	p.curX, p.curY = p.startX, p.startY
}

// Bounds reports a box containing the path.
func (p *Path) Bounds() (x0, y0, x1, y1 float64) {
	var pts [6]float64
	first := true
	for i, n := 0, 0; i < len(p.ops); i++ {
		np := 0
		switch p.ops[i] {
		case 'm', 'l':
			pts[0], pts[1] = p.pts[n], p.pts[n+1]
			np, n = 1, n+2
		case 'c':
			copy(pts[:6], p.pts[n:n+6])
			np, n = 3, n+6
		case 'r':
			x, y, w, h := p.pts[n], p.pts[n+1], p.pts[n+2], p.pts[n+3]
			pts[0], pts[1], pts[2], pts[3] = x, y, x+w, y+h
			np, n = 2, n+4
		}
		for j := range np {
			x, y := pts[j*2], pts[j*2+1]
			if first {
				x0, y0, x1, y1 = x, y, x, y
				first = false
				continue
			}
			x0, y0 = min(x0, x), min(y0, y)
			x1, y1 = max(x1, x), max(y1, y)
		}
	}
	return x0, y0, x1, y1
}

// push records one segment, latching an overflow when the path is bound to
// caller-owned buffers that cannot hold it.
func (p *Path) push(op byte, n int, v [6]float64) {
	if p.err != nil {
		return
	}
	if p.bounded && (len(p.ops) == cap(p.ops) || len(p.pts)+n > cap(p.pts)) {
		p.err = io.ErrShortBuffer
		return
	}
	p.ops = append(p.ops, op)
	p.pts = append(p.pts, v[:n]...)
}

// emit writes the path's construction operators. The caller follows them with a
// painting operator, which is what ends a path in a content stream.
func (p *Path) emit(e *piulex.Emitter) {
	for i, n := 0, 0; i < len(p.ops); i++ {
		// The operator names are spelled out rather than converted from the
		// opcode byte, which would allocate a string per segment.
		var name string
		takes := 0
		switch p.ops[i] {
		case 'm':
			name, takes = "m", 2
		case 'l':
			name, takes = "l", 2
		case 'c':
			name, takes = "c", 6
		case 'r':
			name, takes = "re", 4
		case 'h':
			name = "h"
		default:
			continue
		}
		for _, v := range p.pts[n : n+takes] {
			e.Real(v)
		}
		n += takes
		e.Ident(name)
	}
}
