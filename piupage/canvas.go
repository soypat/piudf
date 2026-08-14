package piupage

import (
	"bytes"
	"image/color"

	"github.com/soypat/piudf/piulex"
)

// Canvas is an imperative drawing surface for one page. It writes PDF
// content-stream operators into an in-memory buffer through a piulex.Emitter
// and records which fonts it referenced so the page's /Resources can be built.
// PDF space has its origin at the bottom-left with y increasing upward.
type Canvas struct {
	emit piulex.Emitter
	buf  bytes.Buffer
	// used lists the fonts referenced in first-use order, assigning /F1, /F2 …
	used []Font
	// Current text font/size, applied by Text.
	curFont Font
	curSize float64
	// links records the page's link areas. They are annotations, not content,
	// so the Canvas only collects them; the page writer emits them.
	links []Link
	// Text encodes
	enc []byte
}

// Link is a undrawn rectangular area of a page that resolves to a URI when clicked.
type Link struct {
	X, Y, W, H float64
	URI        string
}

// Mark is a position in a Canvas's content stream, taken by [Canvas.Mark] and
// retracted to by [Canvas.Rewind]. A Mark is invalidated by [Canvas.Reset].
type Mark struct {
	n       int
	fonts   int
	links   int
	curFont Font
	curSize float64
}

// Mark records the canvas's current state so a later [Canvas.Rewind] can
// retract everything drawn after it.
func (c *Canvas) Mark() Mark {
	c.emit.Flush()
	return Mark{
		n: c.buf.Len(), fonts: len(c.used), links: len(c.links),
		curFont: c.curFont, curSize: c.curSize,
	}
}

// Rewind discards every operator, font reference, and link recorded since m was
// taken. Passing a Mark from a different canvas or one taken before a
// [Canvas.Reset] is a programming error and does nothing.
func (c *Canvas) Rewind(m Mark) {
	c.emit.Flush()
	if m.n > c.buf.Len() || m.fonts > len(c.used) || m.links > len(c.links) {
		return
	}
	c.buf.Truncate(m.n)
	c.used = c.used[:m.fonts]
	c.links = c.links[:m.links]
	c.curFont = m.curFont
	c.curSize = m.curSize
}

// Reset clears the content buffer and font set for reuse on a new page.
func (c *Canvas) Reset(scratch []byte) error {
	c.buf.Reset()
	c.used = c.used[:0]
	c.curFont = nil
	c.curSize = 0
	c.links = c.links[:0]
	return c.emit.Reset(&c.buf, scratch)
}

// SetFont selects f at the given size for subsequent Text calls, registering it
// in this page's resources.
func (c *Canvas) SetFont(f Font, size float64) int {
	c.curFont = f
	c.curSize = size
	return c.ensure(f)
}

// ensure registers f and returns its resource name ("F1"…).
func (c *Canvas) ensure(f Font) int {
	for i := range c.used {
		if c.used[i] == f || c.used[i].BaseName() == f.BaseName() {
			return i + 1
		}
	}
	c.used = append(c.used, f)
	return len(c.used)
}

// Text draws s with its baseline origin at (x, y) in the current font and fill
// color col.
func (c *Canvas) Text(x, y float64, s string, col color.Color) {
	if c.curFont == nil {
		return
	}
	num := c.ensure(c.curFont)
	r, g, b := rgb(col)
	c.emit.Ident("BT")
	c.emit.NameNum("F", int64(num))
	c.emit.Real(c.curSize)
	c.emit.Ident("Tf")
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("rg")
	c.emit.Real(x)
	c.emit.Real(y)
	c.emit.Ident("Td")
	c.emit.StringBytes(c.encode(s))
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
	c.enc = c.enc[:0]
	for _, r := range s {
		c.enc = c.curFont.Encode(c.enc, r)
	}
	return c.enc
}

// Line strokes a segment from (x0,y0) to (x1,y1) with width w and color col.
func (c *Canvas) Line(x0, y0, x1, y1, w float64, col color.Color) {
	r, g, b := rgb(col)
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("RG")
	c.emit.Real(w)
	c.emit.Ident("w")
	c.emit.Real(x0)
	c.emit.Real(y0)
	c.emit.Ident("m")
	c.emit.Real(x1)
	c.emit.Real(y1)
	c.emit.Ident("l")
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
	c.emit.Real(x)
	c.emit.Real(y)
	c.emit.Real(w)
	c.emit.Real(h)
	c.emit.Ident("re")
	c.emit.Ident("f")
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
	c.emit.Real(lineWidth)
	c.emit.Ident("w")
	c.emit.Real(x)
	c.emit.Real(y)
	c.emit.Real(w)
	c.emit.Real(h)
	c.emit.Ident("re")
	c.emit.Ident("S")
	c.emit.EOL()
}

// Bytes flushes and returns the content-stream bytes emitted so far.
func (c *Canvas) Bytes() []byte {
	c.emit.Flush()
	return c.buf.Bytes()
}

// Fonts returns the fonts referenced on this page, in first-use order.
func (c *Canvas) Fonts() []Font { return c.used }

// Err reports the first emission error, if any.
func (c *Canvas) Err() error { return c.emit.Err() }
