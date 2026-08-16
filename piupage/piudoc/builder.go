package piudoc

import (
	"bytes"
	"strconv"
)

// Builder constructs the text-bearing elements of a story. It exists for three
// reasons: it holds the one parser a whole document parses through, it parses
// when an element is built rather than every time one is drawn, and it has
// somewhere to put an error that a constructor cannot return.
//
// Every element it hands back owns its memory and is finished. The builder's
// buffers are scratch it lends to itself and takes back — no element ever holds
// a view of them — so one builder can build a document, and the next document
// after it, without growing.
//
// The zero Builder is ready to use.
type Builder struct {
	// CellStyle is the style [Builder.TextCell] and [Builder.MarkupCell] give a
	// cell. The zero value means [Normal].
	CellStyle Style

	// parser is the single parse workspace, in place of the one every Paragraph
	// carries today.
	parser atomParser
	// err is the first error a constructor hit; hasErr says whether there was
	// one. They are kept apart so that a zero BuildError still reads as no
	// error, and so Err can hand out a pointer into the builder rather than
	// allocate one.
	err    BuildError
	hasErr bool
}

// Err returns the first error any constructor hit, so a run of them can be
// written without checking each one. It stays set until [Builder.Reset].
func (bld *Builder) Err() error {
	if !bld.hasErr {
		return nil
	}
	return &bld.err
}

// Reset clears the error and readies the builder for another document. Its
// buffers are kept: reusing them is the point of reusing a builder.
func (bld *Builder) Reset() {
	bld.err, bld.hasErr = BuildError{}, false
}

// fail records the first error and ignores the rest. Reporting one allocates
// nothing: the error lives on the builder, and near views the caller's text.
func (bld *Builder) fail(kind ErrKind, off int, near string) {
	if bld.hasErr {
		return
	}
	bld.err, bld.hasErr = BuildError{Kind: kind, Off: off, Near: near}, true
}

// P builds a paragraph from markup, a small reportlab-compatible subset:
//
//	<b> </b>                     bold
//	<i> </i>                     italic
//	<font size="8" color="#555"> size and color, either attribute optional
//	<a href="…"> </a>            link, also spelled <link href="…">
//	<br/>                        line break, also <br> and <br />
//
// and the entities &amp; &lt; &gt; and &#160;, resolved in text and in an href.
// Tags nest, and a closing tag restores what the matching opening tag changed.
//
// A tag the dialect does not carry, one left unclosed, and an attribute that
// will not parse are all errors: they are reported to [Builder.Err], and the
// paragraph comes back holding what parsed before the fault. Nothing is drawn
// silently wrong.
//
// Markup is a promise that the text is formatting the caller wrote, not data the
// caller received. Data goes through [Builder.Text], which cannot fail.
func (bld *Builder) P(markup string, s Style) *Paragraph {
	return bld.build(bld.parser.parse(s2b(markup), s), s)
}

// Text builds a paragraph from literal text. Every byte of it draws as itself:
// no tag is recognized, no entity is resolved, and there is nothing to fail on.
// It is also the cheaper of the two doors, since the scanner never runs.
func (bld *Builder) Text(text string, s Style) *Paragraph {
	return bld.build(bld.parser.parseText(s2b(text), s), s)
}

// PBytes is [Builder.P] for a caller holding a buffer rather than a string. The
// slice is read and not kept: what the paragraph draws is its own copy.
func (bld *Builder) PBytes(markup []byte, s Style) *Paragraph {
	return bld.build(bld.parser.parse(markup, s), s)
}

// TextBytes is [Builder.Text] for a caller holding a buffer, on the same terms
// as [Builder.PBytes].
func (bld *Builder) TextBytes(text []byte, s Style) *Paragraph {
	return bld.build(bld.parser.parseText(text, s), s)
}

// build packs a parse into a paragraph of its own.
func (bld *Builder) build(atoms []atom, s Style) *Paragraph {
	p := &Paragraph{Style: s}
	p.text, p.line = bld.pack(atoms)
	return p
}

// TextCell wraps one cell's worth of literal text, styled by
// [Builder.CellStyle].
func (bld *Builder) TextCell(text string) Cell {
	return Cell{Drawer: bld.Text(text, bld.cellStyle())}
}

// MarkupCell wraps one cell's worth of markup, styled by [Builder.CellStyle].
// It fails on the same things [Builder.P] does.
func (bld *Builder) MarkupCell(markup string) Cell {
	return Cell{Drawer: bld.P(markup, bld.cellStyle())}
}

// cellStyle is CellStyle with its zero value resolved to Normal.
func (bld *Builder) cellStyle() Style {
	if bld.CellStyle.Size == 0 {
		return Normal
	}
	return bld.CellStyle
}

// Cell wraps any drawer — a paragraph with a style of its own, a nested table —
// as a cell.
func (bld *Builder) Cell(d Drawer) Cell { return Cell{Drawer: d} }

// Row wraps drawers as a row of cells, one each.
func (bld *Builder) Row(ds ...Drawer) []Cell {
	row := make([]Cell, len(ds))
	for i, d := range ds {
		row[i] = Cell{Drawer: d}
	}
	return row
}

// TextRow builds a whole row of literal cells, which is what most rows are.
func (bld *Builder) TextRow(texts ...string) []Cell {
	row := make([]Cell, len(texts))
	for i, t := range texts {
		row[i] = bld.TextCell(t)
	}
	return row
}

// Bookmark names a place in the reader's navigation pane. It draws nothing; it
// goes through the builder so that an empty title or a negative level is caught
// where the mistake was made rather than silently written into the outline.
func (bld *Builder) Bookmark(title string, level int) Bookmark {
	if title == "" {
		bld.fail(ErrEmptyBookmark, 0, "")
	}
	if level < 0 {
		level = 0
	}
	return Bookmark{Title: title, Level: level}
}

// pack copies parsed pieces into memory the element owns: the words and link
// targets end to end in one buffer, sized exactly once so that the views into it
// cannot be invalidated as the rest go in, and the pieces as views of that.
func (bld *Builder) pack(src []atom) (text []byte, out []piece) {
	if len(src) == 0 {
		return nil, nil
	}
	// Size first, by the same rule the copy below uses, so the buffer is exact
	// and appending into it can never move a view already handed out.
	n := 0
	var prev []byte
	for i := range src {
		n += len(src[i].word)
		if h := src[i].href; len(h) > 0 && !bytes.Equal(h, prev) {
			n += len(h)
		}
		prev = src[i].href
	}
	text = make([]byte, 0, n)
	out = make([]piece, len(src))
	prev = nil
	var packedHref []byte
	for i := range src {
		a := &src[i]
		if len(a.href) > 0 {
			// Every word of a span carries the same target: pack it once and let
			// them share it, which is also what keeps them comparing equal when
			// a line coalesces the run.
			if !bytes.Equal(a.href, prev) {
				start := len(text)
				text = append(text, a.href...)
				packedHref = text[start:]
			}
			prev = a.href
		} else {
			prev, packedHref = nil, nil
		}
		var word []byte
		if len(a.word) > 0 {
			start := len(text)
			text = append(text, a.word...)
			word = text[start:]
		}
		out[i] = piece{text: word, font: a.font, size: a.size, col: a.col, href: packedHref, brk: a.brk}
	}
	return text, out
}

// ParagraphBuilder enables the concatenation of distinct
// PDF elements commonly found in paragraphs like text, links, and markup.
type ParagraphBuilder struct {
	buf []byte
}

// Text adds raw text to paragraph.
func (pb *ParagraphBuilder) Text(s string) *ParagraphBuilder {
	pb.buf = appendEscaped(pb.buf, s)
	return pb
}

// Markup adds markup to paragraph. See [Builder.P] documentation,
func (pb *ParagraphBuilder) Markup(s string) *ParagraphBuilder {
	pb.buf = append(pb.buf, s...)
	return pb
}

// Link adds a href/anchor element to paragraph.
func (pb *ParagraphBuilder) Link(text, uri string) *ParagraphBuilder {
	pb.buf = append(pb.buf, `<a href="`...)
	pb.buf = appendEscapedAttr(pb.buf, uri)
	pb.buf = append(pb.buf, `">`...)
	pb.buf = appendEscaped(pb.buf, text)
	pb.buf = append(pb.buf, `</a>`...)
	return pb
}

// Break adds a newline-like paragraph break.
func (pb *ParagraphBuilder) Break() *ParagraphBuilder {
	pb.buf = append(pb.buf, "<br/>"...)
	return pb
}

func (pb *ParagraphBuilder) Reset() { pb.buf = pb.buf[:0] }

func (pb *ParagraphBuilder) Build(bld *Builder, s Style) *Paragraph {
	p := bld.build(bld.parser.parse(pb.buf, s), s)
	pb.buf = pb.buf[:0]
	return p
}

func appendEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			dst = append(dst, "&amp;"...)
		case '<':
			dst = append(dst, "&lt;"...)
		case '>':
			dst = append(dst, "&gt;"...)
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// appendEscapedAttr appends s to dst fit to sit inside the quotes of an href,
// which is not the job appendEscaped does and does not have the same answer.
func appendEscapedAttr(dst []byte, s string) []byte {
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '&':
			dst = append(dst, "&amp;"...)
		case '<', '>', '"', '\'', ' ':
			dst = append(dst, '%', hex[c>>4], hex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// ErrKind is what a [Builder] found wrong.
type ErrKind uint8

const (
	ErrNone ErrKind = iota
	// ErrUnterminatedTag is a '<' with no '>' after it.
	ErrUnterminatedTag
	// ErrUnknownTag is a tag the dialect does not carry, which is usually a
	// typo — <bold> for <b> — or markup written for a richer reportlab.
	ErrUnknownTag
	// ErrUnclosedSpan is text that ended inside <b>, <i>, <font> or <a>.
	ErrUnclosedSpan
	// ErrStrayClose is a closing tag with nothing open to close.
	ErrStrayClose
	// ErrNestingTooDeep is more than maxSpanDepth spans open at once.
	ErrNestingTooDeep
	// ErrBadSize is a <font size=> that is not a number.
	ErrBadSize
	// ErrBadColor is a <font color=> that is not #rrggbb.
	ErrBadColor
	// ErrEmptyBookmark is a bookmark with no title, which would land in the
	// reader's navigation pane as an unclickable blank.
	ErrEmptyBookmark
	// ErrBadFamily is a [Builder.SetFamily] with no name or no Regular face.
	ErrBadFamily
)

// String returns the kind's message, without the offset or the offending text
// that [BuildError] adds.
func (k ErrKind) String() string {
	switch k {
	case ErrNone:
		return "no error"
	case ErrUnterminatedTag:
		return "unterminated tag"
	case ErrUnknownTag:
		return "unknown tag"
	case ErrUnclosedSpan:
		return "unclosed span"
	case ErrStrayClose:
		return "closing tag with nothing open"
	case ErrNestingTooDeep:
		return "markup nested too deep"
	case ErrBadSize:
		return "bad font size"
	case ErrBadColor:
		return "bad color"
	case ErrEmptyBookmark:
		return "bookmark with no title"
	case ErrBadFamily:
		return "family with no name or no Regular face"
	}
	return "unknown error"
}

// BuildError is what [Builder.Err] returns. A builder holds one by value, so
// reporting an error allocates nothing; only rendering its message does.
type BuildError struct {
	Kind ErrKind
	// Off is the byte offset into the text the element was built from.
	Off int
	// Near is the offending tag or attribute value. It views the string the
	// caller passed in, which is immutable, so it stays valid uncopied.
	Near string
}

func (e *BuildError) Error() string {
	s := "piudoc: " + e.Kind.String() + " at offset " + strconv.Itoa(e.Off)
	if e.Near != "" {
		s += ": " + e.Near
	}
	return s
}
