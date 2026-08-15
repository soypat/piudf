package piudoc

import (
	"bytes"
	"image/color"
	"io"
	"strconv"
	"strings"
	"unsafe"

	"github.com/soypat/piudf/internal"
	"github.com/soypat/piudf/piupage"
)

// Paragraph is wrapped rich text. Text may carry a small markup subset —
// <b>, <i>, <br/>, <font size= color=> — with unknown tags ignored. Style
// supplies the defaults.
type Paragraph struct {
	Text  []byte
	Style Style
	// parser holds the words the markup parses to and the memory they view; see
	// [atomParser]. line is the one line currently being filled. Both are scratch
	// reused across draws.
	parser atomParser
	line   []piece
}

// piece is a run of same-style text within a laid-out line. Its text and href
// are the atom's views carried one step further; see [atom].
type piece struct {
	text []byte
	font piupage.Font
	size float64
	col  color.Color
	// href is the link target the piece belongs to, empty for ordinary text.
	href []byte
}

// spaceWord is the text of the pieces holding the gaps between words. They
// advance the cursor and draw nothing: a PDF text run needs no space glyph.
var spaceWord = []byte(" ")

// isSpaceWord reports whether the piece is one of those gaps. It is an identity
// test, not a comparison: every gap is this one slice, and no word ever is.
func isSpaceWord(b []byte) bool { return len(b) == 1 && &b[0] == &spaceWord[0] }

// P constructs a Paragraph from text and a style. It is the convenience door
// for text a program has as a string: the conversion copies once, here, and
// never again. A caller with a buffer of its own sets [Paragraph.Text] instead.
func P(text string, s Style) *Paragraph { return &Paragraph{Text: []byte(text), Style: s} }

// CopyFrom loads src's text and style into p, keeping p's own scratch. It is how
// one paragraph is reused to draw many: p parses into the buffers it has already
// grown, and src is only read.
//
// The scratch is what must not come across. Those buffers belong to the
// paragraph that grew them — taking src's would have p's next draw write through
// into memory src owns, and would hand p's own away one source at a time.
func (p *Paragraph) CopyFrom(src *Paragraph) {
	p.Text = append(p.Text[:0], src.Text...)
	p.line = append(p.line[:0], src.line...)
	p.Style = src.Style
}

// Draw breaks the text greedily to the frame's width and paints it from yTop
// down, continuing onto further pages as it fills them.
func (p *Paragraph) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	avail := f.Width - p.Style.LeftIndent - p.Style.RightIndent
	if avail <= 0 {
		return 0, yTop, nil
	}
	atoms := p.parser.parse(p.Text, p.Style)
	p.line = p.line[:0]

	y := yTop - p.Style.SpaceBefore
	var lineW, maxSize float64
	for _, a := range atoms {
		if a.brk {
			adv, y, err = p.emitLine(dst, f, adv, y, avail, lineW, maxSize)
			if err != nil {
				return adv, y, err
			}
			lineW, maxSize = 0, 0
			continue
		}
		wordW := piupage.StringWidth(a.font, b2s(a.word), a.size)
		space := 0.0
		if len(p.line) > 0 {
			space = piupage.StringWidth(a.font, " ", a.size)
		}
		if len(p.line) > 0 && lineW+space+wordW > avail {
			adv, y, err = p.emitLine(dst, f, adv, y, avail, lineW, maxSize)
			if err != nil {
				return adv, y, err
			}
			lineW, maxSize, space = 0, 0, 0
		}
		if space > 0 {
			// The space belongs to a link only when it sits between two words
			// of the same one. Giving it the incoming atom's href would let a
			// link's rectangle swallow the space in front of it.
			var href []byte
			if prev := p.line[len(p.line)-1]; bytes.Equal(prev.href, a.href) {
				href = a.href
			}
			p.line = append(p.line, piece{spaceWord, a.font, a.size, a.col, href})
			lineW += space
		}
		p.line = append(p.line, piece{a.word, a.font, a.size, a.col, a.href})
		lineW += wordW
		if a.size > maxSize {
			maxSize = a.size
		}
	}
	adv, y, err = p.emitLine(dst, f, adv, y, avail, lineW, maxSize)
	if err != nil {
		return adv, y, err
	}
	return adv, y - p.Style.SpaceAfter, nil
}

// emitLine paints the line assembled in p.line at the cursor, moving to the
// next page first if the line no longer fits on this one. It reports the page
// and cursor the paragraph continues from, and empties p.line.
func (p *Paragraph) emitLine(dst []piupage.Canvas, f Frame, adv int, y, avail, lineW, maxSize float64) (int, float64, error) {
	lineH := p.Style.leading()
	// A line that does not fit moves to the next page — unless the cursor is
	// already at the top of one, in which case no amount of breaking will help
	// and the line overflows rather than looping forever.
	if y-lineH < f.Bottom && y < f.Top {
		adv++
		if adv >= len(dst) {
			return adv, y, io.ErrShortBuffer
		}
		y = f.Top
	}
	cv := &dst[adv]
	x0 := f.X + p.Style.LeftIndent
	startX := x0
	switch p.Style.Align {
	case Right:
		startX = x0 + avail - lineW
	case Center:
		startX = x0 + (avail-lineW)/2
	}
	h := maxf(maxSize, p.Style.Size)
	base := y - 0.8*h
	px := startX
	// A link is registered once per maximal run of pieces sharing an href, not
	// once per word-a PDF has no multi-piece annotation, and one object per word
	// would be both wasteful and visibly gappy. A run broken by wrapping ends
	// here and resumes on the next line, which is exactly the rect a reader
	// wants.
	var runHref []byte
	runX := 0.0
	for _, pc := range p.line {
		if !bytes.Equal(pc.href, runHref) {
			p.closeRun(cv, runHref, runX, px, base, h)
			runHref, runX = pc.href, px
		}
		// b2s is safe here and in closeRun for one reason: neither callee keeps
		// the string. Text encodes it into the content stream before it returns
		// and StringWidth only measures it, so no view outlives the call.
		if !isSpaceWord(pc.text) {
			cv.SetFont(pc.font, pc.size)
			cv.Text(px, base, b2s(pc.text), pc.col)
		}
		px += piupage.StringWidth(pc.font, b2s(pc.text), pc.size)
	}
	p.closeRun(cv, runHref, runX, px, base, h)
	p.line = p.line[:0]
	return adv, y - lineH, nil
}

// closeRun registers the linked run spanning [x0, x1) on the line whose
// baseline is at base, and underlines it if the style asks. It is a no-op for
// the unlinked stretches between runs.
func (p *Paragraph) closeRun(cv *piupage.Canvas, href []byte, x0, x1, base, h float64) {
	if len(href) == 0 || x1 <= x0 {
		return
	}
	// The annotation covers the line box the text sits in: 0.8*h of it is above
	// the baseline, matching how base itself was derived.
	cv.Link(x0, base-0.2*h, x1-x0, h, b2s(href))
	if p.Style.Link.Underline {
		col := p.Style.Link.Color
		if col == nil {
			col = p.Style.color()
		}
		cv.Line(x0, base-0.1*h, x1, base-0.1*h, maxf(0.05*h, 0.4), col)
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// atom is one word (or an explicit break) with its resolved style.
type atom struct {
	word []byte
	brk  bool
	font piupage.Font
	size float64
	col  color.Color
	href []byte
}

// spanStyle is the mutable markup state during parsing. Its href is resolved
// once, when the tag opens, and every atom in the span views the same bytes.
type spanStyle struct {
	size float64
	col  color.Color
	href []byte
	bold bool
	ital bool
}

// atomParser turns markup into styled word atoms, owning every buffer a parse
// needs so that parsing again — as every table cell does, once to measure and
// once to draw — writes into what the last parse already allocated.
//
// One rule covers the whole type: what it hands out is a view, and it owns the
// only memory behind those views. Nothing here is ever copied, and nothing here
// survives the next parse.
type atomParser struct {
	atoms []atom
	// esc holds the runs and link targets whose entities had to be resolved,
	// which are the only bytes a parse writes.
	esc []byte
}

// resolve returns v with its entities resolved: v itself when it carries none,
// otherwise a view of the copy made in esc.
func (ap *atomParser) resolve(v []byte) []byte {
	if bytes.IndexByte(v, '&') < 0 {
		return v
	}
	start := len(ap.esc)
	ap.esc = appendUnescaped(ap.esc, b2s(v))
	return ap.esc[start:]
}

// parse scans text's markup subset into styled word atoms. The result is the
// parser's own slice and stays valid until the next call, as do the words in it.
func (ap *atomParser) parse(text []byte, base Style) []atom {
	ap.atoms = ap.atoms[:0]
	ap.esc = ap.esc[:0]
	if bytes.IndexByte(text, '&') >= 0 {
		// Sized once, before a single word can point into it. See atomParser.esc.
		internal.SliceReuse(&ap.esc, len(text))
	}
	family := baseFamily(base.family())
	top := spanStyle{
		size: base.Size,
		col:  base.color(),
		bold: strings.Contains(base.family(), "Bold"),
		ital: strings.Contains(base.family(), "Oblique") || strings.Contains(base.family(), "Italic"),
	}
	var stackArr [8]spanStyle
	stack := append(stackArr[:0], top)

	for i := 0; i < len(text); {
		if text[i] != '<' {
			end := len(text)
			if j := bytes.IndexByte(text[i:], '<'); j >= 0 {
				end = i + j
			}
			ap.appendWords(text[i:end], stack[len(stack)-1], family)
			i = end
			continue
		}
		k := bytes.IndexByte(text[i:], '>')
		if k < 0 {
			// An unterminated tag is not markup, it is text.
			ap.appendWords(text[i:], stack[len(stack)-1], family)
			break
		}
		// The tag is read through both views of itself.
		tagb := bytes.TrimSpace(text[i+1 : i+k])
		tag := b2s(tagb)
		i += k + 1
		switch {
		case tag == "br" || tag == "br/" || tag == "br /":
			ap.atoms = append(ap.atoms, atom{brk: true})
		case tag == "b":
			cur := stack[len(stack)-1]
			cur.bold = true
			stack = append(stack, cur)
		case tag == "i":
			cur := stack[len(stack)-1]
			cur.ital = true
			stack = append(stack, cur)
		case strings.HasPrefix(tag, "font"):
			cur := stack[len(stack)-1]
			if v := attr(tagb, "size"); len(v) > 0 {
				if f, err := strconv.ParseFloat(b2s(v), 64); err == nil {
					cur.size = f
				}
			}
			if v := attr(tagb, "color"); len(v) > 0 {
				cur.col = piupage.HexColor(b2s(v))
			}
			stack = append(stack, cur)
		// <link> is reportlab's spelling of the same thing. "a" is matched
		// exactly rather than by prefix so it cannot swallow another tag that
		// happens to start with it.
		case tag == "a" || strings.HasPrefix(tag, "a ") || strings.HasPrefix(tag, "link"):
			cur := stack[len(stack)-1]
			cur.href = ap.resolve(attr(tagb, "href"))
			if base.Link.Color != nil {
				cur.col = base.Link.Color
			}
			stack = append(stack, cur)
		case tag == "/b" || tag == "/i" || tag == "/font" || tag == "/a" || tag == "/link":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return ap.atoms
}

// appendWords appends each whitespace-separated word of s as an atom styled by cur.
func (ap *atomParser) appendWords(s []byte, cur spanStyle, family string) {
	if bytes.IndexByte(s, '&') >= 0 {
		start := len(ap.esc)
		ap.esc = appendUnescaped(ap.esc, b2s(s))
		s = ap.esc[start:]
	}
	f := resolveFont(family, cur.bold, cur.ital)
	for i := 0; i < len(s); {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		j := i
		for j < len(s) && !isSpace(s[j]) {
			j++
		}
		if j > i {
			ap.atoms = append(ap.atoms, atom{word: s[i:j], font: f, size: cur.size, col: cur.col, href: cur.href})
		}
		i = j
	}
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// appendUnescaped appends s to dst with its entities resolved.
func appendUnescaped(dst []byte, s string) []byte {
	for len(s) > 0 {
		i := strings.IndexByte(s, '&')
		if i < 0 {
			return append(dst, s...)
		}
		dst = append(dst, s[:i]...)
		s = s[i:]
		n, b := entity(s)
		if n == 0 {
			// Not an entity this dialect knows: the ampersand is just text.
			n, b = 1, '&'
		}
		dst = append(dst, b)
		s = s[n:]
	}
	return dst
}

// entity reports the length of the entity at the head of s and the byte it
// resolves to.
func entity(s string) (n int, b byte) {
	switch {
	case strings.HasPrefix(s, "&amp;"):
		return len("&amp;"), '&'
	case strings.HasPrefix(s, "&lt;"):
		return len("&lt;"), '<'
	case strings.HasPrefix(s, "&gt;"):
		return len("&gt;"), '>'
	case strings.HasPrefix(s, "&#160;"):
		return len("&#160;"), ' '
	}
	return 0, 0
}

// baseFamily strips a weight/style suffix, e.g. "Helvetica-Bold" -> "Helvetica".
func baseFamily(name string) string {
	if i := strings.IndexByte(name, '-'); i >= 0 {
		return name[:i]
	}
	return name
}

// resolveFont maps a family plus bold/italic flags to a standard-14 font.
func resolveFont(family string, bold, ital bool) piupage.Font {
	if f, ok := piupage.Standard14(styleName(family, bold, ital)); ok {
		return f
	}
	f, _ := piupage.Standard14("Helvetica")
	return f
}

// styleName is the /BaseFont name for a family at a weight and slant. The names
// are spelled out rather than concatenated so that resolving a font allocates
// nothing.
func styleName(family string, bold, ital bool) string {
	switch family {
	case "Times":
		switch {
		case bold && ital:
			return "Times-BoldItalic"
		case bold:
			return "Times-Bold"
		case ital:
			return "Times-Italic"
		}
		return "Times-Roman"
	case "Courier":
		switch {
		case bold && ital:
			return "Courier-BoldOblique"
		case bold:
			return "Courier-Bold"
		case ital:
			return "Courier-Oblique"
		}
		return "Courier"
	}
	switch {
	case bold && ital:
		return "Helvetica-BoldOblique"
	case bold:
		return "Helvetica-Bold"
	case ital:
		return "Helvetica-Oblique"
	}
	return "Helvetica"
}

// attr extracts name="value" (or name='value') from a tag body as a view of it;
// nil if absent. The name is matched against the tag in place rather than by
// building "name=" to search for, which would allocate on every tag.
func attr(tag []byte, name string) []byte {
	i := indexAttr(tag, name)
	if i < 0 {
		return nil
	}
	rest := tag[i+len(name)+1:]
	if len(rest) == 0 {
		return nil
	}
	q := rest[0]
	if q != '"' && q != '\'' {
		return nil
	}
	j := bytes.IndexByte(rest[1:], q)
	if j < 0 {
		return nil
	}
	return rest[1 : 1+j]
}

// indexAttr finds name followed by '=' in tag, the position attr reads a value
// from; -1 if the tag does not carry it.
func indexAttr(tag []byte, name string) int {
	for i := 0; i+len(name) < len(tag); i++ {
		if tag[i+len(name)] == '=' && b2s(tag[i:i+len(name)]) == name {
			return i
		}
	}
	return -1
}

func b2s(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}
