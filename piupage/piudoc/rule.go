package doc

import canvas "github.com/soypat/piudf/piupage"

// Spacer is vertical blank space between flowables.
type Spacer struct {
	H float64
}

// Wrap reports the fixed height of the spacer.
func (s Spacer) Wrap(availWidth float64) (w, h float64) { return availWidth, s.H }

// Draw paints nothing; the spacer only consumes vertical space.
func (s Spacer) Draw(c *canvas.Canvas, x, yTop float64) {}

// HRule is a horizontal rule, the reportlab HRFlowable analogue.
type HRule struct {
	Width       float64 // 0 or negative means the full available width
	Thickness   float64
	Color       canvas.Color
	SpaceBefore float64
	SpaceAfter  float64
}

// Wrap reports the rule's height including its surrounding space.
func (h HRule) Wrap(availWidth float64) (w, height float64) { panic("todo: HRule.Wrap") }

// Draw strokes the rule.
func (h HRule) Draw(c *canvas.Canvas, x, yTop float64) { panic("todo: HRule.Draw") }
