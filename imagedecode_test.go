package piudf

import (
	"bytes"
	"compress/zlib"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/soypat/piudf/piulex"
)

var dumpDir = flag.String("dump", "", "write every corpus image decoded to this directory as PNG")

// obj is one object of a test document: dictionary body dict, followed by
// payload when stream is set.
type obj struct {
	dict    string
	payload []byte
	stream  bool
}

func streamObj(dict string, payload []byte) obj {
	return obj{dict: dict, payload: payload, stream: true}
}

// doc writes objs, numbered from 1 in order, and decodes the result.
func doc(t *testing.T, objs ...obj) (*bytes.Reader, *PDF, *Codec) {
	t.Helper()
	var out bytes.Buffer
	var e Encoder
	if err := e.Reset(&out, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	for i, o := range objs {
		id := e.NewID()
		if id.Num != uint32(i+1) {
			t.Fatalf("object %d numbered %d", i, id.Num)
		}
		e.BeginObject(id)
		if !o.stream {
			e.Raw([]byte("<< " + o.dict + " >>"))
			e.EndObject()
			continue
		}
		e.Raw(fmt.Appendf(nil, "<< /Length %d %s >>", len(o.payload), o.dict))
		e.BeginStreamPayload()
		e.PayloadWriter().Write(o.payload)
		e.EndStreamPayload()
		e.EndObject()
	}
	cat := e.NewID()
	e.BeginObject(cat)
	e.Raw([]byte("<< /Type /Catalog >>"))
	e.EndObject()
	if err := e.Close(cat, ObjectID{}); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(out.Bytes())
	pdf, codec := decodeDoc(t, r, r.Size())
	return r, pdf, codec
}

func decodeDoc(t *testing.T, r interface {
	ReadAt([]byte, int64) (int, error)
}, size int64) (*PDF, *Codec) {
	t.Helper()
	var codec Codec
	err := codec.Configure(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})
	if err != nil {
		t.Fatal(err)
	}
	pdf := new(PDF)
	if err := pdf.Decode(r, size, &codec); err != nil {
		t.Fatal(err)
	}
	return pdf, &codec
}

// decodeObj decodes image object num.
func decodeObj(t *testing.T, d *ImageDecoder, r *bytes.Reader, pdf *PDF, codec *Codec, num uint32) (image.Image, error) {
	t.Helper()
	v, err := pdf.Resolve(r, ObjectID{Num: num}, codec)
	if err != nil {
		t.Fatal(err)
	}
	info, err := pdf.ImageInfo(r, v, codec)
	if err != nil {
		t.Fatal(err)
	}
	return d.Decode(nil, pdf, r, info, codec)
}

// pixels returns img's pixels as non-premultiplied RGBA, row major.
func pixels(img image.Image) []color.NRGBA {
	b := img.Bounds()
	var out []color.NRGBA
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out = append(out, color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA))
		}
	}
	return out
}

func gray(v uint8) color.NRGBA          { return color.NRGBA{v, v, v, 255} }
func rgb(r, g, b uint8) color.NRGBA     { return color.NRGBA{r, g, b, 255} }
func rgba(r, g, b, a uint8) color.NRGBA { return color.NRGBA{r, g, b, a} }

// flateW and flateH are the size of imageDoc's FlateDecode image, whose pixel
// (x, y) is gray level x+y. Its samples outnumber the decoder's read buffer.
const flateW, flateH = 100, 50

// imageDoc writes one image of each kind ImageDecoder handles, and some it
// does not, and returns the JPEG file object 13 holds.
func imageDoc(t *testing.T) (r *bytes.Reader, pdf *PDF, codec *Codec, jpg []byte) {
	t.Helper()
	const img = "/Type /XObject /Subtype /Image "
	var jbuf bytes.Buffer
	src := image.NewGray(image.Rect(0, 0, 8, 8))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 4)
	}
	if err := jpeg.Encode(&jbuf, src, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	for y := range flateH {
		for x := range flateW {
			zw.Write([]byte{byte(x + y)})
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	r, pdf, codec = doc(t,
		// 1: RGB, 2x1.
		streamObj(img+"/Width 2 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceRGB", []byte{255, 0, 0, 0, 0, 255}),
		// 2: 1-bit gray, inverted, 3x2: rows pad to a byte.
		streamObj(img+"/Width 3 /Height 2 /BitsPerComponent 1 /ColorSpace /DeviceGray /Decode [1 0]", []byte{0b101_00000, 0b010_00000}),
		// 3: 2-bit indexed over RGB, hex lookup, 4x1.
		streamObj(img+"/Width 4 /Height 1 /BitsPerComponent 2 /ColorSpace [/Indexed /DeviceRGB 2 <ff000000ff000000ff>]", []byte{0b00_01_10_11}),
		// 4: indexed with the lookup in a stream (object 5), over gray.
		streamObj(img+"/Width 2 /Height 1 /BitsPerComponent 8 /ColorSpace [/Indexed /DeviceGray 1 5 0 R]", []byte{1, 0}),
		streamObj("", []byte{10, 200}),
		// 6: 16-bit RGB.
		streamObj(img+"/Width 1 /Height 1 /BitsPerComponent 16 /ColorSpace /DeviceRGB", []byte{0xff, 0xff, 0x80, 0x00, 0, 0}),
		// 7: CMYK.
		streamObj(img+"/Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceCMYK", []byte{0, 255, 255, 0}),
		// 8: RGB with soft mask 9 at half resolution.
		streamObj(img+"/Width 2 /Height 2 /BitsPerComponent 8 /ColorSpace /DeviceRGB /SMask 9 0 R",
			[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}),
		streamObj(img+"/Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray", []byte{128}),
		// 10: gray with a color key keying out 0..10.
		streamObj(img+"/Width 3 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray /Mask [0 10]", []byte{5, 11, 200}),
		// 11: stencil mask, paints where 0.
		streamObj(img+"/Width 2 /Height 1 /ImageMask true", []byte{0b01_000000}),
		// 12: gray with stencil /Mask 11.
		streamObj(img+"/Width 2 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray /Mask 11 0 R", []byte{50, 60}),
		// 13: JPEG.
		streamObj(img+"/Width 8 /Height 8 /BitsPerComponent 8 /ColorSpace /DeviceGray /Filter /DCTDecode", jbuf.Bytes()),
		// 14: Lab and 15: JPX, unsupported.
		streamObj(img+"/Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace [/Lab << /WhitePoint [1 1 1] >>]", []byte{1, 2, 3}),
		streamObj(img+"/Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceRGB /Filter /JPXDecode", []byte{1}),
		// 16: truncated: second row missing.
		streamObj(img+"/Width 1 /Height 2 /BitsPerComponent 8 /ColorSpace /DeviceGray", []byte{77}),
		// 17: FlateDecode gray.
		streamObj(img+fmt.Sprintf("/Width %d /Height %d /BitsPerComponent 8 /ColorSpace /DeviceGray /Filter /FlateDecode", flateW, flateH), zbuf.Bytes()),
		// 18: RGB with soft mask 19 at a higher resolution, and a color key.
		streamObj(img+"/Width 2 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceRGB /SMask 19 0 R /Mask [0 0 0 0 0 0]", []byte{1, 2, 3, 0, 0, 0}),
		streamObj(img+"/Width 4 /Height 3 /BitsPerComponent 8 /ColorSpace /DeviceGray", []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120}),
		// 20: JPEG in a Separation space: its samples are ink tints, which
		// read as gray levels come out inverted.
		streamObj(img+"/Width 8 /Height 8 /BitsPerComponent 8 /ColorSpace [/Separation /Black /DeviceGray 5 0 R] /Filter /DCTDecode", jbuf.Bytes()),
	)
	return r, pdf, codec, jbuf.Bytes()
}

func TestImageDecode(t *testing.T) {
	r, pdf, codec, jpg := imageDoc(t)
	jpegWant, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatal(err)
	}
	var flateWant []color.NRGBA
	for y := range flateH {
		for x := range flateW {
			flateWant = append(flateWant, gray(uint8(x+y)))
		}
	}
	for _, tc := range []struct {
		num  uint32
		want []color.NRGBA
		err  error
	}{
		{1, []color.NRGBA{rgb(255, 0, 0), rgb(0, 0, 255)}, nil},
		{2, []color.NRGBA{gray(0), gray(255), gray(0), gray(255), gray(0), gray(255)}, nil},
		{3, []color.NRGBA{rgb(255, 0, 0), rgb(0, 255, 0), rgb(0, 0, 255), rgb(0, 0, 255)}, nil}, // Index 3 clamps to hival.
		{4, []color.NRGBA{gray(200), gray(10)}, nil},
		{6, []color.NRGBA{rgb(255, 128, 0)}, nil},
		{7, []color.NRGBA{rgb(255, 0, 0)}, nil},
		{8, []color.NRGBA{rgba(1, 2, 3, 128), rgba(4, 5, 6, 128), rgba(7, 8, 9, 128), rgba(10, 11, 12, 128)}, nil},
		{10, []color.NRGBA{rgba(5, 5, 5, 0), gray(11), gray(200)}, nil},
		// An *image.Alpha carries no color: opaque reads back as white.
		{11, []color.NRGBA{rgba(255, 255, 255, 255), rgba(0, 0, 0, 0)}, nil},
		{12, []color.NRGBA{gray(50), rgba(60, 60, 60, 0)}, nil},
		{13, pixels(jpegWant), nil},
		{14, nil, ErrUnsupportedImage},
		{15, nil, ErrUnsupportedImage},
		{16, []color.NRGBA{gray(77), gray(0)}, nil},
		{17, flateWant, nil},
		// Mask row 0 of 3 lands, columns 0 and 2 of 4; the second pixel is
		// keyed out whatever the mask says.
		{18, []color.NRGBA{rgba(1, 2, 3, 10), rgba(0, 0, 0, 0)}, nil},
		{20, nil, ErrUnsupportedImage},
	} {
		var d ImageDecoder
		got, err := decodeObj(t, &d, r, pdf, codec, tc.num)
		if !errors.Is(err, tc.err) {
			t.Errorf("object %d: err = %v, want %v", tc.num, err, tc.err)
			continue
		} else if err != nil {
			continue
		}
		if px := pixels(got); fmt.Sprint(px) != fmt.Sprint(tc.want) {
			t.Errorf("object %d (%T): got %v, want %v", tc.num, got, px, tc.want)
		}
	}

	d := ImageDecoder{MaxPixels: 1}
	if _, err := decodeObj(t, &d, r, pdf, codec, 1); !errors.Is(err, errImageTooLarge) {
		t.Errorf("MaxPixels 1: err = %v, want %v", err, errImageTooLarge)
	}
}

// TestImageDecodeAdobeCMYK pins a CMYK JPEG without /Decode decoding to its
// samples as stored, which is how PDF readers take them: the corpus file's
// photographs show right that way and as negatives through image/jpeg, which
// inverts four-component JPEGs as a JPEG file reader must.
func TestImageDecodeAdobeCMYK(t *testing.T) {
	f, err := os.ReadFile("testdata/basis-of-embedded.pdf")
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(f)
	pdf, codec := decodeDoc(t, r, r.Size())
	codec.rows.setCache(make([]byte, pdf.XrefCacheSize()))
	sizeV, err := codec.DictGet(pdf, r, pdf.Trailer(), "Size")
	if err != nil {
		t.Fatal(err)
	}
	size, _ := sizeV.Int()
	checked := 0
	for num := uint32(1); num < uint32(size); num++ {
		v, err := pdf.Resolve(r, ObjectID{Num: num}, codec)
		if err != nil || v.Tok != piulex.TokStream {
			continue
		}
		info, err := pdf.ImageInfo(r, v, codec)
		if err != nil || !isDCT(&info) {
			continue
		}
		if !info.Decode.IsNull() {
			t.Fatalf("object %d has a /Decode; the corpus is expected to have none", num)
		}
		var d ImageDecoder
		img, err := d.Decode(nil, pdf, r, info, codec)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := img.(*image.CMYK)
		if !ok {
			t.Fatalf("object %d decoded to %T, want *image.CMYK", num, img)
		}
		sp := info.Payload
		jimg, err := jpeg.Decode(bytes.NewReader(f[sp.Offset : sp.Offset+sp.Length]))
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range jimg.(*image.CMYK).Pix {
			if got.Pix[i] != 255-v {
				t.Fatalf("object %d: sample %d = %d, want the stored %d", num, i, got.Pix[i], 255-v)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no DCTDecode images in the corpus file")
	}
}

// TestImageDecodeJPEGHeaderSize pins MaxPixels holding against the JPEG's own
// frame header, which is what image/jpeg allocates from: a dictionary claiming
// 1x1 over a JPEG claiming 65535x65535 must be refused, not decoded.
func TestImageDecodeJPEGHeaderSize(t *testing.T) {
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	jpg := jbuf.Bytes()
	sof := bytes.Index(jpg, []byte{0xff, 0xc0})
	if sof < 0 {
		t.Fatal("no SOF0 in encoded JPEG")
	}
	// SOF0: marker, length(2), precision(1), height(2), width(2).
	jpg[sof+5], jpg[sof+6], jpg[sof+7], jpg[sof+8] = 0xff, 0xff, 0xff, 0xff
	const img = "/Type /XObject /Subtype /Image /BitsPerComponent 8 /ColorSpace /DeviceGray /Filter /DCTDecode "
	r, pdf, codec := doc(t,
		streamObj(img+"/Width 1 /Height 1", jpg),
		// The same JPEG as another image's soft mask.
		streamObj(img+"/Width 1 /Height 1 /SMask 1 0 R", jpg),
	)
	for num := uint32(1); num <= 2; num++ {
		var d ImageDecoder
		if _, err := decodeObj(t, &d, r, pdf, codec, num); !errors.Is(err, errImageTooLarge) {
			t.Errorf("object %d: err = %v, want %v", num, err, errImageTooLarge)
		}
	}
}

// TestImageDecodeAllocs pins decoding costing nothing once warm: the decoder's
// scratch is fixed-size and the caller's image is reused. A decoder that
// buffered the samples, or built a fresh image per call, would still pass
// TestImageDecode. JPEG is left out, image/jpeg allocating as it decodes.
func TestImageDecodeAllocs(t *testing.T) {
	r, pdf, codec, _ := imageDoc(t)
	var d ImageDecoder
	for _, num := range []uint32{1, 2, 3, 4, 6, 7, 8, 10, 11, 12, 16, 17, 18} {
		v, err := pdf.Resolve(r, ObjectID{Num: num}, codec)
		if err != nil {
			t.Fatal(err)
		}
		info, err := pdf.ImageInfo(r, v, codec)
		if err != nil {
			t.Fatal(err)
		}
		dst, err := d.Decode(nil, pdf, r, info, codec)
		if err != nil {
			t.Fatalf("object %d: %v", num, err)
		}
		want := fmt.Sprint(pixels(dst))
		allocs := testing.AllocsPerRun(20, func() {
			dst, err = d.Decode(dst, pdf, r, info, codec)
		})
		if err != nil {
			t.Fatalf("object %d: %v", num, err)
		} else if allocs > 0 {
			t.Errorf("object %d: Decode allocates %.1f times into a warm dst", num, allocs)
		}
		if got := fmt.Sprint(pixels(dst)); got != want {
			t.Errorf("object %d: decoding into dst gives %v, want %v", num, got, want)
		}
	}
}

// TestImageDecodeCorpus decodes every image the corpus holds. Run with
// -dump=<dir> to look at them.
func TestImageDecodeCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus sweep")
	}
	for _, fpath := range []string{"testdata/sto.pdf", "testdata/rp2350-datasheet.pdf", "testdata/basis-of-embedded.pdf"} {
		f, err := os.Open(fpath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		st, _ := f.Stat()
		pdf, codec := decodeDoc(t, f, st.Size())
		codec.rows.setCache(make([]byte, pdf.XrefCacheSize()))
		sizeV, err := codec.DictGet(pdf, f, pdf.Trailer(), "Size")
		if err != nil {
			t.Fatal(err)
		}
		size, _ := sizeV.Int()
		var d ImageDecoder
		var last image.Image
		ok, unsupported := 0, 0
		for num := uint32(1); num < uint32(size); num++ {
			e, err := pdf.Lookup(f, num, codec)
			if err != nil || e.Kind == XrefFree {
				continue
			}
			v, err := pdf.Resolve(f, e.ID, codec)
			if err != nil || v.Tok != piulex.TokStream {
				continue
			}
			st, err := codec.DictGet(pdf, f, v, "Subtype")
			if err != nil {
				t.Fatal(err)
			}
			if is, _ := codec.NameIs(pdf, f, st, "Image"); !is {
				continue
			}
			info, err := pdf.ImageInfo(f, v, codec)
			if err != nil {
				t.Errorf("%s: object %d: %v", fpath, num, err)
				continue
			}
			// Each image decodes into the last, as a viewer would.
			img, err := d.Decode(last, pdf, f, info, codec)
			switch {
			case errors.Is(err, ErrUnsupportedImage):
				unsupported++
				t.Logf("%s: object %d unsupported: %v %v", fpath, num, info.ColorSpace, info.Payload.Filter(0))
				continue
			case err != nil:
				t.Errorf("%s: object %d: %v", fpath, num, err)
				continue
			}
			ok++
			last = img
			if b := img.Bounds(); b.Dx() != info.Width || b.Dy() != info.Height {
				t.Errorf("%s: object %d: decoded %v, want %dx%d", fpath, num, b, info.Width, info.Height)
			}
			if *dumpDir != "" {
				name := fmt.Sprintf("%s-%d.png", filepath.Base(fpath), num)
				var buf bytes.Buffer
				if err := png.Encode(&buf, img); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(*dumpDir, name), buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Logf("%s: %d decoded, %d unsupported", fpath, ok, unsupported)
	}
}
