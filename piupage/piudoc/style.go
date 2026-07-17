// Package doc is the flowable layer of a reportlab-style report generator: a
// story of Paragraph/Table/Spacer/HRule flowed top-to-bottom into pages and
// written as a PDF. It is the platypus analogue built on the imperative
// github.com/soypat/piudf/canvas surface and the piudf ppdf.Encoder.
package doc

import canvas "github.com/soypat/piudf/piupage"

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
// paragraph or table cell.
type Style struct {
	Font        string // canvas /BaseFont name; "" inherits the caller default
	Size        float64
	Leading     float64 // baseline-to-baseline; 0 means 1.2*Size
	Color       canvas.Color
	Align       Align
	SpaceBefore float64
	SpaceAfter  float64
	LeftIndent  float64
	RightIndent float64
}

// Predefined styles, the getSampleStyleSheet analogue. TODO: assign real
// defaults (fonts/sizes/leading) once metrics land.
var (
	Normal   Style
	BodyText Style
	Title    Style
	Heading1 Style
)
