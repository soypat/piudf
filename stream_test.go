package piudf

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"testing"

	pzlib "github.com/soypat/piudf/internal/zlib"
)

// newStream returns a Stream configured with default inflate memory.
func newStream(t testing.TB) *Stream {
	t.Helper()
	var s Stream
	if err := s.Configure(pzlib.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	return &s
}

// payload returns src as a file: the compressed bytes preceded by padding, so
// the span offset under test is not zero and a Stream ignoring it would read
// the wrong bytes.
func payload(t testing.TB, src []byte) (r io.ReaderAt, off, length int64) {
	t.Helper()
	const pad = "%PDF-1.5 padding"
	var buf bytes.Buffer
	buf.WriteString(pad)
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes()), int64(len(pad)), int64(buf.Len() - len(pad))
}

func testData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

// TestStreamReadAt covers the seek a Stream emulates. Reading backward is the
// case worth pinning: the bytes are gone, so the answer can only come from
// decoding the span again, and it must be the same answer.
func TestStreamReadAt(t *testing.T) {
	src := testData(5000)
	r, off, length := payload(t, src)
	s := newStream(t)
	if err := s.Reset(r, off, length, true); err != nil {
		t.Fatal(err)
	}
	// Offsets out of order: forward skips, backward rewinds, and one repeat.
	for _, at := range []int64{0, 100, 4000, 7, 4999, 4000, 0, 100} {
		var got [3]byte
		n, err := s.ReadAt(got[:], at)
		want := src[at:min(at+3, int64(len(src)))]
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt(%d): %v", at, err)
		}
		if !bytes.Equal(got[:n], want) {
			t.Errorf("ReadAt(%d) = %q, want %q", at, got[:n], want)
		}
	}
}

// TestStreamReadAtEOF pins what a Window reads the end of the data from: a
// short read must report io.EOF, and a read past the end must report it
// without bytes.
func TestStreamReadAtEOF(t *testing.T) {
	src := testData(10)
	r, off, length := payload(t, src)
	s := newStream(t)
	if err := s.Reset(r, off, length, true); err != nil {
		t.Fatal(err)
	}
	var buf [8]byte
	n, err := s.ReadAt(buf[:], 6)
	if n != 4 || err != io.EOF {
		t.Errorf("ReadAt at 6 of 10 bytes = (%d, %v), want (4, EOF)", n, err)
	}
	if !bytes.Equal(buf[:n], src[6:]) {
		t.Errorf("ReadAt at 6 = %q, want %q", buf[:n], src[6:])
	}
	if n, err = s.ReadAt(buf[:], 10); n != 0 || err != io.EOF {
		t.Errorf("ReadAt at the end = (%d, %v), want (0, EOF)", n, err)
	}
}

// TestStreamUncompressed pins the pass-through: an unfiltered span is the file
// itself, and must read the same as a compressed one.
func TestStreamUncompressed(t *testing.T) {
	const file = "0123456789abcdef"
	var s Stream
	if err := s.Reset(bytes.NewReader([]byte(file)), 4, 6, false); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(&s)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "456789" {
		t.Errorf("read %q, want %q: the span is not [4,10)", got, "456789")
	}
}

// TestStreamRebind pins the reuse the Codec depends on: one Stream serves many
// spans, and Reset must leave nothing of the last behind.
func TestStreamRebind(t *testing.T) {
	a, b := testData(3000), testData(40)
	ra, offA, lenA := payload(t, a)
	rb, offB, lenB := payload(t, b)
	s := newStream(t)
	if err := s.Reset(ra, offA, lenA, true); err != nil {
		t.Fatal(err)
	}
	var buf [4]byte
	if _, err := s.ReadAt(buf[:], 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(rb, offB, lenB, true); err != nil {
		t.Fatal(err)
	}
	if s.Pos() != 0 {
		t.Errorf("Pos is %d after Reset, want 0", s.Pos())
	}
	if s.Reader() != rb || s.Offset() != offB {
		t.Error("Reader/Offset still report the old span")
	}
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, b) {
		t.Errorf("read %q after rebinding, want %q", got, b)
	}
}

func TestStreamUnbound(t *testing.T) {
	var s Stream
	var buf [1]byte
	if _, err := s.Read(buf[:]); err != ErrStreamUnbound {
		t.Errorf("Read on the zero Stream = %v, want ErrStreamUnbound", err)
	}
	if _, err := s.ReadAt(buf[:], 0); err != ErrStreamUnbound {
		t.Errorf("ReadAt on the zero Stream = %v, want ErrStreamUnbound", err)
	}
}

// TestStreamAllocs pins reading costing nothing, which is the whole claim: the
// inflate memory is configured once and every read after works out of it. A
// Stream that reallocated per read would still pass every test above.
func TestStreamAllocs(t *testing.T) {
	src := testData(2000)
	r, off, length := payload(t, src)
	s := newStream(t)
	if err := s.Reset(r, off, length, true); err != nil {
		t.Fatal(err)
	}
	var buf [16]byte
	at := int64(0)
	allocs := testing.AllocsPerRun(100, func() {
		// Forward, the way a lexer reads: no rewind, no Reset.
		at = (at + 16) % 1500
		s.ReadAt(buf[:], at)
	})
	if allocs > 0 {
		t.Errorf("ReadAt allocates %.1f times per call", allocs)
	}
}

// pngIDAT encodes img with image/png and returns its concatenated IDAT data.
// That is a zlib stream of PNG-filtered rows, which is byte for byte what a
// PDF FlateDecode stream with /Predictor 15 holds: an encoder this package did
// not write, choosing a filter per row, is the oracle.
func pngIDAT(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()[8:] // Signature.
	var idat []byte
	for len(b) >= 12 {
		n := int(binary.BigEndian.Uint32(b))
		if string(b[4:8]) == "IDAT" {
			idat = append(idat, b[8:8+n]...)
		}
		b = b[12+n:]
	}
	return idat
}

// spanOf places data, already compressed, in a file after some padding and
// returns its span.
func spanOf(data []byte) (r io.ReaderAt, off, length int64) {
	const pad = "%PDF-1.5 padding"
	return bytes.NewReader(append([]byte(pad), data...)), int64(len(pad)), int64(len(data))
}

// flateCodec returns a FlateDecode codec with the given predictor parameters.
func flateCodec(predictor uint8, columns uint16, colors, bpc uint8) streamCodec {
	return streamCodec{
		filters: [maxFilters]Filter{FilterFlate}, nfilters: 1,
		predictor: predictor, columns: columns, colors: colors, bpc: bpc,
	}
}

// TestStreamPredictorPNG decodes images image/png encoded, at every pixel
// width a PDF image commonly has: Sub, Average and Paeth reach back bpp bytes,
// so a bpp fixed at one decodes gray right and everything else wrong.
func TestStreamPredictorPNG(t *testing.T) {
	const w, h = 37, 23 // Odd sizes: no row or pixel lines up with anything.
	gray := image.NewGray(image.Rect(0, 0, w, h))
	gray16 := image.NewGray16(gray.Rect)
	rgb := image.NewNRGBA(gray.Rect)
	rgb16 := image.NewNRGBA64(gray.Rect)
	for y := range h {
		for x := range w {
			// Gradients with noise make the encoder pick every filter.
			v := uint8(x*7 + y*3 + (x*y)%5)
			gray.SetGray(x, y, color.Gray{v})
			gray16.SetGray16(x, y, color.Gray16{uint16(v)<<8 | uint16(x)})
			rgb.SetNRGBA(x, y, color.NRGBA{v, uint8(y * 11), uint8(x ^ y), 255})
			rgb16.SetNRGBA64(x, y, color.NRGBA64{uint16(v) * 257, uint16(y), uint16(x * y), 0xffff})
		}
	}
	for _, tc := range []struct {
		name        string
		img         image.Image
		colors, bpc uint8
		want        []byte
	}{
		{"gray8", gray, 1, 8, gray.Pix},
		{"gray16", gray16, 1, 16, gray16.Pix},
		// Opaque, so the encoder writes RGB and drops alpha.
		{"rgb8", rgb, 3, 8, dropAlpha(rgb.Pix, 1)},
		{"rgb16", rgb16, 3, 16, dropAlpha(rgb16.Pix, 2)},
	} {
		r, off, n := spanOf(pngIDAT(t, tc.img))
		sc := flateCodec(15, w, tc.colors, tc.bpc)
		s := newStream(t)
		if err := s.resetCodec(r, off, n, &sc); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, err := io.ReadAll(s)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("%s: decoded %d bytes differ from %d source bytes", tc.name, len(got), len(tc.want))
		}
		// Reading backward rewinds, which must restart the row chain from
		// the zero row.
		var buf [5]byte
		if _, err := s.ReadAt(buf[:], 3); err != nil || !bytes.Equal(buf[:], got[3:8]) {
			t.Errorf("%s: ReadAt after full read = % x, %v; want % x", tc.name, buf, err, got[3:8])
		}
	}
}

// dropAlpha returns the color samples of NRGBA or NRGBA64 pixels pix, whose
// samples are size bytes each, without their alpha.
func dropAlpha(pix []byte, size int) []byte {
	var out []byte
	for i := 0; i < len(pix); i += 4 * size {
		out = append(out, pix[i:i+3*size]...)
	}
	return out
}

// TestStreamPredictorTIFF covers predictor 2, which stores each sample as its
// difference from the same component one pixel left.
func TestStreamPredictorTIFF(t *testing.T) {
	const cols, colors = 5, 3
	for _, bpc := range []uint8{8, 16} {
		size := int(bpc) / 8
		rowlen := cols * colors * size
		want := testData(3 * rowlen)
		enc := append([]byte(nil), want...)
		for row := 0; row < len(enc); row += rowlen {
			for i := colors * size; i < rowlen; i += size {
				j := row + i
				if size == 1 {
					enc[j] = want[j] - want[j-colors]
					continue
				}
				v := binary.BigEndian.Uint16(want[j:]) - binary.BigEndian.Uint16(want[j-colors*2:])
				binary.BigEndian.PutUint16(enc[j:], v)
			}
		}
		r, off, n := payload(t, enc)
		sc := flateCodec(2, cols, colors, bpc)
		s := newStream(t)
		if err := s.resetCodec(r, off, n, &sc); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(s)
		if err != nil {
			t.Fatalf("bpc %d: %v", bpc, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("bpc %d: got % x\nwant % x", bpc, got, want)
		}
	}
}

// TestStreamPredictorShortRow pins that a final row cut short is returned as
// far as it goes, not dropped and not reported as corruption.
func TestStreamPredictorShortRow(t *testing.T) {
	rows := []byte{
		2, 1, 2, 3, 4, // Up against the zero row.
		2, 1, 1, 1, 1, // Up.
		2, 5, 5, // Cut short.
	}
	r, off, n := payload(t, rows)
	sc := flateCodec(12, 4, 1, 8)
	s := newStream(t)
	if err := s.resetCodec(r, off, n, &sc); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 2, 3, 4, 2, 3, 4, 5, 7, 8}
	if !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

// TestStreamPredictorBounds pins the parameters refused up front, before any
// row storage is sized from them.
func TestStreamPredictorBounds(t *testing.T) {
	r, off, n := payload(t, []byte{0})
	for _, tc := range []struct {
		sc   streamCodec
		want error
	}{
		{flateCodec(15, 65535, 255, 8), ErrCodecMemLimit},
		{flateCodec(7, 1, 1, 8), errBadPredictor},
		{flateCodec(2, 1, 1, 4), errTODO},
	} {
		s := newStream(t)
		if err := s.resetCodec(r, off, n, &tc.sc); !errors.Is(err, tc.want) {
			t.Errorf("predictor %d: err = %v, want %v", tc.sc.predictor, err, tc.want)
		}
		if _, err := s.Read(make([]byte, 1)); err != ErrStreamUnbound {
			t.Errorf("predictor %d: read after failed reset = %v, want unbound", tc.sc.predictor, err)
		}
	}
}

// TestStreamPredictorAllocs pins the row storage as a one-time cost: a second
// image through the same Stream reuses it.
func TestStreamPredictorAllocs(t *testing.T) {
	r, off, n := payload(t, make([]byte, 4*(1+64)))
	sc := flateCodec(12, 64, 1, 8)
	s := newStream(t)
	if err := s.resetCodec(r, off, n, &sc); err != nil {
		t.Fatal(err)
	}
	var buf [64]byte
	allocs := testing.AllocsPerRun(50, func() {
		s.resetCodec(r, off, n, &sc)
		for {
			if _, err := s.Read(buf[:]); err != nil {
				break
			}
		}
	})
	if allocs > 0 {
		t.Errorf("predicted read allocates %.1f times per image", allocs)
	}
}
