package piupage

import (
	"errors"
	"image/color"
	"math"
)

var (
	errGDepth     = errors.New("piupage: graphics state nested too deeply")
	errUnclosed   = errors.New("piupage: content stream ends with an unrestored Save")
	errImageType  = errors.New("piupage: image is neither an image.Image nor a *JPEG")
	errImageEmpty = errors.New("piupage: image has no pixels")
	errJPEG       = errors.New("piupage: not a baseline or progressive 8-bit JPEG with 1, 3 or 4 components")

	errImageNotComparable = errors.New("piupage: image cannot be compared with ==; pass a pointer")
	errPlaceSelf          = errors.New("piupage: canvas placed on itself")
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
	c.extend(p.Bounds())
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
	c.extendStroke(p, pen)
}

// extendStroke grows the extent by p traced with pen. A miter join reaches up
// to the miter limit half-widths past its corner; caps and other joins reach
// at most √2, a square cap's corner.
func (c *Canvas) extendStroke(p *Path, pen Pen) {
	reach := math.Sqrt2
	if pen.Join == MiterJoin {
		miter := pen.Miter
		if miter <= 0 {
			miter = 10 // PDF's default; see Pen.
		}
		reach = max(reach, miter)
	}
	x0, y0, x1, y1 := p.Bounds()
	c.extendPad(x0, y0, x1, y1, strokePad(pen.Width, reach))
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
	c.extendStroke(p, pen) // The stroke covers the fill.
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
