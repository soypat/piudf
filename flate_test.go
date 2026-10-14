package piudf

import (
	"bytes"
	"compress/zlib"
	"io"
	"testing"
)

// TestFlateWriter writes a compressed stream with an indirect /Length, reads it
// back through the decoder, and pins a warm FlateWriter allocating nothing.
func TestFlateWriter(t *testing.T) {
	payload := bytes.Repeat([]byte("piudf flate "), 500)
	var fw FlateWriter
	var out bytes.Buffer
	var enc Encoder
	scratch := make([]byte, 512)
	var id ObjectID
	write := func() {
		out.Reset()
		if err := enc.Reset(&out, scratch); err != nil {
			t.Fatal(err)
		}
		id, lenID := enc.NewID(), enc.NewID()
		enc.BeginObject(id)
		enc.DictOpen()
		enc.Name("Filter")
		enc.Name("FlateDecode")
		enc.Name("Length")
		enc.Ref(lenID.Num, lenID.Gen)
		enc.DictClose()
		enc.BeginStreamPayload()
		w, err := fw.Reset(enc.PayloadWriter())
		if err != nil {
			t.Fatal(err)
		}
		w.Write(payload)
		if err := fw.Close(); err != nil {
			t.Fatal(err)
		}
		n := enc.EndStreamPayload()
		enc.EndObject()
		enc.WriteLength(lenID, n)
		if err := enc.Err(); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if allocs := testing.AllocsPerRun(10, write); allocs > 0 {
		t.Errorf("FlateWriter allocates %.1f times once warm", allocs)
	}

	write()
	id = ObjectID{Num: 1}
	cat := enc.NewID()
	enc.BeginObject(cat)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("Catalog")
	enc.DictClose()
	enc.EndObject()
	if err := enc.Close(cat, ObjectID{}); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(out.Bytes())
	pdf, codec := decodeDoc(t, r, r.Size())
	v, err := pdf.Resolve(r, id, codec)
	if err != nil {
		t.Fatal(err)
	}
	var s Stream
	s.ConfigureDefault()
	rd, err := pdf.OpenStream(r, v, &s, codec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("read back %d bytes, want the %d written", len(got), len(payload))
	}

	fw.Level = 42
	if _, err := fw.Reset(io.Discard); err == nil {
		t.Error("Reset accepted level 42")
	}
	fw.Level = zlib.BestSpeed
	if _, err := fw.Reset(io.Discard); err != nil {
		t.Errorf("changing Level: %v", err)
	}
}
