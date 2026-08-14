// package piudoc is the layout layer of a reportlab-style report generator: a
// story of Paragraph/Table/Spacer/HRule flowed top-to-bottom into pages and
// written as a PDF. It is the platypus analogue built on the imperative
// github.com/soypat/piudf/piupage surface and the piudf Encoder.
//
// Unlike reportlab, an element measures nothing up front: it draws itself in
// one pass over as many pages as it needs and reports where it left off. See
// [Drawer].
package piudoc

import (
	"io"
	"math"
	"time"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piupage"
)

// Frame is the content box a story flows through — the same rectangle on every
// page, being the page size less its margins.
type Frame struct {
	// X and Width are the box's left edge and width.
	X, Width float64
	// Top and Bottom are the y of the box's upper and lower edges. A cursor
	// starts at Top on a fresh page and may descend to Bottom.
	Top, Bottom float64
}

// Height reports the frame's usable vertical extent.
func (f Frame) Height() float64 { return f.Top - f.Bottom }

// Drawer is a layout element that paints itself into a story's pages.
//
// Draw starts at (f.X, yTop) on dst[0] and paints downward. yTop is the story
// cursor, which is at or below f.Top; an element that runs out of room may
// continue onto dst[1], dst[2] … each of which begins at f.Top. It reports how
// many pages it moved on by — 0 if it stayed on dst[0] — and yEnd, the cursor
// on the last page it touched, which is where the next element begins. An
// element that cannot fit in the pages it was given returns [io.ErrShortBuffer].
//
// The pages in dst are ready to draw on: the frame resets them and writes their
// page furniture. An element must not touch a page past the one it reports, and
// must leave the cursor where it says it did — those two are the whole contract
// that lets elements nest.
type Drawer interface {
	Draw(dst []piupage.Canvas, f Frame, yTop float64) (pagesAdvanced int, yEnd float64, err error)
}

// noBreak is a frame bottom no element will ever reach, so a draw made against
// it lays out as though the page were unbounded.
const noBreak = -math.MaxFloat64 / 4

// Measure reports the height dr would occupy at yTop without leaving a trace of
// it. It draws dr onto dst[0] against an unbounded page and retracts the
// result, which is how a container learns a child's extent before it can decide
// where the child goes — a table row's height is the tallest of its cells, but
// the row's background and rules have to be painted before any cell's text.
//
// It is only valid for elements that fit on one page; a child measured this way
// never splits.
func Measure(dst []piupage.Canvas, dr Drawer, f Frame, yTop float64) (h float64, err error) {
	f.Bottom = noBreak
	cv := &dst[0]
	m := cv.Mark()
	_, yEnd, err := dr.Draw(dst[:1], f, yTop)
	cv.Rewind(m)
	if err != nil {
		return 0, err
	}
	return yTop - yEnd, nil
}

// Doc is the flowable frame plus PDF writer, the reportlab SimpleDocTemplate
// analogue. It allocates nothing itself: pages, their content buffers and the
// encoder's buffer are all supplied by the caller.
type Doc struct {
	Size    PageSize
	Margins Margins
	Title   string
	Author  string
	Subject string
	Creator string
	Date    time.Time
	Lang    string
	OnPage  func(c *piupage.Canvas, p PageInfo)

	w   io.Writer
	enc piudf.Encoder
	// global objects.
	catalog piudf.ObjectID
	pages   piudf.ObjectID
	// Auxiliary buffers:
	unplacedbookmarks []Bookmark
	scratch           []byte
	marks             []mark
	fonts             []piupage.Font
	fontids           []piudf.ObjectID
	pageIDs           []piudf.ObjectID
}
type PageSize struct {
	W, H float64
}

// Common page sizes.
var (
	A4     = PageSize{595.276, 841.890}
	Letter = PageSize{612, 792}
)

type Margins struct {
	Left, Right, Top, Bottom float64
}

// PageInfo describes one finished page to a decoration callback.
type PageInfo struct {
	Num   int // 1-based
	Total int
	Size  PageSize
	// Margins are the frame's margins, which is where running heads and folios.
	Margins Margins
}

func (d *Doc) Reset(w io.Writer, encBuf []byte) (err error) {
	err = d.enc.Reset(w, encBuf)
	if err != nil {
		return err
	}
	d.w = w
	d.catalog = piudf.ObjectID{}
	d.pages = piudf.ObjectID{}
	d.pageIDs = d.pageIDs[:0]
	d.fonts = d.fonts[:0]
	d.fontids = d.fontids[:0]
	d.marks = d.marks[:0]
	return nil
}

// Frame returns the content box the document flows stories through.
func (d *Doc) Frame() Frame {
	return Frame{
		X:      d.Margins.Left,
		Width:  d.Size.W - d.Margins.Left - d.Margins.Right,
		Top:    d.Size.H - d.Margins.Top,
		Bottom: d.Margins.Bottom,
	}
}

// Build writes story to w as a complete PDF. dst holds the pages it may use and
// canvasBuf is divided evenly among them as their content buffers; both bound
// the document, and a story that needs more pages than dst holds fails with
// [io.ErrShortBuffer].
func (d *Doc) Build(w io.Writer, dst []piupage.Canvas, story []Drawer, encBuf, canvasBuf []byte) error {
	err := d.Reset(w, encBuf)
	if err != nil {
		return err
	} else if err = d.open(); err != nil {
		return err
	} else if _, _, err = d.flow(dst, story, canvasBuf); err != nil {
		return err
	}
	return d.close()
}

func (d *Doc) flow(dst []piupage.Canvas, story []Drawer, canvasBuf []byte) (nCanv int, nStories int, err error) {
	if len(dst) == 0 {
		return 0, 0, io.ErrShortBuffer
	}
	chunk := len(canvasBuf) / len(dst)
	for i := range dst {
		err = dst[i].Reset(canvasBuf[i*chunk : (i+1)*chunk])
		if err != nil {
			return 0, 0, err
		}
	}

	f := d.Frame()
	pageOff := len(d.pageIDs)
	nCanv = 1
	cursorY := f.Top
	for _, fl := range story {
		// A bookmark draws nothing, so where it lands is the frame's to record:
		// it is wherever the story had got to when it went past.
		d.unplacedbookmarks = collectBookmarks(fl, d.unplacedbookmarks[:0])
		for _, bm := range d.unplacedbookmarks {
			d.marks = append(d.marks, mark{
				title: bm.Title, level: bm.Level,
				page: pageOff + nCanv - 1, y: cursorY,
			})
		}
		adv, yEnd, err := fl.Draw(dst[nCanv-1:], f, cursorY)
		if err != nil {
			return nCanv, nStories, err
		}
		nCanv += adv
		cursorY = yEnd
		nStories++
	}

	// Page furniture, once the total is known.
	if d.OnPage != nil {
		for i := range nCanv {
			d.OnPage(&dst[i], PageInfo{
				Num: pageOff + i + 1, Total: pageOff + nCanv,
				Size: d.Size, Margins: d.Margins,
			})
		}
	}

	// Write all fonts and store in document for later.
	for i := range nCanv {
		cv := &dst[i]
		for _, ft := range cv.Fonts() {
			idx := d.fontidx(ft.BaseName())
			if idx >= 0 {
				continue
			}
			d.fonts = append(d.fonts, ft)
			id, err := piupage.WriteFont(&d.enc, ft)
			if err != nil {
				return nCanv, nStories, err
			}
			d.fontids = append(d.fontids, id)
		}
	}

	d.pageIDs = append(d.pageIDs, make([]piudf.ObjectID, nCanv)...)
	for icv := range nCanv {
		cv := &dst[icv]
		if err := cv.Err(); err != nil {
			return nCanv, nStories, err
		}
		content := d.enc.NewID()
		body := cv.Bytes()
		d.enc.BeginObject(content)
		d.enc.DictOpen()
		d.enc.Name("Length")
		d.enc.Int(int64(len(body)))
		d.enc.DictClose()
		d.enc.BeginStreamPayload()
		if _, err := d.enc.PayloadWriter().Write(body); err != nil {
			return nCanv, nStories, err
		}
		d.enc.EndStreamPayload()
		d.enc.EndObject()
		d.pageIDs[pageOff+icv] = d.enc.NewID()
		d.enc.BeginObject(d.pageIDs[pageOff+icv])
		d.enc.DictOpen()
		d.enc.Name("Type")
		d.enc.Name("Page")
		d.enc.Name("Parent")
		d.enc.Ref(d.pages.Num, d.pages.Gen)
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
		for i, ft := range cv.Fonts() {
			idx := d.fontidx(ft.BaseName())
			id := d.fontids[idx]
			d.enc.NameNum("F", int64(i+1))
			d.enc.Ref(id.Num, id.Gen)
		}
		d.enc.DictClose()
		d.enc.DictClose()
		d.enc.DictClose()
		d.enc.EndObject()
	}
	return nCanv, nStories, d.enc.Err()
}

func (d *Doc) open() error {
	d.catalog = d.enc.NewID()
	d.pages = d.enc.NewID()
	return d.enc.Err()
}

func (d *Doc) close() error {
	// Pages tree.
	d.enc.BeginObject(d.pages)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Pages")
	d.enc.Name("Kids")
	d.enc.ArrayOpen()
	for _, id := range d.pageIDs {
		d.enc.Ref(id.Num, id.Gen)
	}
	d.enc.ArrayClose()
	d.enc.Name("Count")
	d.enc.Int(int64(len(d.pageIDs)))
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
			d.enc.String(d.Title)
		}
		if d.Author != "" {
			d.enc.Name("Author")
			d.enc.String(d.Author)
		}
		d.enc.DictClose()
		d.enc.EndObject()
	}

	// Catalog.
	d.enc.BeginObject(d.catalog)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Catalog")
	d.enc.Name("Pages")
	d.enc.Ref(d.pages.Num, d.pages.Gen)
	d.enc.DictClose()
	d.enc.EndObject()
	return d.enc.Close(d.catalog, info)
}

func (d *Doc) fontidx(basename string) int {
	for i, f := range d.fonts {
		if basename == f.BaseName() {
			return i
		}
	}
	return -1
}

// mark is a bookmark that has been placed: the frame records where the story
// was when it went past, since a Bookmark draws nothing and cannot know.
type mark struct {
	title string
	level int
	page  int
	y     float64
}

// Bookmark names a place in the document for the reader's navigation pane. Does not draw anything.
type Bookmark struct {
	Title string
	Level int
}

// Draw paints nothing and consumes no space.
func (Bookmark) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	return 0, yTop, nil
}

// Unwrappable is a grouping element that can name its children, so the frame
// can see what a group carries without drawing it.
type Unwrappable interface {
	Drawer
	Unwrap() []Drawer
}

// collectBookmarks appends the bookmarks fl carries, descending into groups.
// A bookmark's destination is wherever the element holding it comes to rest,
// which is what the frame is about to decide.
func collectBookmarks(fl Drawer, dst []Bookmark) []Bookmark {
	switch v := fl.(type) {
	case Bookmark:
		return append(dst, v)
	case Unwrappable:
		for _, kid := range v.Unwrap() {
			dst = collectBookmarks(kid, dst)
		}
	}
	return dst
}
