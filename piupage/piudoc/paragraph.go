package doc

import (
	"image/color"
	"strconv"
	"strings"

	canvas "github.com/soypat/piudf/piupage"
)

// Paragraph is wrapped rich text. Text may carry a small markup subset —
// <b>, <i>, <br/>, <font size= color=> — with unknown tags ignored. Style
// supplies the defaults; lines are filled by Wrap and consumed by Draw.
type Paragraph struct {
	Text   string
	Style  Style
	lines  []pline
	availW float64
}

// piece is a run of same-style text within a laid-out line.
type piece struct {
	text string
	font canvas.Font
	size float64
	col  color.Color
}

// pline is one laid-out line.
type pline struct {
	pieces  []piece
	width   float64
	maxSize float64
}

// P constructs a Paragraph from text and a style.
func P(text string, s Style) *Paragraph { return &Paragraph{Text: text, Style: s} }

// Wrap parses the markup and greedily breaks the text to availWidth, reporting
// the size the paragraph will occupy (including SpaceBefore/After).
func (p *Paragraph) Wrap(availWidth float64) (w, h float64) {
	p.availW = availWidth
	avail := availWidth - p.Style.LeftIndent - p.Style.RightIndent
	atoms := parseAtoms(p.Text, p.Style)

	var lines []pline
	cur := pline{}
	for _, a := range atoms {
		if a.brk {
			lines = append(lines, cur)
			cur = pline{}
			continue
		}
		wordW := canvas.StringWidth(a.font, a.word, a.size)
		space := 0.0
		if len(cur.pieces) > 0 {
			space = canvas.StringWidth(a.font, " ", a.size)
		}
		if len(cur.pieces) > 0 && cur.width+space+wordW > avail {
			lines = append(lines, cur)
			cur = pline{}
			space = 0
		}
		if space > 0 {
			cur.pieces = append(cur.pieces, piece{" ", a.font, a.size, a.col})
			cur.width += space
		}
		cur.pieces = append(cur.pieces, piece{a.word, a.font, a.size, a.col})
		cur.width += wordW
		if a.size > cur.maxSize {
			cur.maxSize = a.size
		}
	}
	lines = append(lines, cur)
	p.lines = lines

	lineH := p.Style.leading()
	h = float64(len(lines))*lineH + p.Style.SpaceBefore + p.Style.SpaceAfter
	return availWidth, h
}

// Draw paints the wrapped lines with the paragraph's top-left at (x, yTop).
func (p *Paragraph) Draw(c *canvas.Canvas, x, yTop, availWidth float64) {
	if p.lines == nil {
		p.Wrap(availWidth)
	}
	lineH := p.Style.leading()
	x0 := x + p.Style.LeftIndent
	avail := p.availW - p.Style.LeftIndent - p.Style.RightIndent
	y := yTop - p.Style.SpaceBefore
	for _, ln := range p.lines {
		ascent := 0.8 * maxf(ln.maxSize, p.Style.Size)
		base := y - ascent
		startX := x0
		switch p.Style.Align {
		case Right:
			startX = x0 + avail - ln.width
		case Center:
			startX = x0 + (avail-ln.width)/2
		}
		px := startX
		for _, pc := range ln.pieces {
			if pc.text != " " {
				c.SetFont(pc.font, pc.size)
				c.Text(px, base, pc.text, pc.col)
			}
			px += canvas.StringWidth(pc.font, pc.text, pc.size)
		}
		y -= lineH
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
	word string
	brk  bool
	font canvas.Font
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

// parseAtoms scans text's markup subset into styled word atoms.
func parseAtoms(text string, base Style) []atom {
	family := baseFamily(base.family())
	top := spanStyle{
		size: base.Size,
		col:  base.color(),
		bold: strings.Contains(base.family(), "Bold"),
		ital: strings.Contains(base.family(), "Oblique") || strings.Contains(base.family(), "Italic"),
	}
	stack := []spanStyle{top}
	var atoms []atom

	emit := func(s string) {
		cur := stack[len(stack)-1]
		f := resolveFont(family, cur.bold, cur.ital)
		for _, word := range strings.Fields(unescape(s)) {
			atoms = append(atoms, atom{word: word, font: f, size: cur.size, col: cur.col})
		}
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
				cur.col = canvas.HexColor(v)
			}
			stack = append(stack, cur)
		case tag == "/b" || tag == "/i" || tag == "/font":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return atoms
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
func resolveFont(family string, bold, ital bool) canvas.Font {
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
	if f, ok := canvas.Standard14(name); ok {
		return f
	}
	f, _ := canvas.Standard14("Helvetica")
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
