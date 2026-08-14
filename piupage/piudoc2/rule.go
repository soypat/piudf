package piudoc2

import (
	"image/color"
	"io"

	"github.com/soypat/piudf/piupage"
)

// Spacer is vertical blank space between elements.
type Spacer struct {
	H float64
}

// Draw paints nothing; the spacer only consumes vertical space. Space that
// would run past the bottom of the page is dropped rather than carried onto the
// next one, so a spacer never forces a page of its own.
func (s Spacer) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	y := yTop - s.H
	if y < f.Bottom {
		y = f.Bottom
	}
	return 0, y, nil
}

// HRule is a horizontal rule, the reportlab HRFlowable analogue.
type HRule struct {
	Width       float64 // 0 or negative means the full available width
	Thickness   float64
	Color       color.Color
	SpaceBefore float64
	SpaceAfter  float64
}

// Draw strokes the rule across the available width, moving to the next page if
// it does not fit on this one.
func (h HRule) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	t := h.Thickness
	if t <= 0 {
		t = 1
	}
	y := yTop
	if y-(h.SpaceBefore+t+h.SpaceAfter) < f.Bottom && y < f.Top {
		adv++
		if adv >= len(dst) {
			return adv, y, io.ErrShortBuffer
		}
		y = f.Top
	}
	w := h.Width
	if w <= 0 {
		w = f.Width
	}
	col := color.Color(color.Black)
	if h.Color != nil {
		col = h.Color
	}
	ruleY := y - h.SpaceBefore - t/2
	dst[adv].Line(f.X, ruleY, f.X+w, ruleY, t, col)
	return adv, y - (h.SpaceBefore + t + h.SpaceAfter), nil
}
