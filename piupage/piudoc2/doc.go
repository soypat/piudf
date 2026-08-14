package piudoc2

import (
	"io"
	"time"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piupage"
)

// Flowable is a layout element that measures itself then paints itself, the
// reportlab wrap/drawOn analogue. Wrap must be called before Draw; Draw places
// the flowable's top-left corner at (x, yTop) in PDF space (origin bottom-left)
// and is told the same availWidth the frame measured it with.
type Flowable interface {
	Wrap(availWidth float64) (w, h float64)
	Draw(c *piupage.Canvas, x, yTop, availWidth float64)
}

// Splittable is a flowable that can be divided across a page boundary. The
// frame asks for a split only when the whole will not fit in what is left of
// the page; a flowable that would rather move than break returns a nil head.
type Splittable interface {
	Flowable
	SplitAt(availWidth, availHeight float64) (head, tail Flowable)
}

// Unwrappable is a grouping flowable that can give up its grouping. The frame
// unwraps a group only as a last resort — when it does not fit even on a page
// of its own — so that "keep these together" never becomes "run off the page".
type Unwrappable interface {
	Flowable
	Unwrap() []Flowable
}

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
	return nil
}

func (d *Doc) flow(dst []piupage.Canvas, story []Flowable, canvasBuf []byte) (nCanv int, nStories int, err error) {
	var (
		cursorY  float64
		pageTop  float64
		contentW = d.Size.W - d.Margins.Left - d.Margins.Right
	)
	pageOff := len(d.pageIDs)
	newPage := func() *piupage.Canvas {
		if nCanv >= len(dst) {
			return nil
		}
		cv := &dst[nCanv]
		cv.Reset(canvasBuf)
		nCanv++
		cursorY = d.Size.H - d.Margins.Top
		pageTop = cursorY
		return cv
	}
	cur := newPage()
	for _, fl := range story {
		_, h := fl.Wrap(contentW)
		if cursorY-h < d.Margins.Bottom && cursorY != pageTop {
			cur = newPage()
			if cur == nil {
				return nCanv, nStories, io.ErrShortBuffer
			}
		}
		fl.Draw(cur, d.Margins.Left, cursorY, contentW)
		cursorY -= h
		nStories++
	}

	// Write all fonts and store in document for later.
	for i := range nCanv {
		cv := &dst[i]
		for _, f := range cv.Fonts() {
			idx := d.fontidx(f.BaseName())
			if idx >= 0 {
				break
			}
			d.fonts = append(d.fonts, f)
			id, err := piupage.WriteFont(&d.enc, f)
			if err != nil {
				return nCanv, nStories, err
			}
			d.fontids = append(d.fontids, id)
		}
	}

	d.pageIDs = append(d.pageIDs, make([]piudf.ObjectID, nCanv)...)
	for icv := range nCanv {
		cv := &dst[icv]
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
		d.enc.BeginObject(d.pageIDs[icv])
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
		for i, f := range cv.Fonts() {
			idx := d.fontidx(f.BaseName())
			id := d.fontids[idx]
			d.enc.NameNum("F", int64(i+1))
			d.enc.Ref(id.Num, id.Gen)
		}
		d.enc.DictClose()
		d.enc.DictClose()
		d.enc.DictClose()
		d.enc.EndObject()
	}
	return 0, 0, err
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

// Wrap reports no size.
func (Bookmark) Wrap(availWidth float64) (w, h float64) { return availWidth, 0 }

// Draw paints nothing.
func (Bookmark) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {}

// collectBookmarks appends the bookmarks fl carries, descending into groups.
// A bookmark's destination is wherever the flowable holding it comes to rest,
// which is what the frame is about to decide.
func collectBookmarks(fl Flowable, dst []Bookmark) []Bookmark {
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
