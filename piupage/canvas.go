package canvas

import (
	"bytes"

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
	// Mirrored graphics state, to elide redundant operators.
	curFont            Font
	curSize            float64
	curFill, curStroke Color
	curLW              float64
	inText             bool
}

// fontUse binds a font to the resource name it was assigned on this page.
type fontUse struct {
	font Font
	name string // "F1", "F2", …
}

// NewCanvas returns a Canvas whose emitter uses scratch as its write-through
// window; scratch is caller-owned and must be at least piulex.MinEmitBuffer.
func NewCanvas(scratch []byte) *Canvas { panic("todo: canvas.NewCanvas") }

// Reset clears the content buffer and font set for reuse on a new page.
func (c *Canvas) Reset() { panic("todo: Canvas.Reset") }

// SetFont selects f at the given size for subsequent Text calls.
func (c *Canvas) SetFont(f Font, size float64) { panic("todo: Canvas.SetFont") }

// SetFill sets the nonstroking (fill) color.
func (c *Canvas) SetFill(col Color) { panic("todo: Canvas.SetFill") }

// SetStroke sets the stroking color.
func (c *Canvas) SetStroke(col Color) { panic("todo: Canvas.SetStroke") }

// SetLineWidth sets the stroke width in points.
func (c *Canvas) SetLineWidth(w float64) { panic("todo: Canvas.SetLineWidth") }

// Text draws s with its baseline origin at (x, y) in the current font.
func (c *Canvas) Text(x, y float64, s string) { panic("todo: Canvas.Text") }

// TextRight draws s right-aligned so its rendered box ends at xRight.
func (c *Canvas) TextRight(xRight, y float64, s string) { panic("todo: Canvas.TextRight") }

// Line strokes a segment from (x0,y0) to (x1,y1) in the current stroke color
// and line width.
func (c *Canvas) Line(x0, y0, x1, y1 float64) { panic("todo: Canvas.Line") }

// Rect draws the rectangle at (x,y) of size w×h, filling and/or stroking it.
func (c *Canvas) Rect(x, y, w, h float64, fill, stroke bool) { panic("todo: Canvas.Rect") }

// Bytes returns the content-stream bytes emitted so far.
func (c *Canvas) Bytes() []byte { panic("todo: Canvas.Bytes") }

// Fonts returns the fonts referenced on this page, in first-use order.
func (c *Canvas) Fonts() []Font { panic("todo: Canvas.Fonts") }

// ResourceName returns the /Resources font name ("F1"…) assigned to f, or ""
// if f was not used on this page.
func (c *Canvas) ResourceName(f Font) string { panic("todo: Canvas.ResourceName") }

// Err reports the first emission error, if any.
func (c *Canvas) Err() error { panic("todo: Canvas.Err") }
