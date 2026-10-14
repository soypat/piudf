package piupage

import (
	"image"
	"image/color"
	"math"
	"strings"
	"testing"
)

func newCanvas(t *testing.T) *Canvas {
	t.Helper()
	c := new(Canvas)
	if err := c.Reset(make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	return c
}

// near reports whether a and b agree to rounding.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestCanvasExtent pins the box each primitive adds, in page space: a Form
// XObject's /BBox is this box, and a box too small clips what was drawn.
func TestCanvasExtent(t *testing.T) {
	black := color.Black
	var p Path
	p.Reset(make([]byte, 0, 8), make([]float64, 0, 16))
	p.MoveTo(0, 0)
	p.LineTo(10, 0)
	p.LineTo(10, 20)
	for _, tc := range []struct {
		name           string
		draw           func(c *Canvas)
		x0, y0, x1, y1 float64
	}{
		{"FillRect", func(c *Canvas) { c.FillRect(1, 2, 3, 4, black) }, 1, 2, 4, 6},
		{"Image", func(c *Canvas) { c.Image(1, 2, 3, 4, image.NewGray(image.Rect(0, 0, 1, 1))) }, 1, 2, 4, 6},
		{"Line", func(c *Canvas) { c.Line(0, 0, 10, 0, 2, black) }, -math.Sqrt2, -math.Sqrt2, 10 + math.Sqrt2, math.Sqrt2},
		{"StrokeRect", func(c *Canvas) { c.StrokeRect(0, 0, 10, 10, 2, black) }, -math.Sqrt2, -math.Sqrt2, 10 + math.Sqrt2, 10 + math.Sqrt2},
		{"Fill", func(c *Canvas) { c.Fill(&p, black) }, 0, 0, 10, 20},
		// A miter join may reach the miter limit (default 10) half-widths out.
		{"Stroke", func(c *Canvas) { c.Stroke(&p, Pen{Width: 2}) }, -10, -10, 20, 30},
		{"Stroke round", func(c *Canvas) { c.Stroke(&p, Pen{Width: 2, Join: RoundJoin}) }, -math.Sqrt2, -math.Sqrt2, 10 + math.Sqrt2, 20 + math.Sqrt2},
		// Under a transform the box is the user-space box mapped.
		{"translated", func(c *Canvas) { c.Translate(100, 200); c.FillRect(1, 2, 3, 4, black) }, 101, 202, 104, 206},
		{"rotated", func(c *Canvas) { c.Rotate(math.Pi / 2); c.FillRect(0, 0, 10, 5, black) }, -5, 0, 0, 10},
		{"Text", func(c *Canvas) {
			c.SetFont(FontHelvetica, 10)
			c.Text(5, 50, "Wave", black)
		}, 5, 45, 5 + StringWidth(FontHelvetica, "Wave", 10), 65},
	} {
		c := newCanvas(t)
		if _, _, _, _, ok := c.Extent(); ok {
			t.Fatalf("%s: fresh canvas has an extent", tc.name)
		}
		tc.draw(c)
		x0, y0, x1, y1, ok := c.Extent()
		if !ok || !near(x0, tc.x0) || !near(y0, tc.y0) || !near(x1, tc.x1) || !near(y1, tc.y1) {
			t.Errorf("%s: extent (%v %v %v %v) ok=%v, want (%v %v %v %v)", tc.name, x0, y0, x1, y1, ok, tc.x0, tc.y0, tc.x1, tc.y1)
		}
	}

	// Links and clips are not ink.
	c := newCanvas(t)
	c.Link(0, 0, 10, 10, "https://example.com")
	c.Clip(&p)
	if _, _, _, _, ok := c.Extent(); ok {
		t.Error("a link and a clip gave the canvas an extent")
	}

	// Rewind takes back the extent with the drawing.
	c.FillRect(0, 0, 1, 1, black)
	m := c.Mark()
	c.FillRect(50, 50, 1, 1, black)
	c.Rewind(m)
	if _, _, x1, y1, _ := c.Extent(); x1 != 1 || y1 != 1 {
		t.Errorf("extent after rewind reaches (%v, %v), want (1, 1)", x1, y1)
	}
}

func TestCanvasPlace(t *testing.T) {
	black := color.Black
	g := newCanvas(t)
	g.FillRect(0, 0, 10, 5, black)
	g.Link(1, 1, 2, 2, "https://example.com")
	h := newCanvas(t)
	h.FillRect(0, 0, 1, 1, black)

	c := newCanvas(t)
	c.Place(100, 200, g)
	c.Place(0, 0, h)
	c.Place(300, 400, g)
	c.Place(0, 0, nil)
	c.Place(0, 0, newCanvas(t)) // Nothing drawn: nothing placed.
	if err := c.Err(); err != nil {
		t.Fatal(err)
	}
	got := string(c.Bytes())
	for _, want := range []string{"q\n1 0 0 1 100 200 cm\n/Fm1 Do\nQ\n", "/Fm2 Do", "1 0 0 1 300 400 cm\n/Fm1 Do"} {
		if !strings.Contains(got, want) {
			t.Errorf("content %q lacks %q", got, want)
		}
	}
	if gs := c.Groups(); len(gs) != 2 || gs[0] != g || gs[1] != h {
		t.Errorf("Groups() = %v, want [g h] in first-use order", gs)
	}
	if c.CTM() != Identity() {
		t.Errorf("CTM after placing = %v, want identity", c.CTM())
	}
	// The extent holds every placement, and the links travel with g.
	if x0, y0, x1, y1, _ := c.Extent(); x0 != 0 || y0 != 0 || x1 != 310 || y1 != 405 {
		t.Errorf("extent (%v %v %v %v), want (0 0 310 405)", x0, y0, x1, y1)
	}
	links := c.Links()
	if len(links) != 2 || links[0].X != 101 || links[0].Y != 201 || links[1].X != 301 || links[1].Y != 401 {
		t.Errorf("links %+v, want g's link at (101, 201) and (301, 401)", links)
	}
	if string(links[0].URI) != "https://example.com" {
		t.Errorf("link URI %q", links[0].URI)
	}

	// Under the caller's transform, a copied link scales with its placement.
	c = newCanvas(t)
	s := c.Save()
	c.Scale(2, 2)
	c.Place(10, 10, g)
	c.Restore(s)
	if ln := c.Links()[0]; ln.X != 22 || ln.Y != 22 || ln.W != 4 || ln.H != 4 {
		t.Errorf("scaled link %+v, want at (22, 22) size 4x4", ln)
	}

	// Rewind takes back a placement.
	m := c.Mark()
	c.Place(0, 0, h)
	c.Rewind(m)
	if n := len(c.Groups()); n != 1 {
		t.Errorf("groups after rewind = %d, want 1", n)
	}

	c.Place(0, 0, c)
	if err := c.Err(); err != errPlaceSelf {
		t.Errorf("placing a canvas on itself: Err() = %v, want %v", err, errPlaceSelf)
	}
}

func TestCanvasPlaceAllocs(t *testing.T) {
	g := newCanvas(t)
	g.FillRect(0, 0, 10, 5, color.Black)
	g.Link(1, 1, 2, 2, "https://example.com")
	c := newCanvas(t)
	start := c.Mark()
	c.Place(0, 0, g) // Warm: group list, link storage and content buffer have room.
	allocs := testing.AllocsPerRun(100, func() {
		c.Rewind(start)
		c.Place(1, 2, g)
	})
	if allocs > 0 {
		t.Errorf("Canvas.Place allocates %.1f times once warm", allocs)
	}
}
