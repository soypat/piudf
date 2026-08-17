package piudoc

import (
	"io"
	"strings"

	"github.com/soypat/piudf/piupage"
)

// Preformatted builds a block whose lines are drawn as written: text is split at
// newlines and never re-wrapped, which is what source code and tabular output need.
// Give it a ground or a rule by putting it in a one-cell [Table].
func (bld *Builder) Preformatted(text string, s Style) Drawer {
	p := &preformatted{style: s, font: bld.parser.resolveFont(baseFamily(s.family()), false, false)}
	for line := range strings.SplitSeq(text, "\n") {
		p.lines = append(p.lines, strings.TrimSuffix(line, "\r"))
	}
	return p
}

// preformatted is what [Builder.Preformatted] returns. It owns its lines; the
// font is resolved once, at build time, as every other element's is.
type preformatted struct {
	lines []string
	style Style
	font  piupage.Font
}

func (p *preformatted) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	lead := p.style.leading()
	y := yTop - p.style.SpaceBefore
	for _, line := range p.lines {
		if y-lead < f.Bottom && y < f.Top {
			adv++
			if adv >= len(dst) {
				return adv, y, io.ErrShortBuffer
			}
			y = f.Top
		}
		cv := &dst[adv]
		cv.SetFont(p.font, p.style.Size)
		// 0.8 of the line box sits above the baseline, as a Paragraph's does.
		cv.Text(f.X+p.style.LeftIndent, y-0.8*lead, line, p.style.color())
		y -= lead
	}
	return adv, y - p.style.SpaceAfter, nil
}
