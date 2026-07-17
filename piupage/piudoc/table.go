package doc

import canvas "github.com/soypat/piudf/piupage"

// Cell is one table cell: either a bare string (styled by the table default) or
// an embedded Flowable such as a Paragraph or nested Table.
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

// tableOp is one styling directive over a cell range. -1 in a range coordinate
// means the last column/row (reportlab convention). f/col/a/v carry whichever
// arguments the kind uses.
type tableOp struct {
	kind           tableOpKind
	c0, r0, c1, r1 int
	f              float64 // padding component or line width
	pl, pr, pt, pb float64 // padding (opPad)
	col            canvas.Color
	a              Align
	v              VAlign
}

// TableStyle is a typed builder of per-cell-range formatting. Its methods chain
// and apply in insertion order.
type TableStyle struct {
	ops []tableOp
}

// Align sets horizontal alignment over the cell range.
func (ts *TableStyle) Align(c0, r0, c1, r1 int, a Align) *TableStyle {
	panic("todo: TableStyle.Align")
}

// Valign sets vertical alignment over the cell range.
func (ts *TableStyle) Valign(c0, r0, c1, r1 int, v VAlign) *TableStyle {
	panic("todo: TableStyle.Valign")
}

// Pad sets cell padding (left, right, top, bottom) over the cell range.
func (ts *TableStyle) Pad(c0, r0, c1, r1 int, left, right, top, bottom float64) *TableStyle {
	panic("todo: TableStyle.Pad")
}

// Background fills the cell range with col.
func (ts *TableStyle) Background(c0, r0, c1, r1 int, col canvas.Color) *TableStyle {
	panic("todo: TableStyle.Background")
}

// LineBelow strokes a line under each row in the range.
func (ts *TableStyle) LineBelow(c0, r0, c1, r1 int, w float64, col canvas.Color) *TableStyle {
	panic("todo: TableStyle.LineBelow")
}

// LineAbove strokes a line above each row in the range.
func (ts *TableStyle) LineAbove(c0, r0, c1, r1 int, w float64, col canvas.Color) *TableStyle {
	panic("todo: TableStyle.LineAbove")
}

// Box strokes the outer border of the range.
func (ts *TableStyle) Box(c0, r0, c1, r1 int, w float64, col canvas.Color) *TableStyle {
	panic("todo: TableStyle.Box")
}

// Grid strokes both the box and every interior cell edge of the range.
func (ts *TableStyle) Grid(c0, r0, c1, r1 int, w float64, col canvas.Color) *TableStyle {
	panic("todo: TableStyle.Grid")
}

// Table lays cells on a fixed column grid; row heights derive from content.
type Table struct {
	Rows      [][]Cell
	ColWidths []float64 // points
	Style     TableStyle
	heights   []float64 // filled by Wrap
}

// Wrap measures row heights against the column grid and reports the table size.
func (t *Table) Wrap(availWidth float64) (w, h float64) { panic("todo: Table.Wrap") }

// Draw paints backgrounds, cell content and lines with the top-left at (x,yTop).
func (t *Table) Draw(c *canvas.Canvas, x, yTop float64) { panic("todo: Table.Draw") }
