package piudoc

import (
	"bytes"
	"testing"

	"github.com/soypat/piudf/piupage"
)

// linker is a Drawer that registers one link under a transform, standing in for
// any element that decorates a region it drew itself.
type linker struct {
	x, y, w, h float64
	uri        string
	// tx, ty is a translate applied before the link is registered, so the test
	// sees whether the rect reaching the file is page space or user space.
	tx, ty float64
}

func (l linker) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	cv := &dst[0]
	s := cv.Save()
	cv.Translate(l.tx, l.ty)
	cv.Link(l.x, l.y, l.w, l.h, l.uri)
	cv.Restore(s)
	return 0, yTop - l.h, nil
}

// build runs a one-shot document and returns its bytes.
func build(t testing.TB, d *Doc, story []Drawer, pages int) []byte {
	t.Helper()
	var buf bytes.Buffer
	dst := make([]piupage.Canvas, pages)
	err := d.Build(&buf, dst, story, make([]byte, 4096), make([]byte, pages*4096))
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDocWritesAnnots(t *testing.T) {
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{
		linker{x: 1, y: 2, w: 30, h: 40, uri: "https://go.dev", tx: 100, ty: 500},
	}, 1)

	if !bytes.Contains(out, []byte("/Subtype/Link")) {
		t.Fatal("no link annotation reached the file")
	}
	if !bytes.Contains(out, []byte("/Annots[")) {
		t.Error("the page dictionary does not reference the annotation")
	}
	// An annotation lives in page space: the translate has to be baked in, so
	// the rect is (101, 502)-(131, 542) and not (1, 2)-(31, 42).
	if !bytes.Contains(out, []byte("/Rect[101 502 131 542]")) {
		t.Errorf("annotation rect ignores the CTM:\n%s", out)
	}
}

func TestDocOmitsEmptyAnnots(t *testing.T) {
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{P("no links here", Normal)}, 1)
	if bytes.Contains(out, []byte("/Annots")) {
		t.Error("wrote an empty /Annots array")
	}
}

func TestMeasuredLinkNeverReachesTheFile(t *testing.T) {
	// Measure draws a child against an unbounded page and retracts it. A link
	// registered during that dry run must not survive, and the canvas holds it
	// in a slice the content-stream truncation knows nothing about — which is
	// exactly why Mark records len(links).
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	var buf bytes.Buffer
	dst := make([]piupage.Canvas, 1)
	story := []Drawer{measurer{child: linker{w: 10, h: 10, uri: "https://phantom.test"}}}
	if err := d.Build(&buf, dst, story, make([]byte, 4096), make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("phantom.test")) {
		t.Error("a measured-then-discarded link became a live annotation")
	}
	if bytes.Contains(buf.Bytes(), []byte("/Annots")) {
		t.Error("the page references annotations it should have none of")
	}
}

// measurer measures its child and then draws nothing, the shape every container
// uses to learn an extent before it commits to a position.
type measurer struct{ child Drawer }

func (m measurer) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	if _, err := Measure(dst, m.child, f, yTop); err != nil {
		return 0, yTop, err
	}
	return 0, yTop, nil
}

func TestParagraphLinkAnnotations(t *testing.T) {
	st := Normal
	st.Link = LinkStyle{Underline: true}
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{
		P(`see <a href="https://go.dev/doc">the docs</a> for more`, st),
	}, 1)

	if got := bytes.Count(out, []byte("/Subtype/Link")); got != 1 {
		t.Errorf("wrote %d annotations for one unwrapped link, want 1", got)
	}
	if !bytes.Contains(out, []byte("(https://go.dev/doc)")) {
		t.Errorf("the href did not reach the annotation:\n%s", out)
	}
}
