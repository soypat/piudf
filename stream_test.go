package piudf

import (
	"bytes"
	"compress/zlib"
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
