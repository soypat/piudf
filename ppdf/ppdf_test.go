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

func openCounted(t testing.TB, name string) (*countingReaderAt, int64) {
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
	err := c.ArrayForEach(nil, strings.NewReader(src), Value{Tok: tokArray, I: 0}, func(v Value) bool {
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
	err := c.ArrayForEach(nil, strings.NewReader(`[1 2 3 4]`), Value{Tok: tokArray, I: 0}, func(Value) bool {
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
	err := c.ArrayForEach(nil, strings.NewReader(`[1 2 3`), Value{Tok: tokArray, I: 0}, func(Value) bool {
		return true
	})
	if err != errUnexpectedEOF {
		t.Errorf("got %v, want %v", err, errUnexpectedEOF)
	}
}

// TestDictForEachKeys pins the literal surviving the pushback queue. Every
// key here follows an integer, so decodeShallow's reference lookahead reads
// each one before DictForEach does and hands it back through pushback; a
// queue that drops literals reports these keys as empty while their values
// stay correct.
func TestDictForEachKeys(t *testing.T) {
	const src = `<</Size 5482/Root 4883 0 R/Prev 12/Info 5481 0 R>>`
	c := newCodec(make([]byte, 2048))
	var got []string
	err := c.DictForEach(nil, strings.NewReader(src), Value{Tok: tokDict, I: 0}, func(key []byte, v Value) bool {
		got = append(got, string(key))
		return true
	})
	if err != nil {
		t.Fatalf("DictForEach: %v", err)
	}
	want := []string{"Size", "Root", "Prev", "Info"}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d: got %q, want %q", i, got[i], want[i])
		}
	}
	// DictGet rides on the same scan: a key it cannot see reads as absent.
	v, err := c.DictGet(nil, strings.NewReader(src), Value{Tok: tokDict, I: 0}, "Root")
	if err != nil {
		t.Fatalf("DictGet: %v", err)
	}
	if id := v.ObjectID(); id.Num != 4883 || id.Gen != 0 {
		t.Errorf("/Root: got %v, want object 4883 gen 0", id)
	}
}

// TestDecodeXrefStream checks the deferred-payload design end to end: the
// chain walks and every section is recorded without a byte of the compressed
// payload being decoded, because /Prev and /W live in the plaintext stream
// dictionary.
func TestDecodeXrefStream(t *testing.T) {
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
	var pdf PDF
	if err := pdf.Decode(c, size, newCodec(make([]byte, 4096))); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(pdf.sections) == 0 {
		t.Fatal("no xref sections decoded")
	}
	t.Logf("%d reads, %d bytes, %d sections, %d revisions",
		c.reads, c.bytes, len(pdf.sections), len(pdf.revs))
	var rows uint32
	for i, s := range pdf.sections {
		if !s.isXrefStream {
			t.Errorf("section %d is classic, want stream", i)
			continue
		}
		if s.length <= 0 || s.fileOff <= 0 || s.fileOff+s.length > size {
			t.Errorf("section %d payload [%d,+%d) outside a %d byte file", i, s.fileOff, s.length, size)
		}
		if s.codec.w[1] == 0 {
			t.Errorf("section %d has zero /W offset width", i)
		}
		if !s.codec.flate {
			t.Errorf("section %d is not FlateDecode", i)
		}
		rows += s.count
	}
	if rows == 0 {
		t.Error("sections cover no rows")
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

// TestLookupSweep walks every object of a cross-reference stream document. It
// is the test the cursor exists for: the sweep asks for 15888 rows in order,
// which one decode pass answers, and PDF holds no more at the end of it than
// at the start. A design that cached the decoded table would pass the
// correctness half of this and fail the held-size half.
func TestLookupSweep(t *testing.T) {
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.MaxLazySections = 4096
	if err := pdf.Decode(c, size, codec); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	held := pdf.SizeOnRAM()
	var kinds [4]int
	var last uint32
	for _, s := range pdf.sections {
		last = max(last, s.firstObj+s.count)
	}
	for num := uint32(0); num < last; num++ {
		e, err := pdf.Lookup(c, num, codec)
		if err != nil {
			t.Fatalf("object %d: %v", num, err)
		}
		if e.ID.Num != num {
			t.Fatalf("object %d came back as %d", num, e.ID.Num)
		}
		switch e.Kind {
		case XrefFree:
			kinds[0]++
		case XrefNormal:
			kinds[1]++
			if e.Offset <= 0 || e.Offset >= size {
				t.Fatalf("object %d at offset %d, outside a %d byte file", num, e.Offset, size)
			}
		case XrefCompressed:
			kinds[2]++
		default:
			t.Fatalf("object %d has kind %v", num, e.Kind)
		}
	}
	t.Logf("%d objects: %d free, %d normal, %d compressed; %d reads, held %d B",
		last, kinds[0], kinds[1], kinds[2], c.reads, held)
	if kinds[1] == 0 || kinds[2] == 0 {
		t.Error("expected both normal and compressed objects")
	}
	if now := pdf.SizeOnRAM(); now != held {
		t.Errorf("held %d B before the sweep and %d B after: the sweep is caching", held, now)
	}
}

// TestLookupBackward pins the cursor's one hard case. Rows are reachable only
// forward, so a backward step restarts the decode from the payload's first
// byte; a restart that kept any state — the row above, the inflate history —
// would answer differently the second time.
func TestLookupBackward(t *testing.T) {
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.MaxLazySections = 4096
	if err := pdf.Decode(c, size, codec); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	nums := [...]uint32{1, 2, 900, 5, 15000, 3, 1}
	var fwd [len(nums)]XrefEntry
	for i, num := range nums {
		e, err := pdf.Lookup(c, num, codec)
		if err != nil {
			t.Fatalf("object %d: %v", num, err)
		}
		fwd[i] = e
	}
	if fwd[0] != fwd[len(nums)-1] {
		t.Errorf("object 1 read as %+v, then as %+v after seeking away", fwd[0], fwd[len(nums)-1])
	}
	// The same objects in increasing order must agree with the jumping order.
	for i, num := range nums {
		var pdf2 PDF
		codec2 := newCodec(make([]byte, 4096))
		codec2.MaxLazySections = 4096
		if err := pdf2.Decode(c, size, codec2); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		e, err := pdf2.Lookup(c, num, codec2)
		if err != nil {
			t.Fatalf("object %d: %v", num, err)
		}
		if e != fwd[i] {
			t.Errorf("object %d: %+v on a fresh cursor, %+v on a seeking one", num, e, fwd[i])
		}
	}
}

// TestResolveCompressed reads an object out of an object stream. The catalog
// is the entry point every other object hangs off, so a /Pages that resolves
// to a page tree node proves the whole chain: xref stream row, object stream
// pair table, and a span whose coordinates address decompressed data.
func TestResolveCompressed(t *testing.T) {
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.MaxLazySections = 4096
	if err := pdf.Decode(c, size, codec); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	rootRef, err := codec.DictGet(&pdf, c, pdf.Trailer(), "Root")
	if err != nil {
		t.Fatalf("/Root: %v", err)
	}
	root, err := pdf.Deref(c, rootRef, codec)
	if err != nil {
		t.Fatalf("resolving /Root: %v", err)
	}
	pagesRef, err := codec.DictGet(&pdf, c, root, "Pages")
	if err != nil {
		t.Fatalf("/Pages: %v", err)
	}
	pages, err := pdf.Deref(c, pagesRef, codec)
	if err != nil {
		t.Fatalf("resolving /Pages: %v", err)
	}
	if pages.Stm == 0 {
		t.Fatal("/Pages is not in an object stream; this test proves nothing")
	}
	countV, err := codec.DictGet(&pdf, c, pages, "Count")
	if err != nil {
		t.Fatalf("/Count: %v", err)
	}
	count, ok := countV.Int()
	if !ok || count <= 0 {
		t.Fatalf("/Count is %v, want a positive integer", countV.Tok)
	}
	t.Logf("catalog in object stream, %d pages", count)
}

// TestLookupAllocs pins the claim the cursors are built on: a lookup decodes
// through the Codec's own storage and the file, and neither escapes. The
// inflate window is allocated once and reused, so it must not show up here.
func TestLookupAllocs(t *testing.T) {
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.MaxLazySections = 4096
	if err := pdf.Decode(c, size, codec); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	num := uint32(0)
	allocs := testing.AllocsPerRun(1000, func() {
		// Increasing object numbers: the sweep the cursor is shaped for.
		num++
		if num > 15000 {
			num = 1
		}
		pdf.Lookup(c, num, codec)
	})
	if allocs > 0 {
		t.Errorf("Lookup allocates %.1f times per call", allocs)
	}
}
