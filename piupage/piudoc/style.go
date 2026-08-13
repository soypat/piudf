// package piudoc is the flowable layer of a reportlab-style report generator: a
// story of Paragraph/Table/Spacer/HRule flowed top-to-bottom into pages and
// written as a PDF. It is the platypus analogue built on the imperative
// github.com/soypat/piudf/piupage surface and the piudf Encoder.
package piudoc

import (
	"image/color"

	"github.com/soypat/piudf/piupage"
)

// Family is the set of faces a Style draws from: the markup's <b> and <i>
// choose among them, so a document names its typography once and the paragraph
// layer never has to know which faces are built in and which are embedded.
// A nil face falls back to the nearest one that is set, and a Family with no
// faces at all falls back to the standard-14 lookup by [Style.Font] name.
type Family struct {
	Regular, Bold, Italic, BoldItalic piupage.Font
}

// face returns the family member for the requested weight and slope.
func (fam *Family) face(bold, ital bool) piupage.Font {
	if fam == nil {
		return nil
	}
	var f piupage.Font
	switch {
	case bold && ital:
		f = or(fam.BoldItalic, fam.Bold, fam.Italic)
	case bold:
		f = or(fam.Bold, fam.BoldItalic)
	case ital:
		f = or(fam.Italic, fam.BoldItalic)
	}
	return or(f, fam.Regular, fam.Bold, fam.Italic, fam.BoldItalic)
}

// or returns the first non-nil font.
func or(fonts ...piupage.Font) piupage.Font {
	for _, f := range fonts {
		if f != nil {
			return f
		}
	}
	return nil
}

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
	// Face is the typeface this style draws with. When nil, Font names one of
	// the standard-14 built-ins instead.
	Face *Family
	Font string // canvas /BaseFont family, e.g. "Helvetica"; "" => Helvetica
	// Bold and Italic select the weight and slope the style starts in, the
	// same state <b> and <i> would put it in. They are how a Face-based style
	// says "set this bold", since with a Family the face is chosen by the
	// markup rather than named in Font.
	Bold    bool
	Italic  bool
	Size    float64
	Leading float64
	// Unit is what Size, Leading and the spacings below are expressed in. It
	// is set by In, and is consulted only to resolve a pt-suffixed size in
	// markup — everything else here is already in it. The zero value is Pt.
	Unit        piupage.Unit
	Color       color.Color
	Align       Align
	SpaceBefore float64
	SpaceAfter  float64
	LeftIndent  float64
	RightIndent float64
	// LinkColor paints <a href> spans; nil leaves them in the body color.
	LinkColor color.Color
	// LinkUnderline rules a line under <a href> spans.
	LinkUnderline bool
}

// In converts s, a style whose lengths are in points, to the unit u. It is how
// a point-flavoured style — the predefined ones below, or one a caller wrote
// out in the units type is normally set in — is used in a document measured in
// something else.
func (s Style) In(u piupage.Unit) Style {
	s.Unit = u
	s.Size = u.FromPt(s.Size)
	s.Leading = u.FromPt(s.Leading)
	s.SpaceBefore = u.FromPt(s.SpaceBefore)
	s.SpaceAfter = u.FromPt(s.SpaceAfter)
	s.LeftIndent = u.FromPt(s.LeftIndent)
	s.RightIndent = u.FromPt(s.RightIndent)
	return s
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
