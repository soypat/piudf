package ppdf

import (
	"bytes"
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
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
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
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
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

// TestLookupAllocs pins the claim the cursors are built on: a lookup decodes
// through the Codec's own storage and the file, and neither escapes. The
// inflate window is allocated once and reused, so it must not show up here.
func TestLookupAllocs(t *testing.T) {
	c, size := openCounted(t, "../testdata/rp2350-datasheet.pdf")
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
	rA, sizeA := openCounted(t, "../testdata/sto.pdf")
	rB, sizeB := openCounted(t, "../testdata/rp2350-datasheet.pdf")
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
func pngFilter(cur, prev []byte, ft byte) []byte {
	const bpp = 1
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
			cur := pngFilter(want, prev, ft)
			if err := unfilterPNG(cur, prev, ft); err != nil {
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
		cur := pngFilter(want, prev, ft)
		if err := unfilterPNG(cur, prev, ft); err != nil {
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
	if err := unfilterPNG(cur, prev, 5); err != errXrefStreamBad {
		t.Errorf("filter 5 = %v, want errXrefStreamBad", err)
	}
}
