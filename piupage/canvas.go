package canvas

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
type Canvas struct {
	emit piulex.Emitter
	buf  bytes.Buffer
	// used lists the fonts referenced in first-use order, assigning /F1, /F2 …
	used []fontUse
	// Current text font/size, applied by Text.
	curFont Font
	curSize float64
}

// fontUse binds a font to the resource name it was assigned on this page.
type fontUse struct {
	font Font
	name string // "F1", "F2", …
}

// NewCanvas returns a Canvas whose emitter uses scratch as its write-through
// window; scratch is caller-owned and must be at least piulex.MinEmitBuffer.
func NewCanvas(scratch []byte) *Canvas {
	c := &Canvas{}
	c.emit.Reset(&c.buf, scratch)
	return c
}

// Reset clears the content buffer and font set for reuse on a new page.
func (c *Canvas) Reset(scratch []byte) {
	c.buf.Reset()
	c.used = c.used[:0]
	c.curFont = nil
	c.curSize = 0
	c.emit.Reset(&c.buf, scratch)
}

// SetFont selects f at the given size for subsequent Text calls, registering it
// in this page's resources.
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
	c.emit.Real(c.curSize)
	c.emit.Ident("Tf")
	c.emit.Real(r)
	c.emit.Real(g)
	c.emit.Real(b)
	c.emit.Ident("rg")
	c.emit.Real(x)
	c.emit.Real(y)
	c.emit.Ident("Td")
	c.emit.String(c.encode(s))
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
