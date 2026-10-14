package piupage

import (
	"bytes"
	"compress/flate"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"strings"
	"testing"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piulex"
)

// opaqueless hides an image's Opaque method and concrete type, so it takes the
// generic At path.
type opaqueless struct{ image.Image }

// testImages returns one image of each kind ImageEncoder has a path for, and
// a gray JPEG file.
func testImages(t *testing.T) (imgs []Image, jpg []byte) {
	t.Helper()
	const w, h = 70, 30 // 70*30*4 outgrows the staging buffer once.
	rect := image.Rect(0, 0, w, h)
	gray := image.NewGray(rect)
	gray16 := image.NewGray16(rect)
	rgba := image.NewRGBA(rect)
	nrgba := image.NewNRGBA(rect)
	rgba64 := image.NewRGBA64(rect)
	nrgba64 := image.NewNRGBA64(rect)
	cmyk := image.NewCMYK(rect)
	pal := image.NewPaletted(rect, color.Palette{
		color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}, color.NRGBA{0, 255, 0, 0},
	})
	ycc := image.NewYCbCr(rect, image.YCbCrSubsampleRatio420)
	for y := range h {
		for x := range w {
			v := uint8(x*3 + y*5)
			a := uint8(255)
			if x%7 == 0 {
				a = uint8(y * 8) // Translucent and transparent columns.
			}
			gray.SetGray(x, y, color.Gray{v})
			gray16.SetGray16(x, y, color.Gray16{uint16(v) * 257})
			nc := color.NRGBA{v, 255 - v, v / 2, a}
			nrgba.SetNRGBA(x, y, nc)
			rgba.Set(x, y, nc)
			nrgba64.SetNRGBA64(x, y, color.NRGBA64{uint16(v) << 8, 0x1234, 0xfedc, uint16(a) * 257})
			rgba64.Set(x, y, color.NRGBA64{uint16(v) << 8, 0x1234, 0xfedc, uint16(a) * 257})
			cmyk.SetCMYK(x, y, color.CMYK{v, 255 - v, 0, v / 3})
			pal.SetColorIndex(x, y, uint8((x+y)%3))
			ycc.Y[ycc.YOffset(x, y)] = v
			ycc.Cb[ycc.COffset(x, y)] = 255 - v
			ycc.Cr[ycc.COffset(x, y)] = v / 2
		}
	}
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, gray, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return []Image{gray, gray16, rgba, nrgba, rgba64, nrgba64, cmyk, pal, ycc, &opaqueless{nrgba}}, jbuf.Bytes()
}

// writeDoc writes imgs as image XObjects into a document and returns it with
// the objects' ids.
func writeDoc(t *testing.T, ie *ImageEncoder, imgs []Image) ([]byte, []piudf.ObjectID) {
	t.Helper()
	var out bytes.Buffer
	var enc piudf.Encoder
	if err := enc.Reset(&out, make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	ids := make([]piudf.ObjectID, len(imgs))
	for i, img := range imgs {
		id, err := ie.Write(&enc, img)
		if err != nil {
			t.Fatalf("image %d (%T): %v", i, img, err)
		}
		ids[i] = id
	}
	cat := enc.NewID()
	enc.BeginObject(cat)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("Catalog")
	enc.DictClose()
	enc.EndObject()
	if err := enc.Close(cat, piudf.ObjectID{}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), ids
}

// readImage decodes image object id of the document in data.
func readImage(t *testing.T, data []byte, id piudf.ObjectID) image.Image {
	t.Helper()
	r := bytes.NewReader(data)
	var codec piudf.Codec
	if err := codec.Configure(piudf.DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 64, MaxDepth: 32}); err != nil {
		t.Fatal(err)
	}
	var pdf piudf.PDF
	if err := pdf.Decode(r, r.Size(), &codec); err != nil {
		t.Fatal(err)
	}
	v, err := pdf.Resolve(r, id, &codec)
	if err != nil {
		t.Fatal(err)
	}
	info, err := pdf.ImageInfo(r, v, &codec)
	if err != nil {
		t.Fatal(err)
	}
	var d piudf.ImageDecoder
	img, err := d.Decode(nil, &pdf, r, info, &codec)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// sameImage reports the first pixel where got and want differ by more than
// tol in a channel, color compared only where want is not fully transparent.
func sameImage(t *testing.T, label string, got, want image.Image, tol int) {
	t.Helper()
	gb, wb := got.Bounds(), want.Bounds()
	if gb.Dx() != wb.Dx() || gb.Dy() != wb.Dy() {
		t.Errorf("%s: got %v, want %v", label, gb, wb)
		return
	}
	near := func(a, b uint8) bool { return int(a)-int(b) <= tol && int(b)-int(a) <= tol }
	for y := range wb.Dy() {
		for x := range wb.Dx() {
			g := color.NRGBAModel.Convert(got.At(gb.Min.X+x, gb.Min.Y+y)).(color.NRGBA)
			w := color.NRGBAModel.Convert(want.At(wb.Min.X+x, wb.Min.Y+y)).(color.NRGBA)
			if !near(g.A, w.A) || w.A != 0 && (!near(g.R, w.R) || !near(g.G, w.G) || !near(g.B, w.B)) {
				t.Errorf("%s: pixel (%d, %d) = %v, want %v", label, x, y, g, w)
				return
			}
		}
	}
}

// TestImageEncoderRoundTrip writes each kind of image and reads it back with
// piudf's decoder, each side the other's oracle.
func TestImageEncoderRoundTrip(t *testing.T) {
	imgs, jpg := testImages(t)
	var j JPEG
	if err := j.Reset(jpg); err != nil {
		t.Fatal(err)
	}
	var ie ImageEncoder
	data, ids := writeDoc(t, &ie, append(imgs, &j))
	for i, img := range imgs {
		// Premultiplied sources and YCbCr convert through 8-bit arithmetic
		// where the image package uses 16-bit: off by one is rounding.
		sameImage(t, fmt.Sprintf("%T", img), readImage(t, data, ids[i]), img.(image.Image), 1)
	}
	want, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatal(err)
	}
	sameImage(t, "*JPEG", readImage(t, data, ids[len(imgs)]), want, 0)
}

// TestImageEncoderAdobeCMYK round-trips an Adobe CMYK JPEG file, whose samples
// are stored inverted. A JPEG reader undoes that and a PDF reader does not, so
// the file must be written with a /Decode for the PDF to show what the file
// shows. image/jpeg cannot encode CMYK, so the file comes from the corpus.
func TestImageEncoderAdobeCMYK(t *testing.T) {
	jpg := corpusJPEG(t, "../testdata/basis-of-embedded.pdf")
	var j JPEG
	if err := j.Reset(jpg); err != nil {
		t.Fatal(err)
	}
	if j.comps != 4 || !j.adobe {
		t.Fatalf("corpus JPEG has %d components, Adobe %v: want Adobe CMYK", j.comps, j.adobe)
	}
	var ie ImageEncoder
	data, ids := writeDoc(t, &ie, []Image{&j})
	if !bytes.Contains(data, []byte("/Decode")) {
		t.Error("Adobe CMYK JPEG written without an inverting /Decode")
	}
	want, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatal(err)
	}
	sameImage(t, "Adobe CMYK", readImage(t, data, ids[0]), want, 0)
}

// corpusJPEG returns the first DCTDecode image payload of the PDF at path.
func corpusJPEG(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(f)
	var codec piudf.Codec
	if err := codec.Configure(piudf.DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 64, MaxDepth: 32}); err != nil {
		t.Fatal(err)
	}
	var pdf piudf.PDF
	if err := pdf.Decode(r, r.Size(), &codec); err != nil {
		t.Fatal(err)
	}
	sizeV, err := codec.DictGet(&pdf, r, pdf.Trailer(), "Size")
	if err != nil {
		t.Fatal(err)
	}
	size, _ := sizeV.Int()
	for num := uint32(1); num < uint32(size); num++ {
		v, err := pdf.Resolve(r, piudf.ObjectID{Num: num}, &codec)
		if err != nil || v.Tok != piulex.TokStream {
			continue
		}
		info, err := pdf.ImageInfo(r, v, &codec)
		if err != nil || info.Payload.NumFilters() != 1 || info.Payload.Filter(0) != piudf.FilterDCT {
			continue
		}
		sp := info.Payload
		return f[sp.Offset : sp.Offset+sp.Length]
	}
	t.Fatalf("%s holds no DCTDecode image", path)
	return nil
}

// TestImageEncoderAllocs pins writing costing nothing once warm, for every
// image/... type and *JPEG: the compressor is reset, not remade, and samples
// are staged through a fixed buffer. The generic At path allocates and is left
// out.
func TestImageEncoderAllocs(t *testing.T) {
	imgs, jpg := testImages(t)
	imgs = imgs[:len(imgs)-1] // The generic image.
	var j JPEG
	if err := j.Reset(jpg); err != nil {
		t.Fatal(err)
	}
	imgs = append(imgs, &j)
	var ie ImageEncoder
	ie.Flate.Level = flate.BestSpeed
	var out bytes.Buffer
	var enc piudf.Encoder
	scratch := make([]byte, 512)
	for _, img := range imgs {
		write := func() {
			out.Reset()
			if err := enc.Reset(&out, scratch); err != nil {
				t.Fatal(err)
			}
			if _, err := ie.Write(&enc, img); err != nil {
				t.Fatal(err)
			}
		}
		write() // Warm: compressor made, buffers grown.
		if allocs := testing.AllocsPerRun(10, write); allocs > 0 {
			t.Errorf("%T: Write allocates %.1f times once warm", img, allocs)
		}
	}
}

func TestCanvasImage(t *testing.T) {
	a := image.NewGray(image.Rect(0, 0, 2, 2))
	b := image.NewGray(image.Rect(0, 0, 2, 2))
	var c Canvas
	if err := c.Reset(make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	c.Image(10, 20, 30, 40, a)
	c.Image(0, 0, 1, 1, b)
	c.Image(0, 0, 1, 1, a)
	c.Image(0, 0, 1, 1, nil)
	if err := c.Err(); err != nil {
		t.Fatal(err)
	}
	got := string(c.Bytes())
	for _, want := range []string{"q\n30 0 0 40 10 20 cm\n/Im1 Do Q\n", "/Im2 Do", "/Im1 Do"} {
		if !strings.Contains(got, want) {
			t.Errorf("content %q lacks %q", got, want)
		}
	}
	if imgs := c.Images(); len(imgs) != 2 || imgs[0] != a || imgs[1] != b {
		t.Errorf("Images() = %v, want [a b] in first-use order", imgs)
	}
	if c.CTM() != Identity() {
		t.Errorf("CTM after drawing images = %v, want identity", c.CTM())
	}

	m := c.Mark()
	c.Image(0, 0, 1, 1, image.NewGray(image.Rect(0, 0, 1, 1)))
	c.Rewind(m)
	if n := len(c.Images()); n != 2 {
		t.Errorf("images after rewind = %d, want 2", n)
	}

	if err := c.Reset(make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	start := c.Mark()
	c.Image(0, 0, 1, 1, a) // Warm: the image list and content buffer have room.
	allocs := testing.AllocsPerRun(100, func() {
		c.Rewind(start)
		c.Image(1, 2, 3, 4, a)
	})
	if allocs > 0 {
		t.Errorf("Canvas.Image allocates %.1f times once warm", allocs)
	}
}

// sliceImage is an image.Image == cannot compare: comparing two panics.
type sliceImage struct{ pix []byte }

func (sliceImage) Bounds() image.Rectangle { return image.Rect(0, 0, 1, 1) }
func (sliceImage) ColorModel() color.Model { return color.GrayModel }
func (sliceImage) At(x, y int) color.Color { return color.Gray{} }

// TestCanvasImageNotComparable pins an image == cannot compare failing the
// canvas instead of panicking when it meets the images drawn before it.
func TestCanvasImageNotComparable(t *testing.T) {
	for _, img := range []Image{
		sliceImage{},
		// Comparable by type, not by the value its interface field holds.
		opaqueless{Image: sliceImage{}},
	} {
		var c Canvas
		if err := c.Reset(make([]byte, 512)); err != nil {
			t.Fatal(err)
		}
		c.Image(0, 0, 1, 1, image.NewGray(image.Rect(0, 0, 1, 1)))
		c.Image(0, 0, 1, 1, img)
		c.Image(0, 0, 1, 1, img)
		if err := c.Err(); err != errImageNotComparable {
			t.Errorf("%T: Err() = %v, want %v", img, err, errImageNotComparable)
		}
		if n := len(c.Images()); n != 1 {
			t.Errorf("%T: %d images registered, want 1", img, n)
		}
	}
}

func TestJPEGReset(t *testing.T) {
	_, jpg := testImages(t)
	var j JPEG
	if err := j.Reset(jpg); err != nil {
		t.Fatal(err)
	}
	if b := j.Bounds(); b.Dx() != 70 || b.Dy() != 30 || j.comps != 1 {
		t.Errorf("Reset read %v with %d components, want 70x30 gray", b, j.comps)
	}
	if &j.Data()[0] != &jpg[0] {
		t.Error("Reset copied the file")
	}
	// Every truncation must fail cleanly, short of the whole frame header.
	for n := range 200 {
		if err := j.Reset(jpg[:n]); err == nil && n < 20 {
			t.Errorf("Reset accepted %d bytes", n)
		}
	}
	for _, bad := range [][]byte{nil, []byte("not a jpeg"), {0xff, 0xd8, 0xff, 0xda, 0, 2}} {
		if err := j.Reset(bad); err == nil {
			t.Errorf("Reset(%q) accepted", bad)
		}
	}
	if j.Data() != nil {
		t.Error("a failed Reset kept the file")
	}
	var ie ImageEncoder
	var enc piudf.Encoder
	var out bytes.Buffer
	if err := enc.Reset(&out, make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	if _, err := ie.Write(&enc, &JPEG{}); err == nil {
		t.Error("writing the zero JPEG succeeded")
	}
}
