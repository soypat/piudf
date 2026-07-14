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
// prev < 0 means no /Prev. Returns the offset of the written xref table.
func (b *pdfBuilder) xrefTrailer(maxObj uint32, trailerExtra string, prev int64) int64 {
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
	b.obj(5, "[ 42 -7 2.5 (hi\\)there) <BEEF> /Näme#20x true false null 6 0 R ]")
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

func TestNameArena(t *testing.T) {
	var na nameArena
	na.reset(64, false)
	a, err := na.intern([]byte("Type"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := na.intern([]byte("Pages"))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := na.intern([]byte("Type"))
	if err != nil {
		t.Fatal(err)
	}
	if a != a2 {
		t.Error("interning same name twice returned different refs")
	}
	if a == b {
		t.Error("distinct names share a ref")
	}
	if string(na.bytes(a)) != "Type" || !na.is(b, "Pages") {
		t.Error("name bytes roundtrip failed")
	}
	if na.lookup("Type") != a || na.lookup("Missing") != 0 {
		t.Error("lookup misbehaved")
	}
	na.reset(4, false)
	if _, err := na.intern([]byte("TooLongName")); err == nil {
		t.Error("expected arena-full error")
	}
}

func TestDecodeBasic(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R", -1)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	if p.Root() != (ObjectID{Num: 1}) {
		t.Fatalf("Root = %v", p.Root())
	}
	cat, err := d.Resolve(p, r, ObjectID{Num: 1})
	if err != nil {
		t.Fatal(err)
	}
	typ, err := p.DictGet(cat, "Type")
	if err != nil || !p.NameIs(typ, "Catalog") {
		t.Fatalf("catalog /Type: %v %v", typ, err)
	}
	pages, err := p.DictGet(cat, "Pages")
	if err != nil || !pages.IsRef() || pages.Ref != (ObjectID{Num: 2}) {
		t.Fatalf("catalog /Pages: %+v %v", pages, err)
	}
	missing, err := p.DictGet(cat, "Nonexistent")
	if err != nil || !missing.IsNull() {
		t.Fatalf("missing key: %+v %v", missing, err)
	}
	tr, err := d.Trailer(p, r)
	if err != nil {
		t.Fatal(err)
	}
	sz, err := p.DictGet(tr, "Size")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := sz.Int(); n != 7 {
		t.Fatalf("trailer /Size = %d", n)
	}
}

func TestDecodeKitchenSinkArray(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R", -1)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	arr, err := d.Resolve(p, r, ObjectID{Num: 5})
	if err != nil {
		t.Fatal(err)
	}
	if arr.Kind != KindArray || arr.Len() != 10 {
		t.Fatalf("got %v len %d, want array len 10", arr.Kind, arr.Len())
	}
	wantKind := []Kind{KindInt, KindInt, KindReal, KindString, KindHexString,
		KindName, KindBool, KindBool, KindNull, KindRef}
	for i, wk := range wantKind {
		v, err := p.ArrayIndex(arr, i)
		if err != nil {
			t.Fatal(err)
		}
		if v.Kind != wk {
			t.Errorf("elem %d: kind %v, want %v", i, v.Kind, wk)
		}
	}
	v0, _ := p.ArrayIndex(arr, 0)
	if n, _ := v0.Int(); n != 42 {
		t.Errorf("elem 0 = %d", n)
	}
	v1, _ := p.ArrayIndex(arr, 1)
	if n, _ := v1.Int(); n != -7 {
		t.Errorf("elem 1 = %d", n)
	}
	v2, _ := p.ArrayIndex(arr, 2)
	if f, _ := v2.Float(); f != 2.5 {
		t.Errorf("elem 2 = %v", f)
	}
	v3, _ := p.ArrayIndex(arr, 3)
	s, err := d.AppendString(nil, r, v3)
	if err != nil || string(s) != "hi)there" {
		t.Errorf("elem 3 string = %q, %v", s, err)
	}
	v4, _ := p.ArrayIndex(arr, 4)
	s, err = d.AppendString(s[:0], r, v4)
	if err != nil || !bytes.Equal(s, []byte{0xBE, 0xEF}) {
		t.Errorf("elem 4 hex = %#x, %v", s, err)
	}
	v5, _ := p.ArrayIndex(arr, 5)
	if !p.NameIs(v5, "Näme x") { // #20 decodes to space; UTF-8 bytes pass through.
		t.Errorf("elem 5 name = %q", p.NameBytes(v5.Name))
	}
	v9, _ := p.ArrayIndex(arr, 9)
	if v9.Ref != (ObjectID{Num: 6}) {
		t.Errorf("elem 9 ref = %v", v9.Ref)
	}
}

func TestRawStream(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R", -1)
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
	if !p.names.is(info.Filter, "FlateDecode") {
		t.Errorf("filter = %q", p.NameBytes(info.Filter))
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
	firstXref := b.xrefTrailer(6, " /Root 1 0 R", -1)
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
	b.xrefTrailer(8, " /Root 1 0 R", -1) // Objects 7, 8 emitted as free.
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	for _, id := range []ObjectID{{Num: 0}, {Num: 7}, {Num: 100}, {Num: 5, Gen: 9}} {
		if _, err := d.Resolve(p, r, id); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Resolve(%v) = %v, want ErrObjectNotFound", id, err)
		}
	}
}

func TestMemoryLimitDegradation(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R", -1)
	// Arena fits the 2-pair trailer (4 values) but not the 10-element array.
	d, p, r := decodeBuilder(t, b, DecodeLimits{ValueArena: 6, NameArena: 512, MaxLiteral: 128, MaxParseDepth: 8})

	if _, err := d.Resolve(p, r, ObjectID{Num: 5}); !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("big array: %v, want ErrMemoryLimit", err)
	}
	// Document soft-degrades: scalars still resolve after the failure.
	v, err := d.Resolve(p, r, ObjectID{Num: 6})
	if err != nil {
		t.Fatalf("scalar after limit hit: %v", err)
	}
	if n, _ := v.Int(); n != 1234 {
		t.Errorf("object 6 = %d", n)
	}
	if p.Stats().Dropped == 0 {
		t.Error("Stats.Dropped not incremented")
	}
	// Tiny name arena fails Decode outright: structural names don't fit.
	var d2 Decoder
	var p2 PDF
	data := b.buf.Bytes()
	err = d2.Decode(&p2, bytes.NewReader(data), int64(len(data)), DecodeLimits{ValueArena: 64, NameArena: 4, MaxLiteral: 128, MaxParseDepth: 8})
	if !errors.Is(err, ErrMemoryLimit) {
		t.Errorf("tiny name arena Decode: %v, want ErrMemoryLimit", err)
	}
}

func TestXrefStreamUnsupported(t *testing.T) {
	// A file whose startxref points at an object header (xref stream form).
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.5\n")
	off := buf.Len()
	buf.WriteString("1 0 obj\n<< /Type /XRef >>\nstream\nendstream\nendobj\n")
	fmt.Fprintf(&buf, "startxref\n%d\n%%%%EOF\n", off)
	var d Decoder
	var p PDF
	err := d.Decode(&p, bytes.NewReader(buf.Bytes()), int64(buf.Len()), DecodeLimits{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

// TestDecoderReuseNoTamper checks the machine/document split contract: one
// Decoder serves several PDF structs without corrupting earlier documents,
// and a reset PDF reuses its capacity.
func TestDecoderReuseNoTamper(t *testing.T) {
	bA := basicPDF()
	bA.xrefTrailer(6, " /Root 1 0 R", -1)
	dataA := bA.buf.Bytes()
	rA := bytes.NewReader(dataA)

	bB := newPDFBuilder()
	bB.obj(1, "<< /Type /Catalog >>")
	bB.obj(2, "(only in B)")
	bB.xrefTrailer(2, " /Root 1 0 R", -1)
	dataB := bB.buf.Bytes()
	rB := bytes.NewReader(dataB)

	var d Decoder
	var pA, pB PDF
	if err := d.Decode(&pA, rA, int64(len(dataA)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	// Resolve into A, then decode B with the same machine.
	arrA, err := d.Resolve(&pA, rA, ObjectID{Num: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(&pB, rB, int64(len(dataB)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	// A's arena must be untouched by B's decode: its Value still reads.
	v0, err := pA.ArrayIndex(arrA, 0)
	if err != nil {
		t.Fatalf("A value after decoding B: %v", err)
	}
	if n, _ := v0.Int(); n != 42 {
		t.Errorf("A arena corrupted: elem 0 = %d", n)
	}
	// B resolves independently.
	sB, err := d.Resolve(&pB, rB, ObjectID{Num: 2})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.AppendString(nil, rB, sB)
	if err != nil || string(got) != "only in B" {
		t.Errorf("B string = %q, %v", got, err)
	}
	// Reusing A's struct for another document keeps its capacity.
	capBefore := cap(pA.values)
	if err := d.Decode(&pA, rB, int64(len(dataB)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	if cap(pA.values) != capBefore {
		t.Errorf("values cap changed on reuse: %d -> %d", capBefore, cap(pA.values))
	}
}

func TestResolveAllocs(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R", -1)
	data := b.buf.Bytes()
	r := bytes.NewReader(data)
	var d Decoder
	var p PDF
	if err := d.Decode(&p, r, int64(len(data)), DecodeLimits{ValueArena: 256, NameArena: 1024, MaxLiteral: 256, MaxParseDepth: 16}); err != nil {
		t.Fatal(err)
	}
	// Warm up name arena and lexer buffers.
	if _, err := d.Resolve(&p, r, ObjectID{Num: 1}); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		v, err := d.Resolve(&p, r, ObjectID{Num: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.DictGet(v, "Pages"); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("Resolve allocates %v times per run, want 0", allocs)
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
