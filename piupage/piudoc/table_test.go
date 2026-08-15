package piudoc

import (
	"bytes"
	"io"
	"testing"

	"github.com/soypat/piudf/piupage"
)

// newCanvases returns n reset pages, as Doc.flow hands to a Drawer.
func newCanvases(t testing.TB, n int) []piupage.Canvas {
	t.Helper()
	dst := make([]piupage.Canvas, n)
	for i := range dst {
		if err := dst[i].Reset(make([]byte, 4096)); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// Normal is 10pt on 12pt leading and cells pad 6pt a side, so a cell of n
// explicit lines is 12n+12 tall. Explicit <br/> keeps the arithmetic
// independent of font metrics.
const lineH, cellPad = 12.0, 6.0

func TestTableRowHeightFromTallestCell(t *testing.T) {
	// The tall cell is last, so a table that sized rows from the first cell it
	// met — or that had to draw cell 0 before measuring cell 2 — would get this
	// wrong.
	tbl := &Table{
		Rows:      [][]Cell{{TextCell("x"), TextCell("y"), TextCell("a<br/>b<br/>c")}},
		ColWidths: []float64{60, 60, 60},
	}
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 180, Top: 500, Bottom: 0}

	adv, yEnd, err := tbl.Draw(dst, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if adv != 0 {
		t.Errorf("advanced %d pages, want 0", adv)
	}
	want := 3*lineH + 2*cellPad
	if got := f.Top - yEnd; got != want {
		t.Errorf("row height = %v, want %v", got, want)
	}
}

func TestTableSplitsBetweenRows(t *testing.T) {
	rowH := 3*lineH + 2*cellPad // 48
	tbl := &Table{
		Rows: [][]Cell{
			{TextCell("a<br/>b<br/>c")},
			{TextCell("d<br/>e<br/>f")},
		},
		ColWidths: []float64{100},
	}
	dst := newCanvases(t, 2)
	// Room for one row only: the second must move to the next page.
	f := Frame{X: 0, Width: 100, Top: 100, Bottom: 20}

	adv, yEnd, err := tbl.Draw(dst, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if adv != 1 {
		t.Fatalf("advanced %d pages, want 1", adv)
	}
	if want := f.Top - rowH; yEnd != want {
		t.Errorf("yEnd = %v, want %v (one row down from the new page's top)", yEnd, want)
	}
	if len(dst[1].Bytes()) == 0 {
		t.Error("second page is empty")
	}
}

func TestTableShortBuffer(t *testing.T) {
	tbl := &Table{
		Rows: [][]Cell{
			{TextCell("a<br/>b<br/>c")},
			{TextCell("d<br/>e<br/>f")},
		},
		ColWidths: []float64{100},
	}
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 100, Top: 100, Bottom: 20}
	if _, _, err := tbl.Draw(dst, f, f.Top); err != io.ErrShortBuffer {
		t.Errorf("err = %v, want io.ErrShortBuffer", err)
	}
}

func TestMeasureLeavesNoTrace(t *testing.T) {
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 200, Top: 500, Bottom: 0}
	p := P("one two three four five six seven eight", Normal)

	dst[0].SetFont(mustFont(t, "Helvetica"), 10)
	dst[0].Text(0, 400, "before", nil)
	want := string(dst[0].Bytes())
	wantFonts := len(dst[0].Fonts())

	h, err := Measure(dst, p, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if h <= 0 {
		t.Errorf("measured height %v, want > 0", h)
	}
	if got := string(dst[0].Bytes()); got != want {
		t.Error("Measure left content on the page")
	}
	if got := len(dst[0].Fonts()); got != wantFonts {
		t.Errorf("Measure left %d fonts, want %d", got, wantFonts)
	}

	// The measurement must match what drawing actually consumes.
	_, yEnd, err := p.Draw(dst, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Top - yEnd; got != h {
		t.Errorf("drawn height %v, measured %v", got, h)
	}
}

func TestParagraphSplitsAcrossPages(t *testing.T) {
	dst := newCanvases(t, 3)
	// Four lines, room for two per page.
	f := Frame{X: 0, Width: 200, Top: 100, Bottom: 100 - 2*lineH}
	p := P("a<br/>b<br/>c<br/>d", Normal)

	adv, yEnd, err := p.Draw(dst, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if adv != 1 {
		t.Fatalf("advanced %d pages, want 1", adv)
	}
	if want := f.Top - 2*lineH; yEnd != want {
		t.Errorf("yEnd = %v, want %v", yEnd, want)
	}
	if len(dst[2].Bytes()) != 0 {
		t.Error("wrote to a page it did not report")
	}
}

func TestDocBuildPages(t *testing.T) {
	var buf bytes.Buffer
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}, Title: "t", Author: "a"}
	story := []Drawer{
		P("first", Heading1),
		Spacer{H: 12},
		HRule{Thickness: 1},
		&Table{
			Rows:      [][]Cell{{TextCell("h1"), TextCell("h2")}, {TextCell("v1"), TextCell("v2")}},
			ColWidths: []float64{100, 100},
		},
		P("last", Normal),
	}
	dst := make([]piupage.Canvas, 4)
	err := d.Build(&buf, dst, story, make([]byte, 4096), make([]byte, 4*1024))
	if err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Errorf("output does not start with a PDF header: %.16q", out)
	}
	if !bytes.Contains(out, []byte("%%EOF")) {
		t.Error("output has no trailer")
	}
	if got := bytes.Count(out, []byte("/Type/Page/Parent")); got != 1 {
		t.Errorf("wrote %d page objects, want 1", got)
	}
	if len(d.pageIDs) != 1 {
		t.Errorf("wrote %d pages, want 1", len(d.pageIDs))
	}
}

func TestDocBuildReuse(t *testing.T) {
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	story := []Drawer{P("hello", Normal)}
	dst := make([]piupage.Canvas, 2)
	var first bytes.Buffer
	for i := range 3 {
		var buf bytes.Buffer
		err := d.Build(&buf, dst, story, make([]byte, 4096), make([]byte, 2*1024))
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		if i == 0 {
			first = buf
		} else if !bytes.Equal(first.Bytes(), buf.Bytes()) {
			t.Fatalf("build %d differs from the first: a Reset is incomplete", i)
		}
	}
}

func mustFont(t testing.TB, name string) piupage.Font {
	t.Helper()
	f, ok := piupage.Standard14(name)
	if !ok {
		t.Fatalf("no font %s", name)
	}
	return f
}
