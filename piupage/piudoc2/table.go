package piudoc2

import (
	"image/color"
	"io"

	"github.com/soypat/piudf/piupage"
)

// Cell is one table cell: either a bare string (styled by the table's default
// cell style) or an embedded Drawer such as a Paragraph or nested Table.
type Cell struct {
	Text string
	Flow Drawer
}

// TextCell wraps a string as a cell.
func TextCell(s string) Cell { return Cell{Text: s} }

// FlowCell wraps a drawer as a cell.
func FlowCell(d Drawer) Cell { return Cell{Flow: d} }

// tableOpKind enumerates the typed TableStyle operations.
type tableOpKind uint8

const (
	opAlign tableOpKind = iota
	opValign
	opPad
	opBackground
	opLineBelow
	opLineAbove
	opBox
	opGrid
)

// tableOp is one styling directive over a cell range. A -1 range coordinate
// means the last column/row (reportlab convention).
type tableOp struct {
	kind           tableOpKind
	c0, r0, c1, r1 int
	f              float64 // line width (line ops)
	pl, pr, pt, pb float64 // padding (opPad)
	col            color.Color
	a              Align
	v              VAlign
}

// TableStyle is a typed builder of per-cell-range formatting. Its methods chain
// and apply in insertion order; later directives override earlier ones.
type TableStyle struct {
	ops []tableOp
}

func (ts *TableStyle) Reset() { ts.ops = ts.ops[:0] }

// Align sets horizontal alignment over the cell range.
func (ts *TableStyle) Align(c0, r0, c1, r1 int, a Align) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opAlign, c0: c0, r0: r0, c1: c1, r1: r1, a: a})
	return ts
}

// Valign sets vertical alignment over the cell range.
func (ts *TableStyle) Valign(c0, r0, c1, r1 int, v VAlign) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opValign, c0: c0, r0: r0, c1: c1, r1: r1, v: v})
	return ts
}

// Pad sets cell padding (left, right, top, bottom) over the cell range.
func (ts *TableStyle) Pad(c0, r0, c1, r1 int, left, right, top, bottom float64) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opPad, c0: c0, r0: r0, c1: c1, r1: r1, pl: left, pr: right, pt: top, pb: bottom})
	return ts
}

// Background fills the cell range with col.
func (ts *TableStyle) Background(c0, r0, c1, r1 int, col color.Color) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opBackground, c0: c0, r0: r0, c1: c1, r1: r1, col: col})
	return ts
}

// LineBelow strokes a line under each row in the range.
func (ts *TableStyle) LineBelow(c0, r0, c1, r1 int, w float64, col color.Color) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opLineBelow, c0: c0, r0: r0, c1: c1, r1: r1, f: w, col: col})
	return ts
}

// LineAbove strokes a line above each row in the range.
func (ts *TableStyle) LineAbove(c0, r0, c1, r1 int, w float64, col color.Color) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opLineAbove, c0: c0, r0: r0, c1: c1, r1: r1, f: w, col: col})
	return ts
}

// Box strokes the outer border of the range.
func (ts *TableStyle) Box(c0, r0, c1, r1 int, w float64, col color.Color) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opBox, c0: c0, r0: r0, c1: c1, r1: r1, f: w, col: col})
	return ts
}

// Grid strokes both the box and every interior cell edge of the range.
func (ts *TableStyle) Grid(c0, r0, c1, r1 int, w float64, col color.Color) *TableStyle {
	ts.ops = append(ts.ops, tableOp{kind: opGrid, c0: c0, r0: r0, c1: c1, r1: r1, f: w, col: col})
	return ts
}

// Table lays cells on a fixed column grid; row heights derive from content. A
// table that outgrows its page continues on the next one, breaking between rows.
type Table struct {
	Rows      [][]Cell
	ColWidths []float64 // points
	Style     TableStyle
	// CellStyle is the default text style for bare-string cells.
	CellStyle Style
	// cell is the paragraph a bare-string cell is drawn as, reused across every
	// such cell so that a table of text allocates nothing.
	cell Paragraph
}

const defaultCellPad = 6

// Draw paints the table one row at a time, moving to the next page whenever the
// row at hand no longer fits.
//
// A row's height is the tallest of its cells, and it must be known before any
// of them is painted: the row's background goes down first, and a cell's
// vertical alignment is measured against it. Since a content stream paints in
// order and cannot be reordered after the fact, each cell is drawn onto the page
// and retracted with [Measure] to learn its extent, then drawn again where the
// finished row geometry puts it.
func (t *Table) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	ncol := len(t.ColWidths)
	if ncol == 0 {
		return 0, yTop, nil
	}
	y := yTop
	for r := range t.Rows {
		rowH, err := t.rowHeight(dst[adv:], f, r)
		if err != nil {
			return adv, y, err
		}
		// A row taller than a whole page cannot be helped by breaking, so it
		// overflows rather than looping onto page after page.
		if y-rowH < f.Bottom && y < f.Top {
			adv++
			if adv >= len(dst) {
				return adv, y, io.ErrShortBuffer
			}
			y = f.Top
		}
		err = t.drawRow(dst[adv:], f, r, y, rowH)
		if err != nil {
			return adv, y, err
		}
		y -= rowH
	}
	return adv, y, nil
}

// rowHeight measures row r: the tallest cell in it, padding included.
func (t *Table) rowHeight(dst []piupage.Canvas, f Frame, r int) (rowH float64, err error) {
	for ci := range t.Rows[r] {
		if ci >= len(t.ColWidths) {
			break
		}
		pl, pr, pt, pb := t.padding(ci, r)
		h, err := Measure(dst, t.cellDrawer(ci, r), t.cellFrame(f, ci, pl, pr), f.Top)
		if err != nil {
			return 0, err
		}
		if hh := h + pt + pb; hh > rowH {
			rowH = hh
		}
	}
	return rowH, nil
}

// drawRow paints row r onto dst[0] with its top edge at top: backgrounds first,
// then cell content, then the rules that cross it.
func (t *Table) drawRow(dst []piupage.Canvas, f Frame, r int, top, rowH float64) error {
	cv := &dst[0]
	rowBot := top - rowH

	x := f.X
	for ci := range t.Rows[r] {
		if ci >= len(t.ColWidths) {
			break
		}
		if col, ok := t.background(ci, r); ok {
			cv.FillRect(x, rowBot, t.ColWidths[ci], rowH, col)
		}
		x += t.ColWidths[ci]
	}

	x = f.X
	for ci := range t.Rows[r] {
		if ci >= len(t.ColWidths) {
			break
		}
		pl, pr, pt, pb := t.padding(ci, r)
		cf := t.cellFrame(f, ci, pl, pr)
		cf.X = x + pl
		dr := t.cellDrawer(ci, r)
		cellTop := top - pt
		// Only a vertically aligned cell needs its own height; a top-aligned one
		// starts at the row's top edge whatever its extent.
		if v := t.valign(ci, r); v != Top {
			h, err := Measure(dst, dr, cf, cellTop)
			if err != nil {
				return err
			}
			inner := rowH - pt - pb
			if v == Middle {
				cellTop -= (inner - h) / 2
			} else {
				cellTop -= inner - h
			}
		}
		if _, _, err := dr.Draw(dst[:1], cf, cellTop); err != nil {
			return err
		}
		x += t.ColWidths[ci]
	}

	t.drawRules(cv, f, r, top, rowBot)
	return nil
}

// drawRules strokes the parts of the table's line directives that fall on row r.
// Working a row at a time is what lets a box or grid spanning a page break come
// out closed on both pages.
func (t *Table) drawRules(cv *piupage.Canvas, f Frame, r int, top, bot float64) {
	ncol, nrow := len(t.ColWidths), len(t.Rows)
	for _, op := range t.Style.ops {
		switch op.kind {
		case opLineBelow, opLineAbove, opBox, opGrid:
		default:
			continue
		}
		c0, c1 := resolveIdx(op.c0, ncol), resolveIdx(op.c1, ncol)
		r0, r1 := resolveIdx(op.r0, nrow), resolveIdx(op.r1, nrow)
		if c0 > c1 {
			c0, c1 = c1, c0
		}
		if r0 > r1 {
			r0, r1 = r1, r0
		}
		if r < r0 || r > r1 {
			continue
		}
		lw := op.f
		if lw <= 0 {
			lw = 0.5
		}
		left, right := t.colLeft(f, c0), t.colLeft(f, c1+1)
		switch op.kind {
		case opLineBelow:
			cv.Line(left, bot, right, bot, lw, op.col)
		case opLineAbove:
			cv.Line(left, top, right, top, lw, op.col)
		case opBox, opGrid:
			if r == r0 {
				cv.Line(left, top, right, top, lw, op.col)
			}
			if r == r1 {
				cv.Line(left, bot, right, bot, lw, op.col)
			}
			cv.Line(left, top, left, bot, lw, op.col)
			cv.Line(right, top, right, bot, lw, op.col)
			if op.kind == opGrid {
				for cc := c0 + 1; cc <= c1; cc++ {
					x := t.colLeft(f, cc)
					cv.Line(x, top, x, bot, lw, op.col)
				}
				if r > r0 {
					cv.Line(left, top, right, top, lw, op.col)
				}
			}
		}
	}
}

// cellFrame is the box a cell's content is laid out in: its column less its
// horizontal padding, and unbounded vertically — a cell is measured against the
// row it will size, never against the page, so it must not try to break.
func (t *Table) cellFrame(f Frame, ci int, pl, pr float64) Frame {
	return Frame{
		X:      t.colLeft(f, ci) + pl,
		Width:  t.ColWidths[ci] - pl - pr,
		Top:    f.Top,
		Bottom: noBreak,
	}
}

// colLeft returns the x of column ci's left edge, ci == len(ColWidths) being the
// table's right edge. It is walked rather than tabulated so that drawing a table
// needs no scratch of its own.
func (t *Table) colLeft(f Frame, ci int) float64 {
	x := f.X
	for i := 0; i < ci && i < len(t.ColWidths); i++ {
		x += t.ColWidths[i]
	}
	return x
}

// cellDrawer returns the drawer for cell (ci,r): its embedded Drawer, or the
// table's reusable paragraph loaded with the cell's text.
func (t *Table) cellDrawer(ci, r int) Drawer {
	cell := t.Rows[r][ci]
	if cell.Flow != nil {
		if p, ok := cell.Flow.(*Paragraph); ok {
			p.Style.Align = t.align(ci, r)
		}
		return cell.Flow
	}
	st := t.CellStyle
	if st.Size == 0 {
		st = Normal
	}
	st.Align = t.align(ci, r)
	t.cell.Text = cell.Text
	t.cell.Style = st
	return &t.cell
}

// padding resolves the cell's padding, defaulting to defaultCellPad on each side.
func (t *Table) padding(ci, r int) (pl, pr, pt, pb float64) {
	pl, pr, pt, pb = defaultCellPad, defaultCellPad, defaultCellPad, defaultCellPad
	for _, op := range t.Style.ops {
		if op.kind == opPad && t.inRange(op, ci, r) {
			pl, pr, pt, pb = op.pl, op.pr, op.pt, op.pb
		}
	}
	return
}

func (t *Table) align(ci, r int) Align {
	a := Left
	for _, op := range t.Style.ops {
		if op.kind == opAlign && t.inRange(op, ci, r) {
			a = op.a
		}
	}
	return a
}

func (t *Table) valign(ci, r int) VAlign {
	v := Top
	for _, op := range t.Style.ops {
		if op.kind == opValign && t.inRange(op, ci, r) {
			v = op.v
		}
	}
	return v
}

func (t *Table) background(ci, r int) (color.Color, bool) {
	var col color.Color
	var ok bool
	for _, op := range t.Style.ops {
		if op.kind == opBackground && t.inRange(op, ci, r) {
			col, ok = op.col, true
		}
	}
	return col, ok
}

// inRange reports whether cell (ci,r) falls in op's resolved range.
func (t *Table) inRange(op tableOp, ci, r int) bool {
	c0 := resolveIdx(op.c0, len(t.ColWidths))
	c1 := resolveIdx(op.c1, len(t.ColWidths))
	r0 := resolveIdx(op.r0, len(t.Rows))
	r1 := resolveIdx(op.r1, len(t.Rows))
	if c0 > c1 {
		c0, c1 = c1, c0
	}
	if r0 > r1 {
		r0, r1 = r1, r0
	}
	return ci >= c0 && ci <= c1 && r >= r0 && r <= r1
}

// resolveIdx maps a possibly-negative index (-1 = last) to an absolute one.
func resolveIdx(v, n int) int {
	if v < 0 {
		return n + v
	}
	return v
}
