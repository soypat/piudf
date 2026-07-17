package doc

import canvas "github.com/soypat/piudf/piupage"

// run is one span of uniformly styled text within a paragraph, produced by the
// markup scanner.
type run struct {
	text  string
	font  canvas.Font
	size  float64
	color canvas.Color
	bold  bool
	ital  bool
}

// line is a laid-out row of runs with its measured extents.
type line struct {
	runs    []run
	w       float64
	ascent  float64
	descent float64
}

// Paragraph is wrapped rich text. Text may carry a small markup subset —
// <b>, <i>, <br/>, <font size= color=> — with unknown tags passed through
// literally. Style supplies the defaults; runs and lines are filled by Wrap.
type Paragraph struct {
	Text  string
	Style Style
	runs  []run
	lines []line
}

// P constructs a Paragraph from text and a style.
func P(text string, s Style) *Paragraph { return &Paragraph{Text: text, Style: s} }

// Wrap parses the markup and breaks the text to availWidth, reporting the size
// the paragraph will occupy.
func (p *Paragraph) Wrap(availWidth float64) (w, h float64) { panic("todo: Paragraph.Wrap") }

// Draw paints the wrapped lines with the paragraph's top-left at (x, yTop).
func (p *Paragraph) Draw(c *canvas.Canvas, x, yTop float64) { panic("todo: Paragraph.Draw") }
