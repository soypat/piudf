package ppdf

import (
	"io"

	"github.com/soypat/piudf/internal"
	"github.com/soypat/piudf/piulex"
)

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
