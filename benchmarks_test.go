package ppdf

import (
	"bytes"
	"os"
	"testing"

	"github.com/soypat/piudf/piulex"
)

const embeddedPDF = "testdata/basis-of-embedded.pdf"

type objloc struct {
	id  ObjectID
	off int64
}

// loadFile decodes the PDF at path and returns the decoded document, a codec,
// the raw bytes and the location of every live object in the file.
func loadFile(tb testing.TB, path string) (*PDF, *Codec, []byte, []objloc) {
	tb.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("document unavailable: %v", err)
	}
	r := bytes.NewReader(data)
	p := new(PDF)
	codec := newCodec(make([]byte, 4096))
	if err := p.Decode(r, int64(len(data)), codec); err != nil {
		tb.Fatal(err)
	}
	sizeV, err := codec.DictGet(p, r, p.Trailer(), "Size")
	if err != nil {
		tb.Fatal(err)
	}
	size, ok := sizeV.Int()
	if !ok {
		tb.Fatal("/Size is not an integer")
	}
	var locs []objloc
	for num := uint32(1); num < uint32(size); num++ {
		rec, err := p.lookupXref(r, num, codec)
		if err != nil || rec.Kind != XrefNormal {
			continue
		}
		locs = append(locs, objloc{id: ObjectID{Num: num, Gen: rec.ID.Gen}, off: rec.Offset})
	}
	if len(locs) == 0 {
		tb.Fatal("no objects found")
	}
	return p, codec, data, locs
}

// rootID returns the ObjectID of the document catalog.
func rootID(tb testing.TB, p *PDF, r *bytes.Reader, codec *Codec) ObjectID {
	tb.Helper()
	ref, err := codec.DictGet(p, r, p.Trailer(), "Root")
	if err != nil {
		tb.Fatal(err)
	}
	if ref.Tok != piulex.TokR {
		tb.Fatalf("/Root is %v, want a reference", ref.Tok)
	}
	return ref.ObjectID()
}

// lexObject tokenizes one object's structural region: from its header up to
// and including the terminating endobj, or up to the stream keyword (payload
// bytes are never lexed; skipping them needs the parsed /Length, which is
// the parser's job and exactly what this benchmark excludes).
func lexObject(tb testing.TB, lx *piulex.Lexer, r *bytes.Reader, buf []byte, off int64) (ntok int, end int64) {
	if err := lx.Reset(r, off, buf); err != nil {
		tb.Fatal(err)
	}
	for {
		tok, pos, _ := lx.NextToken()
		switch tok {
		case piulex.TokEndobj, piulex.TokStream, piulex.TokEOF:
			return ntok + 1, int64(pos)
		case piulex.TokIllegal:
			tb.Fatalf("illegal token at %v: %v", pos, lx.Err())
		}
		ntok++
	}
}

// TestEmbeddedPDFResolveAll sanity-checks the corpus the benchmarks use:
// every object in the file must resolve.
func TestEmbeddedPDFResolveAll(t *testing.T) {
	p, codec, data, locs := loadFile(t, embeddedPDF)
	r := bytes.NewReader(data)
	for _, loc := range locs {
		if _, err := p.Resolve(r, loc.id, codec); err != nil {
			t.Fatalf("Resolve(%v): %v", loc.id, err)
		}
	}
	t.Logf("%d objects resolved", len(locs))
}

// BenchmarkDecodeInit measures trailer plus cross-reference chain parsing,
// the whole cost of opening a document (no object contents are read).
func BenchmarkDecodeInit(b *testing.B) {
	_, _, data, _ := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	buf := make([]byte, 4096)
	var p PDF
	b.ReportAllocs()
	b.ResetTimer()
	codec := newCodec(buf)
	for b.Loop() {
		p.Reset()
		if err := p.Decode(r, int64(len(data)), codec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLexObjects measures pure lexing: every object's structural bytes
// are tokenized, with no value conversion, interning, or tree assembly.
func BenchmarkLexObjects(b *testing.B) {
	_, _, data, locs := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	var lx piulex.Lexer
	lx.ReuseLiteralBuffer = true
	buf := make([]byte, 4096)
	// Pre-compute structural byte count so ns/op pairs with MB/s.
	var structuralBytes, ntok int64
	for _, loc := range locs {
		n, end := lexObject(b, &lx, r, buf, loc.off)
		ntok += int64(n)
		structuralBytes += end - loc.off
	}
	b.SetBytes(structuralBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, loc := range locs {
			lexObject(b, &lx, r, buf, loc.off)
		}
	}
	b.ReportMetric(float64(ntok), "tokens/op")
}

// BenchmarkResolveObjects measures the full lazy pipeline per document
// sweep: xref record lookup, lexing, value conversion, name interning and
// composite arena assembly for every object.
func BenchmarkResolveObjects(b *testing.B) {
	p, codec, data, locs := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	var structuralBytes int64
	{
		var lx piulex.Lexer
		lx.ReuseLiteralBuffer = true
		buf := make([]byte, 4096)
		for _, loc := range locs {
			_, end := lexObject(b, &lx, r, buf, loc.off)
			structuralBytes += end - loc.off
		}
	}
	b.SetBytes(structuralBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, loc := range locs {
			if _, err := p.Resolve(r, loc.id, codec); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkDecodeInitXrefStream measures opening the PDF 1.5 corpus:
// dominated by inflating and predictor-decoding the xref records.
func BenchmarkDecodeInitXrefStream(b *testing.B) {
	_, _, data, _ := loadFile(b, fpathMCUSpec)
	r := bytes.NewReader(data)
	codec := newCodec(make([]byte, 4096))
	var p PDF
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := p.Decode(r, int64(len(data)), codec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolveCompressedHit resolves an object inside an object stream
// with a warm cache: pair-table skip plus a shallow parse, no inflation.
func BenchmarkResolveCompressedHit(b *testing.B) {
	p, codec, data, _ := loadFile(b, fpathMCUSpec)
	r := bytes.NewReader(data)
	id := ObjectID{Num: 1352} // A page dictionary of ordinary size.
	if _, err := p.Resolve(r, id, codec); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := p.Resolve(r, id, codec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolveCompressedMiss alternates objects living in different
// object streams, forcing a full stream decompression per resolve — the
// worst case of the one-stream cache.
func BenchmarkResolveCompressedMiss(b *testing.B) {
	p, codec, data, _ := loadFile(b, fpathMCUSpec)
	r := bytes.NewReader(data)
	ids := [2]ObjectID{{Num: 1}, {Num: 1352}} // Different object streams.
	for _, id := range ids {
		if _, err := p.Resolve(r, id, codec); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for b.Loop() {
		if _, err := p.Resolve(r, ids[n&1], codec); err != nil {
			b.Fatal(err)
		}
		n++
	}
}

// BenchmarkResolveCatalog measures a single hot-object resolve: one xref
// record ReadAt plus lexing and parsing of a small dictionary.
func BenchmarkResolveCatalog(b *testing.B) {
	p, codec, data, _ := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	root := rootID(b, p, r, codec)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := p.Resolve(r, root, codec)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := codec.DictGet(p, r, v, "Pages"); err != nil {
			b.Fatal(err)
		}
	}
}
