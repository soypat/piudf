package ppdf

import (
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unsafe"

	"github.com/soypat/piudf/ppdf/piulex"
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
	errUnexpectedEOF                // unexpected end of file
)

func (g genericErr) Error() string { return g.String() }

// IsLexed signals the error was encountered during lexing, so the [piulex.Lexer]
// has state on where the error was encountered in Pos and Error methods.
func (g genericErr) IsLexed() bool { return g > _errLexErrorsStart }

type PDF struct {
	sections []xrefSection
	revs     []revInfo
	recbuf   [classicRecLen]byte
}

// xrefSection describes one cross-reference subsection without materializing
// its entries. Classic on-disk tables are already random-access arrays of
// fixed-width records, so lookups read single records via ReadAt; the
// decoded rows of a cross-reference stream live in PDF.xbuf and are read
// from memory. Memory cost is O(number of subsections) plus, for streams,
// the decoded rows.
type xrefSection struct {
	firstObj uint32
	count    uint32
	fileOff  int64    // sectClassic: file offset of the first record. sectStream: offset into PDF.xbuf.
	w        [3]uint8 // Field widths; classic is {10, 5, 1}, streams use /W.

	isXrefStream bool
}

// xrefRecord is a single decoded cross-reference entry.
type xrefRecord struct {
	offset int64 // recNormal: absolute object offset. recCompressed: index in stream.
	stream uint32
	gen    uint16
	kind   recordKind
}

type recordKind uint8

const (
	recordFree       = 'f'
	recordCompressed = 'c'
	recordNormal     = 'n'
)

// revInfo is one cross-reference chain step recorded during Decode.
type revInfo struct {
	xrefOff      int64
	trailerOff   int64
	firstSection int
	classic      bool
}

func (pdf *PDF) Reset() {
	pdf.sections = pdf.sections[:0]
	pdf.revs = pdf.revs[:0]
}

func (pdf *PDF) Decode(r io.ReaderAt, size int64, codec *Codec) error {
	err := codec.Validate()
	if err != nil {
		return err
	}
	buf := codec.buf
	// Literals are consumed before the next token is lexed, so one reused
	// buffer serves them all.
	codec.lex.ReuseLiteralBuffer = true
	codec.lex.MaxLiteral = len(buf)
	pdf.Reset()
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
		pdf.revs = append(pdf.revs, revInfo{xrefOff: off, firstSection: len(pdf.sections)})
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
	for len(pdf.sections) < codec.MaxLazySections {
		tok, _, lit := lx.NextToken()
		switch tok {
		case piulex.TokTrailer:
			// The dictionary follows the keyword; record it so callers can
			// read /Root and /Info without rewalking the chain.
			trailerOff := int64(lx.Pos())
			if n := len(pdf.revs); n > 0 {
				pdf.revs[n-1].trailerOff = trailerOff
				pdf.revs[n-1].classic = true
			}
			return pdf.trailerPrev(r, trailerOff, codec)
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
					w:            [3]uint8{10, 5, 1},
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

// trailerPrev returns the /Prev offset of the trailer dictionary starting at
// off. A trailer without /Prev is the oldest revision and ends the chain.
func (pdf *PDF) trailerPrev(r io.ReaderAt, off int64, codec *Codec) (prev int64, err error) {
	v, err := codec.DictGet(r, Value{Tok: tokDict, I: off}, "Prev")
	if err != nil {
		return 0, err
	}
	prev, ok := v.Int()
	if !ok {
		return 0, nil
	}
	return prev, nil
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
	_, err = codec.lex.StreamDataStart()
	if err != nil {
		return 0, err
	}
	// Scalar entries; ISO 32000-1 7.5.8.2 requires them to be direct.
	size := codec.dictGetAccum(r, dictV, "Size", piulex.TokInt)
	length := codec.dictGetAccum(r, dictV, "Length", piulex.TokInt)

	wV := codec.dictGetAccum(r, dictV, "W", tokArray)
	if codec.accumErr != nil {
		return 0, codec.accumErr
	}
	codec.auxcounter = 0
	var w [3]uint8
	err = codec.ArrayIterate(r, wV, func(v Value) bool {
		codec.auxcounter++
		n, ok := v.Int()
		ok = ok && n >= 0 && n <= 8 && codec.auxcounter < 3
		if ok {
			w[codec.auxcounter] = uint8(n)
		} else {
			codec.accumErr = errXrefStreamBad
		}
		return ok
	})
	if err != nil {
		return 0, err
	} else if codec.accumErr != nil {
		return 0, codec.accumErr
	}

	// TODO: decode the /W rows of the stream payload into sections and return
	// the dict's /Prev. Undecided: who owns the decompressed rows. They are
	// not random-access on disk like classic records, so something must hold
	// them; PDF must stay lazy, so not there.
	return 0, errTODO
}

func (codec *Codec) ArrayIterate(r io.ReaderAt, arrVal Value, push func(Value) bool) error {
	v := arrVal
	if !v.IsArray() {
		return errValueMismatch
	} else if err := codec.lexValueSpan(r, v); err != nil {
		return err
	}
	tok, _, _ := codec.lex.NextToken()
	if tok != piulex.TokArrayOpen {
		return errUnexpectedToken
	}
	for tok != piulex.TokArrayClose {
		nt, _, err := codec.nextRaw()
		if err != nil {
			return err
		}
		if !push(nt) {
			break
		}
		tok = nt.Tok
	}
	return nil
}

func (p *PDF) lookupXref(r io.ReaderAt, num uint32, codec *Codec) (xrefRecord, error) {
	rec := p.recbuf[:classicRecLen]
	for i := range p.sections {
		s := &p.sections[i]
		if num < s.firstObj || num >= s.firstObj+s.count {
			continue
		}
		if s.isXrefStream {
			// TODO: rows holds the section's decoded cross-reference stream
			// rows; who owns them is undecided. Not PDF, which stays lazy,
			// and not the Codec arena, which is scratch reused across calls.
			var rows []byte
			if rows == nil {
				return xrefRecord{}, errTODO
			}
			rowlen := int64(s.w[0]) + int64(s.w[1]) + int64(s.w[2])
			off := s.fileOff + rowlen*int64(num-s.firstObj)
			if off < 0 || off+rowlen > int64(len(rows)) {
				return xrefRecord{}, errXrefStreamBad // TODO fmt.Errorf("%w: xref stream row outside decoded data", ErrCorrupt)
			}
			return parseStreamRecord(rows[off:off+rowlen], s.w)
		}
		recOff := s.fileOff + classicRecLen*int64(num-s.firstObj)
		_, err := readAtFull(r, rec, recOff)
		if err != nil {
			return xrefRecord{}, err // TODO fmt.Errorf("piudf: reading xref record %d/%d at %#x: %w", n, len(rec), recOff, err)
		}
		return parseClassicRecord(rec, recOff)
	}
	return xrefRecord{}, errors.New("object not found")
}

// parseStreamRecord decodes one row of a cross-reference stream: up to
// three big-endian fields of the widths declared by /W. A zero-width type
// field defaults to type 1 (ISO 32000-1 7.5.8.3); unknown types read as
// free, which the spec equates with references to the null object.
func parseStreamRecord(rec []byte, w [3]uint8) (xrefRecord, error) {
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
			return xrefRecord{}, errBadXrefStreamObjectHeader // TODO fmt.Errorf("%w: xref stream generation %d", ErrCorrupt, f3)
		}
		return xrefRecord{kind: recordNormal, offset: f2, gen: uint16(f3)}, nil
	case 2:
		if f2 <= 0 || f2 > math.MaxUint32 {
			return xrefRecord{}, errBadXrefStreamObjectHeader // TODO fmt.Errorf("%w: xref stream object stream number %d", ErrCorrupt, f2)
		}
		return xrefRecord{kind: recordCompressed, stream: uint32(f2), offset: f3}, nil
	}
	return xrefRecord{kind: recordFree}, nil
}

// parseClassicRecord decodes a 20-byte "nnnnnnnnnn ggggg n" record.
func parseClassicRecord(rec []byte, recOff int64) (xrefRecord, error) {
	off, ok1 := parseInt(rec[0:10])
	gen, ok2 := parseInt(rec[11:16])
	kw := rec[17]
	if ok1 != nil || ok2 != nil {
		return xrefRecord{}, errors.Join(ok1, ok2)
	}
	r := xrefRecord{offset: off, gen: uint16(gen)}
	if kw == 'f' {
		r.kind = recordFree
	} else {
		r.kind = recordNormal
	}
	return r, nil
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
