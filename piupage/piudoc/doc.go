// package piudoc is the flowable layer of a reportlab-style report generator: a
// story of Paragraph/Table/Spacer/HRule flowed top-to-bottom into pages and
// written as a PDF. It is the platypus analogue built on the imperative
// github.com/soypat/piudf/piupage surface and the piudf Encoder.
package piudoc

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

// PageInfo describes one finished page to a decoration callback.
type PageInfo struct {
	Num   int // 1-based
	Total int
	Size  PageSize
	// Margins are the frame's margins, which is where running heads and folios.
	Margins Margins
}

// Document is the flowable frame plus PDF writer, the reportlab
// SimpleDocTemplate analogue.
type Document struct {
	Size    PageSize
	Margins Margins

	// Document information is optional:

	Title    string
	Author   string
	Subject  string
	Keywords string
	Creator  string
	Date     time.Time
	// Lang is the document's natural language as a BCP 47 tag, e.g. "en-US".
	// Screen readers use it to pick a voice.
	Lang string

	// OnPage runs after final page count already known to decorate
	// i.e: "page 2 of 7".
	OnPage func(c *piupage.Canvas, p PageInfo)

	w       io.Writer
	enc     piudf.Encoder
	scratch []byte
}

// New returns a Document that will write to w with the given page size and
// margins.
func New(w io.Writer, size PageSize, m Margins) *Document {
	return &Document{Size: size, Margins: m, w: w, scratch: make([]byte, 512)}
}

// mark is a bookmark that has been placed: the frame records where the story
// was when it went past, since a Bookmark draws nothing and cannot know.
type mark struct {
	title string
	level int
	page  int
	y     float64
}

// Build flows story top-to-bottom, opening pages as needed, then writes the
// page tree, fonts, annotations, outline, cross-reference table and trailer.
// It is the single-call reportlab doc.build(story) analogue.
func (d *Document) Build(story []Flowable) error {
	if err := d.enc.Reset(d.w, d.scratch); err != nil {
		return err
	}
	catalog := d.enc.NewID()
	pagesID := d.enc.NewID()

	canvases, marks := d.flow(story)
	if d.OnPage != nil {
		for i, cv := range canvases {
			d.OnPage(cv, PageInfo{Num: i + 1, Total: len(canvases), Size: d.Size, Margins: d.Margins})
		}
	}
	for _, cv := range canvases {
		if err := cv.Err(); err != nil {
			return err
		}
	}

	// Fonts are written only now: an embedded font subsets itself to the
	// glyphs it was asked to draw, so it must not be asked until every page is.
	fontID := map[string]piudf.ObjectID{}
	for _, cv := range canvases {
		for _, f := range cv.Fonts() {
			if _, ok := fontID[f.BaseName()]; ok {
				continue
			}
			id, err := piupage.WriteFont(&d.enc, f)
			if err != nil {
				return err
			}
			fontID[f.BaseName()] = id
		}
	}

	pageIDs := make([]piudf.ObjectID, len(canvases))
	for i := range pageIDs {
		pageIDs[i] = d.enc.NewID()
	}
	for i, cv := range canvases {
		if err := d.writePage(pageIDs[i], pagesID, cv, fontID); err != nil {
			return err
		}
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

	outlines := d.writeOutline(marks, pageIDs)
	info := d.writeInfo()

	// Catalog.
	d.enc.BeginObject(catalog)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Catalog")
	d.enc.Name("Pages")
	d.enc.Ref(pagesID.Num, pagesID.Gen)
	if outlines.Num != 0 {
		d.enc.Name("Outlines")
		d.enc.Ref(outlines.Num, outlines.Gen)
		d.enc.Name("PageMode")
		d.enc.Name("UseOutlines")
	}
	if d.Lang != "" {
		d.enc.Name("Lang")
		d.enc.String([]byte(d.Lang))
	}
	d.enc.Name("ViewerPreferences")
	d.enc.DictOpen()
	d.enc.Name("DisplayDocTitle")
	d.enc.Bool(true)
	d.enc.DictClose()
	d.enc.DictClose()
	d.enc.EndObject()

	return d.enc.Close(catalog, info)
}

// flow lays the story onto canvases, breaking pages as it fills them.
func (d *Document) flow(story []Flowable) ([]*piupage.Canvas, []mark) {
	contentW := d.Size.W - d.Margins.Left - d.Margins.Right
	var (
		canvases []*piupage.Canvas
		marks    []mark
		cursorY  float64
		pageTop  float64
	)
	newPage := func() *piupage.Canvas {
		cv := piupage.NewCanvas(make([]byte, 512))
		canvases = append(canvases, cv)
		cursorY = d.Size.H - d.Margins.Top
		pageTop = cursorY
		return cv
	}
	cur := newPage()

	// unplaced holds bookmarks read but not yet resolved to a destination
	var unplaced []Bookmark
	// place resolves every pending bookmark, plus any fl carries itself
	place := func(fl Flowable) {
		unplaced = collectBookmarks(fl, unplaced)
		for _, b := range unplaced {
			marks = append(marks, mark{title: b.Title, level: b.Level, page: len(canvases) - 1, y: cursorY})
		}
		unplaced = unplaced[:0]
	}

	// pending is the work the frame owes itself: the tail of a split flowable,
	// a flowable deferred to the next page, or a group it had to take apart.
	// It is a stack, so an unwrapped group's children stay in order.
	var pending []Flowable
	pop := func() Flowable {
		fl := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		return fl
	}
	push := func(fls ...Flowable) {
		for i := len(fls) - 1; i >= 0; i-- {
			pending = append(pending, fls[i])
		}
	}

	for i := 0; i < len(story) || len(pending) > 0; {
		var fl Flowable
		if len(pending) > 0 {
			fl = pop()
		} else {
			fl, i = story[i], i+1
		}
		atTop := cursorY == pageTop

		switch v := fl.(type) {
		case PageBreak:
			if !atTop {
				cur = newPage()
			}
			continue
		case Bookmark:
			unplaced = append(unplaced, v)
			continue
		}

		_, h := fl.Wrap(contentW)
		avail := cursorY - d.Margins.Bottom
		if h <= avail {
			place(fl)
			fl.Draw(cur, d.Margins.Left, cursorY, contentW)
			cursorY -= h
			continue
		}
		if sp, ok := fl.(Splittable); ok {
			if head, tail := sp.SplitAt(contentW, avail); head != nil {
				_, hh := head.Wrap(contentW)
				place(head)
				head.Draw(cur, d.Margins.Left, cursorY, contentW)
				cursorY -= hh
				cur = newPage()
				push(tail)
				continue
			}
		}
		if !atTop {
			cur = newPage()
			push(fl)
			continue
		}
		// It does not fit on a page of its own. A group gives up its grouping;
		// anything else is drawn where it is and allowed to overflow, which at
		// least puts the content in the document.
		if u, ok := fl.(Unwrappable); ok {
			if kids := u.Unwrap(); len(kids) > 0 {
				push(kids...)
				continue
			}
		}
		place(fl)
		fl.Draw(cur, d.Margins.Left, cursorY, contentW)
		cursorY -= h
	}
	// A story ending in a bookmark still gets a destination: the end.
	place(nil)
	return canvases, marks
}

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

// writePage emits one page's content stream, link annotations and page dict.
func (d *Document) writePage(pageID, pagesID piudf.ObjectID, cv *piupage.Canvas, fontID map[string]piudf.ObjectID) error {
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

	links := cv.Links()
	annotIDs := make([]piudf.ObjectID, len(links))
	for i, ln := range links {
		annotIDs[i] = d.enc.NewID()
		d.enc.BeginObject(annotIDs[i])
		d.enc.DictOpen()
		d.enc.Name("Type")
		d.enc.Name("Annot")
		d.enc.Name("Subtype")
		d.enc.Name("Link")
		d.enc.Name("Rect")
		d.enc.ArrayOpen()
		d.enc.Real(ln.X)
		d.enc.Real(ln.Y)
		d.enc.Real(ln.X + ln.W)
		d.enc.Real(ln.Y + ln.H)
		d.enc.ArrayClose()
		// A zero-width border: the link is styled by the text under it, not by
		// the box every viewer used to draw around it.
		d.enc.Name("Border")
		d.enc.ArrayOpen()
		d.enc.Int(0)
		d.enc.Int(0)
		d.enc.Int(0)
		d.enc.ArrayClose()
		d.enc.Name("A")
		d.enc.DictOpen()
		d.enc.Name("S")
		d.enc.Name("URI")
		d.enc.Name("URI")
		d.enc.String([]byte(ln.URI))
		d.enc.DictClose()
		d.enc.DictClose()
		d.enc.EndObject()
	}

	d.enc.BeginObject(pageID)
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
	if len(annotIDs) > 0 {
		d.enc.Name("Annots")
		d.enc.ArrayOpen()
		for _, id := range annotIDs {
			d.enc.Ref(id.Num, id.Gen)
		}
		d.enc.ArrayClose()
	}
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
	return d.enc.Err()
}

// writeInfo emits the document information dictionary, or a zero id when the
// document carries no metadata worth one.
func (d *Document) writeInfo() piudf.ObjectID {
	fields := [...]struct{ key, val string }{
		{"Title", d.Title}, {"Author", d.Author}, {"Subject", d.Subject},
		{"Keywords", d.Keywords}, {"Creator", d.Creator},
	}
	any := !d.Date.IsZero()
	for _, f := range fields {
		any = any || f.val != ""
	}
	if !any {
		return piudf.ObjectID{}
	}
	id := d.enc.NewID()
	d.enc.BeginObject(id)
	d.enc.DictOpen()
	for _, f := range fields {
		if f.val == "" {
			continue
		}
		d.enc.Name(f.key)
		d.textString(f.val)
	}
	if !d.Date.IsZero() {
		d.enc.Name("CreationDate")
		d.enc.String([]byte(d.Date.Format("D:20060102150405-07'00'")))
	}
	d.enc.DictClose()
	d.enc.EndObject()
	return id
}

// outlineItem is one node of the navigation tree, resolved into sibling and
// child links over the flat list of marks.
type outlineItem struct {
	mark
	id                       piudf.ObjectID
	parent, prev, next       int // indices into the item slice, -1 for none
	first, last, descendants int
}

// writeOutline emits the document outline built from marks, returning a zero
// id when there are none.
func (d *Document) writeOutline(marks []mark, pageIDs []piudf.ObjectID) piudf.ObjectID {
	if len(marks) == 0 {
		return piudf.ObjectID{}
	}
	items := make([]outlineItem, len(marks))
	// ancestors[l] is the index of the open item at level l, so a mark's
	// parent is whatever sits one level above it, and levels that skip a step
	// simply attach to the nearest open ancestor.
	var ancestors []int
	for i, m := range marks {
		it := outlineItem{mark: m, id: d.enc.NewID(), parent: -1, prev: -1, next: -1, first: -1, last: -1}
		level := min(max(m.level, 0), len(ancestors))
		ancestors = ancestors[:level]
		if level > 0 {
			it.parent = ancestors[level-1]
		}
		items[i] = it
		// Link to the previous sibling: the last item sharing this parent.
		for j := i - 1; j >= 0; j-- {
			if items[j].parent == it.parent {
				items[i].prev, items[j].next = j, i
				break
			}
		}
		if p := it.parent; p >= 0 {
			if items[p].first < 0 {
				items[p].first = i
			}
			items[p].last = i
			for a := p; a >= 0; a = items[a].parent {
				items[a].descendants++
			}
		}
		ancestors = append(ancestors, i)
	}

	root := d.enc.NewID()
	rootFirst, rootLast, rootCount := -1, -1, 0
	for i := range items {
		if items[i].parent < 0 {
			if rootFirst < 0 {
				rootFirst = i
			}
			rootLast = i
			rootCount++
		}
	}

	for _, it := range items {
		d.enc.BeginObject(it.id)
		d.enc.DictOpen()
		d.enc.Name("Title")
		d.textString(it.title)
		d.enc.Name("Parent")
		if it.parent < 0 {
			d.enc.Ref(root.Num, root.Gen)
		} else {
			d.enc.Ref(items[it.parent].id.Num, items[it.parent].id.Gen)
		}
		d.outlineRef("Prev", items, it.prev)
		d.outlineRef("Next", items, it.next)
		d.outlineRef("First", items, it.first)
		d.outlineRef("Last", items, it.last)
		if it.descendants > 0 {
			// Positive means the branch opens with the pane; the reader sees
			// the structure rather than a row of closed triangles.
			d.enc.Name("Count")
			d.enc.Int(int64(it.descendants))
		}
		d.enc.Name("Dest")
		d.enc.ArrayOpen()
		page := min(max(it.page, 0), len(pageIDs)-1)
		d.enc.Ref(pageIDs[page].Num, pageIDs[page].Gen)
		d.enc.Name("XYZ")
		d.enc.Real(0)
		d.enc.Real(it.y)
		d.enc.Null()
		d.enc.ArrayClose()
		d.enc.DictClose()
		d.enc.EndObject()
	}

	d.enc.BeginObject(root)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("Outlines")
	d.outlineRef("First", items, rootFirst)
	d.outlineRef("Last", items, rootLast)
	d.enc.Name("Count")
	d.enc.Int(int64(rootCount))
	d.enc.DictClose()
	d.enc.EndObject()
	return root
}

// textString emits s as a PDF text string. A string a reader will see —
// a title, an outline entry — is PDFDocEncoded unless it is written as
// UTF-16BE behind a byte order mark, and PDFDocEncoding agrees with ASCII and
// almost nothing else. Anything past ASCII therefore goes out as UTF-16BE, or
// an em dash in a document title reaches the viewer as mojibake.
func (d *Document) textString(s string) {
	if isASCII(s) {
		d.enc.String([]byte(s))
		return
	}
	b := []byte{0xfe, 0xff}
	for _, r := range s {
		if r > 0xffff {
			r -= 0x10000
			b = append(b, byte(0xd8|r>>18), byte(r>>10), byte(0xdc|r>>8&0x3), byte(r))
			continue
		}
		b = append(b, byte(r>>8), byte(r))
	}
	d.enc.String(b)
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// outlineRef emits key and its item reference, or nothing when idx is -1.
func (d *Document) outlineRef(key string, items []outlineItem, idx int) {
	if idx < 0 {
		return
	}
	d.enc.Name(key)
	d.enc.Ref(items[idx].id.Num, items[idx].id.Gen)
}
