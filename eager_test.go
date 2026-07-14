package piudf

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// decodeEagerBuilder eagerly decodes b's document. The returned reader is
// only needed for bulk data (strings, stream payloads).
func decodeEagerBuilder(t *testing.T, b *pdfBuilder, lim DecodeLimits) (*PDFEager, *bytes.Reader) {
	t.Helper()
	var d Decoder
	var pe PDFEager
	data := b.buf.Bytes()
	r := bytes.NewReader(data)
	if err := d.DecodeEager(&pe, r, int64(len(data)), lim); err != nil {
		t.Fatalf("DecodeEager: %v", err)
	}
	return &pe, r
}

func TestEagerDecodeBasic(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	pe, _ := decodeEagerBuilder(t, b, DecodeLimits{})

	catalog, err := pe.Resolve(pe.Root())
	if err != nil {
		t.Fatalf("Resolve(Root): %v", err)
	}
	if catalog.Kind != KindDict {
		t.Fatalf("catalog kind = %v, want dict", catalog.Kind)
	}
	typ, err := pe.DictGet(catalog, "Type")
	if err != nil {
		t.Fatal(err)
	}
	if !pe.NameIs(typ, "Catalog") {
		t.Errorf("catalog /Type is not /Catalog")
	}
	pages, err := pe.DictGet(catalog, "Pages")
	if err != nil || !pages.IsRef() {
		t.Fatalf("catalog /Pages = %v, %v; want ref", pages, err)
	}
	pagesDict, err := pe.ResolveRef(pages, 1)
	if err != nil {
		t.Fatal(err)
	}
	kids, err := pe.DictGet(pagesDict, "Kids")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := pe.ArrayLen(kids); err != nil || n != 1 {
		t.Fatalf("ArrayLen(Kids) = %d, %v; want 1", n, err)
	}
	first, err := pe.ArrayIndex(kids, 0)
	if err != nil || first.Ref != (ObjectID{Num: 3}) {
		t.Fatalf("Kids[0] = %v, %v; want 3 0 R", first, err)
	}
	page, err := pe.Resolve(first.Ref)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := pe.DictGet(page, "MediaBox")
	if err != nil {
		t.Fatal(err)
	}
	w, err := pe.ArrayIndex(mb, 2)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := w.Float(); err != nil || f != 612.5 {
		t.Errorf("MediaBox[2] = %v, %v; want 612.5", f, err)
	}
	// Trailer needs no reader at all.
	tr, err := pe.Trailer()
	if err != nil {
		t.Fatal(err)
	}
	if sz, err := pe.DictGet(tr, "Size"); err != nil || sz.Kind != KindInt || sz.I != 7 {
		t.Errorf("trailer /Size = %v, %v; want 7", sz, err)
	}
}

func TestEagerKitchenSinkArray(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	pe, r := decodeEagerBuilder(t, b, DecodeLimits{})

	arr, err := pe.Resolve(ObjectID{Num: 5})
	if err != nil {
		t.Fatal(err)
	}
	if arr.Kind != KindArray {
		t.Fatalf("object 5 kind = %v, want array", arr.Kind)
	}
	if n, err := pe.ArrayLen(arr); err != nil || n != 11 {
		t.Fatalf("ArrayLen = %d, %v; want 11", n, err)
	}
	get := func(i int) Value {
		t.Helper()
		v, err := pe.ArrayIndex(arr, i)
		if err != nil {
			t.Fatalf("ArrayIndex(%d): %v", i, err)
		}
		return v
	}
	if v := get(0); v.Kind != KindInt || v.I != 42 {
		t.Errorf("elem 0 = %+v, want int 42", v)
	}
	if v := get(1); v.Kind != KindInt || v.I != -7 {
		t.Errorf("elem 1 = %+v, want int -7", v)
	}
	if f, err := get(2).Float(); err != nil || f != 2.5 {
		t.Errorf("elem 2 = %v, %v; want 2.5", f, err)
	}
	if s, err := pe.AppendString(nil, r, get(3)); err != nil || string(s) != "hi)there" {
		t.Errorf("elem 3 = %q, %v; want hi)there", s, err)
	}
	if s, err := pe.AppendString(nil, r, get(4)); err != nil || string(s) != "\xbe\xef" {
		t.Errorf("elem 4 = %q, %v; want BEEF bytes", s, err)
	}
	if v := get(5); !pe.NameIs(v, "Näme x") {
		t.Errorf("elem 5 is not name 'Näme x'")
	}
	if s, err := pe.AppendName(nil, get(5)); err != nil || string(s) != "Näme x" {
		t.Errorf("AppendName = %q, %v", s, err)
	}
	if v := get(6); v.Kind != KindBool || v.I != 1 {
		t.Errorf("elem 6 = %+v, want true", v)
	}
	if v := get(7); v.Kind != KindBool || v.I != 0 {
		t.Errorf("elem 7 = %+v, want false", v)
	}
	if v := get(8); !v.IsNull() {
		t.Errorf("elem 8 = %+v, want null", v)
	}
	if v := get(9); v.Ref != (ObjectID{Num: 6}) {
		t.Errorf("elem 9 = %+v, want 6 0 R", v)
	}
	nested := get(10)
	if nested.Kind != KindDict {
		t.Fatalf("elem 10 kind = %v, want dict", nested.Kind)
	}
	inner, err := pe.DictGet(nested, "Nested")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := pe.ArrayLen(inner); err != nil || n != 2 {
		t.Fatalf("nested array len = %d, %v; want 2", n, err)
	}
	if v, err := pe.ArrayIndex(inner, 1); err != nil || v.I != 2 {
		t.Errorf("nested[1] = %+v, %v; want 2", v, err)
	}
	// Out of range.
	if _, err := pe.ArrayIndex(arr, 11); err == nil {
		t.Error("ArrayIndex(11) succeeded, want out of range")
	}
}

func TestEagerRawStream(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	pe, r := decodeEagerBuilder(t, b, DecodeLimits{})

	sv, err := pe.Resolve(ObjectID{Num: 4})
	if err != nil {
		t.Fatal(err)
	}
	if sv.Kind != KindStream {
		t.Fatalf("object 4 kind = %v, want stream", sv.Kind)
	}
	sr, info, err := pe.RawStream(r, sv)
	if err != nil {
		t.Fatal(err)
	}
	if !pe.NameIs(info.Filter, "FlateDecode") {
		t.Errorf("filter is not /FlateDecode")
	}
	payload, err := io.ReadAll(sr)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "fake-deflate-payload" {
		t.Errorf("payload = %q", payload)
	}
	if info.Length != int64(len(payload)) {
		t.Errorf("info.Length = %d, want %d", info.Length, len(payload))
	}
}

// TestEagerStreamLengthRef exercises /Length given as an indirect
// reference, resolved through the object table at access time.
func TestEagerStreamLengthRef(t *testing.T) {
	b := newPDFBuilder()
	b.obj(1, "<< /Type /Catalog >>")
	payload := []byte("0123456789")
	b.offsets[2] = int64(b.buf.Len())
	b.buf.WriteString("2 0 obj\n<< /Length 3 0 R >>\nstream\n")
	b.buf.Write(payload)
	b.buf.WriteString("\nendstream\nendobj\n")
	b.obj(3, "10")
	b.xrefTrailer(3, " /Root 1 0 R")
	pe, r := decodeEagerBuilder(t, b, DecodeLimits{})

	sv, err := pe.Resolve(ObjectID{Num: 2})
	if err != nil {
		t.Fatal(err)
	}
	sr, info, err := pe.RawStream(r, sv)
	if err != nil {
		t.Fatal(err)
	}
	if info.Length != 10 {
		t.Fatalf("length = %d, want 10", info.Length)
	}
	got, _ := io.ReadAll(sr)
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %q", got)
	}
}

func TestEagerIncrementalUpdateShadowing(t *testing.T) {
	b := basicPDF()
	prev := b.xrefTrailer(6, " /Root 1 0 R")
	b.obj(6, "9999") // Newer revision shadows the original 1234.
	b.update([]uint32{6}, 7, " /Root 1 0 R", prev)
	pe, _ := decodeEagerBuilder(t, b, DecodeLimits{})

	v, err := pe.Resolve(ObjectID{Num: 6})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindInt || v.I != 9999 {
		t.Errorf("object 6 = %+v, want shadowing 9999", v)
	}
	// Objects only in the older revision still resolve.
	if _, err := pe.Resolve(ObjectID{Num: 1}); err != nil {
		t.Errorf("Resolve(1): %v", err)
	}
}

func TestEagerObjectNotFound(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	pe, _ := decodeEagerBuilder(t, b, DecodeLimits{})

	for _, id := range []ObjectID{{Num: 99}, {Num: 0}, {Num: 6, Gen: 3}} {
		if _, err := pe.Resolve(id); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Resolve(%v) = %v, want ErrObjectNotFound", id, err)
		}
	}
}

// TestEagerMemoryLimitDegradation checks both failure grades: an object
// table that cannot fit fails DecodeEager; an entry pool that fills up
// drops individual objects while the rest of the document stays usable.
// Entry counts follow the basicPDF fixture (see cumulative tally below).
func TestEagerMemoryLimitDegradation(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	data := b.buf.Bytes()
	r := bytes.NewReader(data)
	var d Decoder
	var pe PDFEager

	// Object table (7 slots) exceeds MaxEntries: structural failure.
	err := d.DecodeEager(&pe, r, int64(len(data)), DecodeLimits{MaxEntries: 4})
	if !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("DecodeEager(MaxEntries=4) = %v, want ErrMemoryLimit", err)
	}

	// Entry pool tally: trailer 2, obj1 +2 = 4, obj2 +4 = 8, obj3 +8 = 16,
	// obj4 +2 = 18. Object 5 needs 14 more and is dropped at 20; the rest
	// of the document survives.
	if err := d.DecodeEager(&pe, r, int64(len(data)), DecodeLimits{MaxEntries: 20}); err != nil {
		t.Fatalf("DecodeEager(MaxEntries=20): %v", err)
	}
	if _, err := pe.Resolve(ObjectID{Num: 5}); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Resolve(5) = %v, want dropped", err)
	}
	if pe.Stats().Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", pe.Stats().Dropped)
	}
	for _, num := range []uint32{1, 2, 3, 4, 6} {
		if _, err := pe.Resolve(ObjectID{Num: num}); err != nil {
			t.Errorf("Resolve(%d) after degradation: %v", num, err)
		}
	}
}

// TestEagerLazyCrossCheck decodes the embedded corpus through both models
// and verifies they agree on every object.
func TestEagerLazyCrossCheck(t *testing.T) {
	d, p, data, locs := loadFile(t, embeddedPDF)
	r := bytes.NewReader(data)
	var pe PDFEager
	if err := d.DecodeEager(&pe, r, int64(len(data)), DecodeLimits{}); err != nil {
		t.Fatalf("DecodeEager: %v", err)
	}
	if dropped := pe.Stats().Dropped; dropped != 0 {
		t.Fatalf("eager decode dropped %d objects", dropped)
	}
	for _, loc := range locs {
		lv, err := d.Resolve(p, r, loc.id)
		if err != nil {
			t.Fatalf("lazy Resolve(%v): %v", loc.id, err)
		}
		ev, err := pe.Resolve(loc.id)
		if err != nil {
			t.Fatalf("eager Resolve(%v): %v", loc.id, err)
		}
		if lv.Kind != ev.Kind {
			t.Errorf("%v: lazy kind %v, eager kind %v", loc.id, lv.Kind, ev.Kind)
			continue
		}
		switch lv.Kind {
		case KindInt, KindBool, KindReal:
			if lv.I != ev.I {
				t.Errorf("%v: lazy scalar %d, eager %d", loc.id, lv.I, ev.I)
			}
		case KindStream:
			lr, li, lerr := d.RawStream(p, r, lv)
			er, ei, eerr := pe.RawStream(r, ev)
			if lerr != nil || eerr != nil {
				t.Errorf("%v: RawStream lazy %v, eager %v", loc.id, lerr, eerr)
				continue
			}
			if li.Offset != ei.Offset || li.Length != ei.Length {
				t.Errorf("%v: stream info lazy %+v, eager %+v", loc.id, li, ei)
			}
			lb, _ := io.ReadAll(lr)
			eb, _ := io.ReadAll(er)
			if !bytes.Equal(lb, eb) {
				t.Errorf("%v: stream payloads differ", loc.id)
			}
		}
	}
}

// TestEagerStringParity verifies the standalone unescape helpers decode
// exactly what the lexer decodes.
func TestEagerStringParity(t *testing.T) {
	b := newPDFBuilder()
	b.obj(1, "<< /Type /Catalog >>")
	b.obj(2, `[ (simple) (esc \n\r\t\b\f \( \) \\ \101\102 \z) (nested (parens) ok) <BE EF 4> (split\
line) ]`)
	b.xrefTrailer(2, " /Root 1 0 R")
	d, p, r := decodeBuilder(t, b, DecodeLimits{})
	var pe PDFEager
	data := b.buf.Bytes()
	if err := d.DecodeEager(&pe, r, int64(len(data)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	la, err := d.Resolve(p, r, ObjectID{Num: 2})
	if err != nil {
		t.Fatal(err)
	}
	ea, err := pe.Resolve(ObjectID{Num: 2})
	if err != nil {
		t.Fatal(err)
	}
	n, err := pe.ArrayLen(ea)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		lv, err := d.ArrayIndex(r, la, i)
		if err != nil {
			t.Fatalf("lazy ArrayIndex(%d): %v", i, err)
		}
		ev, err := pe.ArrayIndex(ea, i)
		if err != nil {
			t.Fatalf("eager ArrayIndex(%d): %v", i, err)
		}
		ls, err := d.AppendString(nil, r, lv)
		if err != nil {
			t.Fatalf("lazy AppendString(%d): %v", i, err)
		}
		es, err := pe.AppendString(nil, r, ev)
		if err != nil {
			t.Fatalf("eager AppendString(%d): %v", i, err)
		}
		if !bytes.Equal(ls, es) {
			t.Errorf("elem %d: lazy %q, eager %q", i, ls, es)
		}
	}
}

// TestEagerNavigationAllocs: structure navigation on a decoded PDFEager
// allocates nothing.
func TestEagerNavigationAllocs(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	pe, _ := decodeEagerBuilder(t, b, DecodeLimits{})
	root := pe.Root()
	allocs := testing.AllocsPerRun(100, func() {
		catalog, err := pe.Resolve(root)
		if err != nil {
			t.Fatal(err)
		}
		typ, err := pe.DictGet(catalog, "Type")
		if err != nil {
			t.Fatal(err)
		}
		if !pe.NameIs(typ, "Catalog") {
			t.Fatal("bad type")
		}
		pages, _ := pe.DictGet(catalog, "Pages")
		pagesDict, err := pe.ResolveRef(pages, 1)
		if err != nil {
			t.Fatal(err)
		}
		kids, _ := pe.DictGet(pagesDict, "Kids")
		if _, err := pe.ArrayIndex(kids, 0); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("navigation allocates %v per run, want 0", allocs)
	}
}

func TestSizeOnRAM(t *testing.T) {
	b := basicPDF()
	b.xrefTrailer(6, " /Root 1 0 R")
	_, p, _ := decodeBuilder(t, b, DecodeLimits{})
	pe, _ := decodeEagerBuilder(t, b, DecodeLimits{})

	lazy, eager := p.SizeOnRAM(), pe.SizeOnRAM()
	if lazy <= 0 || eager <= 0 {
		t.Fatalf("sizes lazy=%d eager=%d, want positive", lazy, eager)
	}
	// Eager holds the object table, entry pool and name pool on top of the
	// same lazy index.
	if eager <= lazy {
		t.Errorf("eager %d <= lazy %d; eager must reference more memory", eager, lazy)
	}
	t.Logf("lazy=%dB eager=%dB", lazy, eager)
}

// TestEagerReuse: recycling one PDFEager across documents reuses its
// tables; state from the first document does not leak into the second.
func TestEagerReuse(t *testing.T) {
	bA := basicPDF()
	bA.xrefTrailer(6, " /Root 1 0 R")
	bB := newPDFBuilder()
	bB.obj(1, "<< /Type /Catalog /Marker (docB) >>")
	bB.xrefTrailer(1, " /Root 1 0 R")

	var d Decoder
	var pe PDFEager
	dataA := bA.buf.Bytes()
	dataB := bB.buf.Bytes()
	if err := d.DecodeEager(&pe, bytes.NewReader(dataA), int64(len(dataA)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := d.DecodeEager(&pe, bytes.NewReader(dataB), int64(len(dataB)), DecodeLimits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pe.Resolve(ObjectID{Num: 5}); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("object 5 from document A leaked into B: %v", err)
	}
	catalog, err := pe.Resolve(ObjectID{Num: 1})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := pe.DictGet(catalog, "Marker")
	if err != nil || marker.Kind != KindString {
		t.Fatalf("marker = %+v, %v; want string", marker, err)
	}
	s, err := pe.AppendString(nil, bytes.NewReader(dataB), marker)
	if err != nil || string(s) != "docB" {
		t.Errorf("marker = %q, %v; want docB", s, err)
	}
}
