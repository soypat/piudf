package piudoc

import (
	"bytes"
	"strings"
	"testing"

	"github.com/soypat/piudf/piupage"
)

var testStyle = Style{Font: "Helvetica", Size: 10, Leading: 12}

// TestInlineTagIsNotAWordBoundary pins the rule that a tag boundary carries no
// space of its own: punctuation after a link is part of the word before it.
func TestInlineTagIsNotAWordBoundary(t *testing.T) {
	// The laid-out text is compared rather than the width: <b> legitimately
	// changes the face, and a bold glyph is wider. What must not change is
	// where the spaces are.
	for _, tc := range []struct{ marked, want string }{
		{`<a href="u">lneto</a>, the stack`, "lneto, the stack"},
		{`<b>bold</b>face word`, "boldface word"},
		{`no <i>time.Now()</i>, no sleep`, "no time.Now(), no sleep"},
		{`a <b>b</b> c`, "a b c"},
		{`spaced <b>out</b> here`, "spaced out here"},
		{`<font color="#ff0000">red</font>ish`, "redish"},
	} {
		p := P(tc.marked, testStyle)
		p.Wrap(400)
		if got := laidOutText(p); got != tc.want {
			t.Errorf("%q laid out as %q, want %q", tc.marked, got, tc.want)
		}
	}
}

// TestSameFaceMarkupPreservesMetrics checks that markup which does not change
// the face does not change the measurements either.
func TestSameFaceMarkupPreservesMetrics(t *testing.T) {
	marked := P(`I am the creator of <a href="u">lneto</a>, the only maintained stack.`, testStyle)
	plain := P(`I am the creator of lneto, the only maintained stack.`, testStyle)
	_, mh := marked.Wrap(160)
	_, ph := plain.Wrap(160)
	if mh != ph {
		t.Errorf("linked text wrapped to height %v, plain to %v", mh, ph)
	}
	if got, want := lineWidths(marked), lineWidths(plain); !equalf(got, want) {
		t.Errorf("linked line widths %v, want %v", got, want)
	}
}

// TestClusterDoesNotBreakAcrossLines checks that the word a tag splits stays
// whole: "lneto" and its comma must land on the same line.
func TestClusterDoesNotBreakAcrossLines(t *testing.T) {
	p := P(`aaa bbb <a href="u">ccccc</a>, ddd`, testStyle)
	// A width that lands the break right at the linked word.
	p.Wrap(52)
	for _, ln := range p.lines {
		var text string
		for _, pc := range ln.pieces {
			text += pc.text
		}
		if strings.HasSuffix(strings.TrimSpace(text), "ccccc") {
			t.Fatalf("line %q ends with the linked word, orphaning its comma", text)
		}
	}
}

func TestParagraphSplitKeepsTwoLinesEachSide(t *testing.T) {
	p := P(strings.Repeat("word ", 60), testStyle)
	_, h := p.Wrap(200)
	if len(p.lines) < 6 {
		t.Fatalf("test needs a multi-line paragraph, got %d lines", len(p.lines))
	}

	head, tail := p.SplitAt(200, h/2)
	if head == nil {
		t.Fatal("paragraph refused to split at half its height")
	}
	_, hh := head.Wrap(200)
	_, th := tail.Wrap(200)
	if hh > h/2 {
		t.Errorf("head height %v exceeds the %v it was given", hh, h/2)
	}
	if got := hh + th; got != h {
		t.Errorf("split heights sum to %v, want the original %v", got, h)
	}

	// Too little room for two lines: the paragraph moves rather than widowing.
	if head, _ := p.SplitAt(200, 13); head != nil {
		t.Error("paragraph split off a single line instead of moving whole")
	}
}

func TestTableSplitRepeatsHeaderRows(t *testing.T) {
	rows := make([][]Cell, 0, 9)
	rows = append(rows, []Cell{TextCell("Project"), TextCell("What it is")})
	for i := range 8 {
		rows = append(rows, []Cell{TextCell("name"), TextCell(strings.Repeat("x", i+1))})
	}
	tb := &Table{Rows: rows, ColWidths: []float64{100, 100}, CellStyle: testStyle, RepeatRows: 1}
	_, h := tb.Wrap(200)

	head, tail := tb.SplitAt(200, h/2)
	if head == nil {
		t.Fatal("table refused to split")
	}
	ht, tt := head.(*Table), tail.(*Table)
	if ht.Rows[0][0].Text != "Project" {
		t.Error("head lost the header row")
	}
	if tt.Rows[0][0].Text != "Project" {
		t.Error("tail did not reprint the header row")
	}
	// Every body row survives exactly once across the two halves; the -1
	// discounts the header the tail reprints.
	if got, want := len(ht.Rows)+len(tt.Rows)-1, len(rows); got != want {
		t.Errorf("split covers %d distinct rows, want the original %d", got, want)
	}
	if _, hh := head.Wrap(200); hh > h/2 {
		t.Errorf("head height %v exceeds the %v it was given", hh, h/2)
	}
}

// TestTableSplitRemapsStyleRanges checks that a style op lands on the same
// cells after the split as it did before: row indices are relative to a table
// whose rows have moved.
func TestTableSplitRemapsStyleRanges(t *testing.T) {
	rows := make([][]Cell, 6)
	for i := range rows {
		rows[i] = []Cell{TextCell("a"), TextCell("b")}
	}
	tb := &Table{Rows: rows, ColWidths: []float64{50, 50}, CellStyle: testStyle}
	tb.Style.Align(0, 4, -1, 5, Right) // the last two rows only
	_, h := tb.Wrap(100)

	_, tail := tb.SplitAt(100, h/2)
	tt := tail.(*Table)
	tt.Wrap(100)
	// Rows 4 and 5 of the original are the last two rows of the tail.
	for r := range tt.Rows {
		want := Left
		if r >= len(tt.Rows)-2 {
			want = Right
		}
		if got := tt.align(0, r); got != want {
			t.Errorf("tail row %d alignment = %v, want %v", r, got, want)
		}
	}
}

// TestKeepTogetherMovesRatherThanOrphan covers the group's whole reason to
// exist: it never leaves a heading behind on its own.
func TestKeepTogetherMovesRatherThanOrphan(t *testing.T) {
	heading := P("A heading", Style{Font: "Helvetica", Size: 14, Leading: 18})
	para := P(strings.Repeat("word ", 40), testStyle)
	k := &KeepTogether{Flows: []Flowable{heading, para}}
	k.Wrap(200)

	// Room for the heading and one line: not enough to leave anything worth
	// reading under it, so the group moves.
	if head, _ := k.SplitAt(200, 30); head != nil {
		t.Error("group split leaving the heading with less than two lines")
	}
	// Room for the heading and most of the paragraph: the paragraph breaks and
	// the heading stays with the part that fits.
	_, ph := para.Wrap(200)
	head, tail := k.SplitAt(200, 18+ph/2)
	if head == nil {
		t.Fatal("group refused to split where the paragraph could break")
	}
	if _, hh := head.Wrap(200); hh > 18+ph/2 {
		t.Error("group head overflows the height it was given")
	}
	if tail == nil {
		t.Fatal("group split produced no tail")
	}
}

// TestBuildFlowsAcrossPages exercises the frame end to end and checks the
// finished file is a PDF with as many pages as the story needed.
func TestBuildFlowsAcrossPages(t *testing.T) {
	var buf bytes.Buffer
	d := New(&buf, A4, Margins{Left: 50, Right: 50, Top: 50, Bottom: 50})
	d.Title = "Flow — test"
	var pages []PageInfo
	d.OnPage = func(_ *piupage.Canvas, p PageInfo) { pages = append(pages, p) }

	story := []Flowable{Bookmark{Title: "Top"}}
	for range 12 {
		story = append(story, P(strings.Repeat("word ", 120), testStyle), Spacer{H: 10})
	}
	story = append(story, PageBreak{}, P("last page", testStyle))
	if err := d.Build(story); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) {
		t.Fatal("output is not a PDF")
	}
	if len(pages) < 2 {
		t.Fatalf("story of %d flowables produced %d pages", len(story), len(pages))
	}
	for i, p := range pages {
		if p.Num != i+1 || p.Total != len(pages) {
			t.Errorf("page %d reported as %d of %d", i+1, p.Num, p.Total)
		}
	}
	// The title carries an em dash, so it must go out as UTF-16, not as raw
	// UTF-8 that a viewer would read as PDFDocEncoding.
	if bytes.Contains(buf.Bytes(), []byte("Flow — test")) {
		t.Error("non-ASCII title written as raw UTF-8")
	}
	if !bytes.Contains(buf.Bytes(), []byte("/Outlines")) {
		t.Error("document with a bookmark has no outline")
	}
}

// laidOutText reassembles a wrapped paragraph's pieces, joining lines with a
// space so the result reads as the source text did.
func laidOutText(p *Paragraph) string {
	var lines []string
	for _, ln := range p.lines {
		var b strings.Builder
		for _, pc := range ln.pieces {
			b.WriteString(pc.text)
		}
		lines = append(lines, b.String())
	}
	return strings.Join(lines, " ")
}

func lineWidths(p *Paragraph) []float64 {
	out := make([]float64, len(p.lines))
	for i, ln := range p.lines {
		out[i] = ln.width
	}
	return out
}

func equalf(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
