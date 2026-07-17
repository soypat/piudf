package ppdf

import (
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unsafe"

	"github.com/soypat/piudf/piulex"
)

//go:generate go tool stringer -type=genericErr -linecomment -output=stringers.go

type genericErr uint8

const (
	_                     genericErr = iota
	ErrMissingHeader                 // missing %PDF- header
	ErrMissingStartXref              // missing startxref
	ErrMalformedStartXref            // malformed startxref
	ErrIncompleteReadAt              // ReadAt did incomplete read
	ErrOOBXref                       // xref offset out of file bounds
	ErrCodecMemLimit                 // Codec memory limit hit
	ErrCodecDepthLimit               // Codec depth limit hit
	ErrInvalidCodecConfig            // Codec invalid config
	errTODO                          // PDF feature not implemented yet
	errValueMismatch                 // value kind/type mismatch
	errNameTooLong                   // name longer than implementation limit
	errObjectNotFound                // object number outside every xref section
	errObjectIDMismatch              // object header does not match its xref entry
	errNotAStream                    // value is not a stream object
	errStreamBadLength               // stream /Length missing or out of bounds
	errObjStmNested                  // object stream within an object stream
	errObjStmBad                     // object stream /N, /First or offset out of bounds
	errEncoderState                  // Encoder method called out of sequence
	errEncoderBase                   // Encoder base document unusable for append
	// Lexer errors below.
	_errLexErrorsStart              //
	errXrefStreamBad                // xref stream generic error
	errXrefStreamMissingDict        // xref stream missing dict
	errXrefStreamMissingTok         // xref stream missing 'stream' keyword
	ErrIllegalToken                 // illegal token
	errUnexpectedToken              // unexpected token
	ErrPrevLoop                     // /Prev loop found
	errPrevChainTooLong             // /Prev chain too long
	errExpectedXref                 // expected 'xref' keyword
	errBadSubsectionStart           // bad subsection start/entry
	errExpectingSubsectionOrTrailer // unexpected token looking for subsections or trailer
	errBadXrefStreamObjectHeader    // bad xref stream object header
	errBadObjectHeader              // bad 'N G obj' object header
	errUnexpectedEOF                // unexpected end of file
)

func (g genericErr) Error() string { return g.String() }

// IsLexed signals the error was encountered during lexing, so the [piulex.Lexer]
// has state on where the error was encountered in Pos and Error methods.
func (g genericErr) IsLexed() bool { return g > _errLexErrorsStart }

type PDF struct {
	sections []xrefSection
	revs     []Revision
	recbuf   [classicRecLen]byte

	// Error information.
	errd DecodeError
}

// DecodeError is the error of the last failed decode, decorated with where in
// the file it was found. It is a field of PDF, not a value callers allocate:
// setError fills it in place, so failing costs no allocation.
type DecodeError struct {
	// err is a non-genericErr cause, i.e. an io error out of ReadAt. Held so
	// the file position still reaches the caller.
	err  error
	errG genericErr
	// errPos is the file offset the error was found at; zero means unknown.
	// Offset 0 is the header, whose errors are recorded at 1 instead — no
	// error worth reporting sits before the first byte, so the ambiguity
	// costs nothing and a flag is avoided.
	errPos piulex.Pos
}

func (de *DecodeError) Error() string {
	msg := "ppdf: "
	switch {
	case de.errG != 0:
		msg += de.errG.String()
	case de.err != nil:
		msg += de.err.Error()
	default:
		return msg + "no error"
	}
	if de.errPos != 0 {
		// Offsets are the currency of this package; an error without one
		// sends the reader hunting through megabytes.
		msg += " at offset 0x" + strconv.FormatInt(int64(de.errPos), 16)
	}
	return msg
}

// Unwrap exposes the cause so errors.Is matches the package's error
// constants through the decoration.
func (de *DecodeError) Unwrap() error {
	if de.err != nil {
		return de.err
	}
	return de.errG
}

// Position returns the file offset the error was found at, or zero when
// unknown. For an error raised while lexing this is where the lexer stood,
// which is at or just past the offending token.
func (de *DecodeError) Position() piulex.Pos { return de.errPos }

// Err returns the error of the last decode, or nil. The returned error stays
// valid until the next failing call on pdf.
func (p *PDF) Err() error {
	if p.errd.errG == 0 && p.errd.err == nil {
		return nil
	}
	return &p.errd
}

// setError records err against the file position codec's lexer stands at and
// returns it decorated. err is optional: a nil err clears the recorded error
// and returns nil, so a call site can route both outcomes through it. A nil
// codec records the error without a position — before Decode has a lexer to
// ask, there is none.
//
// The result aliases pdf's own storage: it is overwritten by the next
// setError, and callers who need it to outlive that must copy it.
func (pdf *PDF) setError(codec *Codec, err error) error {
	pdf.errd = DecodeError{}
	if err == nil {
		return nil
	}
	if de, ok := err.(*DecodeError); ok {
		// Already decorated by a nested call: keep the position it was found
		// at rather than the one the lexer has since moved to.
		pdf.errd = *de
		return &pdf.errd
	}
	if g, ok := err.(genericErr); ok {
		pdf.errd.errG = g
	} else {
		pdf.errd.err = err
	}
	if codec != nil {
		pdf.errd.errPos = max(1, codec.lex.Pos())
	}
	return &pdf.errd
}

// xrefSection describes one cross-reference subsection as coordinates only;
// no entry is ever materialized here. Classic on-disk tables are already
// random-access arrays of fixed-width records, so lookups read a single
// record via ReadAt. A cross-reference stream instead records where its
// encoded payload lives and how it is encoded, leaving the decode to
// whoever resolves an object. Memory cost is O(number of subsections).
type xrefSection struct {
	firstObj uint32
	count    uint32
	// fileOff is the file offset of the subsection's first record when
	// classic, or of the encoded stream payload when isXrefStream. Payload
	// bytes are addressed the same way as everything else in this package:
	// by file coordinate.
	fileOff int64
	length  int64 // isXrefStream: encoded payload length, from /Length.
	// rowFirst is the subsection's first row within the *decoded* payload.
	// Subsections of one stream share a payload and index it cumulatively,
	// so this is a row number and not any kind of offset.
	rowFirst uint32
	codec    streamCodec // isXrefStream: how to decode the payload.

	isXrefStream bool
}

// XrefEntry is the decoded cross-reference record of one object: where the
// object is, as the newest revision mentioning it says. It is a result, never
// state — nothing here stores one, which is what separates it from
// [xrefSection]: O(objects) of these is the whole index in memory, while
// O(subsections) of those is 144 bytes.
type XrefEntry struct {
	// Offset is the file offset of the object's "N G obj" header when Kind is
	// XrefNormal, or the object's index within Stream when XrefCompressed.
	// One field, because Kind says which it is and the two never coexist.
	Offset int64
	// Stream is the object stream holding the object, when XrefCompressed.
	Stream uint32
	// ID is the object number looked up, with the generation recorded for it.
	ID ObjectID
	// Kind discriminates the fields above.
	Kind XrefKind
}

// streamCodec is the decoded /W, /Filter and /DecodeParms of an internal
// (xref or object) stream: everything needed to turn its payload back into
// rows. Only FlateDecode, optionally behind a predictor, is valid on one. The
// zero value decodes nothing; see readCodec for the defaults the format
// mandates.
type streamCodec struct {
	// w is /W, the row layout: three big-endian field widths. It is a decode
	// parameter like the ones below, and a classic table has no equivalent —
	// its 10/5/1 record shape is fixed by ISO 32000-1 7.5.4 and lives in
	// parseClassicRecord, not in any field.
	w         [3]uint8
	columns   uint16
	predictor uint8
	colors    uint8
	bpc       uint8
	flate     bool
}

// rowLen returns the byte length of one decoded row.
func (sc streamCodec) rowLen() int64 {
	return int64(sc.w[0]) + int64(sc.w[1]) + int64(sc.w[2])
}

// XrefKind is what a cross-reference record says about an object. The values
// are the keywords a classic table spells them with.
type XrefKind uint8

const (
	// XrefFree is an object not present in the file. A reference to one is a
	// reference to null (ISO 32000-1 7.3.10).
	XrefFree XrefKind = 'f'
	// XrefCompressed is an object inside an object stream.
	XrefCompressed XrefKind = 'c'
	// XrefNormal is an object at a file offset of its own.
	XrefNormal XrefKind = 'n'
)

func (k XrefKind) String() string {
	switch k {
	case XrefFree:
		return "free"
	case XrefCompressed:
		return "compressed"
	case XrefNormal:
		return "normal"
	}
	return "invalid"
}

// Revision is one step of the cross-reference chain as recorded by Decode.
// Revisions are ordered newest first: index 0 is the revision startxref
// points at, and each following one is its /Prev.
type Revision struct {
	trailerOff int64
	// XrefOffset is the file offset of the 'xref' keyword when Classic, or of
	// the cross-reference stream's object header otherwise.
	XrefOffset int64
	// FirstSection indexes this revision's first cross-reference subsection.
	FirstSection int
	// Classic reports a 7.5.4 table rather than a 1.5 cross-reference stream.
	Classic bool
}

// Trailer is the revision's trailer dictionary. A cross-reference stream
// has no separate trailer: its own dictionary carries the trailer keys.
func (rev Revision) Trailer() Value {
	// Length is unknown and unneeded: accessors lex a dictionary from its
	// opening bracket and stop at the matching close.
	return Value{Tok: tokDict, I: rev.trailerOff}
}

// NumRevisions returns how many cross-reference chain steps Decode walked.
func (pdf *PDF) NumRevisions() int { return len(pdf.revs) }

// Revision returns chain step i, newest first. It panics if i is out of range.
func (pdf *PDF) Revision(i int) Revision {
	return pdf.revs[i]
}

// Trailer returns the newest revision's trailer dictionary, the entry point
// to the document: /Root, /Info, /Size and /Encrypt live there.
func (pdf *PDF) Trailer() Value {
	if len(pdf.revs) == 0 {
		return Value{Tok: piulex.TokNull}
	}
	return pdf.Revision(0).Trailer()
}

// NumXrefSections returns the number of cross-reference subsections recorded
// across every revision.
func (pdf *PDF) NumXrefSections() int { return len(pdf.sections) }

// SizeOnRAM returns the bytes pdf holds: the struct plus the backing arrays of
// its slices, counted at capacity since that is what is actually retained.
// The document's own bytes are not among them — a PDF is coordinates, so this
// grows with the number of cross-reference subsections and revisions and not
// with the file's size. It excludes the Codec, whose arena is caller-owned.
func (pdf *PDF) SizeOnRAM() int {
	return int(unsafe.Sizeof(*pdf)) +
		cap(pdf.sections)*int(unsafe.Sizeof(xrefSection{})) +
		cap(pdf.revs)*int(unsafe.Sizeof(Revision{}))
}

// XrefCacheSize returns the [DecoderConfig.XrefCache] bytes that would hold
// every cross-reference row of pdf, so a caller can size the buffer from the
// document rather than guess at it. It is zero when the document has no
// cross-reference stream: a classic table is random-access on disk and is read
// a record at a time, so nothing is cached and nothing needs to be.
//
// The figure is what buying O(1) lookups costs, and buying them is optional —
// see [DecoderConfig.XrefCache]. A smaller buffer is not refused; it caches
// what it covers.
func (pdf *PDF) XrefCacheSize() int {
	var rows, rowlen int64
	for i := range pdf.sections {
		s := &pdf.sections[i]
		if !s.isXrefStream {
			continue
		}
		// Sections of one stream index a shared payload cumulatively, and the
		// cursor caches one stream at a time, so the answer is the widest row
		// over the most rows any one of them reaches.
		rows = max(rows, int64(s.rowFirst)+int64(s.count))
		rowlen = max(rowlen, s.codec.rowLen())
	}
	return int(rows * rowlen)
}

func (pdf *PDF) Reset() {
	pdf.sections = pdf.sections[:0]
	pdf.revs = pdf.revs[:0]
}

// Decode walks the cross-reference chain of the document r, recording where
// every object can later be found. No object is decoded and no file bytes are
// retained; see [PDF.SizeOnRAM] for what is.
//
// On failure the returned error carries the file offset it was found at, and
// [PDF.Err] returns it again until the next decode.
func (pdf *PDF) Decode(r io.ReaderAt, size int64, codec *Codec) error {
	// One funnel: nothing lexes while an error unwinds, so the lexer still
	// stands where it failed and every return below decorates identically.
	return pdf.setError(codec, pdf.decode(r, size, codec))
}

func (pdf *PDF) decode(r io.ReaderAt, size int64, codec *Codec) error {
	err := codec.validate()
	if err != nil {
		return err
	}
	buf := codec.buf
	pdf.Reset()
	// A reused Codec still holds the previous decode's scratch: pushed-back
	// tokens, the two stream cursors, and — the one that silently corrupts —
	// the lexer window's resident bytes. The header and tail reads below land
	// straight in buf, which is the window's fill buffer, so the window's
	// base/n would still describe bytes those reads have overwritten. Reset
	// forgets all of it so this decode starts as a fresh Codec would.
	codec.Reset()
	header := buf[:5]
	n, err := readAtFull(r, header, 0)
	if err != nil {
		return err
	} else if !bequal(header, "%PDF-") {
		return ErrMissingHeader
	}

	// Look for startxref string.
	readlim := min(size, int64(len(buf)))
	tail := buf[:readlim]
	n, err = readAtFull(r, tail, size-readlim)
	if err != nil {
		return err
	}
	tail = tail[:n]
	const startxref = "startxref"
	i := blastidx(tail, startxref)
	if i < 0 {
		return ErrMissingStartXref
	}
	i += len(startxref)
	i += countWhitespace(tail[i:])
	off, n, err := consumeInt(tail[i:])
	i += n
	if err != nil {
		return ErrMalformedStartXref
	}
	// Walk the xref chain starting at startxref.
	const maxXrefChain = 64
	for range maxXrefChain {
		if off < 0 || off >= size {
			return ErrOOBXref
		}
		pdf.revs = append(pdf.revs, Revision{XrefOffset: off, FirstSection: len(pdf.sections)})
		prev, err := pdf.decodeXrefTable(r, off, codec)
		if err != nil {
			return err
		} else if prev == 0 {
			return nil // Done!
		} else if prev == off {
			return ErrPrevLoop
		}
		off = prev
	}
	return errPrevChainTooLong
}

// classicRecLen is the record length mandated by ISO 32000-1 7.5.4:
// 10-digit offset, space, 5-digit generation, space, keyword, 2-byte EOL.
const classicRecLen = 20

func (pdf *PDF) decodeXrefTable(r io.ReaderAt, off int64, codec *Codec) (prev int64, err error) {
	lx := &codec.lex
	if err = codec.lexAt(r, off); err != nil {
		return 0, err
	}
	tok, _, _ := lx.NextToken()
	if tok == piulex.TokInt {
		// An "N G obj" header here means a cross-reference stream.
		return pdf.decodeXrefStream(r, off, codec)
	}
	if tok != piulex.TokXref {
		return 0, errExpectedXref
	}
	for len(pdf.sections) < codec.maxLazySections {
		tok, _, lit := lx.NextToken()
		switch tok {
		case piulex.TokTrailer:
			// The dictionary follows the keyword; record it so callers can
			// read /Root and /Info without rewalking the chain.
			trailerOff := int64(lx.Pos())
			if n := len(pdf.revs); n > 0 {
				pdf.revs[n-1].trailerOff = trailerOff
				pdf.revs[n-1].Classic = true
			}
			return codec.dictPrev(pdf, r, Value{Tok: tokDict, I: trailerOff})
		case piulex.TokInt:
			first, _, err := consumeInt(lit)
			if err != nil {
				return 0, errBadSubsectionStart
			}
			tok, _, lit = lx.NextToken()
			count, _, err := consumeInt(lit)
			if err != nil {
				return 0, errBadSubsectionStart
			}
			// Advance lexer position to start of record.
			lx.SkipWhitespace()
			recOff := int64(lx.Pos())
			if count > 0 {
				sec := xrefSection{
					firstObj:     uint32(first),
					count:        uint32(count),
					fileOff:      recOff,
					isXrefStream: false,
				}
				pdf.sections = append(pdf.sections, sec)
			}
			// Jump over the fixed-width record area instead of tokenizing it.
			if err := codec.lexAt(r, recOff+classicRecLen*count); err != nil {
				return 0, err
			}
		default:
			return 0, errExpectingSubsectionOrTrailer
		}
	}
	return 0, ErrCodecMemLimit
}

func (pdf *PDF) decodeXrefStream(r io.ReaderAt, off int64, codec *Codec) (prev int64, err error) {
	if err = codec.lexAt(r, off); err != nil {
		return 0, err
	}
	codec.nextValue(piulex.TokInt, errBadXrefStreamObjectHeader) // Object number.
	codec.nextValue(piulex.TokInt, errBadXrefStreamObjectHeader) // Generation.
	codec.nextValue(piulex.TokObj, errBadXrefStreamObjectHeader)
	if codec.accumErr != nil {
		return 0, codec.accumErr
	}
	dictV, err := codec.decodeShallow()
	if err != nil {
		return 0, err
	} else if dictV.Tok != tokDict {
		return 0, errXrefStreamMissingDict
	}
	if n := len(pdf.revs); n > 0 {
		pdf.revs[n-1].trailerOff = dictV.I // the stream dict is the trailer.
	}
	codec.nextValue(piulex.TokStream, errXrefStreamMissingTok)
	if codec.accumErr != nil {
		return 0, codec.accumErr
	}
	// The payload span must be taken before any DictGet below: those re-lex
	// the dictionary and move the lexer off the stream keyword.
	dataStart, err := codec.lex.StreamDataStart()
	if err != nil {
		return 0, err
	}
	// Scalar entries; ISO 32000-1 7.5.8.2 requires them to be direct. That is
	// not a convenience: an xref stream whose /Length were an indirect
	// reference could not be read without the xref it defines.
	size := codec.dictGetAccum(pdf, r, dictV, "Size", piulex.TokInt)
	length := codec.dictGetAccum(pdf, r, dictV, "Length", piulex.TokInt)
	wV := codec.dictGetAccum(pdf, r, dictV, "W", tokArray)
	if codec.accumErr != nil {
		return 0, codec.accumErr
	} else if size.I <= 0 || size.I > math.MaxUint32 || length.I < 0 {
		return 0, errXrefStreamBad
	}
	sc, err := codec.readCodec(pdf, r, dictV)
	if err != nil {
		return 0, err
	}
	// /W is read into sc separately: readCodec serves object streams too, and
	// only a cross-reference stream has a row layout.
	sc.w, err = codec.readWidths(pdf, r, wV)
	if err != nil {
		return 0, err
	}
	// The payload stays on disk. Every subsection of this stream shares these
	// coordinates; /Index below varies only the object and row ranges.
	proto := xrefSection{
		fileOff:      int64(dataStart),
		length:       length.I,
		codec:        sc,
		isXrefStream: true,
	}
	if err = pdf.appendStreamSections(r, codec, dictV, proto, uint32(size.I)); err != nil {
		return 0, err
	}
	return codec.dictPrev(pdf, r, dictV)
}

// readWidths reads the three /W field widths, which are the row layout of a
// cross-reference stream's records.
func (codec *Codec) readWidths(pdf *PDF, r io.ReaderAt, wV Value) (w [3]uint8, err error) {
	codec.auxcounter = 0
	err = codec.ArrayForEach(pdf, r, wV, func(v Value) bool {
		n, ok := v.Int()
		ok = ok && n >= 0 && n <= 8 && codec.auxcounter < len(w)
		if !ok {
			codec.accumErr = errXrefStreamBad
			return false
		}
		w[codec.auxcounter] = uint8(n)
		codec.auxcounter++
		return true
	})
	if err != nil {
		return w, err
	} else if codec.accumErr != nil {
		return w, codec.accumErr
	} else if codec.auxcounter != len(w) || w[1] == 0 {
		// /W is exactly three widths, and field 2 always carries the offset:
		// a zero there leaves every record pointing at byte 0.
		return w, errXrefStreamBad
	}
	return w, nil
}

// appendStreamSections records one section per /Index pair, defaulting to the
// single range [0, /Size). proto carries the payload coordinates shared by
// every subsection of the stream; only the object and row ranges differ.
func (pdf *PDF) appendStreamSections(r io.ReaderAt, codec *Codec, dictV Value, proto xrefSection, size uint32) error {
	idxV, err := codec.DictGet(pdf, r, dictV, "Index")
	if err != nil {
		return err
	}
	if idxV.IsNull() {
		proto.firstObj, proto.count, proto.rowFirst = 0, size, 0
		return pdf.appendSection(codec, proto)
	} else if !idxV.IsArray() {
		return errXrefStreamBad
	}
	var first, rows uint32
	codec.auxcounter = 0
	err = codec.ArrayForEach(pdf, r, idxV, func(v Value) bool {
		n, ok := v.Int()
		if !ok || n < 0 || n > math.MaxUint32 {
			codec.accumErr = errXrefStreamBad
			return false
		}
		even := codec.auxcounter%2 == 0
		codec.auxcounter++
		if even {
			first = uint32(n)
			return true
		}
		count := uint32(n)
		if count == 0 {
			return true // Empty subsection: no rows, nothing to record.
		}
		proto.firstObj, proto.count, proto.rowFirst = first, count, rows
		rows += count
		codec.accumErr = pdf.appendSection(codec, proto)
		return codec.accumErr == nil
	})
	if err != nil {
		return err
	} else if codec.accumErr != nil {
		return codec.accumErr
	} else if codec.auxcounter%2 != 0 {
		return errXrefStreamBad // /Index is first/count pairs.
	}
	return nil
}

func (pdf *PDF) appendSection(codec *Codec, s xrefSection) error {
	if len(pdf.sections) >= codec.maxLazySections {
		return ErrCodecMemLimit
	}
	pdf.sections = append(pdf.sections, s)
	return nil
}

// Lookup returns the newest cross-reference record for object num. Unlike
// [PDF.Resolve] it does not touch the object itself, and it reports a free
// entry rather than failing on one.
func (pdf *PDF) Lookup(r io.ReaderAt, num uint32, codec *Codec) (XrefEntry, error) {
	e, err := pdf.lookupXref(r, num, codec)
	if err != nil {
		return XrefEntry{}, pdf.setError(codec, err)
	}
	return e, nil
}

func (p *PDF) lookupXref(r io.ReaderAt, num uint32, codec *Codec) (XrefEntry, error) {
	for i := range p.sections {
		s := &p.sections[i]
		if num < s.firstObj || num >= s.firstObj+s.count {
			continue
		}
		e, err := p.sectionEntry(r, codec, s, num)
		if err != nil {
			return XrefEntry{}, err
		}
		// The sections know rows, not object numbers; only the caller's num
		// completes the entry.
		e.ID.Num = num
		return e, nil
	}
	return XrefEntry{}, errObjectNotFound
}

// sectionEntry reads object num's record out of section s, whichever form it
// takes.
func (p *PDF) sectionEntry(r io.ReaderAt, codec *Codec, s *xrefSection, num uint32) (XrefEntry, error) {
	if s.isXrefStream {
		// A row index within the decoded payload, not a byte offset into the
		// file: subsections of one stream share a payload and index it
		// cumulatively from rowFirst.
		row := int64(s.rowFirst) + int64(num-s.firstObj)
		rec, err := codec.rows.at(r, s, row)
		if err != nil {
			return XrefEntry{}, err
		}
		return parseStreamRecord(rec, s.codec.w)
	}
	rec := p.recbuf[:classicRecLen]
	recOff := s.fileOff + classicRecLen*int64(num-s.firstObj)
	if _, err := readAtFull(r, rec, recOff); err != nil {
		return XrefEntry{}, err
	}
	return parseClassicRecord(rec)
}

// parseStreamRecord decodes one row of a cross-reference stream: up to
// three big-endian fields of the widths declared by /W. A zero-width type
// field defaults to type 1 (ISO 32000-1 7.5.8.3); unknown types read as
// free, which the spec equates with references to the null object.
func parseStreamRecord(rec []byte, w [3]uint8) (XrefEntry, error) {
	be := func(b []byte) int64 {
		var v int64
		for _, c := range b {
			v = v<<8 | int64(c)
		}
		return v
	}
	typ := int64(1)
	i := int(w[0])
	if i > 0 {
		typ = be(rec[:i])
	}
	f2 := be(rec[i : i+int(w[1])])
	f3 := be(rec[i+int(w[1]) : i+int(w[1])+int(w[2])])
	switch typ {
	case 1:
		if f3 < 0 || f3 > math.MaxUint16 {
			return XrefEntry{}, errBadXrefStreamObjectHeader
		}
		return XrefEntry{Kind: XrefNormal, Offset: f2, ID: ObjectID{Gen: uint16(f3)}}, nil
	case 2:
		if f2 <= 0 || f2 > math.MaxUint32 {
			return XrefEntry{}, errBadXrefStreamObjectHeader
		}
		// Offset is the index within the stream here; an object stream's
		// members all carry generation 0 (ISO 32000-1 7.5.8.3).
		return XrefEntry{Kind: XrefCompressed, Stream: uint32(f2), Offset: f3}, nil
	}
	return XrefEntry{Kind: XrefFree}, nil
}

// parseClassicRecord decodes a 20-byte "nnnnnnnnnn ggggg n" record.
func parseClassicRecord(rec []byte) (XrefEntry, error) {
	off, ok1 := parseInt(rec[0:10])
	gen, ok2 := parseInt(rec[11:16])
	kw := rec[17]
	if ok1 != nil || ok2 != nil {
		return XrefEntry{}, errors.Join(ok1, ok2)
	}
	e := XrefEntry{Offset: off, ID: ObjectID{Gen: uint16(gen)}, Kind: XrefNormal}
	if kw == 'f' {
		e.Kind = XrefFree
	}
	return e, nil
}

func readAtFull(r io.ReaderAt, b []byte, off int64) (int, error) {
	n, err := r.ReadAt(b, off)
	if err != nil {
		return n, err
	} else if n != len(b) {
		return n, ErrIncompleteReadAt
	}
	return n, nil
}

func bequal(b []byte, s string) bool {
	return b2s(b) == s
}

func blastidx(b []byte, needle string) int {
	return strings.LastIndex(b2s(b), needle)
}

func b2s(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func consumeInt(b []byte) (int64, int, error) {
	i := 0
	for i < len(b) && isDigit(b[i]) {
		i++
	}
	v, err := parseInt(b[:i])
	return v, i, err
}

func parseInt(b []byte) (int64, error) {
	return strconv.ParseInt(b2s(b), 10, 64)
}

func parseReal(b []byte) (float64, error) {
	return strconv.ParseFloat(b2s(b), 64)
}

func countWhitespace(b []byte) int {
	i := 0
	for i < len(b) && isWhitespace(b[i]) {
		i++
	}
	return i
}

// isWhitespace reports PDF whitespace bytes (ISO 32000-1 Table 1).
func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == 0
}

// isDelimiter reports PDF delimiter bytes (ISO 32000-1 Table 2).
func isDelimiter(b byte) bool {
	switch b {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

// isRegular reports bytes that may appear in names and bare keywords.
func isRegular(b byte) bool {
	return !isWhitespace(b) && !isDelimiter(b)
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

func hexVal(b byte) (v byte, ok bool) {
	switch {
	case '0' <= b && b <= '9':
		return b - '0', true
	case 'a' <= b && b <= 'f':
		return b - 'a' + 10, true
	case 'A' <= b && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}
