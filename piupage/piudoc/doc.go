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
	"slices"
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

	// CheckGlyphs fails [Doc.Build] on a glyph the font cannot draw or a font
	// too damaged to describe itself. NOT IMPLEMENTED: setting it does nothing.
	CheckGlyphs bool

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
	// glyphs is every glyph the pages drew, sorted by font then glyph and
	// deduplicated, naming its font by index into fonts.
	glyphs  []piupage.GlyphUse
	pageIDs []piudf.ObjectID
	// annots is scratch: the link annotations of the page being written.
	annots []piudf.ObjectID
}
type PageSize struct {
	W, H float64
}

func SizeLetter() PageSize { return PageSize{612, 792} }
func SizeA4() PageSize     { return PageSize{595.276, 841.890} }

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
	d.glyphs = d.glyphs[:0]
	d.fontids = d.fontids[:0]
	d.marks = d.marks[:0]
	d.annots = d.annots[:0]
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

	// Register every face and gather what each was asked to draw. A subset is
	// cut once for the document, not once per page, so the glyphs have to be
	// merged before any font is written.
	fontFirst := len(d.fonts)
	for i := range nCanv {
		cv := &dst[i]
		fonts := cv.Fonts()
		for _, ft := range fonts {
			if d.fontidx(ft.PostScriptName()) < 0 {
				d.fonts = append(d.fonts, ft)
			}
		}
		for _, gu := range cv.Glyphs() {
			gu.Font = uint16(d.fontidx(fonts[gu.Font].PostScriptName()))
			d.glyphs = append(d.glyphs, gu)
		}
	}
	// Sorting by font and glyph puts each face's glyphs in one ascending run,
	// which is the order the subsetter, the /W array and the /ToUnicode map all
	// want, and it is what makes the duplicates adjacent.
	slices.SortFunc(d.glyphs, func(a, b piupage.GlyphUse) int {
		if a.Font != b.Font {
			return int(a.Font) - int(b.Font)
		}
		return int(a.Glyph) - int(b.Glyph)
	})
	d.glyphs = slices.CompactFunc(d.glyphs, func(a, b piupage.GlyphUse) bool {
		return a.Font == b.Font && a.Glyph == b.Glyph
	})
	for i := fontFirst; i < len(d.fonts); i++ {
		id, err := piupage.WriteFont(&d.enc, d.fonts[i], d.fontGlyphs(i))
		if err != nil {
			return nCanv, nStories, err
		}
		d.fontids = append(d.fontids, id)
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

		// Annotations are separate objects the page dictionary points at, and
		// BeginObject pairs cannot nest, so they have to be written before the
		// page is opened — the same shape the content stream just followed.
		d.annots = d.annots[:0]
		for _, ln := range cv.Links() {
			id, err := piupage.WriteLink(&d.enc, ln)
			if err != nil {
				return nCanv, nStories, err
			}
			d.annots = append(d.annots, id)
		}

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
		// An empty /Annots is legal but is noise on the pages that carry no
		// link, which is most of them.
		if len(d.annots) > 0 {
			d.enc.Name("Annots")
			d.enc.ArrayOpen()
			for _, id := range d.annots {
				d.enc.Ref(id.Num, id.Gen)
			}
			d.enc.ArrayClose()
		}
		d.enc.Name("Resources")
		d.enc.DictOpen()
		d.enc.Name("Font")
		d.enc.DictOpen()
		for i, ft := range cv.Fonts() {
			idx := d.fontidx(ft.PostScriptName())
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
	if d.Title != "" || d.Author != "" || d.Subject != "" || d.Creator != "" || !d.Date.IsZero() {
		info = d.enc.NewID()
		d.enc.BeginObject(info)
		d.enc.DictOpen()
		for _, e := range [...]struct{ key, val string }{
			{"Title", d.Title}, {"Author", d.Author},
			{"Subject", d.Subject}, {"Creator", d.Creator},
		} {
			if e.val != "" {
				d.enc.Name(e.key)
				d.enc.TextString(e.val)
			}
		}
		if !d.Date.IsZero() {
			// A PDF date is ASCII by construction, so it is a literal string and
			// not a text string. Both keys carry it: a document written in one
			// pass was never modified after it was created.
			d.scratch = appendPDFDate(d.scratch[:0], d.Date)
			d.enc.Name("CreationDate")
			d.enc.StringBytes(d.scratch)
			d.enc.Name("ModDate")
			d.enc.StringBytes(d.scratch)
		}
		d.enc.DictClose()
		d.enc.EndObject()
	}

	// Outline tree, which the catalog points at.
	var outlines piudf.ObjectID
	if len(d.marks) > 0 {
		outlines = d.writeOutlines()
	}

	// Catalog.
	d.enc.BeginObject(d.catalog)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Catalog")
	d.enc.Name("Pages")
	d.enc.Ref(d.pages.Num, d.pages.Gen)
	if outlines.Num != 0 {
		d.enc.Name("Outlines")
		d.enc.Ref(outlines.Num, outlines.Gen)
	}
	if d.Lang != "" {
		// A language tag is ASCII, so TextString spends nothing on it and stays
		// right if one ever is not.
		d.enc.Name("Lang")
		d.enc.TextString(d.Lang)
	}
	d.enc.DictClose()
	d.enc.EndObject()
	return d.enc.Close(d.catalog, info)
}

func (d *Doc) fontidx(basename string) int {
	for i, f := range d.fonts {
		if basename == f.PostScriptName() {
			return i
		}
	}
	return -1
}

// fontGlyphs is the run of d.glyphs belonging to the font at index i, which the
// sort left contiguous and ascending.
func (d *Doc) fontGlyphs(i int) []piupage.GlyphUse {
	lo, _ := slices.BinarySearchFunc(d.glyphs, uint16(i), cmpGlyphFont)
	hi, _ := slices.BinarySearchFunc(d.glyphs, uint16(i+1), cmpGlyphFont)
	return d.glyphs[lo:hi]
}

func cmpGlyphFont(g piupage.GlyphUse, font uint16) int { return int(g.Font) - int(font) }

// mark is a bookmark that has been placed: the frame records where the story
// was when it went past, since a Bookmark draws nothing and cannot know.
type mark struct {
	title string
	level int
	page  int
	y     float64
	// id is the outline item this is written as, filled by writeOutlines before
	// any item is written because an item names siblings and children that
	// follow it.
	id piudf.ObjectID
}

// The outline tree is the run of placed marks read through their levels: a
// mark's descendants are exactly the marks after it at a deeper level, which is
// contiguous, so every link an item needs is a scan and none has to be stored.

// markParent is the nearest earlier mark at a shallower level; -1 means the item
// hangs off the outline root.
func (d *Doc) markParent(i int) int {
	for j := i - 1; j >= 0; j-- {
		if d.marks[j].level < d.marks[i].level {
			return j
		}
	}
	return -1
}

// markSibling is the adjacent sibling of i in direction step (-1 or 1). The
// first mark at or above i's level ends the search: at the same level it is the
// sibling, above it is the parent, and i is an outer child.
func (d *Doc) markSibling(i, step int) int {
	for j := i + step; j >= 0 && j < len(d.marks); j += step {
		if d.marks[j].level > d.marks[i].level {
			continue
		}
		if d.marks[j].level == d.marks[i].level {
			return j
		}
		break
	}
	return -1
}

// markChildren is i's first and last child, or -1, -1. The first child can only
// be the very next mark; the last is the end of its sibling chain.
func (d *Doc) markChildren(i int) (first, last int) {
	if i+1 >= len(d.marks) || d.marks[i+1].level <= d.marks[i].level {
		return -1, -1
	}
	last = i + 1
	for next := d.markSibling(last, 1); next >= 0; next = d.markSibling(last, 1) {
		last = next
	}
	return i + 1, last
}

// markCount is how many descendants i has at every depth, which is the /Count a
// reader wants because every item is written open.
func (d *Doc) markCount(i int) (n int) {
	for j := i + 1; j < len(d.marks) && d.marks[j].level > d.marks[i].level; j++ {
		n++
	}
	return n
}

// writeOutlines emits the outline tree and returns its root. Every id is
// allocated before any item is written, because an item names siblings and
// children that come after it.
func (d *Doc) writeOutlines() piudf.ObjectID {
	root := d.enc.NewID()
	for i := range d.marks {
		d.marks[i].id = d.enc.NewID()
	}
	// ref emits item i, or the root, which is what a top-level item's parent is.
	ref := func(i int) {
		id := root
		if i >= 0 {
			id = d.marks[i].id
		}
		d.enc.Ref(id.Num, id.Gen)
	}

	for i := range d.marks {
		m := &d.marks[i]
		first, last := d.markChildren(i)
		d.enc.BeginObject(m.id)
		d.enc.DictOpen()
		d.enc.Name("Title")
		d.enc.TextString(m.title)
		d.enc.Name("Parent")
		ref(d.markParent(i))
		for _, e := range [...]struct {
			key string
			to  int
		}{
			{"Prev", d.markSibling(i, -1)}, {"Next", d.markSibling(i, 1)},
			{"First", first}, {"Last", last},
		} {
			if e.to >= 0 {
				d.enc.Name(e.key)
				ref(e.to)
			}
		}
		if n := d.markCount(i); n > 0 {
			d.enc.Name("Count")
			d.enc.Int(int64(n))
		}
		if m.page >= 0 && m.page < len(d.pageIDs) {
			// /XYZ with a null left and zoom scrolls the page to the mark
			// without disturbing the reader's horizontal position or zoom.
			pid := d.pageIDs[m.page]
			d.enc.Name("Dest")
			d.enc.ArrayOpen()
			d.enc.Ref(pid.Num, pid.Gen)
			d.enc.Name("XYZ")
			d.enc.Null()
			d.enc.Real(m.y)
			d.enc.Null()
			d.enc.ArrayClose()
		}
		d.enc.DictClose()
		d.enc.EndObject()
	}

	last := -1
	for i := range d.marks {
		if d.markParent(i) < 0 {
			last = i
		}
	}
	d.enc.BeginObject(root)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Outlines")
	if last >= 0 {
		// The first mark is always top-level: nothing precedes it to be its parent.
		d.enc.Name("First")
		ref(0)
		d.enc.Name("Last")
		ref(last)
	}
	// Every item is open, so the root's visible count is all of them.
	d.enc.Name("Count")
	d.enc.Int(int64(len(d.marks)))
	d.enc.DictClose()
	d.enc.EndObject()
	return root
}

// appendPDFDate appends t as (D:YYYYMMDDHHmmSSOHH'mm'), the only date form the
// spec gives. The zone is spelled out here because Go has no layout for the
// apostrophes.
func appendPDFDate(dst []byte, t time.Time) []byte {
	dst = t.AppendFormat(dst, "D:20060102150405")
	_, off := t.Zone()
	sign := byte('+')
	if off < 0 {
		sign, off = '-', -off
	}
	off /= 60
	dst = append(dst, sign)
	dst = appendPad2(dst, off/60)
	dst = append(dst, '\'')
	dst = appendPad2(dst, off%60)
	return append(dst, '\'')
}

func appendPad2(dst []byte, v int) []byte {
	return append(dst, byte('0'+v/10%10), byte('0'+v%10))
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
