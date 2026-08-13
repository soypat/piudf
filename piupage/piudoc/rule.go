package piudoc

import (
	"image/color"

	"github.com/soypat/piudf/piupage"
)

// Spacer is vertical blank space between flowables.
type Spacer struct {
	H float64
}

// Wrap reports the fixed height of the spacer.
func (s Spacer) Wrap(availWidth float64) (w, h float64) { return availWidth, s.H }

// Draw paints nothing; the spacer only consumes vertical space.
func (s Spacer) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {}

// HRule is a horizontal rule, the reportlab HRFlowable analogue.
type HRule struct {
	Width       float64 // 0 or negative means the full available width
	Thickness   float64
	Color       color.Color
	SpaceBefore float64
	SpaceAfter  float64
}

// Wrap reports the rule's height including its surrounding space.
func (h HRule) Wrap(availWidth float64) (w, height float64) {
	t := h.Thickness
	if t < 0 {
		t = 0 // PDF's hairline, the one width that is unit-independent.
	}
	return availWidth, h.SpaceBefore + t + h.SpaceAfter
}

// Draw strokes the rule across the available width at (x, yTop).
func (h HRule) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {
	t := h.Thickness
	if t < 0 {
		t = 0 // PDF's hairline, the one width that is unit-independent.
	}
	w := h.Width
	if w <= 0 {
		w = availWidth
	}
	col := color.Color(color.Black)
	if h.Color != nil {
		col = h.Color
	}
	y := yTop - h.SpaceBefore - t/2
	c.Line(x, y, x+w, y, t, col)
}
