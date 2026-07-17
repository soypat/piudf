package piudf

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// pdfBuilder assembles a syntactically valid PDF in memory, tracking object
// offsets so the generated cross-reference table is exact.
type pdfBuilder struct {
	buf     bytes.Buffer
	offsets map[uint32]int64
}

func newPDFBuilder() *pdfBuilder {
	b := &pdfBuilder{offsets: make(map[uint32]int64)}
	b.buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	return b
}

// obj writes "num 0 obj <body> endobj" recording its offset.
func (b *pdfBuilder) obj(num uint32, body string) {
	b.offsets[num] = int64(b.buf.Len())
	fmt.Fprintf(&b.buf, "%d 0 obj\n%s\nendobj\n", num, body)
}

// streamObj writes a stream object with correct /Length.
func (b *pdfBuilder) streamObj(num uint32, dictExtra string, payload []byte) {
	b.offsets[num] = int64(b.buf.Len())
	fmt.Fprintf(&b.buf, "%d 0 obj\n<< /Length %d%s >>\nstream\n", num, len(payload), dictExtra)
	b.buf.Write(payload)
	b.buf.WriteString("\nendstream\nendobj\n")
}

// xrefTrailer writes a cross-reference table covering objects [0, maxObj]
// (0 free, missing objects free) plus a trailer, and the startxref tail.
// Returns the offset of the written xref table.
func (b *pdfBuilder) xrefTrailer(maxObj uint32, trailerExtra string) int64 {
	xrefOff := int64(b.buf.Len())
	fmt.Fprintf(&b.buf, "xref\n0 %d\n", maxObj+1)
	b.buf.WriteString("0000000000 65535 f\r\n")
	for num := uint32(1); num <= maxObj; num++ {
		if off, ok := b.offsets[num]; ok {
			fmt.Fprintf(&b.buf, "%010d %05d n\r\n", off, 0)
		} else {
			b.buf.WriteString("0000000000 65535 f\r\n")
		}
	}
	fmt.Fprintf(&b.buf, "trailer\n<< /Size %d%s >>\nstartxref\n%d\n%%%%EOF\n", maxObj+1, trailerExtra, xrefOff)
	return xrefOff
}

// update writes an incremental-update xref covering only nums, chained to
// the previous xref at prev.
func (b *pdfBuilder) update(nums []uint32, size uint32, trailerExtra string, prev int64) {
	xrefOff := int64(b.buf.Len())
	b.buf.WriteString("xref\n")
	for _, num := range nums {
		fmt.Fprintf(&b.buf, "%d 1\n%010d %05d n\r\n", num, b.offsets[num], 0)
	}
	fmt.Fprintf(&b.buf, "trailer\n<< /Size %d /Prev %d%s >>\nstartxref\n%d\n%%%%EOF\n",
		size, prev, trailerExtra, xrefOff)
}

// basicPDF builds a small document: catalog, page tree, one page, one
// content stream, and a kitchen-sink array object.
func basicPDF() *pdfBuilder {
	b := newPDFBuilder()
	b.obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	b.obj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.obj(3, "<< /Type /Page /Parent 2 0 R /Contents 4 0 R /MediaBox [0 0 612.5 792] >>")
	b.streamObj(4, " /Filter /FlateDecode", []byte("fake-deflate-payload"))
	b.obj(5, "[ 42 -7 2.5 (hi\\)there) <BEEF> /Näme#20x true false null 6 0 R << /Nested [1 2] >> ]")
	b.obj(6, "1234")
	return b
}

// decodeBuilder decodes b's document and returns the machine, the document
// and the reader the lazy methods need.
func decodeBuilder(t *testing.T, b *pdfBuilder, lim DecodeLimits) (*Decoder, *PDF, *bytes.Reader) {
	t.Helper()
	var d Decoder
	var p PDF
	data := b.buf.Bytes()
	r := bytes.NewReader(data)
	if err := d.Decode(&p, r, int64(len(data)), lim); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return &d, &p, r
}

func TestLexerTokens(t *testing.T) {
	src := "12 -3 +4.5 .5 34. /Name /A#42C (str(nested)\\n) <4E6f> << /K [1 R] >> " +
		"obj endobj stream endstream xref trailer startxref true false null 7 0 R % comment\nfoo"
	var lx Lexer
	if err := lx.Reset(strings.NewReader(src), 0); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		tok Token
		lit string
	}{
		{TokInt, "12"}, {TokInt, "-3"}, {TokReal, "+4.5"}, {TokReal, ".5"}, {TokReal, "34."},
		{TokName, "Name"}, {TokName, "ABC"},
		{TokString, "str(nested)\n"}, {TokHexString, "No"},
		{TokDictOpen, ""}, {TokName, "K"}, {TokArrayOpen, ""}, {TokInt, "1"}, {TokR, "R"},
		{TokArrayClose, ""}, {TokDictClose, ""},
		{TokObj, "obj"}, {TokEndobj, "endobj"}, {TokStream, "stream"}, {TokEndstream, "endstream"},
		{TokXref, "xref"}, {TokTrailer, "trailer"}, {TokStartxref, "startxref"},
		{TokTrue, "true"}, {TokFalse, "false"}, {TokNull, "null"},
		{TokInt, "7"}, {TokInt, "0"}, {TokR, "R"},
		{TokIdent, "foo"}, {TokEOF, ""},
	}
	for i, w := range want {
		tok, pos, lit := lx.NextToken()
		if tok != w.tok {
			t.Fatalf("token %d at %v: got %v (%q), want %v", i, pos, tok, lit, w.tok)
		}
		if w.lit != "" && string(lit) != w.lit {
			t.Fatalf("token %d (%v): literal %q, want %q", i, tok, lit, w.lit)
		}
	}
	if err := lx.Err(); err != nil {
		t.Fatalf("lexer error: %v", err)
	}
}

func TestLexerStringEscapes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`(\n\r\t\b\f\(\)\\)`, "\n\r\t\b\f()\\"},
		{`(\101\12\0)`, "A\n\x00"},
		{"(a\\\nb)", "ab"},   // Line continuation.
		{"(a\r\nb)", "a\nb"}, // EOL normalization.
		{"(a\rb)", "a\nb"},
		{`(\q)`, "q"}, // Unknown escape: backslash dropped.
		{"<4 16\nf>", "A\x6f"},
		{"<BEE>", "\xbe\xe0"}, // Odd digit padded with zero.
		{"()", ""},
		{"<>", ""},
	}
	var lx Lexer
	for _, c := range cases {
		if err := lx.Reset(strings.NewReader(c.src), 0); err != nil {
			t.Fatal(err)
		}
		tok, _, lit := lx.NextToken()
		if tok != TokString && tok != TokHexString {
			t.Errorf("%q: got token %v, err %v", c.src, tok, lx.Err())
			continue
		}
		if string(lit) != c.want {
			t.Errorf("%q: got %q, want %q", c.src, lit, c.want)
		}
	}
}

func TestLexerPositions(t *testing.T) {
	src := "  12 /N\n(s)"
	var lx Lexer
	if err := lx.Reset(strings.NewReader(src), 0); err != nil {
		t.Fatal(err)
	}
	wantPos := []Pos{2, 5, 8}
	for i, want := range wantPos {
		_, pos, _ := lx.NextToken()
		if pos != want {
			t.Errorf("token %d: pos %v, want %v", i, pos, want)
		}
	}
	// Offset reset must produce absolute positions.
	if err := lx.Reset(strings.NewReader(src), 5); err != nil {
		t.Fatal(err)
	}
	tok, pos, lit := lx.NextToken()
	if tok != TokName || pos != 5 || string(lit) != "N" {
		t.Errorf("reset at 5: got %v %v %q", tok, pos, lit)
	}
}

func TestLexerMaxLiteral(t *testing.T) {
	var lx Lexer
	lx.MaxLiteral = 4
	if err := lx.Reset(strings.NewReader("(abcdefgh)"), 0); err != nil {
		t.Fatal(err)
	}
	tok, _, _ := lx.NextToken()
	if tok != TokIllegal || !errors.Is(lx.Err(), ErrMemoryLimit) {
		t.Fatalf("got %v err=%v, want TokIllegal with ErrMemoryLimit", tok, lx.Err())
	}
}

func TestDecodeBasic(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	if p.Root() != (ObjectID{Num: 1}) {
		t.Fatalf("Root = %v", p.Root())
	}
	cat, err := d.Resolve(p, r, ObjectID{Num: 1})
	if err != nil {
		t.Fatal(err)
	}
	typ, err := d.DictGet(p, r, cat, "Type")
	if err != nil || !d.NameIs(p, r, typ, "Catalog") {
		t.Fatalf("catalog /Type: %v %v", typ, err)
	}
	pages, err := d.DictGet(p, r, cat, "Pages")
	if err != nil || !pages.IsRef() || pages.Ref != (ObjectID{Num: 2}) {
		t.Fatalf("catalog /Pages: %+v %v", pages, err)
	}
	missing, err := d.DictGet(p, r, cat, "Nonexistent")
	if err != nil || !missing.IsNull() {
		t.Fatalf("missing key: %+v %v", missing, err)
	}
	tr, err := d.Trailer(p, r)
	if err != nil {
		t.Fatal(err)
	}
	sz, err := d.DictGet(p, r, tr, "Size")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := sz.Int(); n != 7 {
		t.Fatalf("trailer /Size = %d", n)
	}
	// Values survive interleaved use of the machine: cat's span still
	// reads after the machine lexed the trailer.
	typ2, err := d.DictGet(p, r, cat, "Type")
	if err != nil || !d.NameIs(p, r, typ2, "Catalog") {
		t.Fatalf("catalog /Type after trailer parse: %v %v", typ2, err)
	}
}

func TestDecodeKitchenSinkArray(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	arr, err := d.Resolve(p, r, ObjectID{Num: 5})
	if err != nil {
		t.Fatal(err)
	}
	if arr.Kind != KindArray {
		t.Fatalf("got %v, want array", arr.Kind)
	}
	if n, err := d.ArrayLen(p, r, arr); err != nil || n != 11 {
		t.Fatalf("ArrayLen = %d, %v; want 11", n, err)
	}
	wantKind := []Kind{KindInt, KindInt, KindReal, KindString, KindHexString,
		KindName, KindBool, KindBool, KindNull, KindRef, KindDict}
	for i, wk := range wantKind {
		v, err := d.ArrayIndex(p, r, arr, i)
		if err != nil {
			t.Fatal(err)
		}
		if v.Kind != wk {
			t.Errorf("elem %d: kind %v, want %v", i, v.Kind, wk)
		}
	}
	v0, _ := d.ArrayIndex(p, r, arr, 0)
	if n, _ := v0.Int(); n != 42 {
		t.Errorf("elem 0 = %d", n)
	}
	v1, _ := d.ArrayIndex(p, r, arr, 1)
	if n, _ := v1.Int(); n != -7 {
		t.Errorf("elem 1 = %d", n)
	}
	v2, _ := d.ArrayIndex(p, r, arr, 2)
	if f, _ := v2.Float(); f != 2.5 {
		t.Errorf("elem 2 = %v", f)
	}
	v3, _ := d.ArrayIndex(p, r, arr, 3)
	s, err := d.AppendString(nil, p, r, v3)
	if err != nil || string(s) != "hi)there" {
		t.Errorf("elem 3 string = %q, %v", s, err)
	}
	v4, _ := d.ArrayIndex(p, r, arr, 4)
	s, err = d.AppendString(s[:0], p, r, v4)
	if err != nil || !bytes.Equal(s, []byte{0xBE, 0xEF}) {
		t.Errorf("elem 4 hex = %#x, %v", s, err)
	}
	v5, _ := d.ArrayIndex(p, r, arr, 5)
	if !d.NameIs(p, r, v5, "Näme x") { // #20 decodes to space; UTF-8 bytes pass through.
		s, _ := d.AppendString(nil, p, r, v5)
		t.Errorf("elem 5 name = %q", s)
	}
	v9, _ := d.ArrayIndex(p, r, arr, 9)
	if v9.Ref != (ObjectID{Num: 6}) {
		t.Errorf("elem 9 ref = %v", v9.Ref)
	}
	// Nested dict span works and out-of-range errors.
	v10, _ := d.ArrayIndex(p, r, arr, 10)
	nested, err := d.DictGet(p, r, v10, "Nested")
	if err != nil || nested.Kind != KindArray {
		t.Errorf("nested dict get: %+v %v", nested, err)
	}
	if _, err := d.ArrayIndex(p, r, arr, 11); err == nil {
		t.Error("index 11: want out of range error")
	}
}

func TestRawStream(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	sv, err := d.Resolve(p, r, ObjectID{Num: 4})
	if err != nil {
		t.Fatal(err)
	}
	if sv.Kind != KindStream {
		t.Fatalf("kind = %v", sv.Kind)
	}
	sr, info, err := d.RawStream(p, r, sv)
	if err != nil {
		t.Fatal(err)
	}
	if !d.NameIs(p, r, info.Filter, "FlateDecode") {
		t.Errorf("filter kind %v not FlateDecode", info.Filter.Kind)
	}
	payload := make([]byte, info.Length)
	if _, err := sr.ReadAt(payload, 0); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "fake-deflate-payload" {
		t.Errorf("payload = %q", payload)
	}
}

func TestIncrementalUpdateShadowing(t *testing.T) {
	b := basicPDF()
	firstXref := b.xrefTrailer(6, " /Root 1 0 R")
	// Incremental update: replace object 6.
	b.obj(6, "5678")
	b.update([]uint32{6}, 7, " /Root 1 0 R", firstXref)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	v, err := d.Resolve(p, r, ObjectID{Num: 6})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := v.Int(); n != 5678 {
		t.Errorf("object 6 = %d, want updated 5678", n)
	}
	// Objects only in the old revision remain reachable through /Prev.
	if _, err := d.Resolve(p, r, ObjectID{Num: 1}); err != nil {
		t.Errorf("object 1 through /Prev chain: %v", err)
	}
}

func TestObjectNotFound(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(8, " /Root 1 0 R") // Objects 7, 8 emitted as free.
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	for _, id := range []ObjectID{{Num: 0}, {Num: 7}, {Num: 100}, {Num: 5, Gen: 9}} {
		if _, err := d.Resolve(p, r, id); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Resolve(%v) = %v, want ErrObjectNotFound", id, err)
		}
	}
}

func TestMemoryLimitDegradation(t *testing.T) {
	b := basicPDF()
	firstXref := b.xrefTrailer(6, " /Root 1 0 R")
	// MaxLiteral 10 covers all keywords (longest is 9) and the trailer
	// names but not /FlateDecode (11 bytes) in the stream object.
	d, p, r := decodeBuilder(t, b, DecodeLimits{MaxLiteral: 10, MaxParseDepth: 8, MaxXrefSections: 8})

	if _, err := d.Resolve(p, r, ObjectID{Num: 4}); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("long literal: %v, want ErrMemoryLimit", err)
	}
	// Document soft-degrades: other objects still resolve after the failure.
	v, err := d.Resolve(p, r, ObjectID{Num: 6})
	if err != nil {
		t.Fatalf("scalar after limit hit: %v", err)
	}
	if n, _ := v.Int(); n != 1234 {
		t.Errorf("object 6 = %d", n)
	}
	// Section cap: incremental update adds a second subsection.
	b.obj(6, "5678")
	b.update([]uint32{6}, 7, " /Root 1 0 R", firstXref)
	var d2 Decoder
	var p2 PDF
	data := b.buf.Bytes()
	err = d2.Decode(&p2, bytes.NewReader(data), int64(len(data)), DecodeLimits{MaxXrefSections: 1, MaxLiteral: 64, MaxParseDepth: 8})
	if !errors.Is(err, ErrMemoryLimit) {
		t.Errorf("section cap Decode: %v, want ErrMemoryLimit", err)
	}
	if p2.Stats().Dropped == 0 {
		t.Error("Stats.Dropped not incremented")
	}
}

func TestXrefStreamMalformed(t *testing.T) {
	// A file whose startxref points at an object header (xref stream form)
	// with a dictionary missing the mandatory entries must fail as corrupt,
	// not crash.
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.5\n")
	off := buf.Len()
	buf.WriteString("1 0 obj\n<< /Type /XRef >>\nstream\nendstream\nendobj\n")
	fmt.Fprintf(&buf, "startxref\n%d\n%%%%EOF\n", off)
	var d Decoder
	var p PDF
	err := d.Decode(&p, bytes.NewReader(buf.Bytes()), int64(buf.Len()), DecodeLimits{})
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("got %v, want ErrCorrupt", err)
	}
}

// TestValueDurability checks the file-coordinate model: Values from
// document A remain valid after the same machine decodes and reads
// document B — nothing a Value depends on is recycled.
func TestValueDurability(t *testing.T) {
	bA := basicPDF()
	bA.xrefTrailer(6, " /Root 1 0 R")
	dataA := bA.buf.Bytes()
	rA := bytes.NewReader(dataA)

	bB := newPDFBuilder()
	bB.obj(1, "<< /Type /Catalog >>")
	bB.obj(2, "(only in B)")
	bB.xrefTrailer(2, " /Root 1 0 R")
	dataB := bB.buf.Bytes()
	rB := bytes.NewReader(dataB)

	var d Decoder
	var pA, pB PDF
	if err := d.Decode(&pA, rA, int64(len(dataA)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	arrA, err := d.Resolve(&pA, rA, ObjectID{Num: 5})
	if err != nil {
		t.Fatal(err)
	}
	catA, err := d.Resolve(&pA, rA, ObjectID{Num: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Decode and read B with the same machine.
	if err := d.Decode(&pB, rB, int64(len(dataB)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	sB, err := d.Resolve(&pB, rB, ObjectID{Num: 2})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.AppendString(nil, &pB, rB, sB)
	if err != nil || string(got) != "only in B" {
		t.Fatalf("B string = %q, %v", got, err)
	}
	// A's Values are file coordinates: still readable, no staleness.
	v0, err := d.ArrayIndex(&pA, rA, arrA, 0)
	if err != nil {
		t.Fatalf("A array after B: %v", err)
	}
	if n, _ := v0.Int(); n != 42 {
		t.Errorf("A elem 0 = %d", n)
	}
	typ, err := d.DictGet(&pA, rA, catA, "Type")
	if err != nil || !d.NameIs(&pA, rA, typ, "Catalog") {
		t.Errorf("A catalog /Type after B: %v %v", typ, err)
	}
	// Reusing A's struct for another document keeps section capacity.
	capBefore := cap(pA.sections)
	if err := d.Decode(&pA, rB, int64(len(dataB)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	if cap(pA.sections) != capBefore {
		t.Errorf("sections cap changed on reuse: %d -> %d", capBefore, cap(pA.sections))
	}
}

func TestResolveAllocs(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	data := b.buf.Bytes()
	r := bytes.NewReader(data)
	var d Decoder
	var p PDF
	if err := d.Decode(&p, r, int64(len(data)), DecodeLimits{MaxLiteral: 256, MaxParseDepth: 16, MaxXrefSections: 8}); err != nil {
		t.Fatal(err)
	}
	// Warm up lexer buffers.
	if _, err := d.Resolve(&p, r, ObjectID{Num: 1}); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		v, err := d.Resolve(&p, r, ObjectID{Num: 1})
		if err != nil {
			t.Fatal(err)
		}
		typ, err := d.DictGet(&p, r, v, "Type")
		if err != nil {
			t.Fatal(err)
		}
		if !d.NameIs(&p, r, typ, "Catalog") {
			t.Fatal("not catalog")
		}
	})
	if allocs != 0 {
		t.Errorf("Resolve+DictGet+NameIs allocates %v times per run, want 0", allocs)
	}
}

func TestPosToLineCol(t *testing.T) {
	src := "line one\nline two\nline three"
	aux := make([]byte, 8)
	line, col, _, err := Pos(14).ToLineCol(strings.NewReader(src), aux)
	if err != nil {
		t.Fatal(err)
	}
	if line != 2 || col != 6 {
		t.Errorf("got %d:%d, want 2:6", line, col)
	}
}
