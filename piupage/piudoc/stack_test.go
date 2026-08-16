package piudoc

import (
	"strings"
	"testing"
)

func TestStackDrawsInOrder(t *testing.T) {
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 400, Top: 500, Bottom: 0}
	s := bld.Stack(
		bld.Text("first", Normal),
		Spacer{H: 20},
		bld.Text("second", Normal),
	)
	adv, yEnd, err := s.Draw(dst, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if adv != 0 {
		t.Errorf("advanced %d pages, want 0", adv)
	}
	// Two one-line paragraphs at 12pt leading with a 20pt spacer between them.
	if want := f.Top - (2*lineH + 20); yEnd != want {
		t.Errorf("yEnd = %v, want %v", yEnd, want)
	}
	out := string(dst[0].Bytes())
	if i, j := strings.Index(out, "first"), strings.Index(out, "second"); i < 0 || j < 0 || i > j {
		t.Errorf("elements did not draw in order: first at %d, second at %d", i, j)
	}
}

// A stack is a group, so what it carries has to stay visible to the document
// without drawing it — otherwise a bookmark inside one is lost.
func TestStackUnwrapsForBookmarks(t *testing.T) {
	s := bld.Stack(bld.Text("body", Normal), bld.Bookmark("Chapter", 0))
	u, ok := s.(Unwrappable)
	if !ok {
		t.Fatal("a stack does not unwrap")
	}
	got := collectBookmarks(u, nil)
	if len(got) != 1 || got[0].Title != "Chapter" {
		t.Fatalf("bookmarks found: %+v", got)
	}
}

func TestColumnsSplitsTheFrameEvenly(t *testing.T) {
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 400, Top: 500, Bottom: 0}
	// No widths at all: the columns divide the frame they are given.
	c := bld.Columns(ColStyle{Valign: Top},
		bld.P(`<a href="a">left</a>`, Normal),
		bld.P(`<a href="b">right</a>`, Normal),
	)
	if _, _, err := c.Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	links := dst[0].Links()
	if len(links) != 2 {
		t.Fatalf("got %d links, want one per column", len(links))
	}
	if links[0].X != 0 {
		t.Errorf("left column starts at %v, want 0", links[0].X)
	}
	if want := f.Width / 2; links[1].X < want {
		t.Errorf("right column starts at %v, want at least %v", links[1].X, want)
	}
}

func TestColumnsZeroWidthTakesTheRemainder(t *testing.T) {
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 400, Top: 500, Bottom: 0}
	// 100pt fixed, then whatever is left — the arithmetic the caller used to do.
	c := bld.Columns(ColStyle{Widths: []float64{100, 0}},
		bld.Text("fixed", Normal),
		bld.P(`<a href="b">free</a>`, Normal),
	)
	if _, _, err := c.Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	links := dst[0].Links()
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	if want := 100.0; links[0].X < want {
		t.Errorf("free column starts at %v, want at or after %v", links[0].X, want)
	}
}

// The failure this exists to prevent: a table inside a cell, with no widths of
// its own, used to compute its cell frames from an empty ColWidths and wrap
// every line to nothing — drawing a page that looks fine and has no text on it.
func TestNestedTableInheritsItsWidth(t *testing.T) {
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 400, Top: 500, Bottom: 0}
	inner := &Table{Rows: [][]Cell{{bld.TextCell("inner text")}}}
	outer := &Table{
		Rows:      [][]Cell{{bld.Cell(inner)}},
		ColWidths: []float64{400},
	}
	adv, yEnd, err := outer.Draw(dst, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if adv != 0 {
		t.Errorf("advanced %d pages, want 0", adv)
	}
	if h := f.Top - yEnd; h <= 0 {
		t.Errorf("nested table took %v of height, so it drew nothing", h)
	}
	if !strings.Contains(string(dst[0].Bytes()), "inner") {
		t.Error("the nested table wrapped its text away")
	}
}
