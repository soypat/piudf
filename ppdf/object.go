package ppdf

import (
	"io"

	"github.com/soypat/piudf/ppdf/internal"
	"github.com/soypat/piudf/ppdf/piulex"
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
