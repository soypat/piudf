package ppdf

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/soypat/piudf/ppdf/piulex"
)

// countingReaderAt records the reads a Decode issues. Read count is the
// primary regression metric of the window rewrite: correctness alone would
// not notice the buffer being discarded on every jump.
type countingReaderAt struct {
	r     io.ReaderAt
	reads int
	bytes int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	n, err := c.r.ReadAt(p, off)
	c.bytes += int64(n)
	return n, err
}

func openCounted(t *testing.T, name string) (*countingReaderAt, int64) {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return &countingReaderAt{r: f}, fi.Size()
}

func newCodec(buf []byte) *Codec {
	c := &Codec{MaxLazySections: 64, MaxDepth: 32}
	c.SetBuffer(buf)
	return c
}

// TestArrayForEach covers the two element kinds a raw token loop gets wrong:
// an indirect reference is three tokens that must arrive as one Value, and a
// nested composite is a span, not its contents. It also pins that the closing
// bracket is never pushed.
func TestArrayForEach(t *testing.T) {
	const src = `[1 0 R 42 (s) [7 8] <</K 1>> /N]`
	c := newCodec(make([]byte, 2048))
	var got []Value
	err := c.ArrayForEach(strings.NewReader(src), Value{Tok: tokArray, I: 0}, func(v Value) bool {
		got = append(got, v)
		return true
	})
	if err != nil {
		t.Fatalf("ArrayForEach: %v", err)
	}
	want := []piulex.Token{
		piulex.TokR, piulex.TokInt, piulex.TokString, tokArray, tokDict, piulex.TokName,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d elements, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Tok != want[i] {
			t.Errorf("element %d: got %v, want %v", i, got[i].Tok, want[i])
		}
	}
	if id := got[0].ObjectID(); id.Num != 1 || id.Gen != 0 {
		t.Errorf("element 0: got %v, want object 1 gen 0", id)
	}
	if n, ok := got[1].Int(); !ok || n != 42 {
		t.Errorf("element 1: got %v (ok=%v), want 42", n, ok)
	}
}

// TestArrayForEachStopsEarly pins the push contract: false means stop, and no
// further element is read.
func TestArrayForEachStopsEarly(t *testing.T) {
	c := newCodec(make([]byte, 2048))
	n := 0
	err := c.ArrayForEach(strings.NewReader(`[1 2 3 4]`), Value{Tok: tokArray, I: 0}, func(Value) bool {
		n++
		return n < 2
	})
	if err != nil {
		t.Fatalf("ArrayForEach: %v", err)
	}
	if n != 2 {
		t.Errorf("push called %d times, want 2", n)
	}
}

// TestArrayForEachUnterminated guards against a truncated array spinning: EOF
// is not the array terminator.
func TestArrayForEachUnterminated(t *testing.T) {
	c := newCodec(make([]byte, 2048))
	err := c.ArrayForEach(strings.NewReader(`[1 2 3`), Value{Tok: tokArray, I: 0}, func(Value) bool {
		return true
	})
	if err != errUnexpectedEOF {
		t.Errorf("got %v, want %v", err, errUnexpectedEOF)
	}
}

// TestDecodeClassicReadCount pins the point of the offset-addressed window:
// sto.pdf is incrementally updated, so its xref chain walks many revisions
// whose trailers sit within a few hundred bytes of each other. Before the
// window each jump refilled, costing ~26 reads.
func TestDecodeClassicReadCount(t *testing.T) {
	c, size := openCounted(t, "../testdata/sto.pdf")
	var pdf PDF
	if err := pdf.Decode(c, size, newCodec(make([]byte, 4096))); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(pdf.sections) == 0 {
		t.Error("no xref sections decoded")
	}
	if len(pdf.revs) < 2 {
		t.Errorf("decoded %d revisions, want >1: /Prev chain did not walk", len(pdf.revs))
	}
	t.Logf("%d reads, %d bytes, %d sections, %d revisions",
		c.reads, c.bytes, len(pdf.sections), len(pdf.revs))
	const maxReads = 12
	if c.reads > maxReads {
		t.Errorf("Decode took %d reads, want <=%d", c.reads, maxReads)
	}
}
