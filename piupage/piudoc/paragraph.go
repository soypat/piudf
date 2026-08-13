package piudoc

import (
	"image/color"
	"strconv"
	"strings"

	"github.com/soypat/piudf/piupage"
)

// Paragraph is wrapped rich text. Text may carry a small markup subset —
// <b>, <i>, <br/>, <font size= color=>, <a href=> — with unknown tags ignored.
// Style supplies the defaults; lines are filled by Wrap and consumed by Draw.
type Paragraph struct {
	Text   string
	Style  Style
	lines  []pline
	availW float64
	// laid marks lines as authoritative: a paragraph produced by SplitAt
	// carries its share of an already-wrapped parent and must not re-parse
	// Text, which no longer describes it.
	laid bool
}

// piece is a run of same-style text within a laid-out line.
type piece struct {
	text string
	font piupage.Font
	size float64
	col  color.Color
	href string
}

// pline is one laid-out line.
type pline struct {
	pieces  []piece
	width   float64
	maxSize float64
	// brk marks a line ended by an explicit <br/> rather than by filling up,
	// which justification must not stretch.
	brk bool
}

// P constructs a Paragraph from text and a style.
func P(text string, s Style) *Paragraph { return &Paragraph{Text: text, Style: s} }

// Wrap parses the markup and greedily breaks the text to availWidth, reporting
// the size the paragraph will occupy (including SpaceBefore/After).
func (p *Paragraph) Wrap(availWidth float64) (w, h float64) {
	if p.laid && p.availW == availWidth {
		return availWidth, p.height()
	}
	p.availW = availWidth
	avail := availWidth - p.Style.LeftIndent - p.Style.RightIndent
	atoms := parseAtoms(p.Text, p.Style)

	var lines []pline
	cur := pline{}
	for i := 0; i < len(atoms); {
		if atoms[i].brk {
			cur.brk = true
			lines = append(lines, cur)
			cur = pline{}
			i++
			continue
		}
		// A cluster is one atom plus every atom glued to it. Markup can end
		// mid-word — "<a>lneto</a>, the" is one word and a comma — and the
		// cluster is what keeps that from becoming two words with a space
		// between them, or from being broken over two lines.
		j := i + 1
		for j < len(atoms) && !atoms[j].brk && atoms[j].glue {
			j++
		}
		var clusterW float64
		for _, a := range atoms[i:j] {
			clusterW += piupage.StringWidth(a.font, a.word, a.size)
		}
		space := 0.0
		if len(cur.pieces) > 0 {
			space = piupage.StringWidth(atoms[i].font, " ", atoms[i].size)
		}
		if len(cur.pieces) > 0 && cur.width+space+clusterW > avail {
			lines = append(lines, cur)
			cur = pline{}
			space = 0
		}
		if space > 0 {
			a := atoms[i]
			cur.pieces = append(cur.pieces, piece{text: " ", font: a.font, size: a.size, col: a.col})
			cur.width += space
		}
		for _, a := range atoms[i:j] {
			cur.pieces = append(cur.pieces, piece{text: a.word, font: a.font, size: a.size, col: a.col, href: a.href})
			if a.size > cur.maxSize {
				cur.maxSize = a.size
			}
		}
		cur.width += clusterW
		i = j
	}
	lines = append(lines, cur)
	p.lines = lines
	return availWidth, p.height()
}

// height is the wrapped paragraph's occupied height, surrounding space included.
func (p *Paragraph) height() float64 {
	return float64(len(p.lines))*p.Style.leading() + p.Style.SpaceBefore + p.Style.SpaceAfter
}

// SplitAt divides the paragraph so its head fits in availHeight, keeping at
// least two lines on each side of the break: a lone line stranded at a page
// boundary is a widow, and a paragraph is better moved whole than orphaned.
// A nil head means the paragraph does not usefully split here and should move
// to the next page intact.
func (p *Paragraph) SplitAt(availWidth, availHeight float64) (head, tail Flowable) {
	p.Wrap(availWidth)
	const minLines = 2
	lineH := p.Style.leading()
	fits := int((availHeight - p.Style.SpaceBefore) / lineH)
	if fits < minLines || len(p.lines)-fits < minLines {
		return nil, p
	}
	return p.slice(0, fits, true, false), p.slice(fits, len(p.lines), false, true)
}

// slice builds a paragraph over lines[i:j], keeping the parent's space only on
// the side that is still an outer edge of the original paragraph.
func (p *Paragraph) slice(i, j int, keepBefore, keepAfter bool) *Paragraph {
	st := p.Style
	if !keepBefore {
		st.SpaceBefore = 0
	}
	if !keepAfter {
		st.SpaceAfter = 0
	}
	return &Paragraph{Style: st, lines: p.lines[i:j], availW: p.availW, laid: true}
}

// Draw paints the wrapped lines with the paragraph's top-left at (x, yTop).
func (p *Paragraph) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {
	if p.lines == nil {
		p.Wrap(availWidth)
	}
	lineH := p.Style.leading()
	x0 := x + p.Style.LeftIndent
	avail := p.availW - p.Style.LeftIndent - p.Style.RightIndent
	y := yTop - p.Style.SpaceBefore
	for i, ln := range p.lines {
		ascent := 0.8 * maxf(ln.maxSize, p.Style.Size)
		base := y - ascent
		startX, stretch := x0, 0.0
		switch p.Style.Align {
		case Right:
			startX = x0 + avail - ln.width
		case Center:
			startX = x0 + (avail-ln.width)/2
		case Justify:
			// The last line of a paragraph, and any line ended by <br/>, set
			// flush left: stretching them is the classic justification bug.
			if gaps := ln.spaces(); gaps > 0 && !ln.brk && i < len(p.lines)-1 {
				stretch = (avail - ln.width) / float64(gaps)
			}
		}
		px := startX
		for _, pc := range ln.pieces {
			w := piupage.StringWidth(pc.font, pc.text, pc.size)
			if pc.text == " " {
				px += w + stretch
				continue
			}
			c.SetFont(pc.font, pc.size)
			c.Text(px, base, pc.text, pc.col)
			if pc.href != "" {
				// The clickable box spans the line's full leading, so a link
				// is as easy to hit as the text is to read.
				c.Link(px, base-0.25*pc.size, w, 1.15*pc.size, pc.href)
				if p.Style.LinkUnderline {
					uy := base - 0.11*pc.size
					c.Line(px, uy, px+w, uy, 0.055*pc.size, pc.col)
				}
			}
			px += w
		}
		y -= lineH
	}
}

// spaces counts the stretchable inter-word gaps on the line.
func (ln pline) spaces() int {
	n := 0
	for _, pc := range ln.pieces {
		if pc.text == " " {
			n++
		}
	}
	return n
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// atom is one word (or an explicit break) with its resolved style.
type atom struct {
	word string
	brk  bool
	// glue marks an atom that had no whitespace between it and its
	// predecessor in the source, and so must not be separated from it.
	glue bool
	font piupage.Font
	size float64
	col  color.Color
	href string
}

// spanStyle is the mutable markup state during parsing.
type spanStyle struct {
	size float64
	col  color.Color
	href string
	bold bool
	ital bool
}

// parseSpanSize resolves a <font size> attribute against enclosing, the size
// the span is nested in, and u, the unit the paragraph's style is written in.
//
// A bare number is a length in the document's unit, like every other length in
// this package. Since that unit may be millimetres, in which a readable type
// size is an unreadable number, two relative forms — "85%" and "0.85em", both
// ratios of the enclosing size — and one absolute escape, "8pt", let markup be
// written without knowing what the document is measured in.
func parseSpanSize(v string, enclosing float64, u piupage.Unit) (float64, bool) {
	if u == 0 {
		u = piupage.Pt
	}
	num := func(s string) (float64, bool) {
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil && f >= 0
	}
	switch {
	case strings.HasSuffix(v, "%"):
		f, ok := num(strings.TrimSuffix(v, "%"))
		return enclosing * f / 100, ok
	case strings.HasSuffix(v, "em"):
		f, ok := num(strings.TrimSuffix(v, "em"))
		return enclosing * f, ok
	case strings.HasSuffix(v, "pt"):
		f, ok := num(strings.TrimSuffix(v, "pt"))
		return u.FromPt(f), ok
	}
	return num(v)
}

// parseAtoms scans text's markup subset into styled word atoms.
func parseAtoms(text string, base Style) []atom {
	family := baseFamily(base.family())
	top := spanStyle{
		size: base.Size,
		col:  base.color(),
		bold: base.Bold || strings.Contains(base.family(), "Bold"),
		ital: base.Italic || strings.Contains(base.family(), "Oblique") || strings.Contains(base.family(), "Italic"),
	}
	stack := []spanStyle{top}
	var atoms []atom
	// openedOnSpace tracks whether the text so far ended in whitespace. A tag
	// is not a word boundary, so whether the word after one is a new word or
	// the tail of the last is decided by the source's spacing, not by the tag.
	openedOnSpace := true

	emit := func(s string) {
		if s == "" {
			return
		}
		cur := stack[len(stack)-1]
		f := base.Face.face(cur.bold, cur.ital)
		if f == nil {
			f = resolveFont(family, cur.bold, cur.ital)
		}
		leads := isSpaceByte(s[0])
		for i, word := range strings.Fields(unescape(s)) {
			glue := i == 0 && !leads && !openedOnSpace && len(atoms) > 0 && !atoms[len(atoms)-1].brk
			atoms = append(atoms, atom{word: word, glue: glue, font: f, size: cur.size, col: cur.col, href: cur.href})
		}
		openedOnSpace = isSpaceByte(s[len(s)-1])
	}

	for i := 0; i < len(text); {
		if text[i] != '<' {
			j := strings.IndexByte(text[i:], '<')
			if j < 0 {
				emit(text[i:])
				break
			}
			emit(text[i : i+j])
			i += j
			continue
		}
		k := strings.IndexByte(text[i:], '>')
		if k < 0 {
			emit(text[i:])
			break
		}
		tag := strings.TrimSpace(text[i+1 : i+k])
		i += k + 1
		switch {
		case tag == "br" || tag == "br/" || tag == "br /":
			atoms = append(atoms, atom{brk: true})
			openedOnSpace = true
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
			if v := attr(tag, "size"); v != "" {
				if f, ok := parseSpanSize(v, cur.size, base.Unit); ok {
					cur.size = f
				}
			}
			if v := attr(tag, "color"); v != "" {
				cur.col = piupage.HexColor(v)
			}
			stack = append(stack, cur)
		case strings.HasPrefix(tag, "a "), tag == "a":
			cur := stack[len(stack)-1]
			cur.href = unescape(attr(tag, "href"))
			if base.LinkColor != nil {
				cur.col = base.LinkColor
			}
			stack = append(stack, cur)
		case tag == "/b" || tag == "/i" || tag == "/font" || tag == "/a":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return atoms
}

// isSpaceByte reports whether b is one of the ASCII spaces strings.Fields
// splits on, which is all the whitespace this markup admits.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// unescape resolves the handful of XML entities the markup may carry.
func unescape(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&#160;", " ")
	return r.Replace(s)
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
	var name string
	switch family {
	case "Times":
		switch {
		case bold && ital:
			name = "Times-BoldItalic"
		case bold:
			name = "Times-Bold"
		case ital:
			name = "Times-Italic"
		default:
			name = "Times-Roman"
		}
	case "Courier":
		name = styleName("Courier", bold, ital)
	default:
		name = styleName("Helvetica", bold, ital)
	}
	if f, ok := piupage.Standard14(name); ok {
		return f
	}
	f, _ := piupage.Standard14("Helvetica")
	return f
}

func styleName(base string, bold, ital bool) string {
	switch {
	case bold && ital:
		return base + "-BoldOblique"
	case bold:
		return base + "-Bold"
	case ital:
		return base + "-Oblique"
	}
	return base
}

// attr extracts name="value" (or name='value') from a tag body; "" if absent.
func attr(tag, name string) string {
	i := strings.Index(tag, name+"=")
	if i < 0 {
		return ""
	}
	rest := tag[i+len(name)+1:]
	if len(rest) == 0 {
		return ""
	}
	q := rest[0]
	if q != '"' && q != '\'' {
		return ""
	}
	j := strings.IndexByte(rest[1:], q)
	if j < 0 {
		return ""
	}
	return rest[1 : 1+j]
}
