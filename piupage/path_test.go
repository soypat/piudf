package piupage

import (
	"image/color"
	"io"
	"strings"
	"testing"
)

// drawn returns the content stream produced by fn.
func drawn(t testing.TB, fn func(c *Canvas)) string {
	t.Helper()
	var c Canvas
	if err := c.Reset(make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	fn(&c)
	s := string(c.Bytes())
	if err := c.Err(); err != nil {
		t.Fatalf("canvas error: %v", err)
	}
	return s
}

func TestPathOperators(t *testing.T) {
	var p Path
	p.MoveTo(1, 2)
	p.LineTo(3, 4)
	p.CubicTo(5, 6, 7, 8, 9, 10)
	p.Rect(0, 0, 20, 30)
	p.Close()
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}

	got := drawn(t, func(c *Canvas) { c.Fill(&p, color.Black) })
	for _, want := range []string{
		"1 2 m", "3 4 l", "5 6 7 8 9 10 c", "0 0 20 30 re", " h", " f",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestPathQuadToElevation(t *testing.T) {
	// From (0,0) with control (3,3) to (6,0), the equivalent cubic has controls
	// at P0 + 2/3(Q-P0) = (2,2) and P2 + 2/3(Q-P2) = (4,2).
	var p Path
	p.MoveTo(0, 0)
	p.QuadTo(3, 3, 6, 0)
	got := drawn(t, func(c *Canvas) { c.Fill(&p, color.Black) })
	if !strings.Contains(got, "2 2 4 2 6 0 c") {
		t.Errorf("quadratic not elevated as expected:\n%s", got)
	}
}

func TestPathFillRule(t *testing.T) {
	var p Path
	p.Rect(0, 0, 10, 10)
	if p.Rule() != NonZero {
		t.Error("default rule is not NonZero")
	}
	if got := drawn(t, func(c *Canvas) { c.Fill(&p, nil) }); !strings.Contains(got, " f\n") {
		t.Errorf("want nonzero fill operator:\n%s", got)
	}
	p.SetRule(EvenOdd)
	for _, tc := range []struct {
		name string
		want string
		fn   func(c *Canvas)
	}{
		{"fill", "f*", func(c *Canvas) { c.Fill(&p, nil) }},
		{"fillstroke", "B*", func(c *Canvas) { c.FillStroke(&p, nil, Pen{}) }},
		{"clip", "W*", func(c *Canvas) { c.Clip(&p) }},
	} {
		if got := drawn(t, tc.fn); !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q in:\n%s", tc.name, tc.want, got)
		}
	}
}

func TestPathBounds(t *testing.T) {
	var p Path
	if x0, y0, x1, y1 := p.Bounds(); x0 != 0 || y0 != 0 || x1 != 0 || y1 != 0 {
		t.Error("an empty path should have a zero box")
	}
	p.MoveTo(10, 10)
	p.LineTo(20, 5)
	p.CubicTo(30, 40, 0, -5, 25, 25)
	x0, y0, x1, y1 := p.Bounds()
	if x0 != 0 || y0 != -5 || x1 != 30 || y1 != 40 {
		t.Errorf("bounds = (%v %v %v %v), want (0 -5 30 40)", x0, y0, x1, y1)
	}

	// A rectangle contributes both corners, negative extents included.
	var r Path
	r.Rect(50, 50, -20, -10)
	x0, y0, x1, y1 = r.Bounds()
	if x0 != 30 || y0 != 40 || x1 != 50 || y1 != 50 {
		t.Errorf("rect bounds = (%v %v %v %v), want (30 40 50 50)", x0, y0, x1, y1)
	}
}

func TestPathBoundedBuffers(t *testing.T) {
	var p Path
	// Room for two segments and four coordinates.
	p.Reset(make([]byte, 0, 2), make([]float64, 0, 4))
	p.MoveTo(0, 0)
	p.LineTo(1, 1)
	if err := p.Err(); err != nil {
		t.Fatalf("path overflowed early: %v", err)
	}
	p.LineTo(2, 2)
	if err := p.Err(); err != io.ErrShortBuffer {
		t.Errorf("err = %v, want io.ErrShortBuffer", err)
	}
	// Sticky: further operations record nothing and do not panic.
	p.CubicTo(1, 2, 3, 4, 5, 6)
	if len(p.ops) != 2 {
		t.Errorf("recorded %d segments past the overflow", len(p.ops)-2)
	}

	p.Clear()
	if !p.IsEmpty() || p.Err() != nil {
		t.Error("Clear should empty the path and drop the error")
	}
	p.MoveTo(9, 9)
	if p.Err() != nil {
		t.Error("a cleared path should record again")
	}
}

func TestPathZeroValueGrows(t *testing.T) {
	var p Path
	for i := range 100 {
		p.LineTo(float64(i), float64(i))
	}
	if err := p.Err(); err != nil {
		t.Errorf("an unbound path should grow, got %v", err)
	}
	if len(p.ops) != 100 {
		t.Errorf("recorded %d segments, want 100", len(p.ops))
	}
}

func TestPathReplay(t *testing.T) {
	// The point of a path being a value: describe once, paint twice.
	var p Path
	p.Rect(0, 0, 10, 10)
	got := drawn(t, func(c *Canvas) {
		c.Fill(&p, color.White)
		c.Stroke(&p, Pen{Width: 2})
	})
	if n := strings.Count(got, "0 0 10 10 re"); n != 2 {
		t.Errorf("geometry emitted %d times, want 2:\n%s", n, got)
	}
}

func TestEmptyPathPaintsNothing(t *testing.T) {
	var p Path
	got := drawn(t, func(c *Canvas) {
		c.Fill(&p, color.Black)
		c.Stroke(&p, Pen{})
		c.FillStroke(&p, color.Black, Pen{})
		c.Clip(&p)
	})
	if got != "" {
		t.Errorf("an empty path emitted %q", got)
	}
}

func TestPenOperators(t *testing.T) {
	var p Path
	p.MoveTo(0, 0)
	p.LineTo(1, 1)

	got := drawn(t, func(c *Canvas) { c.Stroke(&p, Pen{}) })
	if !strings.Contains(got, "1 w") {
		t.Errorf("a zero Pen should stroke a 1pt hairline:\n%s", got)
	}
	for _, unwanted := range []string{" J", " j", " M"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a zero Pen emitted default state %q:\n%s", unwanted, got)
		}
	}

	got = drawn(t, func(c *Canvas) {
		c.Stroke(&p, Pen{Width: 3, Cap: RoundCap, Join: BevelJoin, Miter: 4, Dash: []float64{5, 2}, Phase: 1})
	})
	// Brackets self-delimit, so the emitter writes no space around them.
	for _, want := range []string{"3 w", "1 J", "2 j", "4 M", "[5 2]1 d"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// PDF rejects an all-zero dash array; a half-filled Pen must not produce one.
	got = drawn(t, func(c *Canvas) { c.Stroke(&p, Pen{Dash: []float64{0, 0}}) })
	if !strings.Contains(got, "[]0 d") {
		t.Errorf("an all-zero dash should stroke solid:\n%s", got)
	}
}
