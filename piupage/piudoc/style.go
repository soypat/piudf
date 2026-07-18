// package piudoc is the flowable layer of a reportlab-style report generator: a
// story of Paragraph/Table/Spacer/HRule flowed top-to-bottom into pages and
// written as a PDF. It is the platypus analogue built on the imperative
// github.com/soypat/piudf/piupage surface and the piudf Encoder.
package piudoc

import "image/color"

// Align is horizontal text/content alignment.
type Align uint8

const (
	Left Align = iota
	Center
	Right
	Justify
)

// VAlign is vertical alignment within a table cell.
type VAlign uint8

const (
	Top VAlign = iota
	Middle
	Bottom
)

// Style mirrors reportlab's ParagraphStyle: the default text formatting for a
// paragraph or table cell. A zero Color means black; a zero Leading means
// 1.2*Size.
type Style struct {
	Font        string // canvas /BaseFont family, e.g. "Helvetica"; "" => Helvetica
	Size        float64
	Leading     float64
	Color       color.Color
	Align       Align
	SpaceBefore float64
	SpaceAfter  float64
	LeftIndent  float64
	RightIndent float64
}

// leading returns the effective baseline-to-baseline distance.
func (s Style) leading() float64 {
	if s.Leading > 0 {
		return s.Leading
	}
	return 1.2 * s.Size
}

// family returns the base font family, defaulting to Helvetica.
func (s Style) family() string {
	if s.Font == "" {
		return "Helvetica"
	}
	return s.Font
}

// color returns the style's text color, defaulting to black.
func (s Style) color() color.Color {
	if s.Color == nil {
		return color.Black
	}
	return s.Color
}

// Predefined styles, the getSampleStyleSheet analogue.
var (
	Normal   = Style{Font: "Helvetica", Size: 10, Leading: 12}
	BodyText = Style{Font: "Helvetica", Size: 10, Leading: 14}
	Title    = Style{Font: "Helvetica-Bold", Size: 18, Leading: 22}
	Heading1 = Style{Font: "Helvetica-Bold", Size: 14, Leading: 18}
)
