package doc

import (
	"io"

	piudf "github.com/soypat/piudf"
	canvas "github.com/soypat/piudf/piupage"
)

// Flowable is a layout element that measures itself then paints itself, the
// reportlab wrap/drawOn analogue. Wrap must be called before Draw; Draw places
// the flowable's top-left corner at (x, yTop) in PDF space (origin bottom-left).
type Flowable interface {
	Wrap(availWidth float64) (w, h float64)
	Draw(c *canvas.Canvas, x, yTop float64)
}

// PageSize is a page's dimensions in points.
type PageSize struct {
	W, H float64
}

// Common page sizes.
var (
	A4     = PageSize{595.276, 841.890}
	Letter = PageSize{612, 792}
)

// Margins are the page margins in points.
type Margins struct {
	Left, Right, Top, Bottom float64
}

// page records one emitted page's objects and its drawing surface.
type page struct {
	id      piudf.ObjectID
	content piudf.ObjectID
	cv      *canvas.Canvas
}

// Document is the flowable frame plus PDF writer, the reportlab
// SimpleDocTemplate analogue.
type Document struct {
	Size    PageSize
	Margins Margins
	Title   string
	Author  string

	enc     piudf.Encoder
	scratch []byte
	pages   []page
}

// New returns a Document that will write to w with the given page size and
// margins.
func New(w io.Writer, size PageSize, m Margins) *Document { panic("todo: doc.New") }

// Build flows story top-to-bottom, opening pages as needed, then writes the
// page tree, fonts, cross-reference table and trailer. It is the single-call
// reportlab doc.build(story) analogue.
func (d *Document) Build(story []Flowable) error { panic("todo: Document.Build") }
