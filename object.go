package ppdf

import (
	"io"

	"github.com/soypat/piudf/internal"
	"github.com/soypat/piudf/piulex"
)

// Resolve returns the value of indirect object id. Composite values come back
// as spans, so resolving a page dictionary reads its header and nothing else;
// the entries are lexed when asked for. A stream object resolves to its
// dictionary, from which [PDF.StreamPayload] locates the payload.
//
// A reference to a free or nonexistent object resolves to null, which is what
// ISO 32000-1 7.3.10 says such a reference means.
func (pdf *PDF) Resolve(r io.ReaderAt, id ObjectID, codec *Codec) (Value, error) {
	v, err := pdf.resolve(r, id, codec)
	if err != nil {
		return Value{}, pdf.setError(codec, err)
	}
	return v, nil
}

func (pdf *PDF) resolve(r io.ReaderAt, id ObjectID, codec *Codec) (Value, error) {
	e, err := pdf.lookupXref(r, id.Num, codec)
	if err != nil {
		return Value{}, err
	}
	switch e.Kind {
	case XrefFree:
		return Value{Tok: piulex.TokNull}, nil
	case XrefCompressed:
		return pdf.resolveCompressed(r, id, e.Stream, e.Offset, codec)
	}
	return codec.decodeObjectAt(r, e.Offset, id)
}

// Deref resolves v when it is an indirect reference and returns it unchanged
// otherwise, which is what most dictionary entries need: the format lets any
// value be indirect, so a caller that skips this reads /Length as "42 0 R".
func (pdf *PDF) Deref(r io.ReaderAt, v Value, codec *Codec) (Value, error) {
	if v.Tok != piulex.TokR {
		return v, nil
	}
	return pdf.Resolve(r, v.ObjectID(), codec)
}

// decodeObjectAt lexes the "N G obj" header at off and returns the object's
// value. The header is checked against want: an offset that lands on the
// wrong object means the cross-reference table disagrees with the file, and
// reading on would return another object's value under this one's name.
func (d *Codec) decodeObjectAt(r io.ReaderAt, off int64, want ObjectID) (Value, error) {
	if err := d.lexAt(r, off); err != nil {
		return Value{}, err
	}
	d.accumErr = nil
	num := d.nextValue(piulex.TokInt, errBadObjectHeader)
	gen := d.nextValue(piulex.TokInt, errBadObjectHeader)
	d.nextValue(tokObj, errBadObjectHeader)
	if d.accumErr != nil {
		return Value{}, d.accumErr
	}
	if uint32(num.I) != want.Num || uint16(gen.I) != want.Gen {
		return Value{}, errObjectIDMismatch
	}
	v, err := d.decodeShallow()
	if err != nil {
		return Value{}, err
	}
	if v.Tok != tokDict {
		return v, nil
	}
	// A stream object is a dictionary followed by the stream keyword. The
	// dictionary is the same span either way; only its tag differs, so that
	// StreamPayload can refuse what is not a stream.
	nt, nlit, err := d.nextRaw()
	if err != nil {
		return Value{}, err
	}
	if nt.Tok == tokStream {
		v.Tok = tokStream
		return v, nil
	}
	return v, d.unread(nt, nlit)
}

// StreamPayload locates the encoded bytes of stream object v and how they are
// encoded. Nothing is read or decoded: the payload is a file span like every
// other coordinate in this package.
type StreamPayload struct {
	// Offset is the file offset of the first payload byte, just past the
	// stream keyword's end-of-line.
	Offset int64
	// Length is the encoded length, from /Length. It is the length on disk,
	// not the length after decoding, which is unknown until decoded.
	Length int64
	codec  streamCodec
}

// objStm is the object stream a Codec currently reads through. Its data is an
// io.ReaderAt, so the lexer reads an object stream through the same window it
// reads the file through and knows no difference — the seeking that costs
// nothing in a file costs a decode here, which is why [internal.Stream] says
// so rather than hide it.
//
// One stream at a time: a second cursor would double the inflate windows to
// serve a case that does not arise, since resolving an object finishes with
// one stream before asking for the next.
type objStm struct {
	data internal.Stream
	// num is the object number of the stream, or zero when none is loaded.
	num uint32
	// n is /N, how many objects the stream holds, and first is /First, the
	// decoded offset where the first of them begins. Everything before first
	// is the pair table stmObjectAt walks.
	n     int64
	first int64
}

// Filtered reports whether the payload needs decoding to be read.
func (sp StreamPayload) Filtered() bool { return sp.codec.flate }

// StreamPayload returns where stream object v's payload lives. /Length may be
// an indirect reference — legal for every stream but the cross-reference ones
// — so this can resolve another object.
func (pdf *PDF) StreamPayload(r io.ReaderAt, v Value, codec *Codec) (StreamPayload, error) {
	sp, err := pdf.streamPayload(r, v, codec)
	if err != nil {
		return StreamPayload{}, pdf.setError(codec, err)
	}
	return sp, nil
}

func (pdf *PDF) streamPayload(r io.ReaderAt, v Value, codec *Codec) (StreamPayload, error) {
	if v.Tok != tokStream {
		return StreamPayload{}, errNotAStream
	}
	sc, err := codec.readCodec(pdf, r, v)
	if err != nil {
		return StreamPayload{}, err
	}
	lengthV, err := codec.DictGet(pdf, r, v, "Length")
	if err != nil {
		return StreamPayload{}, err
	}
	// The dict must be re-lexed after the DictGets above, which leave the
	// lexer wherever their scan ended. Resolving an indirect /Length moves it
	// out of this object entirely.
	if lengthV, err = pdf.Deref(r, lengthV, codec); err != nil {
		return StreamPayload{}, err
	}
	length, ok := lengthV.Int()
	if !ok || length < 0 {
		return StreamPayload{}, errStreamBadLength
	}
	if err = codec.lexValueSpan(pdf, r, v); err != nil {
		return StreamPayload{}, err
	}
	if _, err = codec.decodeShallow(); err != nil { // The dictionary.
		return StreamPayload{}, err
	}
	codec.accumErr = nil
	codec.nextValue(tokStream, errNotAStream)
	if codec.accumErr != nil {
		return StreamPayload{}, codec.accumErr
	}
	start, err := codec.lex.StreamDataStart()
	if err != nil {
		return StreamPayload{}, err
	}
	return StreamPayload{Offset: int64(start), Length: length, codec: sc}, nil
}

// OpenStream returns a reader over the decoded bytes of stream object v.
// Decoding is streamed: nothing is buffered, so a reader over a 4 MB content
// stream costs its inflate window and no more. The reader is valid until r is
// closed and does not depend on codec afterwards.
func (pdf *PDF) OpenStream(r io.ReaderAt, v Value, codec *Codec) (io.Reader, error) {
	sp, err := pdf.StreamPayload(r, v, codec)
	if err != nil {
		return nil, err
	}
	return pdf.OpenPayload(r, sp, codec)
}

// OpenPayload returns a reader over the decoded bytes of sp, for callers that
// already located it.
//
// The returned reader is the caller's: it owns an inflate window of its own
// rather than borrowing the Codec's, which is what lets it outlive the next
// call on codec. Callers reading many streams and holding none — the shape the
// package is built for — should reach for [PDF.Resolve] and friends instead,
// which reuse the Codec's cursors.
func (pdf *PDF) OpenPayload(r io.ReaderAt, sp StreamPayload, codec *Codec) (io.Reader, error) {
	if sp.codec.predictor > 1 {
		// TODO: predictors. A cross-reference stream needs them and reads rows
		// rather than bytes, which is xrefRows' business; a content stream
		// does not.
		return nil, pdf.setError(codec, errTODO)
	}
	s := new(internal.Stream)
	if err := s.Reset(r, sp.Offset, sp.Length, sp.codec.flate); err != nil {
		return nil, pdf.setError(codec, err)
	}
	return s, nil
}

// loadObjStm points codec's object stream cursor at object stream num, unless
// it is already there.
func (pdf *PDF) loadObjStm(r io.ReaderAt, num uint32, codec *Codec) error {
	stm := &codec.stm
	if stm.num == num && stm.data.Reader() == r {
		return nil
	}
	if codec.stmDepth > 0 {
		// Loading a stream while loading one would clobber the cursor
		// mid-read. ISO 32000-1 7.5.7 forbids the arrangement that asks for it:
		// an object stream cannot hold another.
		return errObjStmNested
	}
	codec.stmDepth++
	defer func() { codec.stmDepth-- }()

	sv, err := pdf.resolve(r, ObjectID{Num: num}, codec)
	if err != nil {
		return err
	}
	sp, err := pdf.streamPayload(r, sv, codec)
	if err != nil {
		return err
	}
	codec.accumErr = nil
	nV := codec.dictGetAccum(pdf, r, sv, "N", piulex.TokInt)
	firstV := codec.dictGetAccum(pdf, r, sv, "First", piulex.TokInt)
	if codec.accumErr != nil {
		return codec.accumErr
	}
	if nV.I < 0 || firstV.I < 0 || firstV.I > sp.Length*maxInflateRatio {
		return errObjStmBad
	}
	if sp.codec.predictor > 1 {
		// TODO: predictors on an object stream. They exist to make a table of
		// similar rows compress; an object stream is not one, and only a
		// cross-reference stream is (see xrefRows).
		return errTODO
	}
	// num is cleared first: a failed reset must not leave the cursor claiming
	// to hold a stream it does not.
	stm.num = 0
	if err = stm.data.Reset(r, sp.Offset, sp.Length, sp.codec.flate); err != nil {
		return err
	}
	stm.num, stm.n, stm.first = num, nV.I, firstV.I
	// The cursor's bytes at a given offset just changed, which the lexer's
	// window cannot see: it keys what it holds by reader identity, and the
	// cursor is the same reader it always was.
	codec.lex.DropBuffered()
	return nil
}

// maxInflateRatio bounds /First against the encoded length, so a corrupt value
// is caught before the cursor decodes its way to it. Flate cannot exceed
// 1032:1 on any input.
const maxInflateRatio = 1032

// stmObjectAt returns the decoded offset of the idx'th object of the loaded
// object stream, which must be object want. The stream begins with /N pairs of
// "object number, offset", so this walks the pair table: forward from the
// cursor's first byte, which is where it already stands.
func (pdf *PDF) stmObjectAt(codec *Codec, idx int64, want uint32) (int64, error) {
	stm := &codec.stm
	if idx < 0 || idx >= stm.n {
		// The xref places the object at an index its own stream denies.
		return 0, errObjStmBad
	}
	if err := codec.lexAt(&stm.data, 0); err != nil {
		return 0, err
	}
	codec.accumErr = nil
	var num, off Value
	for range idx + 1 {
		num = codec.nextValue(piulex.TokInt, errObjStmBad)
		off = codec.nextValue(piulex.TokInt, errObjStmBad)
	}
	if codec.accumErr != nil {
		return 0, codec.accumErr
	}
	if uint32(num.I) != want {
		return 0, errObjectIDMismatch
	}
	if off.I < 0 {
		return 0, errObjStmBad
	}
	return stm.first + off.I, nil
}

// resolveCompressed returns the value of object id, which lives at index idx
// of object stream stm. Members of an object stream have no "N G obj" header —
// the pair table is their header — and cannot themselves be streams
// (ISO 32000-1 7.5.7).
func (pdf *PDF) resolveCompressed(r io.ReaderAt, id ObjectID, stm uint32, idx int64, codec *Codec) (Value, error) {
	if err := pdf.loadObjStm(r, stm, codec); err != nil {
		return Value{}, err
	}
	off, err := pdf.stmObjectAt(codec, idx, id.Num)
	if err != nil {
		return Value{}, err
	}
	if err = codec.lexAt(&codec.stm.data, off); err != nil {
		return Value{}, err
	}
	v, err := codec.decodeShallow()
	if err != nil {
		return Value{}, err
	}
	// The value's coordinates address the stream's decoded data, not the file.
	// Everything reached through it inherits the tag; see tagObjStm.
	return tagObjStm(v, stm), nil
}
