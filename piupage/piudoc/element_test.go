package piudoc

import (
	"strings"
	"testing"

	"github.com/soypat/piudf/piupage"
)

// frame returns a page-sized frame and the canvases to draw it on.
func frame(t testing.TB, pages int) ([]piupage.Canvas, Frame) {
	t.Helper()
	dst := make([]piupage.Canvas, pages)
	for i := range dst {
		if err := dst[i].Reset(make([]byte, 4096)); err != nil {
			t.Fatal(err)
		}
	}
	return dst, Frame{X: 72, Width: 400, Top: 700, Bottom: 100}
}

// A group that fits below the cursor stays where it is; one that does not moves
// whole, rather than leaving its first element behind on the page before.
func TestKeepTogetherMovesWhole(t *testing.T) {
	var bld Builder
	st := Style{Size: 10, Leading: 12}
	group := func() Drawer {
		return bld.KeepTogether(bld.Text("heading", st), Spacer{H: 40}, bld.Text("body", st))
	}

	dst, f := frame(t, 2)
	if adv, _, err := group().Draw(dst, f, f.Top-200); err != nil || adv != 0 {
		t.Errorf("a group with room to spare advanced %d pages, err %v", adv, err)
	}

	// 30pt of room left, against a group needing about 64.
	dst, f = frame(t, 2)
	adv, yEnd, err := group().Draw(dst, f, f.Bottom+30)
	if err != nil {
		t.Fatal(err)
	}
	if adv != 1 {
		t.Errorf("a group that did not fit advanced %d pages, want 1", adv)
	}
	if yEnd > f.Top {
		t.Errorf("the group ended at %v, above the next page's top %v", yEnd, f.Top)
	}
	// Nothing may be drawn on the page it left.
	if n := strings.Count(string(dst[0].Bytes()), "Tj"); n != 0 {
		t.Errorf("the group left %d runs on the page it moved off", n)
	}
}

// A plain Stack breaks where it lies: the same content, unkept, stays put.
func TestStackDoesNotKeep(t *testing.T) {
	var bld Builder
	st := Style{Size: 10, Leading: 12}
	dst, f := frame(t, 2)
	s := bld.Stack(bld.Text("heading", st), Spacer{H: 40}, bld.Text("body", st))
	if _, _, err := s.Draw(dst, f, f.Bottom+30); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(dst[0].Bytes()), "Tj"); n == 0 {
		t.Error("a plain stack moved off the page instead of breaking on it")
	}
}

// A group taller than any page cannot be helped by moving, so it draws where it
// stands rather than skipping a page to no purpose.
func TestKeepTogetherOverflowsRatherThanLoops(t *testing.T) {
	var bld Builder
	st := Style{Size: 10, Leading: 12}
	dst, f := frame(t, 2)
	tall := bld.KeepTogether(bld.Text("head", st), Spacer{H: f.Height() + 200}, bld.Text("tail", st))
	if _, _, err := tall.Draw(dst, f, f.Top-50); err != nil {
		t.Fatal(err)
	}
	// Its head belongs on the page the cursor was on: skipping ahead would cost
	// a blank page and still not make the group fit.
	if n := strings.Count(string(dst[0].Bytes()), "Tj"); n != 1 {
		t.Errorf("the page the group started on carries %d runs, want its head", n)
	}
}

// Preformatted draws what it was given: one line per newline, no re-wrapping,
// however far past the frame's width a line runs.
func TestPreformattedKeepsItsLines(t *testing.T) {
	var bld Builder
	dst, f := frame(t, 2)
	const src = "func main() {\n\tprintln(\"a line far longer than the frame is wide, wrapped by nothing at all\")\n}"
	p := bld.Preformatted(src, Style{Size: 9, Leading: 12})
	_, yEnd, err := p.Draw(dst, f, f.Top)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(dst[0].Bytes()), "Tj"); n != 3 {
		t.Errorf("drew %d runs, want one per source line (3)", n)
	}
	if got, want := f.Top-yEnd, 36.0; got != want {
		t.Errorf("consumed %v, want %v — three lines at its leading", got, want)
	}
}

// The marker sits in the gutter and the content in the rest, so an item that
// wraps aligns under itself rather than under its own bullet.
func TestBulletIndentsContent(t *testing.T) {
	var bld Builder
	dst, f := frame(t, 2)
	st := Style{Size: 10, Leading: 12}
	b := bld.Bullet(BulletStyle{Marker: "-", Style: st, Indent: 15}, bld.Text("item", st))
	if _, _, err := b.Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	out := string(dst[0].Bytes())
	if !strings.Contains(out, "72 ") {
		t.Error("the marker is not at the frame's left edge")
	}
	if !strings.Contains(out, "87 ") {
		t.Error("the content does not start at the frame's left edge plus the indent")
	}
}
