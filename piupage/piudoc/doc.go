package doc

import (
	"io"

	piudf "github.com/soypat/piudf"
	canvas "github.com/soypat/piudf/piupage"
)

// Flowable is a layout element that measures itself then paints itself, the
// reportlab wrap/drawOn analogue. Wrap must be called before Draw; Draw places
// the flowable's top-left corner at (x, yTop) in PDF space (origin bottom-left)
// and is told the same availWidth the frame measured it with.
type Flowable interface {
	Wrap(availWidth float64) (w, h float64)
	Draw(c *canvas.Canvas, x, yTop, availWidth float64)
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

// Document is the flowable frame plus PDF writer, the reportlab
// SimpleDocTemplate analogue.
type Document struct {
	Size    PageSize
	Margins Margins
	Title   string
	Author  string

	w       io.Writer
	enc     piudf.Encoder
	scratch []byte
}

// New returns a Document that will write to w with the given page size and
// margins.
func New(w io.Writer, size PageSize, m Margins) *Document {
	return &Document{Size: size, Margins: m, w: w, scratch: make([]byte, 512)}
}

// Build flows story top-to-bottom, opening pages as needed, then writes the
// page tree, fonts, cross-reference table and trailer. It is the single-call
// reportlab doc.build(story) analogue.
func (d *Document) Build(story []Flowable) error {
	if err := d.enc.Reset(d.w, d.scratch); err != nil {
		return err
	}
	catalog := d.enc.NewID()
	pagesID := d.enc.NewID()

	contentW := d.Size.W - d.Margins.Left - d.Margins.Right

	// Flow the story into per-page canvases.
	var (
		canvases []*canvas.Canvas
		cursorY  float64
		pageTop  float64
	)
	newPage := func() *canvas.Canvas {
		cv := canvas.NewCanvas(make([]byte, 512))
		canvases = append(canvases, cv)
		cursorY = d.Size.H - d.Margins.Top
		pageTop = cursorY
		return cv
	}
	cur := newPage()
	for _, fl := range story {
		_, h := fl.Wrap(contentW)
		if cursorY-h < d.Margins.Bottom && cursorY != pageTop {
			cur = newPage()
		}
		fl.Draw(cur, d.Margins.Left, cursorY, contentW)
		cursorY -= h
	}

	// Write shared font objects, keyed by BaseName.
	fontID := map[string]piudf.ObjectID{}
	for _, cv := range canvases {
		for _, f := range cv.Fonts() {
			if _, ok := fontID[f.BaseName()]; ok {
				continue
			}
			id, err := canvas.WriteFont(&d.enc, f)
			if err != nil {
				return err
			}
			fontID[f.BaseName()] = id
		}
	}

	// Write each page's content stream and page dictionary.
	pageIDs := make([]piudf.ObjectID, len(canvases))
	for i, cv := range canvases {
		content := d.enc.NewID()
		body := cv.Bytes()
		d.enc.BeginObject(content)
		d.enc.DictOpen()
		d.enc.Name("Length")
		d.enc.Int(int64(len(body)))
		d.enc.DictClose()
		d.enc.BeginStreamPayload()
		if _, err := d.enc.PayloadWriter().Write(body); err != nil {
			return err
		}
		d.enc.EndStreamPayload()
		d.enc.EndObject()

		pageIDs[i] = d.enc.NewID()
		d.enc.BeginObject(pageIDs[i])
		d.enc.DictOpen()
		d.enc.Name("Type")
		d.enc.Name("Page")
		d.enc.Name("Parent")
		d.enc.Ref(pagesID.Num, pagesID.Gen)
		d.enc.Name("MediaBox")
		d.enc.ArrayOpen()
		d.enc.Int(0)
		d.enc.Int(0)
		d.enc.Real(d.Size.W)
		d.enc.Real(d.Size.H)
		d.enc.ArrayClose()
		d.enc.Name("Contents")
		d.enc.Ref(content.Num, content.Gen)
		d.enc.Name("Resources")
		d.enc.DictOpen()
		d.enc.Name("Font")
		d.enc.DictOpen()
		for _, f := range cv.Fonts() {
			id := fontID[f.BaseName()]
			d.enc.Name(cv.ResourceName(f))
			d.enc.Ref(id.Num, id.Gen)
		}
		d.enc.DictClose()
		d.enc.DictClose()
		d.enc.DictClose()
		d.enc.EndObject()
	}

	// Pages tree.
	d.enc.BeginObject(pagesID)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Pages")
	d.enc.Name("Kids")
	d.enc.ArrayOpen()
	for _, id := range pageIDs {
		d.enc.Ref(id.Num, id.Gen)
	}
	d.enc.ArrayClose()
	d.enc.Name("Count")
	d.enc.Int(int64(len(pageIDs)))
	d.enc.DictClose()
	d.enc.EndObject()

	// Info dictionary (optional).
	var info piudf.ObjectID
	if d.Title != "" || d.Author != "" {
		info = d.enc.NewID()
		d.enc.BeginObject(info)
		d.enc.DictOpen()
		if d.Title != "" {
			d.enc.Name("Title")
			d.enc.String([]byte(d.Title))
		}
		if d.Author != "" {
			d.enc.Name("Author")
			d.enc.String([]byte(d.Author))
		}
		d.enc.DictClose()
		d.enc.EndObject()
	}

	// Catalog.
	d.enc.BeginObject(catalog)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Catalog")
	d.enc.Name("Pages")
	d.enc.Ref(pagesID.Num, pagesID.Gen)
	d.enc.DictClose()
	d.enc.EndObject()

	return d.enc.Close(catalog, info)
}
