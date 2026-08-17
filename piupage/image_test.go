package piupage

import (
	"bytes"
	"compress/zlib"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"testing"

	"github.com/soypat/piudf"
)

// testRGB is an opaque picture with enough going on in it that the encoder
// cannot collapse it to something unrepresentative.
func testRGB(w, h int) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			im.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 5), B: uint8(x ^ y), A: 255})
		}
	}
	return im
}

func TestPNGPassthrough(t *testing.T) {
	const w, h = 61, 43
	var buf bytes.Buffer
	if err := png.Encode(&buf, testRGB(w, h)); err != nil {
		t.Fatal(err)
	}
	im, err := PNG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if im.W != w || im.H != h {
		t.Errorf("size = %dx%d, want %dx%d", im.W, im.H, w, h)
	}
	if im.Space != DeviceRGB || im.Bits != 8 {
		t.Errorf("space/bits = %v/%d, want DeviceRGB/8", im.Space, im.Bits)
	}
	if im.Filter != "FlateDecode" || im.Predictor != 15 {
		t.Errorf("filter/predictor = %q/%d, want FlateDecode/15", im.Filter, im.Predictor)
	}

	// The whole claim of the pass-through is that the IDAT payload, read as
	// flate under PNG's predictors, is exactly the sample rows a PDF reader
	// expects. If the chunks were joined wrongly this will not inflate, and if
	// the geometry were wrong the row count would not come out.
	zr, err := zlib.NewReader(bytes.NewReader(im.Data))
	if err != nil {
		t.Fatal("IDAT is not a zlib stream:", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal("IDAT does not inflate:", err)
	}
	// One filter byte opens each row, which is what Predictor 15 means.
	if want := h * (1 + w*3); len(raw) != want {
		t.Errorf("inflated %d bytes, want %d rows of 1+%d", len(raw), want, w*3)
	}
}

func TestPNGGray(t *testing.T) {
	const w, h = 17, 9
	g := image.NewGray(image.Rect(0, 0, w, h))
	for i := range g.Pix {
		g.Pix[i] = uint8(i)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, g); err != nil {
		t.Fatal(err)
	}
	im, err := PNG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if im.Space != DeviceGray || im.Space.Comps() != 1 {
		t.Errorf("space = %v with %d components, want DeviceGray with 1", im.Space, im.Space.Comps())
	}
}

// A PNG that cannot be passed through has to say so rather than be written as
// something a reader will render as garbage.
func TestPNGRefusesWhatPDFCannotRead(t *testing.T) {
	const w, h = 8, 8
	alpha := image.NewRGBA(image.Rect(0, 0, w, h)) // not opaque: encoder writes type 6
	pal := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	for _, tc := range []struct {
		name string
		img  image.Image
		want error
	}{
		{"alpha", alpha, errPNGAlpha},
		{"paletted", pal, errPNGPalette},
	} {
		var buf bytes.Buffer
		if err := png.Encode(&buf, tc.img); err != nil {
			t.Fatal(err)
		}
		if _, err := PNG(buf.Bytes()); err != tc.want {
			t.Errorf("%s gave %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := PNG([]byte("not a png at all, no")); err != errPNGSignature {
		t.Errorf("a non-PNG gave %v", err)
	}
	// Truncation must be caught rather than read past.
	var buf bytes.Buffer
	png.Encode(&buf, testRGB(w, h))
	good := buf.Bytes()
	for _, n := range []int{len(good) / 2, len(good) - 3} {
		if _, err := PNG(good[:n]); err == nil {
			t.Errorf("a PNG cut to %d bytes parsed without complaint", n)
		}
	}
}

func TestJPEGPassthrough(t *testing.T) {
	const w, h = 64, 48
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testRGB(w, h), nil); err != nil {
		t.Fatal(err)
	}
	im, err := JPEG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if im.W != w || im.H != h {
		t.Errorf("size = %dx%d, want %dx%d", im.W, im.H, w, h)
	}
	if im.Space != DeviceRGB || im.Bits != 8 {
		t.Errorf("space/bits = %v/%d, want DeviceRGB/8", im.Space, im.Bits)
	}
	if im.Filter != "DCTDecode" || im.Predictor != 0 {
		t.Errorf("filter/predictor = %q/%d, want DCTDecode/0", im.Filter, im.Predictor)
	}
	if len(im.Data) != buf.Len() {
		t.Error("the JPEG's own bytes are not what would be written")
	}

	gray := image.NewGray(image.Rect(0, 0, w, h))
	buf.Reset()
	if err := jpeg.Encode(&buf, gray, nil); err != nil {
		t.Fatal(err)
	}
	if im, err := JPEG(buf.Bytes()); err != nil || im.Space != DeviceGray {
		t.Errorf("a grey JPEG gave %v/%v, want DeviceGray", im.Space, err)
	}
	if _, err := JPEG([]byte{0xff, 0xd9, 0, 0}); err != errJPEGSOI {
		t.Errorf("a non-JPEG gave %v", err)
	}
	if _, err := JPEG(buf.Bytes()[:len(buf.Bytes())/2]); err == nil {
		t.Error("a truncated JPEG parsed without complaint")
	}
}

// The dictionary is the whole of what a reader is told about the samples, so
// every field that decides how they are read has to reach it.
func TestWriteImageDictionary(t *testing.T) {
	got := writeImage(t, Image{
		W: 3, H: 2, Bits: 8, Space: DeviceRGB,
		Filter: "FlateDecode", Predictor: 15,
		Data: []byte("payload"),
	})
	for _, want := range []string{
		"/Type/XObject", "/Subtype/Image", "/Width 3", "/Height 2",
		"/ColorSpace/DeviceRGB", "/BitsPerComponent 8", "/Filter/FlateDecode",
		"/Predictor 15", "/Colors 3", "/Columns 3", "/Length 7",
		"stream", "payload", "endstream",
	} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("the image object does not carry %q:\n%s", want, got)
		}
	}
	// A DCTDecode image has no predictor and must not be given one.
	got = writeImage(t, Image{W: 1, H: 1, Bits: 8, Space: DeviceGray, Filter: "DCTDecode", Data: []byte{1}})
	if bytes.Contains([]byte(got), []byte("DecodeParms")) {
		t.Error("a DCTDecode image was written with /DecodeParms")
	}
}

// writeImage emits one image into a fresh document and returns the bytes.
func writeImage(t *testing.T, im Image) string {
	t.Helper()
	var buf bytes.Buffer
	var enc piudf.Encoder
	if err := enc.Reset(&buf, make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteImage(&enc, im); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestImageValid(t *testing.T) {
	ok := Image{W: 1, H: 1, Bits: 8, Space: DeviceGray, Data: []byte{0}}
	if err := ok.Valid(); err != nil {
		t.Fatal(err)
	}
	for name, im := range map[string]Image{
		"no extent":  {W: 0, H: 1, Bits: 8, Space: DeviceGray, Data: []byte{0}},
		"no space":   {W: 1, H: 1, Bits: 8, Data: []byte{0}},
		"odd depth":  {W: 1, H: 1, Bits: 7, Space: DeviceGray, Data: []byte{0}},
		"no samples": {W: 1, H: 1, Bits: 8, Space: DeviceGray},
	} {
		if err := im.Valid(); err == nil {
			t.Errorf("%s passed validation", name)
		}
	}
}
