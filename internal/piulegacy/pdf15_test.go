package piudf

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"os"
	"testing"
)

// Cross-reference stream payload encodings exercised by the tests.
const (
	xsRaw       = iota // No filter: records stored verbatim.
	xsFlate            // /Filter /FlateDecode.
	xsFlatePred        // FlateDecode behind PNG Up predictor, like most writers emit.
)

func zlibCompress(data []byte) []byte {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(data)
	zw.Close()
	return z.Bytes()
}

// pngEncode forward-filters rows with one PNG filter type, producing the
// on-disk form applyPNGPredictor must invert.
func pngEncode(orig []byte, cols, bpp int, ft byte) []byte {
	var out []byte
	for r := 0; r*cols < len(orig); r++ {
		row := orig[r*cols : (r+1)*cols]
		out = append(out, ft)
		for i := range row {
			var a, b, c byte
			if i >= bpp {
				a = row[i-bpp]
			}
			if r > 0 {
				b = orig[(r-1)*cols+i]
				if i >= bpp {
					c = orig[(r-1)*cols+i-bpp]
				}
			}
			var pred byte
			switch ft {
			case 1:
				pred = a
			case 2:
				pred = b
			case 3:
				pred = byte((int(a) + int(b)) / 2)
			case 4:
				pred = paeth(a, b, c)
			}
			out = append(out, row[i]-pred)
		}
	}
	return out
}

// objStmSpec is one bare object stored inside an object stream.
type objStmSpec struct {
	num  uint32
	body string
}

// objStm writes an object stream holding objs and records each contained
// object's (stream, index) into compressed for xref stream construction.
func (b *pdfBuilder) objStm(num uint32, objs []objStmSpec, flate bool, compressed map[uint32][2]int64) {
	var pairs, data bytes.Buffer
	for i, o := range objs {
		fmt.Fprintf(&pairs, "%d %d ", o.num, data.Len())
		data.WriteString(o.body)
		data.WriteString("\n")
		compressed[o.num] = [2]int64{int64(num), int64(i)}
	}
	first := pairs.Len()
	payload := append(pairs.Bytes(), data.Bytes()...)
	extra := fmt.Sprintf(" /Type /ObjStm /N %d /First %d", len(objs), first)
	if flate {
		payload = zlibCompress(payload)
		extra += " /Filter /FlateDecode"
	}
	b.streamObj(num, extra, payload)
}

// xrefStream writes a cross-reference stream as object num — covering
// object numbers [0, num], itself included — and the startxref tail.
// compressed maps object number → {containing stream, index within}.
func (b *pdfBuilder) xrefStream(num uint32, trailerExtra string, mode int, compressed map[uint32][2]int64) int64 {
	b.offsets[num] = int64(b.buf.Len())
	size := num + 1
	var recs bytes.Buffer
	putRec := func(typ byte, f2, f3 int64) {
		recs.WriteByte(typ)
		recs.Write([]byte{byte(f2 >> 24), byte(f2 >> 16), byte(f2 >> 8), byte(f2)})
		recs.Write([]byte{byte(f3 >> 8), byte(f3)})
	}
	for n := uint32(0); n < size; n++ {
		if c, ok := compressed[n]; ok {
			putRec(2, c[0], c[1])
		} else if off, ok := b.offsets[n]; ok && n != 0 {
			putRec(1, off, 0)
		} else {
			putRec(0, 0, 65535)
		}
	}
	const cols = 1 + 4 + 2
	payload := recs.Bytes()
	extra := fmt.Sprintf(" /Type /XRef /Size %d /W [1 4 2]%s", size, trailerExtra)
	switch mode {
	case xsFlate:
		payload = zlibCompress(payload)
		extra += " /Filter /FlateDecode"
	case xsFlatePred:
		payload = zlibCompress(pngEncode(payload, cols, 1, 2))
		extra += fmt.Sprintf(" /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns %d >>", cols)
	}
	xrefOff := b.offsets[num]
	fmt.Fprintf(&b.buf, "%d 0 obj\n<< /Length %d%s >>\nstream\n", num, len(payload), extra)
	b.buf.Write(payload)
	b.buf.WriteString("\nendstream\nendobj\n")
	fmt.Fprintf(&b.buf, "startxref\n%d\n%%%%EOF\n", xrefOff)
	return xrefOff
}

func TestPNGPredictor(t *testing.T) {
	const cols, bpp = 5, 1
	orig := []byte{
		10, 20, 30, 40, 50,
		15, 25, 200, 45, 55,
		0, 255, 128, 1, 254,
		7, 7, 7, 7, 7,
	}
	for ft := byte(0); ft <= 4; ft++ {
		enc := pngEncode(orig, cols, bpp, ft)
		got, err := applyPNGPredictor(append([]byte{}, enc...), cols, 1, 8)
		if err != nil {
			t.Fatalf("filter %d: %v", ft, err)
		}
		if !bytes.Equal(got, orig) {
			t.Errorf("filter %d: got % x, want % x", ft, got, orig)
		}
	}
	// The filter byte is chosen per row: reconstruct a mix.
	var mixed []byte
	for r := 0; r < 4; r++ {
		row := orig[r*cols : (r+1)*cols]
		// Encode row r with filter r%5 against the true previous row.
		full := pngEncode(orig[:(r+1)*cols], cols, bpp, byte(r))
		mixed = append(mixed, full[r*(cols+1):]...)
		_ = row
	}
	got, err := applyPNGPredictor(mixed, cols, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("mixed filters: got % x, want % x", got, orig)
	}
	// Misaligned data is corrupt, not a crash.
	if _, err := applyPNGPredictor(make([]byte, cols+2), cols, 1, 8); !errors.Is(err, ErrCorrupt) {
		t.Errorf("misaligned: %v, want ErrCorrupt", err)
	}
}

func TestParseStreamRecord(t *testing.T) {
	w := [3]uint8{1, 4, 2}
	rec, err := parseStreamRecord([]byte{1, 0, 0, 0x12, 0x34, 0, 5}, w)
	if err != nil || rec.kind != recNormal || rec.offset != 0x1234 || rec.gen != 5 {
		t.Errorf("type 1: %+v, %v", rec, err)
	}
	rec, err = parseStreamRecord([]byte{2, 0, 0, 0, 9, 0, 3}, w)
	if err != nil || rec.kind != recCompressed || rec.stream != 9 || rec.offset != 3 {
		t.Errorf("type 2: %+v, %v", rec, err)
	}
	rec, err = parseStreamRecord([]byte{0, 0, 0, 0, 7, 0xff, 0xff}, w)
	if err != nil || rec.kind != recFree {
		t.Errorf("type 0: %+v, %v", rec, err)
	}
	// Width-0 type field defaults to type 1.
	rec, err = parseStreamRecord([]byte{0, 0, 0, 0x42, 0, 0}, [3]uint8{0, 4, 2})
	if err != nil || rec.kind != recNormal || rec.offset != 0x42 {
		t.Errorf("default type: %+v, %v", rec, err)
	}
	// Unknown types read as free (null object), not an error.
	rec, err = parseStreamRecord([]byte{7, 0, 0, 0, 1, 0, 0}, w)
	if err != nil || rec.kind != recFree {
		t.Errorf("unknown type: %+v, %v", rec, err)
	}
}

func TestXrefStreamDecode(t *testing.T) {
	for mode, name := range []string{"raw", "flate", "flate+predictor"} {
		t.Run(name, func(t *testing.T) {
			b := basicPDF()
			b.xrefStream(7, " /Root 1 0 R", mode, nil)
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
			// All six document objects resolve, as does the xref stream itself.
			for num := uint32(1); num <= 7; num++ {
				if _, err := d.Resolve(p, r, ObjectID{Num: num}); err != nil {
					t.Errorf("Resolve(%d): %v", num, err)
				}
			}
			tr, err := d.Trailer(p, r)
			if err != nil {
				t.Fatal(err)
			}
			sz, err := d.DictGet(p, r, tr, "Size")
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := sz.Int(); n != 8 {
				t.Errorf("trailer /Size = %d, want 8", n)
			}
		})
	}
}

// objStmPDF builds a document whose catalog and two more objects live
// inside a compressed object stream (object 3); object 2 stays normal.
func objStmPDF(flate bool, mode int) (*pdfBuilder, int64) {
	b := newPDFBuilder()
	b.obj(2, "<< /Type /Pages /Count 1 >>")
	compressed := make(map[uint32][2]int64)
	b.objStm(3, []objStmSpec{
		{1, "<< /Type /Catalog /Pages 2 0 R /Title (Hel\\)lo) >>"},
		{4, "1234"},
		{5, "[ 42 (s\\)tr) /Näme#20x [2 3] ]"},
	}, flate, compressed)
	xrefOff := b.xrefStream(6, " /Root 1 0 R", mode, compressed)
	return b, xrefOff
}

func TestObjStmLazy(t *testing.T) {
	b, _ := objStmPDF(true, xsFlatePred)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	// The catalog lives in an object stream: its Value must be tagged and
	// every accessor must read it from the decompressed cache.
	cat, err := d.Resolve(p, r, ObjectID{Num: 1})
	if err != nil {
		t.Fatal(err)
	}
	if cat.Kind != KindDict || cat.Ref.Num != 3 {
		t.Fatalf("catalog = %+v, want dict tagged with stream 3", cat)
	}
	typ, err := d.DictGet(p, r, cat, "Type")
	if err != nil || !d.NameIs(p, r, typ, "Catalog") {
		t.Fatalf("catalog /Type: %+v %v", typ, err)
	}
	pages, err := d.DictGet(p, r, cat, "Pages")
	if err != nil || pages.Ref != (ObjectID{Num: 2}) {
		t.Fatalf("catalog /Pages: %+v %v", pages, err)
	}
	title, err := d.DictGet(p, r, cat, "Title")
	if err != nil || title.Kind != KindString || title.Ref.Num != 3 {
		t.Fatalf("catalog /Title: %+v %v", title, err)
	}
	s, err := d.AppendString(nil, p, r, title)
	if err != nil || string(s) != "Hel)lo" {
		t.Fatalf("title = %q, %v", s, err)
	}
	// Reference out of the stream into a normal object still works.
	pagesDict, err := d.ResolveRef(p, r, pages, 1)
	if err != nil || pagesDict.Kind != KindDict || pagesDict.Ref.Num != 0 {
		t.Fatalf("pages dict: %+v %v", pagesDict, err)
	}
	// Scalars carry no tag.
	v4, err := d.Resolve(p, r, ObjectID{Num: 4})
	if err != nil || v4.Kind != KindInt || v4.Ref.Num != 0 {
		t.Fatalf("object 4 = %+v, %v", v4, err)
	}
	if n, _ := v4.Int(); n != 1234 {
		t.Errorf("object 4 = %d", n)
	}
	// Array navigation inside the stream, including a nested composite.
	arr, err := d.Resolve(p, r, ObjectID{Num: 5})
	if err != nil || arr.Kind != KindArray || arr.Ref.Num != 3 {
		t.Fatalf("object 5 = %+v, %v", arr, err)
	}
	if n, err := d.ArrayLen(p, r, arr); err != nil || n != 4 {
		t.Fatalf("ArrayLen = %d, %v", n, err)
	}
	e1, _ := d.ArrayIndex(p, r, arr, 1)
	s, err = d.AppendString(s[:0], p, r, e1)
	if err != nil || string(s) != "s)tr" {
		t.Errorf("elem 1 = %q, %v", s, err)
	}
	e2, _ := d.ArrayIndex(p, r, arr, 2)
	if !d.NameIs(p, r, e2, "Näme x") {
		t.Errorf("elem 2 name mismatch: %+v", e2)
	}
	e3, _ := d.ArrayIndex(p, r, arr, 3)
	if e3.Kind != KindArray || e3.Ref.Num != 3 {
		t.Fatalf("elem 3 = %+v", e3)
	}
	if n, err := d.ArrayLen(p, r, e3); err != nil || n != 2 {
		t.Errorf("nested ArrayLen = %d, %v", n, err)
	}
	// Objects in object streams always have generation 0.
	if _, err := d.Resolve(p, r, ObjectID{Num: 1, Gen: 2}); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("gen 2: %v, want ErrObjectNotFound", err)
	}
}

func TestObjStmLazyCacheHitAllocs(t *testing.T) {
	b, _ := objStmPDF(true, xsFlate)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})
	cat, err := d.Resolve(p, r, ObjectID{Num: 1})
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		typ, err := d.DictGet(p, r, cat, "Type")
		if err != nil || !d.NameIs(p, r, typ, "Catalog") {
			t.Fatal("catalog type", err)
		}
	})
	if allocs != 0 {
		t.Errorf("cache-hit DictGet+NameIs allocates %v times per run, want 0", allocs)
	}
}

func TestObjStmEager(t *testing.T) {
	b, _ := objStmPDF(true, xsFlatePred)
	data := b.buf.Bytes()
	r := bytes.NewReader(data)
	var d Decoder
	var pe PDFEager
	if err := d.DecodeEager(&pe, r, int64(len(data)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	if got := pe.Stats().Dropped; got != 0 {
		t.Fatalf("Dropped = %d", got)
	}
	cat, err := pe.Resolve(ObjectID{Num: 1})
	if err != nil {
		t.Fatal(err)
	}
	typ, err := pe.DictGet(cat, "Type")
	if err != nil || !pe.NameIs(typ, "Catalog") {
		t.Fatalf("catalog /Type: %+v %v", typ, err)
	}
	// Strings from the stream were interned decoded: no reader needed.
	title, err := pe.DictGet(cat, "Title")
	if err != nil || title.Kind != KindString || title.Ref.Num == 0 {
		t.Fatalf("title = %+v, %v", title, err)
	}
	s, err := pe.AppendString(nil, nil, title)
	if err != nil || string(s) != "Hel)lo" {
		t.Fatalf("title = %q, %v", s, err)
	}
	arr, err := pe.Resolve(ObjectID{Num: 5})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := pe.ArrayLen(arr); n != 4 {
		t.Fatalf("ArrayLen = %d", n)
	}
	e1, _ := pe.ArrayIndex(arr, 1)
	s, err = pe.AppendString(s[:0], nil, e1)
	if err != nil || string(s) != "s)tr" {
		t.Errorf("elem 1 = %q, %v", s, err)
	}
	v4, err := pe.Resolve(ObjectID{Num: 4})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := v4.Int(); n != 1234 {
		t.Errorf("object 4 = %d", n)
	}
}

func TestObjStmShadowing(t *testing.T) {
	b, xrefOff := objStmPDF(true, xsFlate)
	// Incremental update moves object 4 out of the stream.
	b.obj(4, "9999")
	b.update([]uint32{4}, 7, " /Root 1 0 R", xrefOff)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	v, err := d.Resolve(p, r, ObjectID{Num: 4})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := v.Int(); n != 9999 {
		t.Errorf("object 4 = %d, want updated 9999", n)
	}
	// Other stream objects unaffected.
	if cat, err := d.Resolve(p, r, ObjectID{Num: 1}); err != nil || cat.Kind != KindDict {
		t.Errorf("object 1: %+v, %v", cat, err)
	}
	// Eager: the pair table still lists object 4, but the xref shadowing
	// check must keep the classic-update revision.
	data := b.buf.Bytes()
	var pe PDFEager
	if err := d.DecodeEager(&pe, bytes.NewReader(data), int64(len(data)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	if got := pe.Stats().Dropped; got != 0 {
		t.Fatalf("Dropped = %d", got)
	}
	ev, err := pe.Resolve(ObjectID{Num: 4})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := ev.Int(); n != 9999 {
		t.Errorf("eager object 4 = %d, want 9999", n)
	}
}

func TestObjStmMaxDecompress(t *testing.T) {
	// Raw xref stream (49 bytes of records) fits; the flate object stream
	// does not: compressed objects degrade softly, normal ones survive.
	b, _ := objStmPDF(true, xsRaw)
	data := b.buf.Bytes()
	r := bytes.NewReader(data)
	var d Decoder
	var p PDF
	if err := d.Decode(&p, r, int64(len(data)), DecodeLimits{MaxDecompress: 64, Grow: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resolve(&p, r, ObjectID{Num: 1}); !errors.Is(err, ErrMemoryLimit) {
		t.Errorf("compressed object: %v, want ErrMemoryLimit", err)
	}
	if v, err := d.Resolve(&p, r, ObjectID{Num: 2}); err != nil || v.Kind != KindDict {
		t.Errorf("normal object: %+v, %v", v, err)
	}
	// A cap too small even for the xref records is structural.
	var p2 PDF
	b2, _ := objStmPDF(true, xsFlate)
	data2 := b2.buf.Bytes()
	err := d.Decode(&p2, bytes.NewReader(data2), int64(len(data2)), DecodeLimits{MaxDecompress: 8, Grow: true})
	if !errors.Is(err, ErrMemoryLimit) {
		t.Errorf("xref cap: %v, want ErrMemoryLimit", err)
	}
	// Eager on the tight budget: stream objects dropped, the rest usable.
	var pe PDFEager
	if err := d.DecodeEager(&pe, r, int64(len(data)), DecodeLimits{MaxDecompress: 64, Grow: true}); err != nil {
		t.Fatal(err)
	}
	if got := pe.Stats().Dropped; got != 3 {
		t.Errorf("Dropped = %d, want 3", got)
	}
	if v, err := pe.Resolve(ObjectID{Num: 2}); err != nil || v.Kind != KindDict {
		t.Errorf("eager normal object: %+v, %v", v, err)
	}
	if _, err := pe.Resolve(ObjectID{Num: 1}); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("eager dropped object: %v, want ErrObjectNotFound", err)
	}
}

const datasheetPDF = "testdata/rp2350-datasheet.pdf"

// loadDatasheet decodes the PDF 1.5 corpus file lazily.
func loadDatasheet(tb testing.TB) (*Decoder, *PDF, *bytes.Reader) {
	tb.Helper()
	data, err := os.ReadFile(datasheetPDF)
	if err != nil {
		tb.Skipf("document unavailable: %v", err)
	}
	var d Decoder
	var p PDF
	r := bytes.NewReader(data)
	if err := d.Decode(&p, r, int64(len(data)), DecodeLimits{}); err != nil {
		tb.Fatal(err)
	}
	return &d, &p, r
}

func TestDatasheetLazy(t *testing.T) {
	d, p, r := loadDatasheet(t)
	cat, err := d.Resolve(p, r, p.Root())
	if err != nil {
		t.Fatal(err)
	}
	typ, err := d.DictGet(p, r, cat, "Type")
	if err != nil || !d.NameIs(p, r, typ, "Catalog") {
		t.Fatalf("catalog /Type: %+v %v", typ, err)
	}
	// The page tree lives inside object streams.
	pages, err := d.DictGet(p, r, cat, "Pages")
	if err != nil {
		t.Fatal(err)
	}
	pagesDict, err := d.ResolveRef(p, r, pages, 1)
	if err != nil {
		t.Fatal(err)
	}
	count, err := d.DictGet(p, r, pagesDict, "Count")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := count.Int(); n <= 100 {
		t.Errorf("page count = %d, want a few hundred", n)
	}
	// So does the info dictionary.
	info, err := d.Resolve(p, r, p.Info())
	if err != nil {
		t.Fatal(err)
	}
	title, err := d.DictGet(p, r, info, "Title")
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.AppendString(nil, p, r, title)
	if err != nil || len(s) == 0 {
		t.Fatalf("info /Title = %q, %v", s, err)
	}
	t.Logf("title: %q", s)
}

func TestDatasheetEagerCrossCheck(t *testing.T) {
	d, p, r := loadDatasheet(t)
	var pe PDFEager
	if err := d.DecodeEager(&pe, r, int64(r.Size()), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	if got := pe.Stats().Dropped; got != 0 {
		t.Fatalf("Dropped = %d, want 0", got)
	}
	size := p.trailer.size
	var checked, present, strEq int
	var lstr, estr []byte
	for num := uint32(1); num < uint32(size); num++ {
		id := ObjectID{Num: num}
		lv, lerr := d.Resolve(p, r, id)
		ev, eerr := pe.Resolve(id)
		if (lerr == nil) != (eerr == nil) {
			t.Fatalf("%v: lazy err %v, eager err %v", id, lerr, eerr)
		}
		if lerr != nil {
			continue
		}
		present++
		if lv.Kind != ev.Kind {
			t.Fatalf("%v: lazy kind %v, eager kind %v", id, lv.Kind, ev.Kind)
		}
		switch lv.Kind {
		case KindInt, KindReal, KindBool:
			if lv.I != ev.I {
				t.Fatalf("%v: lazy I %d, eager I %d", id, lv.I, ev.I)
			}
		case KindArray:
			ln, lerr := d.ArrayLen(p, r, lv)
			en, eerr := pe.ArrayLen(ev)
			if lerr != nil || eerr != nil || ln != en {
				t.Fatalf("%v: array len lazy %d (%v), eager %d (%v)", id, ln, lerr, en, eerr)
			}
		case KindString, KindHexString:
			var err1, err2 error
			lstr, err1 = d.AppendString(lstr[:0], p, r, lv)
			estr, err2 = pe.AppendString(estr[:0], r, ev)
			if err1 != nil || err2 != nil || !bytes.Equal(lstr, estr) {
				t.Fatalf("%v: string lazy %q (%v), eager %q (%v)", id, lstr, err1, estr, err2)
			}
			strEq++
		}
		checked++
	}
	if present < 15000 {
		t.Errorf("only %d objects present, want ~15887", present)
	}
	t.Logf("%d objects cross-checked (%d string comparisons)", checked, strEq)
}
