package piudf

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/soypat/piudf/piulex"
)

const (
	fpathBook    = "testdata/sto.pdf"
	fpathMCUSpec = "testdata/rp2350-datasheet.pdf"
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
	return newCodecCfg(DecoderConfig{Buffer: buf, MaxLazySections: 64, MaxDepth: 32})
}

func newCodecCfg(cfg DecoderConfig) *Codec {
	c := new(Codec)
	if err := c.Configure(cfg); err != nil {
		panic(err)
	}
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
	c, size := openCounted(t, fpathMCUSpec)
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
		if !s.codec.flate() {
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
	c, size := openCounted(t, fpathBook)
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
	c, size := openCounted(t, fpathMCUSpec)
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.maxLazySections = 4096
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
	c, size := openCounted(t, fpathMCUSpec)
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.maxLazySections = 4096
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
		codec2 := newCodecCfg(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})
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
	c, size := openCounted(t, fpathMCUSpec)
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.maxLazySections = 4096
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

// TestResolveCompressedLookahead resolves every compressed object of the
// datasheet ascending, where the pairs a walk keeps serve the objects after it,
// then a sample descending, where each resolve walks the table afresh. Both
// must agree. The stride is coprime with objStmAhead so the sample lands on
// every position of the window; descending is the slow path, hence a sample.
func TestResolveCompressedLookahead(t *testing.T) {
	c, size := openCounted(t, fpathMCUSpec)
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.maxLazySections = 4096
	if err := pdf.Decode(c, size, codec); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	// The descending pass would otherwise restart the xref decode per object.
	codec.rows.setCache(make([]byte, pdf.XrefCacheSize()))
	var last uint32
	for _, s := range pdf.sections {
		last = max(last, s.firstObj+s.count)
	}
	var ids []ObjectID
	for num := uint32(1); num < last; num++ {
		e, err := pdf.Lookup(c, num, codec)
		if err != nil {
			t.Fatalf("object %d: %v", num, err)
		}
		if e.Kind == XrefCompressed {
			ids = append(ids, e.ID)
		}
	}
	asc := make([]Value, len(ids))
	for i, id := range ids {
		v, err := pdf.Resolve(c, id, codec)
		if err != nil {
			t.Fatalf("object %d ascending: %v", id.Num, err)
		}
		asc[i] = v
	}
	for i := len(ids) - 1; i >= 0; i -= 17 {
		v, err := pdf.Resolve(c, ids[i], codec)
		if err != nil {
			t.Fatalf("object %d descending: %v", ids[i].Num, err)
		}
		if v != asc[i] {
			t.Errorf("object %d: %+v descending, %+v ascending", ids[i].Num, v, asc[i])
		}
	}
	t.Logf("%d compressed objects", len(ids))
}

// TestLookupAllocs pins the claim the cursors are built on: a lookup decodes
// through the Codec's own storage and the file, and neither escapes. The
// inflate window is allocated once and reused, so it must not show up here.
func TestLookupAllocs(t *testing.T) {
	c, size := openCounted(t, fpathMCUSpec)
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.maxLazySections = 4096
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

// TestAppendString covers the three spellings PDF gives the same text, and the
// escapes each one hides. Reading a span raw and stripping the delimiters —
// which is what a caller without this does — gets every one of these wrong.
func TestAppendString(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		tok  piulex.Token
		want string
	}{
		{"plain name", `/Type`, piulex.TokName, "Type"},
		{"name with hex escape", `/Adobe#20Green`, piulex.TokName, "Adobe Green"},
		{"name with escaped slash", `/A#2FB`, piulex.TokName, "A/B"},
		{"plain string", `(hello)`, piulex.TokString, "hello"},
		{"string with escapes", `(a\(b\)c\\d)`, piulex.TokString, `a(b)c\d`},
		{"string with newline escape", `(line\nnext)`, piulex.TokString, "line\nnext"},
		{"string with octal", `(\101\102)`, piulex.TokString, "AB"},
		{"nested parens", `(a (b) c)`, piulex.TokString, "a (b) c"},
		{"hex string", `<48656C6C6F>`, piulex.TokHexString, "Hello"},
		{"hex odd digit", `<48656C6C6F7>`, piulex.TokHexString, "Hellop"}, // Trailing digit pads with 0.
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCodec(make([]byte, 4096))
			v := Value{Tok: tc.tok, I: 0}
			got, err := c.AppendString(nil, nil, strings.NewReader(tc.src), v)
			if err != nil {
				t.Fatalf("AppendString(%s): %v", tc.src, err)
			}
			if string(got) != tc.want {
				t.Errorf("AppendString(%s) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestAppendStringAppends pins the append contract: dst is the caller's and is
// added to, not replaced.
func TestAppendStringAppends(t *testing.T) {
	c := newCodec(make([]byte, 4096))
	dst := []byte("prefix:")
	dst, err := c.AppendString(dst, nil, strings.NewReader(`/Name`), Value{Tok: piulex.TokName})
	if err != nil {
		t.Fatal(err)
	}
	if string(dst) != "prefix:Name" {
		t.Errorf("got %q, want %q", dst, "prefix:Name")
	}
}

// TestAppendStringMismatch pins the refusal: a Value that is not text has no
// text, and saying so beats returning the bytes that happen to sit there.
func TestAppendStringMismatch(t *testing.T) {
	c := newCodec(make([]byte, 4096))
	for _, v := range []Value{
		{Tok: piulex.TokInt, I: 42},
		{Tok: tokDict, I: 0},
		{Tok: piulex.TokR, N: 1},
	} {
		if _, err := c.AppendString(nil, nil, strings.NewReader(`(text)`), v); err != errValueMismatch {
			t.Errorf("AppendString of %v = %v, want errValueMismatch", v.Tok, err)
		}
	}
}

// TestValueDurability pins the file-coordinate model against the one thing
// that could break it. A Value is coordinates and a PDF is an index, so
// neither goes stale — but a Codec is not stateless: its lexer window and its
// two cursors hold bytes of whatever document was read last, and they tell
// documents apart by comparing io.ReaderAt identity. Get that comparison wrong
// and document A is answered with document B's bytes, which is a wrong answer
// rather than an error.
//
// The two files are chosen to land on opposite paths: sto.pdf is a classic
// table read a record at a time, rp2350 is a cross-reference stream read
// through the row cursor with a catalog inside an object stream. Reading
// either moves every cursor the other one uses.
func TestValueDurability(t *testing.T) {
	rA, sizeA := openCounted(t, fpathBook)
	rB, sizeB := openCounted(t, fpathMCUSpec)
	codec := newCodecCfg(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})

	// One Codec, two documents. The Codec is the scratch both share; the
	// indexes are separate and must stay so.
	var pA, pB PDF
	if err := pA.Decode(rA, sizeA, codec); err != nil {
		t.Fatalf("Decode A: %v", err)
	}
	catA := mustCatalog(t, &pA, rA, codec)
	sizeValA := mustTrailerSize(t, &pA, rA, codec)
	lookA, err := pA.Lookup(rA, catA.num, codec)
	if err != nil {
		t.Fatalf("Lookup A: %v", err)
	}

	if err := pB.Decode(rB, sizeB, codec); err != nil {
		t.Fatalf("Decode B: %v", err)
	}
	catB := mustCatalog(t, &pB, rB, codec)
	sizeValB := mustTrailerSize(t, &pB, rB, codec)
	if sizeValA == sizeValB {
		t.Fatal("the two documents have the same /Size; this test proves nothing")
	}
	// Resolving B's page tree puts the object stream cursor on B, which is the
	// state most likely to be mistaken for A's.
	pagesB, err := pB.Deref(rB, mustGet(t, &pB, rB, codec, catB.v, "Pages"), codec)
	if err != nil {
		t.Fatalf("B /Pages: %v", err)
	}
	if pagesB.Stm == 0 {
		t.Fatal("B's page tree is not in an object stream; this test proves less than it should")
	}

	// A's Values were made before B existed and are read after B moved every
	// cursor. Same answers, or the coordinates mean nothing.
	if got := mustType(t, &pA, rA, codec, catA.v); got != "Catalog" {
		t.Errorf("A catalog /Type after reading B = %q, want %q", got, "Catalog")
	}
	if got := mustTrailerSize(t, &pA, rA, codec); got != sizeValA {
		t.Errorf("A /Size after reading B = %d, want %d", got, sizeValA)
	}
	if got, err := pA.Lookup(rA, catA.num, codec); err != nil || got != lookA {
		t.Errorf("A xref entry after reading B = %+v (%v), want %+v", got, err, lookA)
	}

	// Interleaved, which is what a caller reading two documents actually does.
	// Once each would not catch a cursor that only goes wrong on the way back.
	for i := range 3 {
		if got := mustType(t, &pA, rA, codec, catA.v); got != "Catalog" {
			t.Fatalf("round %d: A /Type = %q", i, got)
		}
		if got := mustType(t, &pB, rB, codec, catB.v); got != "Catalog" {
			t.Fatalf("round %d: B /Type = %q", i, got)
		}
		if got := mustTrailerSize(t, &pA, rA, codec); got != sizeValA {
			t.Fatalf("round %d: A /Size = %d, want %d", i, got, sizeValA)
		}
		if got := mustTrailerSize(t, &pB, rB, codec); got != sizeValB {
			t.Fatalf("round %d: B /Size = %d, want %d", i, got, sizeValB)
		}
	}

	// The checks above only compare a hot Codec with itself, which a Codec
	// confusing two documents can still be consistent about. So compare it
	// against one that has read nothing else: a cold Codec cannot hold the
	// wrong document's bytes, so it is the oracle for what B's objects are.
	//
	// The sample alternates object streams and revisits them, because a cursor
	// serving a stale stream is only wrong on the way back to one it left.
	cold := newCodecCfg(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})
	var pCold PDF
	if err := pCold.Decode(rB, sizeB, cold); err != nil {
		t.Fatalf("Decode cold: %v", err)
	}
	nums := [...]uint32{1, 5000, 2, 12000, 5000, 300, 12000, 1, 300}
	for _, num := range nums {
		want, errCold := pCold.Resolve(rB, ObjectID{Num: num}, cold)
		// The hot Codec reads A in between, so every cursor it owns is on the
		// wrong document when B asks.
		mustTrailerSize(t, &pA, rA, codec)
		got, errHot := pB.Resolve(rB, ObjectID{Num: num}, codec)
		if (errCold == nil) != (errHot == nil) {
			t.Fatalf("object %d: cold %v, hot %v", num, errCold, errHot)
		}
		if errCold != nil {
			continue
		}
		if got != want {
			t.Errorf("object %d: hot Codec says %+v, cold Codec says %+v", num, got, want)
		}
		// A Value only names bytes, so agreeing on the name is not agreeing on
		// what is there. Read through it: a stale window answers with bytes
		// that are someone else's but still lex.
		if !got.IsDict() {
			continue
		}
		gotT, err1 := codec.DictGet(&pB, rB, got, "Type")
		wantT, err2 := cold.DictGet(&pCold, rB, want, "Type")
		if err1 != nil || err2 != nil || gotT.IsNull() || wantT.IsNull() {
			continue // Not every dictionary carries a /Type.
		}
		gotS, err1 := codec.AppendString(nil, &pB, rB, gotT)
		wantS, err2 := cold.AppendString(nil, &pCold, rB, wantT)
		if err1 != nil || err2 != nil {
			t.Fatalf("object %d /Type text: hot %v, cold %v", num, err1, err2)
		}
		if !bytes.Equal(gotS, wantS) {
			t.Errorf("object %d /Type: hot Codec reads %q, cold Codec reads %q", num, gotS, wantS)
		}
	}

	// Switching object streams back to back, with nothing in between. The
	// checks above cannot see a cursor that serves a stale stream, because
	// reading A between two B reads points the lexer at a different file and
	// drops its window as a side effect — which hides the bug rather than
	// tests it. Here nothing intervenes: every read lands on the same cursor
	// the last one left somewhere else.
	streamThrash(t, rB, sizeB, &pB, codec)

	// Reusing A's index for another document keeps its capacity: Reset empties
	// rather than releases, which is what makes a PDF reusable without
	// allocating.
	capBefore := cap(pA.sections)
	if err := pA.Decode(rB, sizeB, codec); err != nil {
		t.Fatalf("re-Decode A with B's file: %v", err)
	}
	if cap(pA.sections) != capBefore {
		t.Errorf("sections cap changed on reuse: %d -> %d", capBefore, cap(pA.sections))
	}
	if got := mustTrailerSize(t, &pA, rB, codec); got != sizeValB {
		t.Errorf("reused index reports /Size %d, want B's %d", got, sizeValB)
	}
}

type catalog struct {
	v   Value
	num uint32
}

func mustCatalog(t *testing.T, p *PDF, r io.ReaderAt, codec *Codec) catalog {
	t.Helper()
	ref := mustGet(t, p, r, codec, p.Trailer(), "Root")
	if ref.Tok != piulex.TokR {
		t.Fatalf("/Root is %v, want a reference", ref.Tok)
	}
	v, err := p.Resolve(r, ref.ObjectID(), codec)
	if err != nil {
		t.Fatalf("resolving /Root: %v", err)
	}
	return catalog{v: v, num: ref.ObjectID().Num}
}

func mustGet(t *testing.T, p *PDF, r io.ReaderAt, codec *Codec, dict Value, key string) Value {
	t.Helper()
	v, err := codec.DictGet(p, r, dict, key)
	if err != nil {
		t.Fatalf("/%s: %v", key, err)
	}
	if v.IsNull() {
		t.Fatalf("/%s: absent", key)
	}
	return v
}

func mustType(t *testing.T, p *PDF, r io.ReaderAt, codec *Codec, dict Value) string {
	t.Helper()
	got, err := codec.AppendString(nil, p, r, mustGet(t, p, r, codec, dict, "Type"))
	if err != nil {
		t.Fatalf("/Type: %v", err)
	}
	return string(got)
}

func mustTrailerSize(t *testing.T, p *PDF, r io.ReaderAt, codec *Codec) int64 {
	t.Helper()
	n, ok := mustGet(t, p, r, codec, p.Trailer(), "Size").Int()
	if !ok {
		t.Fatal("/Size is not an integer")
	}
	return n
}

// streamThrash resolves objects from several object streams in an order that
// keeps returning to streams it has left, and checks each against an oracle
// that cannot be stale: a Codec decoded fresh for that one object. A cursor
// holding the wrong stream's bytes is not an error — the bytes still lex — so
// only an independent reader says what is actually there.
func streamThrash(t *testing.T, r io.ReaderAt, size int64, p *PDF, codec *Codec) {
	t.Helper()
	// Find one object in each of several distinct object streams. Objects in
	// one stream share a decode; objects in different ones evict each other.
	var nums []uint32
	seen := map[uint32]bool{}
	for num := uint32(1); num < 15888 && len(nums) < 4; num++ {
		e, err := p.Lookup(r, num, codec)
		if err != nil || e.Kind != XrefCompressed || seen[e.Stream] {
			continue
		}
		seen[e.Stream] = true
		nums = append(nums, num)
	}
	if len(nums) < 4 {
		t.Fatalf("found objects in only %d object streams; this check needs several", len(nums))
	}
	// Revisit: a cursor is only wrong on the way back to a stream it left.
	order := []int{0, 1, 0, 2, 1, 3, 2, 0, 3, 1}
	for _, i := range order {
		num := nums[i]
		got, err := p.Resolve(r, ObjectID{Num: num}, codec)
		if err != nil {
			t.Fatalf("object %d: %v", num, err)
		}
		want := resolveFresh(t, r, size, num)
		if got != want {
			t.Fatalf("object %d after switching streams: %+v, want %+v", num, got, want)
		}
		// The Value names bytes; a stale stream is bytes that lex and lie.
		if !got.IsDict() {
			continue
		}
		gotS, err := dictText(codec, p, r, got, "Type")
		if err != nil {
			continue
		}
		freshCodec := newCodecCfg(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})
		var freshPDF PDF
		if err := freshPDF.Decode(r, size, freshCodec); err != nil {
			t.Fatal(err)
		}
		wantS, err := dictText(freshCodec, &freshPDF, r, want, "Type")
		if err != nil {
			t.Fatalf("object %d fresh /Type: %v", num, err)
		}
		if !bytes.Equal(gotS, wantS) {
			t.Errorf("object %d /Type after switching streams: %q, want %q", num, gotS, wantS)
		}
	}
}

// resolveFresh resolves num through a Codec that has read nothing else, which
// is the only reader guaranteed to hold no other document's bytes.
func resolveFresh(t *testing.T, r io.ReaderAt, size int64, num uint32) Value {
	t.Helper()
	codec := newCodecCfg(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})
	var p PDF
	if err := p.Decode(r, size, codec); err != nil {
		t.Fatal(err)
	}
	v, err := p.Resolve(r, ObjectID{Num: num}, codec)
	if err != nil {
		t.Fatalf("fresh resolve of object %d: %v", num, err)
	}
	return v
}

func dictText(codec *Codec, p *PDF, r io.ReaderAt, dict Value, key string) ([]byte, error) {
	v, err := codec.DictGet(p, r, dict, key)
	if err != nil {
		return nil, err
	}
	if v.IsNull() {
		return nil, errValueMismatch
	}
	return codec.AppendString(nil, p, r, v)
}

// pngFilter applies PNG row filter ft to row cur against the reconstructed row
// above it, which is the transform unfilterPNG undoes. It is written from
// RFC 2083 section 6 rather than in terms of the code under test — an encoder
// that called paeth would round-trip a broken paeth happily.
func pngFilter(cur, prev []byte, ft byte, bpp int) []byte {
	out := make([]byte, len(cur))
	for i := range cur {
		var a, c byte
		if i >= bpp {
			a = cur[i-bpp]
			c = prev[i-bpp]
		}
		b := prev[i]
		var pred byte
		switch ft {
		case 1:
			pred = a
		case 2:
			pred = b
		case 3:
			pred = byte((int(a) + int(b)) / 2)
		case 4:
			// Paeth spelled out from the RFC: the neighbour closest to the
			// linear estimate a+b-c wins, ties going a, then b.
			p := int(a) + int(b) - int(c)
			pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
			switch {
			case pa <= pb && pa <= pc:
				pred = a
			case pb <= pc:
				pred = b
			default:
				pred = c
			}
		}
		out[i] = cur[i] - pred
	}
	return out
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// rows is the sample table every case below reconstructs: values chosen to
// wrap a byte under every filter, which is where an int-vs-byte slip shows.
var predictorRows = [][]byte{
	{10, 20, 30, 40, 50},
	{15, 25, 200, 45, 55},
	{0, 255, 128, 1, 254},
	{7, 7, 7, 7, 7},
}

// TestUnfilterPNG covers every row filter. Only filter 2 occurs in the test
// corpus — every cross-reference stream in testdata is written with /Predictor
// 12 and Up rows — so without this the other four are code that has never run.
func TestUnfilterPNG(t *testing.T) {
	for ft := byte(0); ft <= 4; ft++ {
		// The row above the first is all zeros (ISO 32000-1 7.4.4.4), which is
		// what the cursor hands in and what the encoder must assume.
		prev := make([]byte, len(predictorRows[0]))
		for i, want := range predictorRows {
			cur := pngFilter(want, prev, ft, 1)
			if err := unfilterPNG(cur, prev, ft, 1); err != nil {
				t.Fatalf("filter %d row %d: %v", ft, i, err)
			}
			if !bytes.Equal(cur, want) {
				t.Fatalf("filter %d row %d: got % x, want % x", ft, i, cur, want)
			}
			// The reconstructed row is the next one's reference, which is the
			// chaining that makes a row unreachable without its predecessors.
			prev = cur
		}
	}
}

// TestUnfilterPNGMixedFilters pins the filter being per-row. /Predictor 12
// declares that filters are present, not which: an encoder picks one per row,
// so a decoder honouring only the declared value decodes most files wrong.
func TestUnfilterPNGMixedFilters(t *testing.T) {
	prev := make([]byte, len(predictorRows[0]))
	for i, want := range predictorRows {
		ft := byte(i % 5) // A different filter every row.
		cur := pngFilter(want, prev, ft, 1)
		if err := unfilterPNG(cur, prev, ft, 1); err != nil {
			t.Fatalf("row %d (filter %d): %v", i, ft, err)
		}
		if !bytes.Equal(cur, want) {
			t.Fatalf("row %d (filter %d): got % x, want % x", i, ft, cur, want)
		}
		prev = cur
	}
}

// TestPaeth pins the predictor against RFC 2083 section 6.6 by known answer,
// not by round trip: an encoder and decoder sharing one broken paeth agree
// with each other and with nothing else.
//
// The rule is that the estimate p = a+b-c picks whichever neighbour it lands
// nearest, ties going to a, then b. The implementation spells the distances
// the RFC's way — |p-a| is b-c, |p-b| is a-c, |p-c| is a+b-2c — so each case
// below states the three it turns on.
func TestPaeth(t *testing.T) {
	for _, tc := range []struct {
		a, b, c, want byte
		why           string
	}{
		{0, 0, 0, 0, "all zero"},
		{10, 10, 10, 10, "all equal: every distance zero, a wins the tie"},
		{10, 20, 30, 10, "p=0: pa=10 pb=20 pc=30, a nearest"},
		{200, 100, 50, 200, "p=250: pa=50 pb=150 pc=200, a nearest"},
		{1, 2, 3, 1, "p=0: pa=1 pb=2 pc=3, a nearest"},
		{10, 5, 5, 10, "p=10: pa=0, a exact"},
		{5, 10, 5, 10, "p=10: pa=5 pb=0 pc=5, b nearest"},
		{100, 200, 150, 150, "p=150: pa=50 pb=50 pc=0, c nearest — the case a tie rule alone gets wrong"},
		{255, 0, 0, 255, "p=255: pa=0, a exact, no byte wrap in the estimate"},
		{0, 255, 255, 0, "p=0: pa=0, a exact"},
		{0, 0, 255, 0, "p=-255: pa=255 pb=255 pc=510, negative estimate, a wins the tie"},
	} {
		if got := paeth(tc.a, tc.b, tc.c); got != tc.want {
			t.Errorf("paeth(%d,%d,%d) = %d, want %d (%s)", tc.a, tc.b, tc.c, got, tc.want, tc.why)
		}
	}
}

// TestUnfilterPNGBadFilter pins an unknown filter being reported. The byte
// comes from the file, so it is input, not a constant.
func TestUnfilterPNGBadFilter(t *testing.T) {
	cur, prev := []byte{1, 2, 3}, []byte{0, 0, 0}
	if err := unfilterPNG(cur, prev, 5, 1); err != errBadPredictor {
		t.Errorf("filter 5 = %v, want errBadPredictor", err)
	}
}

// TestStreamPayloadFilters pins that locating a payload never depends on being
// able to decode it: every chain is named, and only OpenPayload refuses.
func TestStreamPayloadFilters(t *testing.T) {
	cases := []struct {
		dict      string
		want      []Filter
		predictor uint8
		colors    uint8
		decodes   bool
	}{
		{dict: "", want: nil, decodes: true},
		{dict: "/Filter /FlateDecode", want: []Filter{FilterFlate}, decodes: true},
		{dict: "/Filter /DCTDecode", want: []Filter{FilterDCT}},
		{dict: "/Filter [/DCTDecode]", want: []Filter{FilterDCT}},
		{dict: "/Filter /JPXDecode", want: []Filter{FilterJPX}},
		{dict: "/Filter /Bogus", want: []Filter{FilterUnknown}},
		{dict: "/Filter [/ASCII85Decode /FlateDecode] /DecodeParms [null << /Predictor 15 /Colors 3 >>]",
			want: []Filter{FilterASCII85, FilterFlate}, predictor: 15, colors: 3},
		{dict: "/Filter [/FlateDecode /DCTDecode] /DecodeParms [<< /Predictor 12 >> null]",
			want: []Filter{FilterFlate, FilterDCT}, predictor: 12, colors: 1},
		// Parameters on a filter that takes no predictor are not read as one.
		{dict: "/Filter /DCTDecode /DecodeParms << /ColorTransform 0 >>", want: []Filter{FilterDCT}},
	}
	dicts := make([]string, len(cases))
	for i, tc := range cases {
		dicts[i] = tc.dict
	}
	r, p, codec, ids := streamDoc(t, dicts...)
	var strm Stream
	strm.ConfigureDefault()
	for i, tc := range cases {
		v, err := p.Resolve(r, ids[i], codec)
		if err != nil {
			t.Fatalf("%q: %v", tc.dict, err)
		}
		sp, err := p.StreamPayload(r, v, codec)
		if err != nil {
			t.Fatalf("%q: StreamPayload: %v", tc.dict, err)
		}
		if sp.Length != 3 {
			t.Errorf("%q: Length = %d, want 3", tc.dict, sp.Length)
		}
		if got := sp.NumFilters(); got != len(tc.want) {
			t.Fatalf("%q: NumFilters = %d, want %d", tc.dict, got, len(tc.want))
		}
		for j, f := range tc.want {
			if got := sp.Filter(j); got != f {
				t.Errorf("%q: Filter(%d) = %v, want %v", tc.dict, j, got, f)
			}
		}
		if tc.predictor != 0 && (sp.codec.predictor != tc.predictor || sp.codec.colors != tc.colors) {
			t.Errorf("%q: predictor %d colors %d, want %d %d", tc.dict,
				sp.codec.predictor, sp.codec.colors, tc.predictor, tc.colors)
		}
		// "abc" is not a zlib stream, so a Flate payload may fail later; what
		// is pinned here is only whether the chain is refused up front.
		_, err = p.OpenPayload(r, sp, &strm)
		if refused := errors.Is(err, errUnsupportedFilter); refused == tc.decodes {
			t.Errorf("%q: OpenPayload err = %v, decodable = %v", tc.dict, err, tc.decodes)
		}
	}
}

// streamDoc writes a document holding one stream object per dictionary body
// in dicts, each with the 3-byte payload "abc", and decodes it. Objects are
// numbered from 1 in order, so a body may reference an earlier one by number.
func streamDoc(t *testing.T, dicts ...string) (r *bytes.Reader, p *PDF, codec *Codec, ids []ObjectID) {
	t.Helper()
	objs := make([]testObj, len(dicts))
	for i, dict := range dicts {
		objs[i] = testObj{dict: dict, stream: true}
	}
	return objDoc(t, objs...)
}

// testObj is one object of a document objDoc writes: dictionary body dict,
// and when stream is set the 3-byte payload "abc" after it.
type testObj struct {
	dict   string
	stream bool
}

// objDoc writes objs, numbered from 1 in order, and decodes the document.
func objDoc(t *testing.T, objs ...testObj) (r *bytes.Reader, p *PDF, codec *Codec, ids []ObjectID) {
	t.Helper()
	var out bytes.Buffer
	var e Encoder
	if err := e.Reset(&out, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	ids = make([]ObjectID, len(objs))
	for i, obj := range objs {
		ids[i] = e.NewID()
		if ids[i].Num != uint32(i+1) {
			t.Fatalf("object %d numbered %d", i, ids[i].Num)
		}
		e.BeginObject(ids[i])
		if !obj.stream {
			e.Raw([]byte("<< " + obj.dict + " >>"))
			e.EndObject()
			continue
		}
		e.Raw([]byte("<< /Length 3 " + obj.dict + " >>"))
		e.BeginStreamPayload()
		e.Raw([]byte("abc"))
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
	r = bytes.NewReader(out.Bytes())
	codec = newCodec(make([]byte, 4096))
	p = new(PDF)
	if err := p.Decode(r, r.Size(), codec); err != nil {
		t.Fatal(err)
	}
	return r, p, codec, ids
}

// TestImageInfo covers the color space shapes an image dictionary takes and
// the entries whose absence or value makes it no image at all.
func TestImageInfo(t *testing.T) {
	const img = "/Type /XObject /Subtype /Image /Width 4 /Height 3 "
	cases := []struct {
		dict  string
		cs    ColorSpace
		comps int
		bpc   int
		err   error
	}{
		// Object 1, referenced by the ICCBased cases below.
		{dict: "/N 3", err: errBadImage},
		{dict: img + "/BitsPerComponent 8 /ColorSpace /DeviceRGB", cs: ColorSpaceDeviceRGB, comps: 3, bpc: 8},
		{dict: img + "/BitsPerComponent 8 /ColorSpace /DeviceGray /Filter /DCTDecode", cs: ColorSpaceDeviceGray, comps: 1, bpc: 8},
		{dict: img + "/BitsPerComponent 16 /ColorSpace /DeviceCMYK", cs: ColorSpaceDeviceCMYK, comps: 4, bpc: 16},
		{dict: img + "/BitsPerComponent 8 /ColorSpace [/ICCBased 1 0 R]", cs: ColorSpaceICCBased, comps: 3, bpc: 8},
		{dict: img + "/BitsPerComponent 4 /ColorSpace [/Indexed /DeviceRGB 15 <00ff00>]", cs: ColorSpaceIndexed, comps: 1, bpc: 4},
		{dict: img + "/BitsPerComponent 8 /ColorSpace [/Indexed [/ICCBased 1 0 R] 1 (abcdef)]", cs: ColorSpaceIndexed, comps: 1, bpc: 8},
		{dict: img + "/BitsPerComponent 8 /ColorSpace [/DeviceN [/Cyan /Black /Spot] /DeviceCMYK 5 0 R]", cs: ColorSpaceDeviceN, comps: 3, bpc: 8},
		{dict: img + "/BitsPerComponent 8 /ColorSpace [/Separation /Gold /DeviceCMYK 5 0 R]", cs: ColorSpaceSeparation, comps: 1, bpc: 8},
		{dict: img + "/BitsPerComponent 8 /ColorSpace [/CalRGB << /WhitePoint [1 1 1] >>]", cs: ColorSpaceCalRGB, comps: 3, bpc: 8},
		// A name from the page's /Resources: known only to the page.
		{dict: img + "/BitsPerComponent 8 /ColorSpace /CS0", cs: ColorSpaceUnknown, comps: 0, bpc: 8},
		// A stencil mask is one bit whatever it claims, and has no color.
		{dict: img + "/ImageMask true /BitsPerComponent 8", comps: 1, bpc: 1},
		// JPX may defer both to its codestream.
		{dict: img + "/Filter /JPXDecode", bpc: 0},
		{dict: img + "/ColorSpace /DeviceRGB /Filter /JPXDecode", cs: ColorSpaceDeviceRGB, comps: 3},
		{dict: img + "/ColorSpace /DeviceRGB", err: errBadImage}, // No /BitsPerComponent.
		{dict: img + "/BitsPerComponent 3 /ColorSpace /DeviceRGB", err: errBadImage},
		{dict: img + "/BitsPerComponent 8 /ColorSpace /Pattern", err: errBadImage},
		{dict: img + "/BitsPerComponent 8 /ColorSpace [/ICCBased 2 0 R]", err: errBadImage}, // Profile lacks /N.
		{dict: "/Subtype /Form /Width 4 /Height 3 /BitsPerComponent 8", err: errBadImage},
		{dict: "/Subtype /Image /Width 0 /Height 3 /BitsPerComponent 8", err: errBadImage},
	}
	dicts := make([]string, len(cases))
	for i, tc := range cases {
		dicts[i] = tc.dict
	}
	r, p, codec, ids := streamDoc(t, dicts...)
	for i, tc := range cases {
		v, err := p.Resolve(r, ids[i], codec)
		if err != nil {
			t.Fatalf("%q: %v", tc.dict, err)
		}
		info, err := p.ImageInfo(r, v, codec)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%q: err = %v, want %v", tc.dict, err, tc.err)
			}
			continue
		} else if err != nil {
			t.Errorf("%q: %v", tc.dict, err)
			continue
		}
		if info.Width != 4 || info.Height != 3 {
			t.Errorf("%q: size %dx%d, want 4x3", tc.dict, info.Width, info.Height)
		}
		if info.ColorSpace != tc.cs || info.Components != tc.comps || info.BitsPerComponent != tc.bpc {
			t.Errorf("%q: got %v/%d comps/%d bpc, want %v/%d/%d", tc.dict,
				info.ColorSpace, info.Components, info.BitsPerComponent, tc.cs, tc.comps, tc.bpc)
		}
		if info.Payload.Length != 3 {
			t.Errorf("%q: payload length %d, want 3", tc.dict, info.Payload.Length)
		}
	}
}

// TestImageInfoMasks pins the mask entries coming back as the references and
// arrays they are, not resolved.
func TestImageInfoMasks(t *testing.T) {
	const img = "/Subtype /Image /Width 2 /Height 2 /BitsPerComponent 8 /ColorSpace /DeviceGray "
	r, p, codec, ids := streamDoc(t,
		img+"/SMask 2 0 R /Decode [1 0]",
		img,
		img+"/Mask [0 10]",
	)
	v, err := p.Resolve(r, ids[0], codec)
	if err != nil {
		t.Fatal(err)
	}
	info, err := p.ImageInfo(r, v, codec)
	if err != nil {
		t.Fatal(err)
	}
	if info.SMask != ids[1] {
		t.Errorf("SMask = %v, want %v", info.SMask, ids[1])
	}
	if !info.Decode.IsArray() || !info.Mask.IsNull() {
		t.Errorf("Decode %v, Mask %v: want array, null", info.Decode.Tok, info.Mask.Tok)
	}
	if v, err = p.Resolve(r, ids[2], codec); err != nil {
		t.Fatal(err)
	}
	if info, err = p.ImageInfo(r, v, codec); err != nil {
		t.Fatal(err)
	}
	if !info.Mask.IsArray() || !info.SMask.IsZero() {
		t.Errorf("Mask %v, SMask %v: want array, zero", info.Mask.Tok, info.SMask)
	}
}

// TestImageInfoCorpus reads every image XObject of the corpus: real files
// write the shapes TestImageInfo spells out by hand, plus whatever it missed.
func TestImageInfoCorpus(t *testing.T) {
	for _, fpath := range []string{fpathBook, fpathMCUSpec, "testdata/basis-of-embedded.pdf"} {
		c, size := openCounted(t, fpath)
		var pdf PDF
		codec := newCodec(make([]byte, 4096))
		codec.maxLazySections = 4096
		if err := pdf.Decode(c, size, codec); err != nil {
			t.Fatalf("%s: Decode: %v", fpath, err)
		}
		codec.rows.setCache(make([]byte, pdf.XrefCacheSize()))
		var last uint32
		for _, s := range pdf.sections {
			last = max(last, s.firstObj+s.count)
		}
		var images, payloads [16]int // By ColorSpace, by last Filter.
		n := 0
		for num := uint32(1); num < last; num++ {
			e, err := pdf.Lookup(c, num, codec)
			if err != nil || e.Kind == XrefFree {
				continue
			}
			v, err := pdf.Resolve(c, e.ID, codec)
			if err != nil || v.Tok != tokStream {
				continue
			}
			st, err := codec.DictGet(&pdf, c, v, "Subtype")
			if err != nil {
				t.Fatalf("%s: object %d: %v", fpath, num, err)
			}
			if ok, _ := codec.NameIs(&pdf, c, st, "Image"); !ok {
				continue
			}
			info, err := pdf.ImageInfo(c, v, codec)
			if err != nil {
				t.Errorf("%s: object %d: %v", fpath, num, err)
				continue
			}
			n++
			images[info.ColorSpace]++
			if k := info.Payload.NumFilters(); k > 0 {
				payloads[info.Payload.Filter(k-1)]++
			}
			if info.Components == 0 && info.ColorSpace != ColorSpaceUnknown {
				t.Errorf("%s: object %d: %v with no components", fpath, num, info.ColorSpace)
			}
		}
		var summary []string
		for cs, k := range images {
			if k > 0 {
				summary = append(summary, ColorSpace(cs).String()+"="+strconv.Itoa(k))
			}
		}
		for f, k := range payloads {
			if k > 0 {
				summary = append(summary, Filter(f).String()+"="+strconv.Itoa(k))
			}
		}
		t.Logf("%s: %d images: %s", fpath, n, strings.Join(summary, " "))
	}
}

func TestFilterString(t *testing.T) {
	for _, tc := range []struct {
		f    Filter
		want string
	}{
		{FilterFlate, "FlateDecode"},
		{FilterDCT, "DCTDecode"},
		{FilterUnknown, "Unknown"},
	} {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.f, got, tc.want)
		}
		if tc.f != FilterUnknown && filterByName([]byte(tc.want)) != tc.f {
			t.Errorf("filterByName(%q) != %v", tc.want, tc.f)
		}
	}
}

// TestAppendPageImages walks the resource shapes discovery must handle:
// inherited, indirect, nested in forms, repeated and cyclic.
func TestAppendPageImages(t *testing.T) {
	const img = "/Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray"
	r, p, codec, ids := objDoc(t,
		// 1: the root, whose resources pages without their own inherit.
		testObj{dict: "/Type /Pages /Kids [2 0 R 3 0 R 4 0 R 5 0 R] /Count 4 /Resources << /XObject << /Im1 6 0 R >> >>"},
		testObj{dict: "/Type /Page /Parent 1 0 R"}, // 2: inherits.
		// 3: own images around a form, which nests another form.
		testObj{dict: "/Type /Page /Parent 1 0 R /Resources << /XObject << /Im1 6 0 R /Fm1 8 0 R /Im9 7 0 R >> >>"},
		// 4: indirect resources naming one image twice and a PostScript XObject.
		testObj{dict: "/Type /Page /Parent 1 0 R /Resources 12 0 R"},
		// 5: a form that draws itself.
		testObj{dict: "/Type /Page /Parent 1 0 R /Resources << /XObject << /Loop 14 0 R >> >>"},
		testObj{dict: img, stream: true}, // 6
		testObj{dict: img, stream: true}, // 7
		testObj{dict: "/Subtype /Form /Resources << /XObject << /X 9 0 R /Y 6 0 R /Z 10 0 R >> >>", stream: true}, // 8
		testObj{dict: img, stream: true}, // 9
		testObj{dict: "/Subtype /Form /Resources << /XObject << /W 11 0 R >> >>", stream: true}, // 10
		testObj{dict: img, stream: true},                                                                    // 11
		testObj{dict: "/XObject << /A 6 0 R /B 6 0 R /P 13 0 R >>"},                                         // 12
		testObj{dict: "/Subtype /PS", stream: true},                                                         // 13
		testObj{dict: "/Subtype /Form /Resources << /XObject << /Self 14 0 R /I 7 0 R >> >>", stream: true}, // 14
	)
	id := func(n int) ObjectID { return ids[n-1] }
	for _, tc := range []struct {
		page int
		want []ImageRef
	}{
		{2, []ImageRef{{ID: id(6)}}},
		{3, []ImageRef{{ID: id(6)}, {ID: id(7)}, {ID: id(9), Form: id(8)}, {ID: id(11), Form: id(10)}}},
		{4, []ImageRef{{ID: id(6)}}},
		{5, []ImageRef{{ID: id(7), Form: id(14)}}},
	} {
		page, err := p.Resolve(r, id(tc.page), codec)
		if err != nil {
			t.Fatal(err)
		}
		// A prefix the caller already holds is kept, and is not what the
		// page's images are deduplicated against.
		prefix := ImageRef{ID: id(6)}
		got, err := p.AppendPageImages([]ImageRef{prefix}, r, page, codec)
		if err != nil {
			t.Fatalf("page %d: %v", tc.page, err)
		}
		want := append([]ImageRef{prefix}, tc.want...)
		if !slices.Equal(got, want) {
			t.Errorf("page %d: got %v, want %v", tc.page, got, want)
		}
	}

	page, err := p.Resolve(r, id(3), codec)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]ImageRef, 0, 64)
	allocs := testing.AllocsPerRun(20, func() {
		dst, _ = p.AppendPageImages(dst[:0], r, page, codec)
	})
	if allocs > 0 {
		t.Errorf("AppendPageImages allocates %.1f times with room in dst", allocs)
	}
}

// TestAppendPageImagesFormGraph pins the walk's cost on a hostile form graph:
// ten forms that each name all ten are 10^8 visits to the depth cap, so
// without the visit budget this test does not finish.
func TestAppendPageImagesFormGraph(t *testing.T) {
	const nforms = 10
	var forms strings.Builder
	for k := range nforms {
		fmt.Fprintf(&forms, "/F%d %d 0 R ", k, k+2) // Forms are objects 2…11.
	}
	objs := []testObj{{dict: "/Type /Page /Resources << /XObject << " + forms.String() + "/Own 13 0 R >> >>"}}
	for range nforms {
		objs = append(objs, testObj{dict: "/Subtype /Form /Resources << /XObject << " + forms.String() + "/I 12 0 R >> >>", stream: true})
	}
	const img = "/Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray"
	objs = append(objs, testObj{dict: img, stream: true}, testObj{dict: img, stream: true}) // 12, 13.
	r, p, codec, ids := objDoc(t, objs...)

	page, err := p.Resolve(r, ids[0], codec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.AppendPageImages(nil, r, page, codec)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []ObjectID
	for _, ref := range got {
		gotIDs = append(gotIDs, ref.ID)
	}
	slices.SortFunc(gotIDs, func(a, b ObjectID) int { return int(a.Num) - int(b.Num) })
	if want := []ObjectID{ids[11], ids[12]}; !slices.Equal(gotIDs, want) {
		t.Errorf("images %v, want %v, each once", gotIDs, want)
	}
}

// TestAppendPageImagesCorpus lists every page's images in the datasheet and
// checks discovery against the object sweep: every image found is one
// ImageInfo reads, and reuse across pages shows as repeated IDs.
func TestAppendPageImagesCorpus(t *testing.T) {
	c, size := openCounted(t, fpathMCUSpec)
	var pdf PDF
	codec := newCodec(make([]byte, 4096))
	codec.maxLazySections = 4096
	if err := pdf.Decode(c, size, codec); err != nil {
		t.Fatal(err)
	}
	// Resources point back at objects the cursor has passed; without the
	// cache each costs the cross-reference stream from its first byte.
	codec.rows.setCache(make([]byte, pdf.XrefCacheSize()))
	cat := mustCatalog(t, &pdf, c, codec)
	pages, err := pdf.Deref(c, mustGet(t, &pdf, c, codec, cat.v, "Pages"), codec)
	if err != nil {
		t.Fatal(err)
	}
	// Leaves of the page tree, depth first.
	var leaves []Value
	var walk func(node Value, depth int)
	walk = func(node Value, depth int) {
		kids, err := codec.DictGet(&pdf, c, node, "Kids")
		if err != nil {
			t.Fatal(err)
		}
		if kids.IsNull() {
			leaves = append(leaves, node)
			return
		}
		if kids, err = pdf.Deref(c, kids, codec); err != nil {
			t.Fatal(err)
		}
		var refs []ObjectID
		codec.ArrayForEach(&pdf, c, kids, func(v Value) bool { refs = append(refs, v.ObjectID()); return true })
		for _, ref := range refs {
			kid, err := pdf.Resolve(c, ref, codec)
			if err != nil {
				t.Fatal(err)
			}
			walk(kid, depth+1)
		}
	}
	walk(pages, 0)

	uses := map[ObjectID]int{}
	var refs []ImageRef
	for i, page := range leaves {
		refs, err = pdf.AppendPageImages(refs[:0], c, page, codec)
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		for _, ref := range refs {
			uses[ref.ID]++
		}
	}
	shared := 0
	for id, n := range uses {
		v, err := pdf.Resolve(c, id, codec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pdf.ImageInfo(c, v, codec); err != nil {
			t.Errorf("image %v: %v", id, err)
		}
		if n > 1 {
			shared++
		}
	}
	t.Logf("%d pages, %d distinct images, %d drawn on more than one page", len(leaves), len(uses), shared)
	if len(uses) == 0 {
		t.Error("found no images")
	}
}

// TestInlineImage reads inline images out of a content stream: abbreviated and
// full keys, a filter chain with a predictor, a stated length, a stencil mask,
// and the offset after each where the content stream resumes.
func TestInlineImage(t *testing.T) {
	// 2x2 gray, PNG Up rows: decodes to 10 20 / 11 22.
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write([]byte{2, 10, 20, 2, 1, 2})
	zw.Close()
	flated := zbuf.String()

	content := "q 2 0 0 2 0 0 cm\n" +
		"BI /W 2 /H 2 /BPC 8 /CS /G /F /Fl /DP << /Predictor 12 /Columns 2 >> ID " + flated + " EI\n" +
		"Q BI /Width 3 /Height 1 /BitsPerComponent 8 /ColorSpace /RGB /Filter [/AHx] ID 00ff00ff00ff00ff00> EI\n" +
		"BI /W 1 /H 1 /CS [/I /RGB 1 <000000ffffff>] /BPC 1 /L 4 ID EI\nx EI Q\n" +
		"BI /W 8 /H 1 /IM true /D [1 0] ID \x5a EI"
	r := strings.NewReader(content)
	codec := newCodec(make([]byte, 4096))
	var p PDF // Inline images never leave the content stream: no document needed.

	type want struct {
		w, h, bpc, comps int
		cs               ColorSpace
		filters          []Filter
		data             string // Raw payload bytes.
		next             string // What the content stream says after EI.
	}
	wants := []want{
		{2, 2, 8, 1, ColorSpaceDeviceGray, []Filter{FilterFlate}, flated, "Q"},
		{3, 1, 8, 3, ColorSpaceDeviceRGB, []Filter{FilterASCIIHex}, "00ff00ff00ff00ff00>", "BI"},
		{1, 1, 1, 1, ColorSpaceIndexed, nil, "EI\nx", "Q"},
		{8, 1, 1, 1, ColorSpaceUnknown, nil, "\x5a", ""},
	}
	off := int64(0)
	for i, w := range wants {
		bi := strings.Index(content[off:], "BI ")
		if bi < 0 {
			t.Fatalf("image %d: no BI", i)
		}
		info, end, err := p.InlineImage(r, off+int64(bi), codec)
		if err != nil {
			t.Fatalf("image %d: %v", i, err)
		}
		if info.Width != w.w || info.Height != w.h || info.BitsPerComponent != w.bpc ||
			info.Components != w.comps || info.ColorSpace != w.cs {
			t.Errorf("image %d: %dx%d %d bpc %d comps %v, want %dx%d %d %d %v", i,
				info.Width, info.Height, info.BitsPerComponent, info.Components, info.ColorSpace,
				w.w, w.h, w.bpc, w.comps, w.cs)
		}
		sp := info.Payload
		if got := content[sp.Offset : sp.Offset+sp.Length]; got != w.data {
			t.Errorf("image %d: payload %q, want %q", i, got, w.data)
		}
		if sp.NumFilters() != len(w.filters) {
			t.Errorf("image %d: %d filters, want %d", i, sp.NumFilters(), len(w.filters))
		}
		for j, f := range w.filters {
			if sp.Filter(j) != f {
				t.Errorf("image %d: filter %d = %v, want %v", i, j, sp.Filter(j), f)
			}
		}
		if rest := strings.TrimLeft(content[end:], " \n"); !strings.HasPrefix(rest, w.next) {
			t.Errorf("image %d: resumes at %q, want %q", i, rest[:min(len(rest), 8)], w.next)
		}
		off = end
	}

	// The first image's samples decode through the content stream reader.
	info, _, err := p.InlineImage(r, int64(strings.Index(content, "BI")), codec)
	if err != nil {
		t.Fatal(err)
	}
	var strm Stream
	strm.ConfigureDefault()
	rd, err := p.OpenPayload(r, info.Payload, &strm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{10, 20, 11, 22}; !bytes.Equal(got, want) {
		t.Errorf("samples % x, want % x", got, want)
	}

	for _, bad := range []string{
		"Q",                        // Not BI.
		"BI /W 2 /H 2 /BPC 8 abc",  // No ID.
		"BI /W 2 /BPC 8 ID x EI",   // No height.
		"BI 7 /H 2 /BPC 8 ID x EI", // Not a key.
	} {
		if _, _, err := p.InlineImage(strings.NewReader(bad), 0, codec); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

// TestInlineImageLength pins the end of unfiltered inline data coming from
// the header, not from a search: samples may hold "EI" between whitespace,
// which a search takes for the end, misparsing the rest of the page.
func TestInlineImageLength(t *testing.T) {
	codec := newCodec(make([]byte, 4096))
	var p PDF
	for _, tc := range []struct {
		content, data string
	}{
		// 4x1 gray: the samples are newline, E, I, newline.
		{"BI /W 4 /H 1 /BPC 8 /CS /G ID \nEI\n EI Q", "\nEI\n"},
		// 2x1 RGB at 4 bits: 3 bytes.
		{"BI /W 2 /H 1 /BPC 4 /CS /RGB ID  EI EI Q", " EI"},
		// Stencil, 9x2: rows pad to 2 bytes.
		{"BI /W 9 /H 2 /IM true ID EI\nE EI Q", "EI\nE"},
		// The header undercounts (2 bytes, 3 written): the search finds EI.
		{"BI /W 2 /H 1 /BPC 8 /CS /G ID abc EI Q", "abc"},
		// A space from the page's resources: unknown, so searched.
		{"BI /W 2 /H 1 /BPC 8 /CS /CS0 ID ab EI Q", "ab"},
	} {
		info, end, err := p.InlineImage(strings.NewReader(tc.content), 0, codec)
		if err != nil {
			t.Errorf("%q: %v", tc.content, err)
			continue
		}
		sp := info.Payload
		if got := tc.content[sp.Offset : sp.Offset+sp.Length]; got != tc.data {
			t.Errorf("%q: data %q, want %q", tc.content, got, tc.data)
		}
		if rest := strings.TrimSpace(tc.content[end:]); rest != "Q" {
			t.Errorf("%q: resumes at %q, want Q", tc.content, rest)
		}
	}
}
