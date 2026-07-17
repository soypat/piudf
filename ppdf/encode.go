package ppdf

import (
	"io"
	"math"
	"slices"

	"github.com/soypat/piudf/ppdf/piulex"
)

// pdfHeader opens a fresh document: the version marker and the conventional
// high-bit comment that keeps transfer tools treating the file as binary.
const pdfHeader = "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"

// maxClassicOff is the largest offset a classic 10-digit record can spell.
const maxClassicOff = 9999999999

// Encoder writes PDF documents: a fresh one from nothing, or an incremental
// update appended to a copy of an existing one. The second form is this
// package's whole editing story — PDF edits by shadowing, so "change object 3"
// is writing a new object 3 and a cross-reference table whose /Prev points at
// the old chain. The source file is never written; output always goes to a new
// writer, which is what keeps every source offset true in the output.
//
// The embedded [piulex.Emitter] provides the token methods object bodies are
// written with; the Encoder itself adds what tokens cannot know: object
// offsets, the write log, and the cross-reference machinery of [Encoder.Close].
// Errors are sticky through the shared emitter path — emit a whole document,
// check Close.
type Encoder struct {
	piulex.Emitter
	// entries is the write log: one XrefEntry per BeginObject, holding the
	// offset the object began at. It is exactly what Close turns into the
	// cross-reference table, and the only Encoder state that grows.
	entries []XrefEntry
	// nextNum is the allocation cursor of NewID: 1 for a fresh document, the
	// base's /Size when appending.
	nextNum uint32
	// prevXref is the base document's newest cross-reference offset, for the
	// trailer's /Prev; negative means fresh.
	prevXref int64
	// cur is the object being written; Num zero when idle.
	cur ObjectID
	// payload is the offset the open stream payload began at; negative when
	// no payload is open.
	payload int64
}

// Reset starts a fresh document on w, emitting the PDF header. buf is the
// emitter's write-through window, at least [piulex.MinEmitBuffer] bytes and
// owned by the caller. The write log's storage is retained across resets.
func (e *Encoder) Reset(w io.Writer, buf []byte) error {
	if err := e.Emitter.Reset(w, buf); err != nil {
		return err
	}
	e.entries = e.entries[:0]
	e.nextNum = 1
	e.prevXref = -1
	e.cur = ObjectID{}
	e.payload = -1
	e.Raw([]byte(pdfHeader))
	return e.Err()
}

// ResetAppend starts an incremental update: the base document's size bytes are
// copied from r to w verbatim, and everything written afterwards becomes a new
// revision shadowing it. base and codec locate the newest trailer's /Size —
// the first object number NewID may allocate — and /Prev. The source behind r
// is only read.
func (e *Encoder) ResetAppend(w io.Writer, buf []byte, r io.ReaderAt, size int64, base *PDF, codec *Codec) error {
	if base == nil || base.NumRevisions() == 0 || size <= 0 {
		return errEncoderBase
	}
	if err := e.Emitter.Reset(w, buf); err != nil {
		return err
	}
	// buf is borrowed for the copy: the emitter holds nothing yet, and
	// SetPos below accounts for the bytes that bypassed it.
	for off := int64(0); off < size; {
		n := min(int64(len(buf)), size-off)
		if _, err := readAtFull(r, buf[:n], off); err != nil {
			return err
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		off += n
	}
	if err := e.SetPos(size); err != nil {
		return err
	}
	e.entries = e.entries[:0]
	e.cur = ObjectID{}
	e.payload = -1
	e.prevXref = base.Revision(0).XrefOffset
	// /Size is what the trailer promises, but the sections are what the
	// document proves; allocation starts above both.
	var next int64
	sizeV, err := codec.DictGet(base, r, base.Trailer(), "Size")
	if err != nil {
		return err
	}
	if n, ok := sizeV.Int(); ok {
		next = n
	}
	for i := range base.sections {
		s := &base.sections[i]
		next = max(next, int64(s.firstObj)+int64(s.count))
	}
	if next <= 0 || next > math.MaxUint32 {
		return errEncoderBase
	}
	e.nextNum = uint32(next)
	// The base may not end in an end-of-line; one here keeps the first
	// appended object off whatever line %%EOF sits on.
	e.EOL()
	return e.Err()
}

// NewID allocates the next unused object number. Allocation is decoupled from
// emission so a reference graph of any shape can be written in one pass: a
// page may point at a content stream that is written after it.
func (e *Encoder) NewID() ObjectID {
	id := ObjectID{Num: e.nextNum}
	e.nextNum++
	return id
}

// BeginObject opens indirect object id, recording its offset in the write log
// and emitting the "N G obj" header. Passing an id from the base document is
// how an object is edited: the new body shadows the old one. The body between
// BeginObject and [Encoder.EndObject] is written with the emitter's token
// methods.
func (e *Encoder) BeginObject(id ObjectID) {
	if e.Err() != nil {
		return
	}
	if e.cur.Num != 0 || id.Num == 0 {
		e.Fail(errEncoderState)
		return
	}
	e.entries = append(e.entries, XrefEntry{Offset: e.Pos(), ID: id, Kind: XrefNormal})
	e.cur = id
	e.Int(int64(id.Num))
	e.Int(int64(id.Gen))
	e.Ident("obj")
	e.EOL()
}

// EndObject closes the open object with the endobj keyword. Dictionaries and
// arrays must be balanced and any stream payload ended.
func (e *Encoder) EndObject() {
	if e.cur.Num == 0 || e.payload >= 0 || e.Depth() != 0 {
		e.Fail(errEncoderState)
		return
	}
	e.EOL()
	e.Ident("endobj")
	e.EOL()
	e.cur = ObjectID{}
}

// BeginStreamPayload emits the stream keyword after the object's dictionary
// and starts counting payload bytes. The dictionary must already carry
// /Length: a direct integer when the caller knows the byte count, or a
// reference to a small integer object written after [Encoder.EndStreamPayload]
// reports it — output is written forward-only, so a length discovered late
// cannot be patched in, and an indirect /Length is PDF's own answer to that.
func (e *Encoder) BeginStreamPayload() {
	if e.cur.Num == 0 || e.payload >= 0 || e.Depth() != 0 {
		e.Fail(errEncoderState)
		return
	}
	e.EOL()
	e.Ident("stream")
	e.EOL()
	if e.Err() == nil {
		e.payload = e.Pos()
	}
}

// PayloadWriter returns the open stream payload as an io.Writer, so anything
// that writes — a flate.Writer for a compressed stream, an image encoder —
// can produce the payload without materializing it. Bytes pass through
// verbatim and count toward /Length.
func (e *Encoder) PayloadWriter() io.Writer {
	return payloadWriter{e}
}

type payloadWriter struct{ e *Encoder }

func (p payloadWriter) Write(b []byte) (int, error) {
	if p.e.payload < 0 {
		p.e.Fail(errEncoderState)
		return 0, errEncoderState
	}
	p.e.Raw(b)
	if err := p.e.Err(); err != nil {
		return 0, err
	}
	return len(b), nil
}

// EndStreamPayload emits the endstream keyword and returns how many payload
// bytes were written — the number an indirect /Length object must carry.
func (e *Encoder) EndStreamPayload() (n int64) {
	if e.payload < 0 {
		e.Fail(errEncoderState)
		return 0
	}
	n = e.Pos() - e.payload
	e.payload = -1
	e.EOL() // Not part of the payload: /Length counts to just before it.
	e.Ident("endstream")
	return n
}

// CopyValue emits v as read from the source document: scalars re-emit from
// the Value itself, and span values — names, strings, dictionaries, arrays,
// whether in the file or inside an object stream — copy their bytes verbatim,
// which is exact because a span is already PDF syntax. It is the edit
// primitive for "keep this entry as it was" when rewriting a dictionary.
//
// Stream objects are refused: a stream's payload is not part of its span, so
// copying one is a dictionary rewrite plus a payload copy, which only the
// caller can compose.
func (e *Encoder) CopyValue(pdf *PDF, r io.ReaderAt, codec *Codec, v Value) {
	if e.Err() != nil {
		return
	}
	switch v.Tok {
	case piulex.TokInt:
		e.Int(v.I)
		return
	case piulex.TokReal:
		e.Real(math.Float64frombits(uint64(v.I)))
		return
	case piulex.TokTrue, piulex.TokFalse:
		e.Bool(v.Tok == piulex.TokTrue)
		return
	case piulex.TokNull:
		e.Null()
		return
	case piulex.TokR:
		e.Ref(v.N, uint16(v.I))
		return
	}
	if !v.isSpan() || v.Tok == tokStream {
		e.Fail(errValueMismatch)
		return
	}
	src := r
	if v.Stm != 0 {
		if err := pdf.loadObjStm(r, v.Stm, codec); err != nil {
			e.Fail(err)
			return
		}
		src = &codec.stm.data
	}
	// Chunked through the stack: the emitter's buffer is its live window and
	// the codec's halves are live lexer windows, so neither can be scratch.
	var chunk [128]byte
	for off, rem := v.I, int64(v.N); rem > 0; {
		n := min(int64(len(chunk)), rem)
		if _, err := readAtFull(src, chunk[:n], off); err != nil {
			e.Fail(err)
			return
		}
		e.Raw(chunk[:n])
		off += n
		rem -= n
	}
}

// Close writes the cross-reference table, trailer, startxref and %%EOF, then
// flushes; it returns the first error of the whole encode. root is the
// document catalog — required, since a PDF without /Root opens nowhere — and
// a zero info omits /Info. The table is classic (7.5.4): universally readable,
// and legal even appended to a document whose own tables are streams, since
// /Prev simply points at whatever form the older revision took.
func (e *Encoder) Close(root, info ObjectID) error {
	if e.cur.Num != 0 || e.payload >= 0 || e.Depth() != 0 || root.Num == 0 {
		e.Fail(errEncoderState)
	}
	if err := e.Err(); err != nil {
		return err
	}
	// The log becomes the table: ordered by object number, later write of the
	// same number winning — one table names each object once.
	slices.SortStableFunc(e.entries, func(a, b XrefEntry) int {
		return int(a.ID.Num) - int(b.ID.Num)
	})
	w := 0
	for i := range e.entries {
		if w > 0 && e.entries[w-1].ID.Num == e.entries[i].ID.Num {
			w--
		}
		e.entries[w] = e.entries[i]
		w++
	}
	e.entries = e.entries[:w]

	xrefOff := e.Pos()
	e.Ident("xref")
	e.EOL()
	// Subsections are runs of consecutive object numbers. A fresh document's
	// table also names object 0, the head of the free list, which exists on
	// disk only — hence the virtual index -1 rather than a log entry.
	num := func(i int) int64 {
		if i < 0 {
			return 0
		}
		return int64(e.entries[i].ID.Num)
	}
	i := 0
	if e.prevXref < 0 {
		i = -1
	}
	for i < len(e.entries) {
		j := i
		for j+1 < len(e.entries) && num(j+1) == num(j)+1 {
			j++
		}
		e.Int(num(i))
		e.Int(int64(j - i + 1))
		e.EOL()
		for k := i; k <= j; k++ {
			var rec [classicRecLen]byte
			if k < 0 {
				e.classicRecord(&rec, 0, math.MaxUint16, 'f')
			} else {
				ent := &e.entries[k]
				e.classicRecord(&rec, ent.Offset, ent.ID.Gen, 'n')
			}
			e.Raw(rec[:])
		}
		i = j + 1
	}

	size := int64(e.nextNum)
	if n := len(e.entries); n > 0 {
		size = max(size, num(n-1)+1)
	}
	e.Ident("trailer")
	e.EOL()
	e.DictOpen()
	e.Name("Size")
	e.Int(size)
	e.Name("Root")
	e.Ref(root.Num, root.Gen)
	if info.Num != 0 {
		e.Name("Info")
		e.Ref(info.Num, info.Gen)
	}
	if e.prevXref >= 0 {
		e.Name("Prev")
		e.Int(e.prevXref)
	}
	e.DictClose()
	e.EOL()
	e.Ident("startxref")
	e.EOL()
	e.Int(xrefOff)
	e.EOL()
	e.Raw([]byte("%%EOF"))
	e.EOL()
	return e.Flush()
}

// classicRecord formats one 20-byte 7.5.4 record: 10-digit offset, 5-digit
// generation, keyword, CRLF.
func (e *Encoder) classicRecord(rec *[classicRecLen]byte, off int64, gen uint16, kw byte) {
	if off < 0 || off > maxClassicOff {
		// A 10 GB offset needs a cross-reference stream, which this Encoder
		// does not write yet.
		e.Fail(errTODO)
		off = 0
	}
	putPadInt(rec[0:10], off)
	rec[10] = ' '
	putPadInt(rec[11:16], int64(gen))
	rec[16] = ' '
	rec[17] = kw
	rec[18] = '\r'
	rec[19] = '\n'
}

// putPadInt right-aligns v in dst with leading zeros, the fixed-width form
// classic records require. Callers guarantee v fits.
func putPadInt(dst []byte, v int64) {
	for i := len(dst) - 1; i >= 0; i-- {
		dst[i] = byte('0' + v%10)
		v /= 10
	}
}
