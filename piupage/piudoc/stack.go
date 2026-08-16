package piudoc

import (
	"io"

	"github.com/soypat/piudf/piupage"
)

// Stack composes elements to be drawn one above another in whatever frame the
// stack is given.
func (bld *Builder) Stack(elems ...Drawer) Drawer { return stack(elems) }

// stack is what [Builder.Stack] returns.
type stack []Drawer

func (s stack) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	y := yTop
	for _, d := range s {
		if d == nil {
			continue
		}
		if adv >= len(dst) {
			return adv, y, io.ErrShortBuffer
		}
		n, yn, err := d.Draw(dst[adv:], f, y)
		adv, y = adv+n, yn
		if err != nil {
			return adv, y, err
		}
	}
	return adv, y, nil
}

func (s stack) Unwrap() []Drawer { return s }

// ColStyle is how a row of columns is laid out.
type ColStyle struct {
	// Widths are the column widths in points. A zero entry takes an equal share
	// of whatever the fixed ones leave, and nil Widths splits the frame evenly —
	// which is what lets a Columns sit inside a cell whose width it cannot know.
	Widths []float64
	// Valign is where a cell shorter than its row sits within it.
	Valign VAlign
	// Pad is the space around every cell's content. The zero value is none,
	// unlike a bare [Table], whose default every caller turns off.
	Pad Padding
}

// Columns draws cells side by side, the one-row case of a [Table]. It is the
// other half of [Builder.Stack]: stack for down the page, columns for across it.
func (bld *Builder) Columns(cs ColStyle, cells ...Drawer) Drawer {
	row := make([]Cell, len(cells))
	for i, d := range cells {
		row[i] = Cell{Drawer: d}
	}
	t := &Table{Rows: [][]Cell{row}, ColWidths: cs.Widths}
	t.Style.Valign(0, 0, -1, -1, cs.Valign).
		Pad(0, 0, -1, -1, cs.Pad.Left, cs.Pad.Right, cs.Pad.Top, cs.Pad.Bottom)
	return t
}
