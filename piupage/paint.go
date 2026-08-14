package piupage

import (
	"errors"
	"image/color"
)

var (
	errGDepth   = errors.New("piupage: graphics state nested too deeply")
	errUnclosed = errors.New("piupage: content stream ends with an unrestored Save")
)

// CapStyle is how a stroke ends an open subpath.
type CapStyle uint8

const (
	ButtCap   CapStyle = iota // squared off at the endpoint
	RoundCap                  // a half circle centred on the endpoint
	SquareCap                 // squared off half a width past the endpoint
)

// JoinStyle is how a stroke turns a corner.
type JoinStyle uint8

const (
	MiterJoin JoinStyle = iota // extended to a point, bevelled past Miter
	RoundJoin                  // an arc centred on the corner
	BevelJoin                  // the notch filled with a triangle
)

// Pen describes a stroke completely: a paint call carrying one needs nothing
// else to be reproducible.
//
// The zero Pen is a one point black hairline with butt caps and miter joins,
// which is PDF's own default state.
type Pen struct {
	Color color.Color
	Width float64
	Cap   CapStyle
	Join  JoinStyle
	// Miter is the ratio at which a miter join is bevelled instead. Zero means
	// PDF's default of 10.
	Miter float64
	// Dash alternates on and off lengths in user space; nil strokes solid. A
	// Dash of all zeros is invalid in PDF and is treated as solid.
	Dash  []float64
	Phase float64
}

// Fill paints the region enclosed by p with col, by the path's own fill rule.
func (c *Canvas) Fill(p *Path, col color.Color) {
	if p.IsEmpty() {
		return
	}
	r, g, b := rgb(col)
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("rg")
	p.emit(&c.emit)
	c.emit.Ident(fillOp(p.rule))
	c.emit.EOL()
}

// Stroke traces p with pen.
func (c *Canvas) Stroke(p *Path, pen Pen) {
	if p.IsEmpty() {
		return
	}
	c.setPen(pen)
	p.emit(&c.emit)
	c.emit.Ident("S")
	c.emit.EOL()
}

// FillStroke fills p with col and traces it with pen in one operation, so that
// the stroke straddles the filled edge exactly as PDF's B operator paints it.
func (c *Canvas) FillStroke(p *Path, col color.Color, pen Pen) {
	if p.IsEmpty() {
		return
	}
	r, g, b := rgb(col)
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("rg")
	c.setPen(pen)
	p.emit(&c.emit)
	if p.rule == EvenOdd {
		c.emit.Ident("B*")
	} else {
		c.emit.Ident("B")
	}
	c.emit.EOL()
}

// Clip intersects the clipping region with p, so that later painting shows only
// where the two overlap.
//
// A clip cannot be narrowed and then widened again.
func (c *Canvas) Clip(p *Path) {
	if p.IsEmpty() {
		return
	}
	p.emit(&c.emit)
	if p.rule == EvenOdd {
		c.emit.Ident("W*")
	} else {
		c.emit.Ident("W")
	}
	// n ends the path without painting it, leaving only the clip behind.
	c.emit.Ident("n")
	c.emit.EOL()
}

// setPen emits the stroke color and line parameters pen describes.
func (c *Canvas) setPen(pen Pen) {
	r, g, b := rgb(pen.Color)
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("RG")
	w := pen.Width
	if w <= 0 {
		w = 1
	}
	c.emit.Real(w)
	c.emit.Ident("w")
	if pen.Cap != ButtCap {
		c.emit.Int(int64(pen.Cap))
		c.emit.Ident("J")
	}
	if pen.Join != MiterJoin {
		c.emit.Int(int64(pen.Join))
		c.emit.Ident("j")
	}
	if pen.Miter > 0 {
		c.emit.Real(pen.Miter)
		c.emit.Ident("M")
	}
	c.emit.ArrayOpen()
	if dashed(pen.Dash) {
		for _, d := range pen.Dash {
			c.emit.Real(d)
		}
	}
	c.emit.ArrayClose()
	c.emit.Real(pen.Phase)
	c.emit.Ident("d")
}

// dashed reports whether d describes a real dash pattern. PDF rejects an
// all-zero array, which is the shape a half-filled Pen tends to carry.
func dashed(d []float64) bool {
	for _, v := range d {
		if v > 0 {
			return true
		}
	}
	return false
}

// fillOp is the painting operator for a fill under rule.
func fillOp(r FillRule) string {
	if r == EvenOdd {
		return "f*"
	}
	return "f"
}
