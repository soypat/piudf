package piudf

import (
	"bytes"
	"os"
	"testing"
)

const embeddedPDF = "testdata/basis-of-embedded.pdf"

// loadFile decodes the PDF at path and returns the decoder, the raw bytes
// and the location of every live object in the file.
func loadFile(tb testing.TB, path string) (*Decoder, *PDF, []byte, []objloc) {
	tb.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("document unavailable: %v", err)
	}
	var d Decoder
	var p PDF
	r := bytes.NewReader(data)
	if err := d.Decode(&p, r, int64(len(data)), DecodeLimits{}); err != nil {
		tb.Fatal(err)
	}
	tr, err := d.Trailer(&p, r)
	if err != nil {
		tb.Fatal(err)
	}
	sizeV, err := p.DictGet(tr, "Size")
	if err != nil {
		tb.Fatal(err)
	}
	size, err := sizeV.Int()
	if err != nil {
		tb.Fatal(err)
	}
	var locs []objloc
	for num := uint32(1); num < uint32(size); num++ {
		rec, err := p.lookupXref(r, num)
		if err != nil || rec.kind != recNormal {
			continue
		}
		locs = append(locs, objloc{id: ObjectID{Num: num, Gen: rec.gen}, off: rec.offset})
	}
	if len(locs) == 0 {
		tb.Fatal("no objects found")
	}
	return &d, &p, data, locs
}

type objloc struct {
	id  ObjectID
	off int64
}

// lexObject tokenizes one object's structural region: from its header up to
// and including the terminating endobj, or up to the stream keyword (payload
// bytes are never lexed; skipping them needs the parsed /Length, which is
// the parser's job and exactly what this benchmark excludes).
func lexObject(tb testing.TB, lx *Lexer, r *bytes.Reader, off int64) (ntok int, end int64) {
	if err := lx.Reset(r, off); err != nil {
		tb.Fatal(err)
	}
	for {
		tok, pos, _ := lx.NextToken()
		switch tok {
		case TokEndobj, TokStream, TokEOF:
			return ntok + 1, int64(pos)
		case TokIllegal:
			tb.Fatalf("illegal token at %v: %v", pos, lx.Err())
		}
		ntok++
	}
}

// TestEmbeddedPDFResolveAll sanity-checks the corpus the benchmarks use:
// every object in the file must resolve.
func TestEmbeddedPDFResolveAll(t *testing.T) {
	d, p, data, locs := loadFile(t, embeddedPDF)
	r := bytes.NewReader(data)
	kinds := make(map[Kind]int)
	for _, loc := range locs {
		v, err := d.Resolve(p, r, loc.id)
		if err != nil {
			t.Fatalf("Resolve(%v): %v", loc.id, err)
		}
		kinds[v.Kind]++
	}
	t.Logf("%d objects: %v, values high water %d", len(locs), kinds, p.Stats().ValuesHighWater)
}

// BenchmarkDecodeInit measures trailer plus cross-reference chain parsing,
// the whole cost of opening a document (no object contents are read).
func BenchmarkDecodeInit(b *testing.B) {
	_, p, data, _ := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	var d Decoder
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := d.Decode(p, r, int64(len(data)), DecodeLimits{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLexObjects measures pure lexing: every object's structural bytes
// are tokenized, with no value conversion, interning, or tree assembly.
func BenchmarkLexObjects(b *testing.B) {
	_, _, data, locs := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	var lx Lexer
	lx.ReuseLiteralBuffer = true
	// Pre-compute structural byte count so ns/op pairs with MB/s.
	var structuralBytes, ntok int64
	for _, loc := range locs {
		n, end := lexObject(b, &lx, r, loc.off)
		ntok += int64(n)
		structuralBytes += end - loc.off
	}
	b.SetBytes(structuralBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, loc := range locs {
			lexObject(b, &lx, r, loc.off)
		}
	}
	b.ReportMetric(float64(ntok), "tokens/op")
}

// BenchmarkResolveObjects measures the full lazy pipeline per document
// sweep: xref record lookup, lexing, value conversion, name interning and
// composite arena assembly for every object.
func BenchmarkResolveObjects(b *testing.B) {
	d, p, data, locs := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	var structuralBytes int64
	{
		var lx Lexer
		lx.ReuseLiteralBuffer = true
		r := bytes.NewReader(data)
		for _, loc := range locs {
			_, end := lexObject(b, &lx, r, loc.off)
			structuralBytes += end - loc.off
		}
	}
	b.SetBytes(structuralBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, loc := range locs {
			if _, err := d.Resolve(p, r, loc.id); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkResolveCatalog measures a single hot-object resolve: one xref
// record ReadAt plus lexing and parsing of a small dictionary.
func BenchmarkResolveCatalog(b *testing.B) {
	d, p, data, _ := loadFile(b, embeddedPDF)
	r := bytes.NewReader(data)
	root := p.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := d.Resolve(p, r, root)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := p.DictGet(v, "Pages"); err != nil {
			b.Fatal(err)
		}
	}
}
