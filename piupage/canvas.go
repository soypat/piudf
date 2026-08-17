package piupage

import (
	"bytes"
	"image/color"

	"github.com/soypat/lefevre/sfnt"
	"github.com/soypat/piudf/internal"
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
	// images lists the images referenced in first-use order, assigning
	// /Im1, /Im2 … the way used does for fonts.
	images []Image
	glyphs []GlyphUse
	// Text encodes
	enc []byte
	// ctm is the transform in effect and stack the ones q has saved.
	ctm   Matrix
	stack [32]Matrix // PDF 1.7 supports 32 level nesting.
	depth int
}

// maxGDepth is the graphics state nesting Canvas admits. See Canvas.stack.
const maxGDepth = 32

// Mark is a position in a Canvas's content stream, taken by [Canvas.Mark] and
// retracted to by [Canvas.Rewind]. A Mark is invalidated by [Canvas.Reset].
type Mark struct {
	n       int
	fonts   int
	links   int
	images  int
	glyphs  int
	curFont Font
	curSize float64
	ctm     Matrix
	depth   int
}

// Mark records the canvas's current state so a later [Canvas.Rewind] can
// retract everything drawn after it.
func (c *Canvas) Mark() Mark {
	c.emit.Flush()
	return Mark{
		n: c.buf.Len(), fonts: len(c.used), links: len(c.links),
		images: len(c.images), glyphs: len(c.glyphs),
		curFont: c.curFont, curSize: c.curSize,
		ctm: c.ctm, depth: c.depth,
	}
}

// Rewind discards every operator, font reference, and link recorded since m was
// taken. Passing a Mark from a different canvas or one taken before a
// [Canvas.Reset] is a programming error and does nothing.
func (c *Canvas) Rewind(m Mark) {
	c.emit.Flush()
	if m.n > c.buf.Len() || m.fonts > len(c.used) || m.links > len(c.links) ||
		m.images > len(c.images) || m.glyphs > len(c.glyphs) {
		return
	}
	c.buf.Truncate(m.n)
	c.used = c.used[:m.fonts]
	c.links = c.links[:m.links]
	c.images = c.images[:m.images]
	c.glyphs = c.glyphs[:m.glyphs]
	c.curFont = m.curFont
	c.curSize = m.curSize
	c.ctm = m.ctm
	c.depth = m.depth
}

// Reset clears the content buffer and font set for reuse on a new page.
func (c *Canvas) Reset(emitScratch []byte) error {
	c.buf.Reset()
	c.used = c.used[:0]
	c.curFont = nil
	c.curSize = 0
	c.links = c.links[:0]
	c.images = c.images[:0]
	c.glyphs = c.glyphs[:0]
	c.ctm = Identity()
	c.depth = 0
	return c.emit.Reset(&c.buf, emitScratch)
}

// Save pushes the graphics state level: transform, clip, colors and line parameters
// and returns a token restoring it. It is PDF's q operator.
func (c *Canvas) Save() StateLevel {
	if c.depth >= maxGDepth {
		c.emit.Fail(errGDepth)
		return StateLevel{depth: c.depth}
	}
	c.stack[c.depth] = c.ctm
	c.depth++
	c.emit.Ident("q")
	c.emit.EOL()
	return StateLevel{depth: c.depth - 1}
}

// Restore unwinds the graphics state to what it was when s was taken, emitting
// PDF's Q operator for each level it pops. Restoring to a token already
// restored, or one from another canvas, does nothing.
func (c *Canvas) Restore(s StateLevel) {
	if s.depth < 0 || s.depth >= c.depth {
		return
	}
	for c.depth > s.depth {
		c.depth--
		c.ctm = c.stack[c.depth]
		c.emit.Ident("Q")
	}
	c.emit.EOL()
}

// StateLevel is a saved graphics state, taken by [Canvas.Save] and unwound to by
// [Canvas.Restore]. A StateLevel is invalidated by [Canvas.Reset].
type StateLevel struct{ depth int }

// Transform concatenates m onto the current transform, so that subsequent
// coordinates are read in the space m describes. It is PDF's cm operator, and
// like it the effect lasts until the enclosing [Canvas.Save] is restored.
func (c *Canvas) Transform(m Matrix) {
	c.ctm = m.Mul(c.ctm)
	c.emit.Real(m.A)
	c.emit.Real(m.B)
	c.emit.Real(m.C)
	c.emit.Real(m.D)
	c.emit.Real(m.E)
	c.emit.Real(m.F)
	c.emit.Ident("cm")
	c.emit.EOL()
}

// Translate moves the origin of user space to (tx, ty).
func (c *Canvas) Translate(tx, ty float64) { c.Transform(mattranslate(tx, ty)) }

// Scale scales user space about its origin.
func (c *Canvas) Scale(sx, sy float64) { c.Transform(matscale(sx, sy)) }

// Rotate rotates user space counterclockwise about its origin by rad radians.
func (c *Canvas) Rotate(rad float64) { c.Transform(matrotate(rad)) }

// CTM returns the transform in effect, mapping user space to page space.
func (c *Canvas) CTM() Matrix { return c.ctm }

// DeviceXY maps a point from the current user space to page space, which is
// where a page's annotations and media box are measured.
func (c *Canvas) DeviceXY(x, y float64) (float64, float64) { return c.ctm.Apply(x, y) }

// SetFont selects f at the given size for subsequent Text calls, registering it
// in this page's resources.
func (c *Canvas) SetFont(f Font, size float64) int {
	if f != nil && f.UnitsPerEm() == 0 {
		c.emit.Fail(errNoUnitsPerEm)
		return 0
	}
	c.curFont = f
	c.curSize = size
	return c.ensure(f)
}

// ensure registers f and returns its resource name ("F1"…).
func (c *Canvas) ensure(f Font) int {
	for i := range c.used {
		if c.used[i] == f || c.used[i].PostScriptName() == f.PostScriptName() {
			return i + 1
		}
	}
	c.used = append(c.used, f)
	return len(c.used)
}

// Image draws im into the rectangle w by h whose lower-left corner is at
// (x, y), registering it in this page's resources.
//
// PDF paints an image into the unit square, so the rectangle is the whole of
// the placement: w and h are a scale, and the raster's own size never reaches
// the page. An image drawn 100 points wide is 100 points wide whether it holds
// a thumbnail or a photograph, and what that leaves for samples per point is
// the caller's to decide.
//
// Drawing one image twice costs its bytes once. The two uses resolve to the
// same resource, so a mark repeated down a page, or a logo on every page of a
// document, is written to the file a single time.
func (c *Canvas) Image(im Image, x, y, w, h float64) {
	if err := im.Valid(); err != nil {
		c.emit.Fail(err)
		return
	}
	if w == 0 || h == 0 {
		return
	}
	num := c.ensureImage(im)
	// Saved and restored rather than left standing: Do is the only operator
	// that reads this transform, and a cm that outlived it would move whatever
	// was drawn next.
	s := c.Save()
	c.Transform(Matrix{A: w, D: h, E: x, F: y})
	c.emit.NameNum("Im", int64(num))
	c.emit.Ident("Do")
	c.emit.EOL()
	c.Restore(s)
}

// Images returns the images the canvas referenced, in the order their resource
// names were assigned. The page writer needs them to build /XObject.
func (c *Canvas) Images() []Image { return c.images }

// ensureImage registers im and returns its resource number ("Im1"…).
func (c *Canvas) ensureImage(im Image) int {
	for i := range c.images {
		if c.images[i].SameAs(im) {
			return i + 1
		}
	}
	c.images = append(c.images, im)
	return len(c.images)
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
	c.emit.StringBytes(c.encode(s, num-1))
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

// encode converts s to the current font's byte codes, recording what an
// embedded font was asked to draw. fontIdx is the font's index in c.used.
func (c *Canvas) encode(s string, fontIdx int) []byte {
	c.enc = c.enc[:0]
	f := c.curFont
	if _, embedded := f.(sfnt.Source); !embedded {
		for _, r := range s {
			g := f.GlyphID(r)
			if g == 0 {
				// A built-in font has no .notdef to draw, so an unencodable
				// rune arrives as '?' rather than as nothing.
				g = '?'
			}
			c.enc = append(c.enc, byte(g))
		}
		return c.enc
	}
	for _, r := range s {
		g := f.GlyphID(r)
		c.enc = append(c.enc, byte(g>>8), byte(g))
		c.glyphs = append(c.glyphs, GlyphUse{Font: uint16(fontIdx), Glyph: g, Rune: r})
	}
	return c.enc
}

// Glyphs returns what the page's embedded fonts were asked to draw.
func (c *Canvas) Glyphs() []GlyphUse { return c.glyphs }

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
	// An annotation is measured in page space and knows nothing of the
	// transform the content was drawn under
	x0, y0 := c.ctm.Apply(x, y)
	x1, y1 := c.ctm.Apply(x+w, y)
	x2, y2 := c.ctm.Apply(x+w, y+h)
	x3, y3 := c.ctm.Apply(x, y+h)
	lo, hi := min(x0, x1, x2, x3), max(x0, x1, x2, x3)
	bot, top := min(y0, y1, y2, y3), max(y0, y1, y2, y3)
	ln := internal.SliceReclaim(&c.links)
	*ln = Link{X: lo, Y: bot, W: hi - lo, H: top - bot, URI: ln.URI[:0]}
	ln.URI = append(ln.URI, uri...)
}

// Links returns the page's link areas in the order they were added. The slice
// is the canvas's own storage and stays valid until the next [Canvas.Reset],
// which is what lets a page writer emit the annotations after the content
// stream is complete.
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

// Err reports the first emission error, if any, and an unrestored [Canvas.Save]
// once the page is complete.
func (c *Canvas) Err() error {
	if err := c.emit.Err(); err != nil {
		return err
	}
	if c.depth != 0 {
		return errUnclosed
	}
	return nil
}
