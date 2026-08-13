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
type Canvas struct {
	emit piulex.Emitter
	buf  bytes.Buffer
	// used lists the fonts referenced in first-use order, assigning /F1, /F2 …
	used []fontUse
	// Current text font/size, applied by Text.
	curFont Font
	curSize float64
	// links records the page's link areas. They are annotations, not content,
	// so the Canvas only collects them; the page writer emits them.
	links []Link
	// enc and fonts are the buffers Text and Fonts hand back. They are grown
	// to what a page needs and then reused for every page after it, which is
	// what keeps drawing off the heap once the first page has been laid down.
	enc   []byte
	fonts []Font
}

// Link is a rectangular area of a page that resolves to a URI when clicked.
// It is page furniture rather than drawn content: nothing about a Link changes
// what the page looks like.
type Link struct {
	X, Y, W, H float64
	URI        string
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
	c.links = c.links[:0]
	c.enc = c.enc[:0]
	c.fonts = c.fonts[:0]
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

var fontResNames = [...]string{
	"F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8",
	"F9", "F10", "F11", "F12", "F13", "F14", "F15", "F16",
}

// ensure registers f and returns its resource name ("F1"…).
func (c *Canvas) ensure(f Font) string {
	for i := range c.used {
		if c.used[i].font == f || c.used[i].font.BaseName() == f.BaseName() {
			return c.used[i].name
		}
	}
	var name string
	if n := len(c.used); n < len(fontResNames) {
		name = fontResNames[n]
	} else {
		name = "F" + strconv.Itoa(n+1)
	}
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

// encode converts s to the current font's byte codes. The result is the
// Canvas's own buffer, valid until the next encode: the emitter writes the
// codes out before Text returns, so nothing outlives the call.
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

// Fonts returns the fonts referenced on this page, in first-use order. Like
// [Canvas.Links] the result is the Canvas's own slice, valid until the next
// call to Fonts or [Canvas.Reset]; copy it to keep it past either.
func (c *Canvas) Fonts() []Font {
	c.fonts = c.fonts[:0]
	for i := range c.used {
		c.fonts = append(c.fonts, c.used[i].font)
	}
	return c.fonts
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
