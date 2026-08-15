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

// LinkStyle is how an <a href> span is drawn. A PDF link annotation paints
// nothing of its own, so without a LinkStyle a link is invisible but clickable.
// The zero value inherits the enclosing span's color and draws no rule, which
// leaves the annotation purely behavioural.
type LinkStyle struct {
	// Color overrides the span's text color; nil inherits it.
	Color color.Color
	// Underline draws a rule just under the baseline, the width of the linked
	// run on each line it wraps to.
	Underline bool
}

// Style is the default text formatting for a
// paragraph or table cell. A zero Color means black; a zero Leading means 1.2*Size.
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
	Link        LinkStyle
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
