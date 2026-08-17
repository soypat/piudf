package piulex

import (
	"errors"
	"io"
	"math"
	"strconv"
	"unsafe"
)

var (
	errNilWriter    = errors.New("piulex: nil writer")
	errEmitBalance  = errors.New("piulex: close without matching open")
	errEmitIdent    = errors.New("piulex: ident with non-regular byte")
	errEmitRealForm = errors.New("piulex: real is NaN, infinite or out of PDF range")
	errEmitSetPos   = errors.New("piulex: SetPos with bytes buffered")
)

// MinEmitBuffer is the smallest buffer [Emitter.Reset] accepts: enough for the
// longest scalar token an emission formats in place.
const MinEmitBuffer = 32

// Emitter writes PDF syntax tokens to an io.Writer: the write-side mirror of
// [Lexer]. It owns the two things token concatenation needs and nothing more —
// whether a separating space is required, which the lexer's regular-byte
// boundary rule decides, and how deeply dictionaries and arrays nest. Objects,
// offsets and cross-reference bookkeeping are a caller's business (see
// ppdf.Encoder); a bare Emitter over any io.Writer is how a content stream is
// built.
//
// Errors are sticky: the first failure latches, every later call no-ops, and
// [Emitter.Err] or the final [Emitter.Flush] reports it. That lets a caller
// emit a whole structure and check once.
type Emitter struct {
	w   io.Writer
	buf []byte // Caller-owned; batches writes and hosts strconv.Append*.
	n   int    // Bytes buffered.
	off int64  // Absolute output offset of buf[0]; Pos() = off + n.
	// depth counts open dictionaries and arrays. The emitter cannot know what
	// balance a caller means, only that a close below zero matches nothing.
	depth int
	// needSep records that the last emitted byte was a regular character, so
	// the next token starting with one must be preceded by a space or the two
	// would lex as a single token.
	needSep bool
	err     error
}

// Reset binds the emitter to w with buf as its write-through window. buf must
// be at least [MinEmitBuffer] bytes; larger batches more per Write call and
// bounds nothing else.
func (e *Emitter) Reset(w io.Writer, buf []byte) error {
	if w == nil {
		return errNilWriter
	} else if len(buf) < MinEmitBuffer {
		return io.ErrShortBuffer
	}
	*e = Emitter{w: w, buf: buf}
	return nil
}

// Pos returns the absolute output offset of the next byte emitted — what an
// object writer records as each object's coordinate.
func (e *Emitter) Pos() int64 { return e.off + int64(e.n) }

// Depth returns how many emitted dictionary and array opens are unclosed.
func (e *Emitter) Depth() int { return e.depth }

// Err returns the first error any emission hit, or nil.
func (e *Emitter) Err() error { return e.err }

// Flush writes buffered bytes through to w. It is safe at any point: buffered
// bytes are always a valid prefix of the output.
func (e *Emitter) Flush() error {
	e.flush()
	return e.err
}

func (e *Emitter) flush() {
	if e.err != nil || e.n == 0 {
		return
	}
	n, err := e.w.Write(e.buf[:e.n])
	if err == nil && n != e.n {
		err = io.ErrShortWrite
	}
	e.off += int64(n)
	e.n -= n
	if err != nil {
		e.err = err
	}
}

// Int emits a decimal integer token.
func (e *Emitter) Int(v int64) {
	e.sep()
	if e.err != nil {
		return
	}
	e.rawint(v)
	e.needSep = true
}

// Real emits a real number token. PDF forbids exponent notation, so the value
// is written in fixed form at shortest round-trip precision; NaN, infinities
// and magnitudes outside PDF's real range (Annex C: ~±3.4e38) are errors, and
// magnitudes too small to mean anything in any PDF unit emit as 0 rather than
// as hundreds of leading zeros.
func (e *Emitter) Real(f float64) {
	if e.err == nil && (f != f || math.Abs(f) > math.MaxFloat32) {
		e.err = errEmitRealForm
		return
	}
	if math.Abs(f) < 1e-20 {
		f = 0
	}
	// Fixed 'f' form of any value passing the guards above fits here: at most
	// sign + 39 integer digits, or "0." + 19 zeros + 17 significant digits.
	var a [48]byte
	b := strconv.AppendFloat(a[:0], f, 'f', -1, 64)
	e.sep()
	e.raw(b)
	e.needSep = true
}

// NameNum emits a name-integer composite name i.e: "F1"
func (e *Emitter) NameNum(s string, num int64) {
	e.Name(s)
	e.rawint(num)
}

// Name emits /s, escaping as #xx every byte the lexer would not read back as
// part of a name.
func (e *Emitter) Name(s string) {
	e.wbyte('/')
	const hexdig = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		b := s[i]
		if isRegular(b) && b != '#' {
			e.wbyte(b)
			continue
		}
		e.wbyte('#')
		e.wbyte(hexdig[b>>4])
		e.wbyte(hexdig[b&0xf])
	}
	e.needSep = true
}

// String emits (s) as a literal string. Backslash and both parentheses are
// escaped, and so are CR and NUL: the lexer normalizes end-of-line inside a
// string and PDF whitespace includes NUL, so escaping is what makes the bytes
// round-trip exactly.
func (e *Emitter) String(s string) {
	e.wbyte('(')
	for i := range len(s) {
		b := s[i]
		switch b {
		case '\\', '(', ')':
			e.wbyte('\\')
			e.wbyte(b)
		case '\r':
			e.wbyte('\\')
			e.wbyte('r')
		case 0:
			e.wbyte('\\')
			e.wbyte('0')
			e.wbyte('0')
			e.wbyte('0')
		default:
			e.wbyte(b)
		}
	}
	e.wbyte(')')
	e.needSep = false
}

// StringBytes wraps [Emitter.String] with a bytes value.
func (e *Emitter) StringBytes(s []byte) {
	e.String(unsafe.String(unsafe.SliceData(s), len(s)))
}

// TextString emits s as a PDF text string: a literal string while every byte is
// ASCII, and UTF-16BE hex behind a U+FEFF byte order mark otherwise when argument is utf8 non-ascii encoded.
func (e *Emitter) TextString(s string) {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			e.utf16String(s)
			return
		}
	}
	e.String(s)
}

// utf16String emits s as UTF-16BE hex behind the byte order mark, a code unit at
// a time so that no buffer is needed to hold the transcoding.
func (e *Emitter) utf16String(s string) {
	e.wbyte('<')
	e.whex16(0xfeff)
	for _, r := range s {
		if r < 0x10000 {
			e.whex16(uint16(r))
			continue
		}
		r -= 0x10000
		e.whex16(uint16(0xd800 + r>>10))
		e.whex16(uint16(0xdc00 + r&0x3ff))
	}
	e.wbyte('>')
	e.needSep = false
}

// whex16 emits one UTF-16 code unit as four hex digits, high byte first.
func (e *Emitter) whex16(v uint16) {
	const hexdig = "0123456789abcdef"
	e.wbyte(hexdig[v>>12])
	e.wbyte(hexdig[v>>8&0xf])
	e.wbyte(hexdig[v>>4&0xf])
	e.wbyte(hexdig[v&0xf])
}

// HexString emits <hex digits of s>.
func (e *Emitter) HexString(s []byte) {
	const hexdig = "0123456789abcdef"
	e.wbyte('<')
	for _, b := range s {
		e.wbyte(hexdig[b>>4])
		e.wbyte(hexdig[b&0xf])
	}
	e.wbyte('>')
	e.needSep = false
}

// Bool emits true or false.
func (e *Emitter) Bool(v bool) {
	if v {
		e.Ident("true")
	} else {
		e.Ident("false")
	}
}

// Null emits null.
func (e *Emitter) Null() { e.Ident("null") }

// Ref emits the indirect reference "num gen R". Raw integers rather than an
// ObjectID, which is a ppdf type this package cannot see.
func (e *Emitter) Ref(num uint32, gen uint16) {
	e.Int(int64(num))
	e.Int(int64(gen))
	e.Ident("R")
}

// DictOpen emits << and pushes nesting.
func (e *Emitter) DictOpen() {
	e.rawstr("<<")
	e.depth++
	e.needSep = false
}

// DictClose emits >> and pops nesting.
func (e *Emitter) DictClose() {
	e.close()
	e.rawstr(">>")
}

// ArrayOpen emits [ and pushes nesting.
func (e *Emitter) ArrayOpen() {
	e.wbyte('[')
	e.depth++
	e.needSep = false
}

// ArrayClose emits ] and pops nesting.
func (e *Emitter) ArrayClose() {
	e.close()
	e.wbyte(']')
}

func (e *Emitter) close() {
	if e.depth == 0 && e.err == nil {
		e.err = errEmitBalance
		return
	}
	e.depth--
	e.needSep = false
}

// Ident emits a bare identifier: content-stream operators (BT, Tf, re) and
// structural keywords (obj, stream, xref). Every byte must be regular, or the
// identifier could not lex back as one token.
func (e *Emitter) Ident(s string) {
	for i := 0; i < len(s); i++ {
		if !isRegular(s[i]) {
			if e.err == nil {
				e.err = errEmitIdent
			}
			return
		}
	}
	e.sep()
	e.rawstr(s)
	e.needSep = true
}

// EOL emits a newline: layout the grammar requires (after obj, around stream
// payloads) rather than a token.
func (e *Emitter) EOL() {
	e.wbyte('\n')
	e.needSep = false
}

// Raw copies b to the output verbatim: payload bytes, cross-reference records
// and pre-formed syntax, where an injected space would corrupt the output. No
// separator is emitted before b — gluing b onto a preceding token is the
// caller's responsibility — but the separator state after is exact, taken from
// b's actual last byte.
func (e *Emitter) Raw(b []byte) {
	e.raw(b)
	if len(b) > 0 {
		e.needSep = isRegular(b[len(b)-1])
	}
}

// Fail latches err as the emitter's sticky error when none is latched yet.
// It is how a writer built on the Emitter routes its own failures through the
// one error path every emission already checks.
func (e *Emitter) Fail(err error) {
	if e.err == nil && err != nil {
		e.err = err
	}
}

// SetPos declares the absolute output offset of the next byte, for a writer
// appending to output that already holds pos bytes. It is only meaningful
// while nothing is buffered — immediately after [Emitter.Reset] — and errors
// otherwise, since buffered bytes already have positions.
func (e *Emitter) SetPos(pos int64) error {
	if e.n != 0 {
		return errEmitSetPos
	}
	e.off = pos
	return nil
}

func (e *Emitter) sep() {
	if e.needSep {
		e.wbyte(' ')
		e.needSep = false
	}
}

func (e *Emitter) wbyte(b byte) {
	if e.err != nil {
		return
	}
	if e.n == len(e.buf) {
		e.flush()
		if e.err != nil {
			return
		}
	}
	e.buf[e.n] = b
	e.n++
}

func (e *Emitter) rawint(v int64) {
	if len(e.buf)-e.n < MinEmitBuffer {
		e.flush()
		if e.err != nil {
			return
		}
	}
	// Appending into the buffer's own free space: capacity is clamped so a
	// miscount would fail loudly rather than reallocate behind the caller.
	b := strconv.AppendInt(e.buf[e.n:e.n:len(e.buf)], v, 10)
	e.n += len(b)
}

func (e *Emitter) raw(b []byte) {
	for e.err == nil && len(b) > 0 {
		if e.n == len(e.buf) {
			e.flush()
			continue
		}
		n := copy(e.buf[e.n:], b)
		e.n += n
		b = b[n:]
	}
}

func (e *Emitter) rawstr(s string) {
	for e.err == nil && len(s) > 0 {
		if e.n == len(e.buf) {
			e.flush()
			continue
		}
		n := copy(e.buf[e.n:], s)
		e.n += n
		s = s[n:]
	}
}
