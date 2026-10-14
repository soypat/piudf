package piudoc

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piulex"
	"github.com/soypat/piudf/piupage"
)

// pageBreaks is a Drawer that moves the story n pages on, drawing nothing.
type pageBreaks int

func (n pageBreaks) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	if int(n) >= len(dst) {
		return int(n), f.Top, io.ErrShortBuffer
	}
	return int(n), f.Top, nil
}

func newGroup(t *testing.T) *piupage.Canvas {
	t.Helper()
	g := new(piupage.Canvas)
	if err := g.Reset(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestGroupOncePerDocument places a letterhead on every page from OnPage and
// checks it, and everything it draws, is written once: one Form XObject, one
// font whose subset holds the letterhead's glyphs with the body's, one image,
// and a link annotation on every page.
func TestGroupOncePerDocument(t *testing.T) {
	f := testFont(t)
	g := newGroup(t)
	g.SetFont(f, 12)
	g.Text(0, 0, "Acme Ltd", color.Black)
	g.Line(0, -4, 100, -4, 1, color.Black)
	g.Image(0, -30, 20, 20, image.NewGray(image.Rect(0, 0, 2, 2)))
	g.Link(0, 0, 50, 12, "https://acme.example")

	d := &Doc{
		Size: SizeA4(), Margins: Margins{72, 72, 72, 72},
		OnPage: func(c *piupage.Canvas, p PageInfo) { c.Place(72, 800, g) },
	}
	out := build(t, d, []Drawer{setter{font: f, text: "xyz"}, pageBreaks(2)}, 3)

	for _, tc := range []struct {
		what string
		n    int
	}{
		{"/Subtype/Form", 1},
		{"/FontFile2", 1},
		{"/Subtype/Image", 1},
		{"/Fm1 Do", 3},
		{"/Subtype/Link", 3},
	} {
		if n := bytes.Count(out, []byte(tc.what)); n != tc.n {
			t.Errorf("%s appears %d times, want %d", tc.what, n, tc.n)
		}
	}
	// One subset for the page's "xyz" and the letterhead's "Acme Ltd": 3+8
	// distinct glyphs, the space included.
	if !bytes.Contains(out, []byte("11 beginbfchar")) {
		t.Errorf("the subset is not the page's glyphs and the group's:\n%s", toUnicodeExcerpt(out))
	}

	// The form's dictionary resolves: its resources name the font and image,
	// and its box holds what was drawn.
	r := bytes.NewReader(out)
	var codec piudf.Codec
	if err := codec.Configure(piudf.DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 64, MaxDepth: 32}); err != nil {
		t.Fatal(err)
	}
	var pdf piudf.PDF
	if err := pdf.Decode(r, r.Size(), &codec); err != nil {
		t.Fatal(err)
	}
	form := findForm(t, &pdf, r, &codec)
	res, err := codec.DictGet(&pdf, r, form, "Resources")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range [][2]string{{"Font", "F1"}, {"XObject", "Im1"}} {
		sub, err := codec.DictGet(&pdf, r, res, path[0])
		if err != nil {
			t.Fatal(err)
		}
		ref, err := codec.DictGet(&pdf, r, sub, path[1])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pdf.Resolve(r, ref.ObjectID(), &codec); err != nil || ref.Tok != piulex.TokR {
			t.Errorf("form resource /%s/%s = %v: %v", path[0], path[1], ref, err)
		}
	}
	var box []float64
	bbox, err := codec.DictGet(&pdf, r, form, "BBox")
	if err != nil {
		t.Fatal(err)
	}
	codec.ArrayForEach(&pdf, r, bbox, func(v piudf.Value) bool {
		f, _ := v.Float()
		box = append(box, f)
		return true
	})
	textW := piupage.StringWidth(f, "Acme Ltd", 12)
	if len(box) != 4 || box[0] > 0 || box[1] > -30 || box[2] < max(textW, 100) || box[3] < 12 {
		t.Errorf("/BBox %v does not hold the text, rule and image drawn", box)
	}
}

// findForm returns the document's one Form XObject.
func findForm(t *testing.T, pdf *piudf.PDF, r *bytes.Reader, codec *piudf.Codec) piudf.Value {
	t.Helper()
	size, err := codec.DictGet(pdf, r, pdf.Trailer(), "Size")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := size.Int()
	for num := uint32(1); num < uint32(n); num++ {
		v, err := pdf.Resolve(r, piudf.ObjectID{Num: num}, codec)
		if err != nil || v.Tok != piulex.TokStream {
			continue
		}
		st, err := codec.DictGet(pdf, r, v, "Subtype")
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := codec.NameIs(pdf, r, st, "Form"); ok {
			return v
		}
	}
	t.Fatal("no Form XObject in the document")
	return piudf.Value{}
}

// TestGroupNested writes a group placed within another once, named in the
// outer group's resources, however the pages place the two.
func TestGroupNested(t *testing.T) {
	inner := newGroup(t)
	inner.FillRect(0, 0, 5, 5, color.Black)
	outer := newGroup(t)
	outer.Place(10, 10, inner)
	outer.FillRect(0, 0, 1, 1, color.Black)

	d := &Doc{Size: SizeA4(), OnPage: func(c *piupage.Canvas, p PageInfo) {
		c.Place(0, 0, outer)
		c.Place(100, 100, inner)
	}}
	out := build(t, d, []Drawer{pageBreaks(1)}, 2)
	if n := bytes.Count(out, []byte("/Subtype/Form")); n != 2 {
		t.Errorf("%d Form XObjects, want 2", n)
	}
	// Every page draws outer as /Fm1 then inner as /Fm2, and outer draws inner
	// itself, as the /Fm1 of its own resources.
	if n := bytes.Count(out, []byte("/Fm1 Do")); n != 2+1 {
		t.Errorf("/Fm1 drawn %d times, want 3", n)
	}
	if n := bytes.Count(out, []byte("/Fm2 Do")); n != 2 {
		t.Errorf("/Fm2 drawn %d times, want 2", n)
	}
}

// TestGroupCycle pins a group placed within itself, through another, failing
// the build rather than writing a document a reader recurses on forever.
func TestGroupCycle(t *testing.T) {
	a, b := newGroup(t), newGroup(t)
	a.FillRect(0, 0, 1, 1, color.Black)
	b.Place(0, 0, a)
	a.Place(0, 0, b)
	d := &Doc{Size: SizeA4(), OnPage: func(c *piupage.Canvas, p PageInfo) { c.Place(0, 0, a) }}
	var buf bytes.Buffer
	err := d.Build(&buf, make([]piupage.Canvas, 1), []Drawer{pageBreaks(0)}, make([]byte, 4096), make([]byte, 4096))
	if !errors.Is(err, errGroupCycle) {
		t.Errorf("Build err = %v, want %v", err, errGroupCycle)
	}
}

// TestDrawGroup lays a paragraph out on a group and pins the height it reports
// to the one Measure gives the same paragraph in a story, and the drawing to
// hang below the group's origin.
func TestDrawGroup(t *testing.T) {
	var bld Builder
	para := bld.P(strings.Repeat("Reusable groups write their drawing once. ", 6), Style{Size: 10})
	g := newGroup(t)
	h, err := DrawGroup(g, para, 200)
	if err != nil {
		t.Fatal(err)
	}
	pages := make([]piupage.Canvas, 1)
	if err := pages[0].Reset(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	want, err := Measure(pages, para, Frame{X: 0, Width: 200, Top: 500, Bottom: 0}, 500)
	if err != nil {
		t.Fatal(err)
	} else if err := pages[0].Err(); err != nil {
		t.Fatal(err)
	}
	if h <= 10 || h != want {
		t.Errorf("DrawGroup height %v, Measure %v", h, want)
	}
	if _, y0, _, y1, ok := g.Extent(); !ok || y0 > -h+10 || y1 > 1.5*10 {
		t.Errorf("drawing spans y %v…%v, want hanging below 0 by about %v", y0, y1, h)
	}
}

// TestGroupFlowable flows two groups that cannot share a page: the second
// moves to the next, and the canvas is still written once.
func TestGroupFlowable(t *testing.T) {
	g := newGroup(t)
	g.FillRect(0, -150, 40, 150, color.Black)
	d := &Doc{Size: PageSize{W: 200, H: 200}}
	out := build(t, d, []Drawer{
		&Group{Canvas: g, Width: 40, Height: 150},
		&Group{Canvas: g, Width: 40, Height: 150, Align: Right},
	}, 2)
	if n := bytes.Count(out, []byte("/Subtype/Form")); n != 1 {
		t.Errorf("group written %d times, want once", n)
	}
	if n := bytes.Count(out, []byte("/Type/Page/")); n != 2 {
		t.Errorf("%d pages, want 2", n)
	}
	// Right-aligned in a 200pt frame: x = 200-40.
	if !bytes.Contains(out, []byte("1 0 0 1 160 200 cm")) {
		t.Error("second group not placed right-aligned at the top of page 2")
	}
}
