package piudoc

import (
	"image/color"

	"github.com/soypat/piudf/piupage"
)

// Cell is one table cell: either a bare string (styled by the table's default
// cell style) or an embedded Flowable such as a Paragraph or nested Table.
type Cell struct {
	Text string
	Flow Flowable
}

// TextCell wraps a string as a cell.
func TextCell(s string) Cell { return Cell{Text: s} }

// FlowCell wraps a flowable as a cell.
func FlowCell(f Flowable) Cell { return Cell{Flow: f} }

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

// Table lays cells on a fixed column grid; row heights derive from content.
type Table struct {
	Rows      [][]Cell
	ColWidths []float64 // points
	Style     TableStyle
	// CellStyle is the default text style for bare-string cells.
	CellStyle Style
	// RepeatRows is how many leading rows are a header: a table split across
	// pages reprints them at the top of each continuation, so the reader is
	// never looking at a column of values whose heading is a page behind.
	RepeatRows int
	heights    []float64
}

const defaultCellPad = 6

// Wrap measures row heights against the column grid and reports the table size.
func (t *Table) Wrap(availWidth float64) (w, h float64) {
	t.heights = make([]float64, len(t.Rows))
	var total, width float64
	for _, cw := range t.ColWidths {
		width += cw
	}
	for r := range t.Rows {
		var rowH float64
		for ci := range t.Rows[r] {
			if ci >= len(t.ColWidths) {
				break
			}
			pl, pr, pt, pb := t.padding(ci, r)
			fl := t.cellFlow(ci, r)
			_, ch := fl.Wrap(t.ColWidths[ci] - pl - pr)
			if hh := ch + pt + pb; hh > rowH {
				rowH = hh
			}
		}
		t.heights[r] = rowH
		total += rowH
	}
	return width, total
}

// Draw paints backgrounds, cell content and lines with the top-left at (x,yTop).
func (t *Table) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {
	if t.heights == nil {
		t.Wrap(availWidth)
	}
	// Column left edges and row top edges.
	colLeft := make([]float64, len(t.ColWidths)+1)
	colLeft[0] = x
	for i, cw := range t.ColWidths {
		colLeft[i+1] = colLeft[i] + cw
	}
	rowTop := make([]float64, len(t.Rows)+1)
	rowTop[0] = yTop
	for r := range t.Rows {
		rowTop[r+1] = rowTop[r] - t.heights[r]
	}

	// Pass 1: backgrounds.
	for r := range t.Rows {
		for ci := range t.Rows[r] {
			if ci >= len(t.ColWidths) {
				break
			}
			if col, ok := t.background(ci, r); ok {
				c.FillRect(colLeft[ci], rowTop[r+1], t.ColWidths[ci], t.heights[r], col)
			}
		}
	}

	// Pass 2: content.
	for r := range t.Rows {
		for ci := range t.Rows[r] {
			if ci >= len(t.ColWidths) {
				break
			}
			pl, pr, pt, pb := t.padding(ci, r)
			cw := t.ColWidths[ci] - pl - pr
			fl := t.cellFlow(ci, r)
			_, ch := fl.Wrap(cw)
			inner := t.heights[r] - pt - pb
			top := rowTop[r] - pt
			switch t.valign(ci, r) {
			case Middle:
				top -= (inner - ch) / 2
			case Bottom:
				top -= inner - ch
			}
			fl.Draw(c, colLeft[ci]+pl, top, cw)
		}
	}

	// Pass 3: lines.
	for _, op := range t.Style.ops {
		switch op.kind {
		case opLineBelow, opLineAbove, opBox, opGrid:
		default:
			continue
		}
		rc0, rr0 := resolveIdx(op.c0, len(t.ColWidths)), resolveIdx(op.r0, len(t.Rows))
		rc1, rr1 := resolveIdx(op.c1, len(t.ColWidths)), resolveIdx(op.r1, len(t.Rows))
		lw := op.f
		if lw <= 0 {
			lw = 0.5
		}
		switch op.kind {
		case opLineBelow:
			for r := rr0; r <= rr1; r++ {
				c.Line(colLeft[rc0], rowTop[r+1], colLeft[rc1+1], rowTop[r+1], lw, op.col)
			}
		case opLineAbove:
			for r := rr0; r <= rr1; r++ {
				c.Line(colLeft[rc0], rowTop[r], colLeft[rc1+1], rowTop[r], lw, op.col)
			}
		case opBox, opGrid:
			left, right := colLeft[rc0], colLeft[rc1+1]
			topY, botY := rowTop[rr0], rowTop[rr1+1]
			c.Line(left, topY, right, topY, lw, op.col)
			c.Line(left, botY, right, botY, lw, op.col)
			c.Line(left, topY, left, botY, lw, op.col)
			c.Line(right, topY, right, botY, lw, op.col)
			if op.kind == opGrid {
				for cc := rc0 + 1; cc <= rc1; cc++ {
					c.Line(colLeft[cc], topY, colLeft[cc], botY, lw, op.col)
				}
				for r := rr0 + 1; r <= rr1; r++ {
					c.Line(left, rowTop[r], right, rowTop[r], lw, op.col)
				}
			}
		}
	}
}

// SplitAt divides the table at a row boundary so its head fits in availHeight,
// reprinting the header rows atop the tail. A nil head means not even one body
// row fits and the table should start on the next page.
func (t *Table) SplitAt(availWidth, availHeight float64) (head, tail Flowable) {
	t.Wrap(availWidth)
	var used float64
	for _, h := range t.heights[:t.repeat()] {
		used += h
	}
	// k is the first row that does not fit; every row before it is drawn now.
	k := t.repeat()
	for k < len(t.Rows) && used+t.heights[k] <= availHeight {
		used += t.heights[k]
		k++
	}
	if k >= len(t.Rows) || k == t.repeat() {
		return nil, t
	}
	return t.slice(0, k, false), t.slice(k, len(t.Rows), true)
}

// repeat is the header row count, clamped to the rows that exist.
func (t *Table) repeat() int {
	return min(max(t.RepeatRows, 0), len(t.Rows))
}

// slice builds the table over rows [lo,hi), optionally prefixed by the repeated
// header, with every style op rewritten onto the new row numbering.
func (t *Table) slice(lo, hi int, withHeader bool) *Table {
	rep := 0
	if withHeader {
		rep = t.repeat()
	}
	rows := make([][]Cell, 0, rep+hi-lo)
	rows = append(rows, t.Rows[:rep]...)
	rows = append(rows, t.Rows[lo:hi]...)
	out := &Table{
		Rows:       rows,
		ColWidths:  t.ColWidths,
		CellStyle:  t.CellStyle,
		RepeatRows: rep,
	}
	for _, op := range t.Style.ops {
		out.Style.ops = append(out.Style.ops, t.remapOp(op, lo, hi, rep)...)
	}
	return out
}

// remapOp rewrites one op's row range for a slice holding rep header rows
// followed by the original rows [lo,hi). An op may survive as two: one over the
// reprinted header and one over the body it originally covered.
func (t *Table) remapOp(op tableOp, lo, hi, rep int) []tableOp {
	r0, r1 := resolveIdx(op.r0, len(t.Rows)), resolveIdx(op.r1, len(t.Rows))
	if r0 > r1 {
		r0, r1 = r1, r0
	}
	var out []tableOp
	emit := func(a, b int) {
		if a > b {
			return
		}
		o := op
		o.r0, o.r1 = a, b
		out = append(out, o)
	}
	if rep > 0 {
		emit(max(r0, 0), min(r1, rep-1))
	}
	// Body rows keep their relative order, shifted to sit after the header.
	if a, b := max(r0, lo), min(r1, hi-1); a <= b {
		emit(a-lo+rep, b-lo+rep)
	}
	return out
}

// cellFlow returns the flowable for cell (ci,r): its embedded Flowable, or a
// Paragraph built from its text with the resolved alignment.
func (t *Table) cellFlow(ci, r int) Flowable {
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
	return P(cell.Text, st)
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
