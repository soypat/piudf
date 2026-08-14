package piudoc

import (
	"image/color"
	"io"
	"strconv"
	"strings"

	"github.com/soypat/piudf/piupage"
)

// Paragraph is wrapped rich text. Text may carry a small markup subset —
// <b>, <i>, <br/>, <font size= color=> — with unknown tags ignored. Style
// supplies the defaults.
type Paragraph struct {
	Text  string
	Style Style
	// atoms and line are scratch reused across draws: the words the markup
	// parses to, and the one line currently being filled.
	atoms []atom
	line  []piece
}

// piece is a run of same-style text within a laid-out line.
type piece struct {
	text string
	font piupage.Font
	size float64
	col  color.Color
}

// P constructs a Paragraph from text and a style.
func P(text string, s Style) *Paragraph { return &Paragraph{Text: text, Style: s} }

// Draw breaks the text greedily to the frame's width and paints it from yTop
// down, continuing onto further pages as it fills them.
func (p *Paragraph) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	avail := f.Width - p.Style.LeftIndent - p.Style.RightIndent
	if avail <= 0 {
		return 0, yTop, nil
	}
	p.atoms = parseAtoms(p.Text, p.Style, p.atoms[:0])
	p.line = p.line[:0]

	y := yTop - p.Style.SpaceBefore
	var lineW, maxSize float64
	for _, a := range p.atoms {
		if a.brk {
			adv, y, err = p.emitLine(dst, f, adv, y, avail, lineW, maxSize)
			if err != nil {
				return adv, y, err
			}
			lineW, maxSize = 0, 0
			continue
		}
		wordW := piupage.StringWidth(a.font, a.word, a.size)
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
			p.line = append(p.line, piece{" ", a.font, a.size, a.col})
			lineW += space
		}
		p.line = append(p.line, piece{a.word, a.font, a.size, a.col})
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
	base := y - 0.8*maxf(maxSize, p.Style.Size)
	px := startX
	for _, pc := range p.line {
		if pc.text != " " {
			cv.SetFont(pc.font, pc.size)
			cv.Text(px, base, pc.text, pc.col)
		}
		px += piupage.StringWidth(pc.font, pc.text, pc.size)
	}
	p.line = p.line[:0]
	return adv, y - lineH, nil
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
	font piupage.Font
	size float64
	col  color.Color
}

// spanStyle is the mutable markup state during parsing.
type spanStyle struct {
	size float64
	col  color.Color
	bold bool
	ital bool
}

// parseAtoms scans text's markup subset into styled word atoms, appending to
// dst so a caller can reuse its buffer. The style stack is kept in a local
// array and the words are substrings of text, so a paragraph whose markup
// nests no deeper than eight spans parses without allocating.
func parseAtoms(text string, base Style, dst []atom) []atom {
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
			if j := strings.IndexByte(text[i:], '<'); j >= 0 {
				end = i + j
			}
			dst = appendWords(dst, text[i:end], stack[len(stack)-1], family)
			i = end
			continue
		}
		k := strings.IndexByte(text[i:], '>')
		if k < 0 {
			// An unterminated tag is not markup, it is text.
			dst = appendWords(dst, text[i:], stack[len(stack)-1], family)
			break
		}
		tag := strings.TrimSpace(text[i+1 : i+k])
		i += k + 1
		switch {
		case tag == "br" || tag == "br/" || tag == "br /":
			dst = append(dst, atom{brk: true})
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
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					cur.size = f
				}
			}
			if v := attr(tag, "color"); v != "" {
				cur.col = piupage.HexColor(v)
			}
			stack = append(stack, cur)
		case tag == "/b" || tag == "/i" || tag == "/font":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return dst
}

// appendWords appends each whitespace-separated word of s as an atom styled by
// cur. The words are substrings of s, so text carrying no entity to unescape
// costs one atom per word and nothing else.
func appendWords(dst []atom, s string, cur spanStyle, family string) []atom {
	s = unescape(s)
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
			dst = append(dst, atom{word: s[i:j], font: f, size: cur.size, col: cur.col})
		}
		i = j
	}
	return dst
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// entities resolves the handful of XML entities the markup may carry. It is
// stateless and shared: a Replacer is safe for concurrent use.
var entities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&#160;", " ")

// unescape resolves s's entities, returning s itself when it carries none.
func unescape(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	return entities.Replace(s)
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
