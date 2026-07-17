package ppdf

import (
	"bytes"
	"compress/zlib"
	"io"
	"testing"

	"github.com/soypat/piudf/piulex"
)

// buildFreshDoc writes a complete one-page document and returns its bytes and
// the ids the checks below need. The emission order is deliberately not the
// reference order — the catalog is written last — since decoupling the two is
// what NewID is for.
func buildFreshDoc(t *testing.T, contents []byte, compress bool) (doc []byte, rootID, contentID, infoID ObjectID) {
	t.Helper()
	var out bytes.Buffer
	var e Encoder
	if err := e.Reset(&out, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	cat, pages, page := e.NewID(), e.NewID(), e.NewID()
	content, clen := e.NewID(), e.NewID()
	font, info := e.NewID(), e.NewID()

	e.BeginObject(content)
	e.DictOpen()
	e.Name("Length")
	e.Ref(clen.Num, clen.Gen)
	if compress {
		e.Name("Filter")
		e.Name("FlateDecode")
	}
	e.DictClose()
	e.BeginStreamPayload()
	if compress {
		zw := zlib.NewWriter(e.PayloadWriter())
		if _, err := zw.Write(contents); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		e.Raw(contents)
	}
	n := e.EndStreamPayload()
	e.EndObject()
	e.BeginObject(clen)
	e.Int(n)
	e.EndObject()

	e.BeginObject(font)
	e.DictOpen()
	e.Name("Type")
	e.Name("Font")
	e.Name("Subtype")
	e.Name("Type1")
	e.Name("BaseFont")
	e.Name("Helvetica")
	e.DictClose()
	e.EndObject()

	e.BeginObject(page)
	e.DictOpen()
	e.Name("Type")
	e.Name("Page")
	e.Name("Parent")
	e.Ref(pages.Num, pages.Gen)
	e.Name("MediaBox")
	e.ArrayOpen()
	e.Int(0)
	e.Int(0)
	e.Real(612)
	e.Real(792)
	e.ArrayClose()
	e.Name("Contents")
	e.Ref(content.Num, content.Gen)
	e.Name("Resources")
	e.DictOpen()
	e.Name("Font")
	e.DictOpen()
	e.Name("F1")
	e.Ref(font.Num, font.Gen)
	e.DictClose()
	e.DictClose()
	e.DictClose()
	e.EndObject()

	e.BeginObject(pages)
	e.DictOpen()
	e.Name("Type")
	e.Name("Pages")
	e.Name("Kids")
	e.ArrayOpen()
	e.Ref(page.Num, page.Gen)
	e.ArrayClose()
	e.Name("Count")
	e.Int(1)
	e.DictClose()
	e.EndObject()

	e.BeginObject(info)
	e.DictOpen()
	e.Name("Title")
	e.String([]byte("piudf test"))
	e.DictClose()
	e.EndObject()

	e.BeginObject(cat)
	e.DictOpen()
	e.Name("Type")
	e.Name("Catalog")
	e.Name("Pages")
	e.Ref(pages.Num, pages.Gen)
	e.DictClose()
	e.EndObject()

	if err := e.Close(cat, info); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), cat, content, info
}

// TestEncoderFresh round-trips a from-scratch document through this package's
// own decoder: the reader is the writer's oracle exactly as the lexer is the
// emitter's.
func TestEncoderFresh(t *testing.T) {
	payload := []byte("BT /F1 12 Tf 72 720 Td (Hello) Tj ET")
	for _, compress := range []bool{false, true} {
		doc, rootID, contentID, _ := buildFreshDoc(t, payload, compress)
		if !bytes.HasPrefix(doc, []byte("%PDF-1.7\n")) {
			t.Fatalf("compress=%v: output starts %q", compress, doc[:16])
		}
		r := bytes.NewReader(doc)
		codec := newCodec(make([]byte, 4096))
		var p PDF
		if err := p.Decode(r, int64(len(doc)), codec); err != nil {
			t.Fatalf("compress=%v: decoding own output: %v\n%s", compress, err, doc)
		}
		if n := p.NumRevisions(); n != 1 {
			t.Errorf("compress=%v: NumRevisions = %d, want 1", compress, n)
		}
		cat := mustCatalog(t, &p, r, codec)
		if cat.num != rootID.Num {
			t.Errorf("compress=%v: /Root resolves object %d, want %d", compress, cat.num, rootID.Num)
		}
		if typ := mustType(t, &p, r, codec, cat.v); typ != "Catalog" {
			t.Errorf("compress=%v: catalog /Type = %q", compress, typ)
		}
		// Walk to the page: every hop crosses an emitted reference.
		pagesV, err := p.Deref(r, mustGet(t, &p, r, codec, cat.v, "Pages"), codec)
		if err != nil {
			t.Fatal(err)
		}
		kids := mustGet(t, &p, r, codec, pagesV, "Kids")
		var kid Value
		if err := codec.ArrayForEach(&p, r, kids, func(v Value) bool { kid = v; return false }); err != nil {
			t.Fatal(err)
		}
		pageV, err := p.Deref(r, kid, codec)
		if err != nil {
			t.Fatal(err)
		}
		if typ := mustType(t, &p, r, codec, pageV); typ != "Page" {
			t.Errorf("compress=%v: page /Type = %q", compress, typ)
		}
		// The content stream decodes back to the payload: /Length was indirect
		// and — when compressed — counted post-compression bytes.
		cv, err := p.Resolve(r, contentID, codec)
		if err != nil {
			t.Fatal(err)
		}
		rd, err := p.OpenStream(r, cv, codec)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rd)
		if err != nil {
			t.Fatalf("compress=%v: reading content stream: %v", compress, err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("compress=%v: content = %q, want %q", compress, got, payload)
		}
		// The title exercises String escaping through a real document.
		title, err := dictText(codec, &p, r, mustGet(t, &p, r, codec, p.Trailer(), "Info"), "Title")
		if err == nil && string(title) != "piudf test" {
			t.Errorf("compress=%v: /Title = %q", compress, title)
		}
		// Object 0 is the free-list head the fresh table must name.
		ent, err := p.Lookup(r, 0, codec)
		if err != nil {
			t.Fatalf("compress=%v: Lookup(0): %v", compress, err)
		}
		if ent.Kind != XrefFree {
			t.Errorf("compress=%v: object 0 Kind = %v, want free", compress, ent.Kind)
		}
	}
}

// appendShadow appends a revision to base that shadows its catalog with a
// rewritten copy carrying one extra key, then checks the output decodes to the
// new catalog while everything unshadowed keeps its exact coordinates. This is
// the editing story end to end, and the second writer-made revision the
// durability test could never have without an encoder.
func appendShadow(t *testing.T, path string) {
	r, size := openCounted(t, path)
	codec := newCodecCfg(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})
	var base PDF
	if err := base.Decode(r, size, codec); err != nil {
		t.Fatal(err)
	}
	rootID := mustGet(t, &base, r, codec, base.Trailer(), "Root").ObjectID()
	cat, err := base.Resolve(r, rootID, codec)
	if err != nil {
		t.Fatal(err)
	}
	// Collect first, emit after: the one-lexer rule — CopyValue lexes nothing,
	// but loadObjStm inside it can, so nothing is copied mid-walk.
	type kv struct {
		key string
		v   Value
	}
	var kept []kv
	err = codec.DictForEach(&base, r, cat, func(k []byte, v Value) bool {
		kept = append(kept, kv{string(k), v})
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	basePages := mustGet(t, &base, r, codec, cat, "Pages")

	var out bytes.Buffer
	var e Encoder
	if err := e.ResetAppend(&out, make([]byte, 4096), r, size, &base, codec); err != nil {
		t.Fatal(err)
	}
	if id := e.NewID(); int64(id.Num) < mustTrailerSize(t, &base, r, codec) {
		t.Errorf("NewID allocated %d, inside base /Size", id.Num)
	}
	e.BeginObject(rootID)
	e.DictOpen()
	for _, kv := range kept {
		e.Name(kv.key)
		e.CopyValue(&base, r, codec, kv.v)
	}
	e.Name("PiudfMarker")
	e.Int(42)
	e.DictClose()
	e.EndObject()
	if err := e.Close(rootID, ObjectID{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes()[:size], readAll(t, r, size)) {
		t.Fatal("append modified the copied base bytes")
	}

	outR := bytes.NewReader(out.Bytes())
	codec2 := newCodecCfg(DecoderConfig{Buffer: make([]byte, 4096), MaxLazySections: 4096, MaxDepth: 32})
	var got PDF
	if err := got.Decode(outR, int64(out.Len()), codec2); err != nil {
		t.Fatalf("decoding appended output: %v", err)
	}
	if got.NumRevisions() != base.NumRevisions()+1 {
		t.Errorf("NumRevisions = %d, want %d", got.NumRevisions(), base.NumRevisions()+1)
	}
	cat2, err := got.Resolve(outR, rootID, codec2)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := mustGet(t, &got, outR, codec2, cat2, "PiudfMarker").Int()
	if !ok || marker != 42 {
		t.Errorf("shadowed catalog marker = %v %v, want 42: shadowing did not take", marker, ok)
	}
	if typ := mustType(t, &got, outR, codec2, cat2); typ != "Catalog" {
		t.Errorf("copied /Type = %q, want Catalog", typ)
	}
	// The unshadowed world is untouched: /Pages resolves through the OLD
	// revisions, so its Value coordinates must match the base's exactly.
	gotPages := mustGet(t, &got, outR, codec2, cat2, "Pages")
	if gotPages.Tok == piulex.TokR && basePages.Tok == piulex.TokR {
		a, err := base.Resolve(r, basePages.ObjectID(), codec)
		if err != nil {
			t.Fatal(err)
		}
		b, err := got.Resolve(outR, gotPages.ObjectID(), codec2)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Errorf("unshadowed /Pages moved: base %+v, output %+v", a, b)
		}
	}

	// Finalize turns the base index into the output's in place; from here on
	// base reads against outR, and must agree with the fresh decode.
	if err := e.Finalize(&base); err != nil {
		t.Fatal(err)
	}
	if base.NumRevisions() != got.NumRevisions() {
		t.Errorf("finalized NumRevisions = %d, fresh decode says %d", base.NumRevisions(), got.NumRevisions())
	}
	cat3, err := base.Resolve(outR, rootID, codec)
	if err != nil {
		t.Fatalf("resolving through finalized index: %v", err)
	}
	if n, ok := mustGet(t, &base, outR, codec, cat3, "PiudfMarker").Int(); !ok || n != 42 {
		t.Errorf("finalized index resolves old catalog: marker %v %v", n, ok)
	}
	if a, b := mustTrailerSize(t, &base, outR, codec), mustTrailerSize(t, &got, outR, codec2); a != b {
		t.Errorf("finalized trailer /Size %d, fresh decode %d", a, b)
	}
	if err := e.Finalize(&base); err == nil {
		t.Error("second Finalize: no error")
	}
}

func readAll(t *testing.T, r io.ReaderAt, size int64) []byte {
	t.Helper()
	b := make([]byte, size)
	if _, err := r.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestEncoderAppendClassic shadows into a classic-table document with many
// revisions already in its chain.
func TestEncoderAppendClassic(t *testing.T) {
	appendShadow(t, "../testdata/sto.pdf")
}

// TestEncoderAppendXrefStream shadows into a 1.5 document: the appended
// classic table's /Prev points at a cross-reference stream (a hybrid chain),
// and the catalog's entries live in object streams, so CopyValue reads spans
// out of decompressed space.
func TestEncoderAppendXrefStream(t *testing.T) {
	appendShadow(t, "../testdata/rp2350-datasheet.pdf")
}

// TestEncoderRewriteWithinRevision writes the same object twice before Close:
// the later body must win, since one table names each object once and the log
// dedups toward the last write.
func TestEncoderRewriteWithinRevision(t *testing.T) {
	var out bytes.Buffer
	var e Encoder
	if err := e.Reset(&out, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	root := e.NewID()
	e.BeginObject(root)
	e.DictOpen()
	e.Name("Type")
	e.Name("Catalog")
	e.Name("Draft")
	e.Int(1)
	e.DictClose()
	e.EndObject()
	e.BeginObject(root) // Rewritten: the first body is now dead bytes.
	e.DictOpen()
	e.Name("Type")
	e.Name("Catalog")
	e.Name("Final")
	e.Int(2)
	e.DictClose()
	e.EndObject()
	if err := e.Close(root, ObjectID{}); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(out.Bytes())
	codec := newCodec(make([]byte, 4096))
	var p PDF
	if err := p.Decode(r, int64(out.Len()), codec); err != nil {
		t.Fatal(err)
	}
	v, err := p.Resolve(r, root, codec)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := mustGet(t, &p, r, codec, v, "Final").Int(); !ok || n != 2 {
		t.Errorf("rewritten object resolved /Final = %v %v; first write won", n, ok)
	}
	if d, err := codec.DictGet(&p, r, v, "Draft"); err != nil || !d.IsNull() {
		t.Errorf("rewritten object still shows /Draft (%v, %v)", d, err)
	}
}

// TestEncoderMisuse pins the state machine: every out-of-sequence call must
// latch rather than corrupt output.
func TestEncoderMisuse(t *testing.T) {
	newEnc := func() *Encoder {
		var e Encoder
		if err := e.Reset(io.Discard, make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
		return &e
	}
	e := newEnc()
	e.BeginObject(ObjectID{Num: 1})
	e.BeginObject(ObjectID{Num: 2}) // Nested object.
	if e.Err() == nil {
		t.Error("nested BeginObject: no error")
	}
	e = newEnc()
	e.EndObject() // No object open.
	if e.Err() == nil {
		t.Error("EndObject while idle: no error")
	}
	e = newEnc()
	e.BeginObject(ObjectID{}) // Object number 0 is the free-list head.
	if e.Err() == nil {
		t.Error("BeginObject(0): no error")
	}
	e = newEnc()
	e.BeginObject(ObjectID{Num: 1})
	e.DictOpen()
	e.EndObject() // Unbalanced dict.
	if e.Err() == nil {
		t.Error("EndObject with open dict: no error")
	}
	e = newEnc()
	if err := e.Close(ObjectID{}, ObjectID{}); err == nil { // A PDF needs /Root.
		t.Error("Close without root: no error")
	}
	e = newEnc()
	e.BeginObject(ObjectID{Num: 1})
	if err := e.Close(ObjectID{Num: 1}, ObjectID{}); err == nil {
		t.Error("Close with object open: no error")
	}
	e = newEnc()
	if _, err := e.PayloadWriter().Write([]byte("x")); err == nil {
		t.Error("payload write outside stream: no error")
	}
}
