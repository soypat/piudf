package piupage

import (
	"bytes"
	"image/color"
	"strconv"

	"github.com/soypat/piudf/piulex"
)

// Canvas is an imperative drawing surface for one page. It writes PDF
// content-stream operators into an in-memory buffer through a piulex.Emitter
// and records which fonts it referenced so the page's /Resources can be built.
// PDF space has its origin at the bottom-left with y increasing upward.
//
// Every length a Canvas is given — coordinates, widths, font sizes — is in the
// [Unit] it was built with, and is converted to points once, at emission. A
// caller therefore works in whatever unit suits the drawing and never converts
// anything itself.
type Canvas struct {
	emit piulex.Emitter
	buf  bytes.Buffer
	// unit is the length unit this canvas' caller works in.
	unit Unit
	// used lists the fonts referenced in first-use order, assigning /F1, /F2 …
	used []fontUse
	// Current text font/size, applied by Text.
	curFont Font
	curSize float64
	// links records the page's link areas. They are annotations, not content,
	// so the Canvas only collects them; the page writer emits them.
	links []Link
}

// Link is a rectangular area of a page that resolves to a URI when clicked.
// It is page furniture rather than drawn content: nothing about a Link changes
// what the page looks like.
//
// Its rectangle is in the canvas' unit, as the caller gave it: an annotation is
// not content, so unlike everything else a Canvas is told it does not pass
// through emission here, and whoever writes the annotation converts it.
type Link struct {
	X, Y, W, H float64
	URI        string
}

// fontUse binds a font to the resource name it was assigned on this page.
type fontUse struct {
	font Font
	name string // "F1", "F2", …
}

// NewCanvas returns a Canvas measured in u whose emitter uses scratch as its
// write-through window; scratch is caller-owned and must be at least
// piulex.MinEmitBuffer. A zero u means [Pt].
func NewCanvas(scratch []byte, u Unit) *Canvas {
	if u == 0 {
		u = Pt
	}
	c := &Canvas{unit: u}
	c.emit.Reset(&c.buf, scratch)
	return c
}

// Unit returns the length unit this canvas is measured in.
func (c *Canvas) Unit() Unit { return c.unit }

// Reset clears the content buffer and font set for reuse on a new page. The
// unit is a property of the surface rather than of what was drawn on it, so it
// survives.
func (c *Canvas) Reset(scratch []byte) {
	c.buf.Reset()
	c.used = c.used[:0]
	c.links = c.links[:0]
	c.curFont = nil
	c.curSize = 0
	c.emit.Reset(&c.buf, scratch)
}

// emitLength emits v, a length in the canvas' unit, as a content-stream real
// in points. It is deliberately not named after the token it writes: an
// operand is either a length and goes through here, or is a pure number — a
// colour component, an opacity — and goes to c.emit.Real directly, and the two
// sit side by side in the operator sequences below.
func (c *Canvas) emitLength(v float64) {
	c.emit.Real(c.unit.EmitPt(v))
}

// SetFont selects f at the given size for subsequent Text calls, registering it
// in this page's resources. The size is a length like any other, so it is in
// the canvas' unit: a 9.5pt face on a millimetre canvas is MM.FromPt(9.5).
func (c *Canvas) SetFont(f Font, size float64) {
	c.curFont = f
	c.curSize = size
	c.ensure(f)
}

// ensure registers f and returns its resource name ("F1"…).
func (c *Canvas) ensure(f Font) string {
	for i := range c.used {
		if c.used[i].font == f || c.used[i].font.BaseName() == f.BaseName() {
			return c.used[i].name
		}
	}
	name := "F" + strconv.Itoa(len(c.used)+1)
	c.used = append(c.used, fontUse{font: f, name: name})
	return name
}

// Text draws s with its baseline origin at (x, y) in the current font and fill
// color col.
func (c *Canvas) Text(x, y float64, s string, col color.Color) {
	if c.curFont == nil {
		return
	}
	name := c.ensure(c.curFont)
	r, g, b := rgb(col)
	c.emit.Ident("BT")
	c.emit.Name(name)
	c.emitLength(c.curSize)
	c.emit.Ident("Tf")
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("rg")
	c.emitLength(x)
	c.emitLength(y)
	c.emit.Ident("Td")
	if c.curFont.hexCodes() {
		c.emit.HexString(c.encode(s))
	} else {
		c.emit.String(c.encode(s))
	}
	c.emit.Ident("Tj")
	c.emit.Ident("ET")
	c.emit.EOL()
}

// TextRight draws s right-aligned so its rendered box ends at xRight.
func (c *Canvas) TextRight(xRight, y float64, s string, col color.Color) {
	if c.curFont == nil {
		return
	}
	c.Text(xRight-StringWidth(c.curFont, s, c.curSize), y, s, col)
}

// encode converts s to the current font's byte codes.
func (c *Canvas) encode(s string) []byte {
	dst := make([]byte, 0, len(s))
	for _, r := range s {
		dst = c.curFont.Encode(dst, r)
	}
	return dst
}

// Line strokes a segment from (x0,y0) to (x1,y1) with width w and color col.
func (c *Canvas) Line(x0, y0, x1, y1, w float64, col color.Color) {
	r, g, b := rgb(col)
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("RG")
	c.emitLength(w)
	c.emit.Ident("w")
	c.emitLength(x0)
	c.emitLength(y0)
	c.emit.Ident("m")
	c.emitLength(x1)
	c.emitLength(y1)
	c.emit.Ident("l")
	c.emit.Ident("S")
	c.emit.EOL()
}

// Link marks the rectangle at (x,y) of size w×h as a hyperlink to uri. An
// empty uri is ignored, which lets a caller pass through an unset link target
// without branching.
func (c *Canvas) Link(x, y, w, h float64, uri string) {
	if uri == "" {
		return
	}
	c.links = append(c.links, Link{X: x, Y: y, W: w, H: h, URI: uri})
}

// Links returns the page's link areas in the order they were added.
func (c *Canvas) Links() []Link { return c.links }

// StrokeRect strokes the outline of the rectangle at (x,y) of size w×h.
func (c *Canvas) StrokeRect(x, y, w, h, lineWidth float64, col color.Color) {
	r, g, b := rgb(col)
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("RG")
	c.emitLength(lineWidth)
	c.emit.Ident("w")
	c.emitLength(x)
	c.emitLength(y)
	c.emitLength(w)
	c.emitLength(h)
	c.emit.Ident("re")
	c.emit.Ident("S")
	c.emit.EOL()
}

// FillRect fills the rectangle at (x,y) of size w×h with col.
func (c *Canvas) FillRect(x, y, w, h float64, col color.Color) {
	r, g, b := rgb(col)
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("rg")
	c.emitLength(x)
	c.emitLength(y)
	c.emitLength(w)
	c.emitLength(h)
	c.emit.Ident("re")
	c.emit.Ident("f")
	c.emit.EOL()
}

// Bytes flushes and returns the content-stream bytes emitted so far.
func (c *Canvas) Bytes() []byte {
	c.emit.Flush()
	return c.buf.Bytes()
}

// Fonts returns the fonts referenced on this page, in first-use order.
func (c *Canvas) Fonts() []Font {
	fs := make([]Font, len(c.used))
	for i := range c.used {
		fs[i] = c.used[i].font
	}
	return fs
}

// ResourceName returns the /Resources font name ("F1"…) assigned to f, or ""
// if f was not used on this page.
func (c *Canvas) ResourceName(f Font) string {
	for i := range c.used {
		if c.used[i].font == f || c.used[i].font.BaseName() == f.BaseName() {
			return c.used[i].name
		}
	}
	return ""
}

// Err reports the first emission error, if any.
func (c *Canvas) Err() error { return c.emit.Err() }
