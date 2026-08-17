package piudoc

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/soypat/piudf/piupage"
)

// imager draws one image and takes h points of the page for it.
type imager struct {
	im   piupage.Image
	w, h float64
	// twice draws the same image a second time, to see whether one picture
	// costs one stream or two.
	twice bool
}

func (i imager) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	cv := &dst[0]
	cv.Image(i.im, f.X, yTop-i.h, i.w, i.h)
	if i.twice {
		cv.Image(i.im, f.X+i.w, yTop-i.h, i.w, i.h)
	}
	return 0, yTop - i.h, nil
}

// testImage is a small opaque PNG passed through as PDF would read it.
func testImage(t testing.TB) piupage.Image {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for y := range 6 {
		for x := range 8 {
			src.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 40), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	im, err := piupage.PNG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return im
}

func TestDocWritesImageXObject(t *testing.T) {
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{imager{im: testImage(t), w: 100, h: 75}}, 1)

	for _, want := range []string{
		"/Subtype/Image", "/Width 8", "/Height 6", "/ColorSpace/DeviceRGB",
		"/Filter/FlateDecode", "/Predictor 15",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the image object does not carry %q", want)
		}
	}
	if !bytes.Contains(out, []byte("/XObject")) {
		t.Error("the page dictionary does not reference the image")
	}
	// The placement is the transform: a 100x75 box at the frame's origin, with
	// the raster's own 8x6 nowhere in the content stream.
	if !bytes.Contains(out, []byte("100 0 0 75 72")) {
		t.Errorf("the image is not placed by a cm of its box:\n%s", out)
	}
	if !bytes.Contains(out, []byte("/Im1 Do")) {
		t.Error("the image is never invoked with Do")
	}
}

func TestDocOmitsEmptyXObject(t *testing.T) {
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{bld.P("no pictures here", Normal)}, 1)
	if bytes.Contains(out, []byte("/XObject")) {
		t.Error("wrote an empty /XObject dictionary")
	}
}

// One picture is one stream however many times it is drawn. This is the whole
// reason images are gathered by the document rather than by the page.
func TestImageDrawnTwiceIsWrittenOnce(t *testing.T) {
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{imager{im: testImage(t), w: 60, h: 45, twice: true}}, 1)
	if got := bytes.Count(out, []byte("/Subtype/Image")); got != 1 {
		t.Errorf("one image was written %d times", got)
	}
	if got := bytes.Count(out, []byte("/Im1 Do")); got != 2 {
		t.Errorf("the image was invoked %d times, want 2", got)
	}
}

// Measure draws a child against an unbounded page and retracts it. An image
// registered by a draw that was rewound would leave the page naming a resource
// nothing draws, and the file carrying a stream nobody reads.
func TestMeasuredImageNeverReachesTheFile(t *testing.T) {
	var cv [1]piupage.Canvas
	if err := cv[0].Reset(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	f := Frame{X: 0, Width: 400, Top: 700, Bottom: 0}
	if _, err := Measure(cv[:], imager{im: testImage(t), w: 50, h: 40}, f, 700); err != nil {
		t.Fatal(err)
	}
	if got := len(cv[0].Images()); got != 0 {
		t.Errorf("a measured image left %d registered on the page", got)
	}
	if got := cv[0].Bytes(); len(got) != 0 {
		t.Errorf("a measured image left %q in the content stream", got)
	}
}
