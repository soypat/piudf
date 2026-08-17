package piudoc

import (
	"bytes"
	"strconv"
	"testing"
	"time"

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
// bld is the package's test builder. Sharing one across every test is the shape
// a document has: one builder, many elements, its buffers reused throughout.
var bld Builder

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
	out := build(t, d, []Drawer{bld.P("no links here", Normal)}, 1)
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
		bld.P(`see <a href="https://go.dev/doc">the docs</a> for more`, st),
	}, 1)

	if got := bytes.Count(out, []byte("/Subtype/Link")); got != 1 {
		t.Errorf("wrote %d annotations for one unwrapped link, want 1", got)
	}
	if !bytes.Contains(out, []byte("(https://go.dev/doc)")) {
		t.Errorf("the href did not reach the annotation:\n%s", out)
	}
}

// TestInfoIsATextString: /Title and /Author are PDF text strings, which a reader
// decodes as PDFDocEncoding unless the bytes open with a UTF-16BE byte order
// mark. Writing UTF-8 into a literal string turns an em dash into three
// characters in every reader's title bar.
func TestInfoIsATextString(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		want  string
	}{
		// ASCII costs nothing to leave as it is, and every document already
		// written this way keeps its bytes.
		{"ascii", "Plain Title", "/Title(Plain Title)"},
		{"em dash", "a — b", "/Title<feff00610020201400200062>"},
		{"astral", "x \U0001F600", "/Title<feff00780020d83dde00>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}, Title: tc.title}
			out := build(t, d, []Drawer{Spacer{H: 10}}, 1)
			if !bytes.Contains(out, []byte(tc.want)) {
				got, _, _ := bytes.Cut(out[bytes.Index(out, []byte("/Title")):], []byte("/Author"))
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestInfoWritesEveryField(t *testing.T) {
	d := &Doc{
		Size: SizeA4(), Margins: Margins{72, 72, 72, 72},
		Title: "T", Author: "A", Subject: "S", Creator: "C", Lang: "en",
		Date: time.Date(2026, 8, 16, 22, 30, 0, 0, time.FixedZone("", -3*3600)),
	}
	out := build(t, d, []Drawer{Spacer{H: 10}}, 1)
	for _, want := range []string{
		"/Title(T)", "/Author(A)", "/Subject(S)", "/Creator(C)",
		// A date is ASCII by construction, so it stays a literal string.
		"/CreationDate(D:20260816223000-03'00')", "/ModDate(D:20260816223000-03'00')",
		"/Lang(en)",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("missing %s", want)
		}
	}
}

// TestOutlineTree: bookmarks at three levels must come out as a tree a reader
// can walk — every item naming its parent, its siblings and its children.
func TestOutlineTree(t *testing.T) {
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	// one
	//   one.a
	//   one.b
	//     one.b.i
	// two
	out := build(t, d, []Drawer{
		Bookmark{Title: "one", Level: 1},
		Bookmark{Title: "one.a", Level: 2},
		Bookmark{Title: "one.b", Level: 2},
		Bookmark{Title: "one.b.i", Level: 3},
		Bookmark{Title: "two", Level: 1},
		Spacer{H: 10},
	}, 1)

	// writeOutlines allocates the root and then one id per mark in order, so the
	// catalog's /Outlines is all the test needs to name every item.
	root := 0
	if _, rest, ok := bytes.Cut(out, []byte("/Outlines ")); !ok {
		t.Fatalf("the catalog does not point at an outline:\n%s", out)
	} else {
		num, _, _ := bytes.Cut(rest, []byte(" 0 R"))
		root, _ = strconv.Atoi(string(num))
	}
	item := func(i int) string { return strconv.Itoa(root+1+i) + " 0 R" }
	for _, tc := range []struct{ title, want string }{
		{"one", "/Parent " + strconv.Itoa(root) + " 0 R/Next " + item(4) + "/First " + item(1) + "/Last " + item(2) + "/Count 3"},
		{"one.a", "/Parent " + item(0) + "/Next " + item(2)},
		{"one.b", "/Parent " + item(0) + "/Prev " + item(1) + "/First " + item(3) + "/Last " + item(3) + "/Count 1"},
		{"one.b.i", "/Parent " + item(2)},
		{"two", "/Parent " + strconv.Itoa(root) + " 0 R/Prev " + item(0)},
	} {
		i := bytes.Index(out, []byte("/Title("+tc.title+")"))
		if i < 0 {
			t.Fatalf("no outline item titled %q:\n%s", tc.title, out)
		}
		got, _, _ := bytes.Cut(out[i:], []byte("/Dest"))
		if !bytes.Contains(got, []byte(tc.want)) {
			t.Errorf("%s: got %s\nwant %s", tc.title, got, tc.want)
		}
	}
	// The root names the two top-level items and counts every visible one.
	if !bytes.Contains(out, []byte("/Type/Outlines/First "+item(0)+"/Last "+item(4)+"/Count 5")) {
		t.Errorf("outline root is not a tree over both top-level items:\n%s", out)
	}
	// Every item is reachable from a page, or it navigates nowhere.
	if got := bytes.Count(out, []byte("/Dest[")); got != 5 {
		t.Errorf("%d destinations for 5 bookmarks", got)
	}
}
