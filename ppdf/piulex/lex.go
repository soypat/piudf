package piulex

import (
	"bytes"
	"errors"
	"io"
	"strconv"

	"github.com/soypat/piudf/ppdf/internal"
)

var ErrMemoryLimit = errors.New("piulex: mem limit exceed")

const defaultLitBuflen = 512

// Lexer tokenizes the structural (non-payload) regions of a PDF file.
// It is modeled on a byte-oriented lookahead lexer: PDF whitespace and
// delimiters are defined as single bytes (ISO 32000-1 7.2), so no UTF-8
// decoding happens at the token layer. The lexer reads from an io.ReaderAt
// and can be Reset to any absolute offset, which is how the parser jumps to
// xref-provided object offsets and past stream payloads.
//
// This Lexer is a faithful adaptation of github.com/soypat/lexer's pato.Lexer implementation.
type Lexer struct {
	w       internal.Window
	pos     int64 // Absolute file offset of ch.
	ch      byte  // Current byte, a copy: window refills do not invalidate it.
	chvalid bool  // ch holds a byte read from the file.
	idbuf   []byte
	err     error

	// ReuseLiteralBuffer makes NextToken reuse the literal buffer between
	// calls: returned literals are only valid until the next NextToken.
	// This bounds lexer memory to the largest single literal.
	ReuseLiteralBuffer bool
	// MaxLiteral bounds the byte length of a single literal (name, string,
	// number). Exceeding it fails the token with ErrMemoryLimit. <=0 means
	// unlimited.
	MaxLiteral int
}

// Pos returns the absolute byte offset of the current byte.
func (l *Lexer) Pos() Pos { return Pos(l.pos) }

// Err returns the lexer error, or nil if the input merely ended.
func (l *Lexer) Err() error {
	if l.err != nil {
		return l.err
	}
	return l.w.Err()
}

// IsDone returns true if the lexer has no current byte, because the input
// ended or a read failed.
func (l *Lexer) IsDone() bool { return !l.chvalid }

// Reset points the lexer at absolute offset off of r, reading the file
// through buf. A nil buf keeps the buffer of a previous Reset, or has the
// lexer allocate a default-sized one on first use; a larger buf spans more of
// the file per read and bounds nothing else, as literals build in a separate
// buffer capped by MaxLiteral.
//
// Bytes buffered by an earlier Reset over the same r and buf are reused, so a
// jump landing inside the window costs no read. ReuseLiteralBuffer, MaxLiteral
// and both buffers survive across resets. See [internal.Window.Reset] for the
// requirement on r's dynamic type.
func (l *Lexer) Reset(r io.ReaderAt, off int64, buf []byte) error {
	if r == nil {
		return errors.New("piulex: nil reader")
	} else if off < 0 {
		return errors.New("piulex: negative offset")
	}
	l.w.Reset(r, buf)
	l.err = nil
	l.pos = off
	l.ch, l.chvalid = l.w.ByteAt(off)
	return l.w.Err()
}

func (l *Lexer) advance() {
	l.pos++
	l.ch, l.chvalid = l.w.ByteAt(l.pos)
}

// peekIs reports whether the byte after the current one is c.
func (l *Lexer) peekIs(c byte) bool {
	b, ok := l.w.ByteAt(l.pos + 1)
	return ok && b == c
}

// NextToken returns the next token, its starting byte offset, and its
// literal value. Returns TokEOF at end of input, TokIllegal on errors; call
// Err for the failure cause.
func (l *Lexer) NextToken() (tok Token, start Pos, literal []byte) {
	l.SkipWhitespace() // Skip early, not after tokenizing: more intuitive lexer behavior.
	start = l.Pos()
	if !l.chvalid {
		if l.err == nil {
			l.err = l.w.Err()
		}
		if l.err == nil || l.err == io.EOF {
			return TokEOF, start, nil
		}
		return TokIllegal, start, nil
	}
	ok := true
	switch ch := l.ch; {
	case ch == '/':
		literal, ok = l.readName()
		tok = TokName
	case ch == '(':
		literal, ok = l.readLiteralString()
		tok = TokString
	case ch == '<':
		if l.peekIs('<') {
			l.advance()
			l.advance()
			return TokDictOpen, start, nil
		}
		literal, ok = l.readHexString()
		tok = TokHexString
	case ch == '>':
		if l.peekIs('>') {
			l.advance()
			l.advance()
			return TokDictClose, start, nil
		}
		l.errAt(start, "unexpected '>'")
		return TokIllegal, start, nil
	case ch == '[':
		l.advance()
		return TokArrayOpen, start, nil
	case ch == ']':
		l.advance()
		return TokArrayClose, start, nil
	case ch == '{':
		l.advance()
		return TokBraceOpen, start, nil
	case ch == '}':
		l.advance()
		return TokBraceClose, start, nil
	case ch == '+' || ch == '-' || ch == '.' || isDigit(ch):
		literal, tok, ok = l.readNumber()
	case isRegular(ch):
		literal, ok = l.readIdent()
		tok = Lookup(string(literal)) // Optimized by compiler to not allocate.
	default:
		l.errAt(start, "unexpected byte")
		return TokIllegal, start, nil
	}
	if !ok {
		return TokIllegal, start, literal
	}
	return tok, start, literal
}

// StreamDataStart returns the absolute offset of the first stream payload
// byte. Call immediately after NextToken returned TokStream: the spec
// requires the keyword be followed by CRLF or LF (lone CR tolerated).
func (l *Lexer) StreamDataStart() (Pos, error) {
	switch {
	case l.chvalid && l.ch == '\n':
		return Pos(l.pos + 1), nil
	case l.chvalid && l.ch == '\r':
		if l.peekIs('\n') {
			return Pos(l.pos + 2), nil
		}
		return Pos(l.pos + 1), nil
	}
	return 0, &SyntaxError{Off: l.Pos(), Msg: "expected EOL after 'stream' keyword"}
}

func (l *Lexer) SkipWhitespace() {
	for l.chvalid {
		switch {
		case isWhitespace(l.ch):
			l.advance()
		case l.ch == '%':
			// Comment runs to end of line. %PDF and %%EOF markers are
			// located by raw scans, never through the lexer.
			for l.chvalid && l.ch != '\n' && l.ch != '\r' {
				l.advance()
			}
		default:
			return
		}
	}
}

// readName lexes a /Name token with #xx hex escapes decoded.
func (l *Lexer) readName() (lit []byte, ok bool) {
	start := l.bufstart()
	nameStart := l.Pos()
	l.advance() // consume '/'
	for l.chvalid && isRegular(l.ch) {
		b := l.ch
		if b == '#' {
			l.advance()
			hi, okhi := hexVal(l.ch)
			if !okhi || !l.chvalid {
				l.errAt(nameStart, "bad hex escape in name")
				return l.idbuf[start:], false
			}
			l.advance()
			lo, oklo := hexVal(l.ch)
			if !oklo || !l.chvalid {
				l.errAt(nameStart, "bad hex escape in name")
				return l.idbuf[start:], false
			}
			b = hi<<4 | lo
		}
		if !l.appendLit(start, b) {
			return l.idbuf[start:], false
		}
		l.advance()
	}
	return l.idbuf[start:], true
}

// readLiteralString lexes a (string) token, decoding escapes and balancing
// nested parentheses. End-of-line markers inside the string normalize to \n.
func (l *Lexer) readLiteralString() (lit []byte, ok bool) {
	start := l.bufstart()
	strStart := l.Pos()
	l.advance() // consume '('
	depth := 1
	for l.chvalid {
		b := l.ch
		switch b {
		case '\\':
			l.advance()
			if !l.chvalid {
				break // EOF inside escape; loop exits and errors below.
			}
			esc := l.ch
			switch esc {
			case 'n':
				b = '\n'
			case 'r':
				b = '\r'
			case 't':
				b = '\t'
			case 'b':
				b = '\b'
			case 'f':
				b = '\f'
			case '\r':
				// Line continuation: backslash + EOL is dropped.
				l.advance()
				if l.chvalid && l.ch == '\n' {
					l.advance()
				}
				continue
			case '\n':
				l.advance()
				continue
			default:
				if esc >= '0' && esc <= '7' {
					// Up to three octal digits.
					v := esc - '0'
					l.advance()
					for range 2 {
						if !l.chvalid || l.ch < '0' || l.ch > '7' {
							break
						}
						v = v<<3 | (l.ch - '0')
						l.advance()
					}
					if !l.appendLit(start, v) {
						return l.idbuf[start:], false
					}
					continue
				}
				// Unknown escape: backslash is ignored, byte kept as-is.
				b = esc
			}
			if !l.appendLit(start, b) {
				return l.idbuf[start:], false
			}
			l.advance()
		case '(':
			depth++
			if !l.appendLit(start, b) {
				return l.idbuf[start:], false
			}
			l.advance()
		case ')':
			depth--
			l.advance()
			if depth == 0 {
				return l.idbuf[start:], true
			}
			if !l.appendLit(start, b) {
				return l.idbuf[start:], false
			}
		case '\r':
			// EOL inside string normalizes to \n; \r\n counts as one.
			l.advance()
			if l.chvalid && l.ch == '\n' {
				l.advance()
			}
			if !l.appendLit(start, '\n') {
				return l.idbuf[start:], false
			}
		default:
			if !l.appendLit(start, b) {
				return l.idbuf[start:], false
			}
			l.advance()
		}
	}
	l.errAt(strStart, "unterminated string")
	return l.idbuf[start:], false
}

// readHexString lexes a <hex> token into decoded bytes. Whitespace between
// digits is allowed; an odd final digit is padded with zero.
func (l *Lexer) readHexString() (lit []byte, ok bool) {
	start := l.bufstart()
	strStart := l.Pos()
	l.advance() // consume '<'
	var hi byte
	havehi := false
	for l.chvalid {
		b := l.ch
		if b == '>' {
			l.advance()
			if havehi {
				if !l.appendLit(start, hi<<4) {
					return l.idbuf[start:], false
				}
			}
			return l.idbuf[start:], true
		}
		if isWhitespace(b) {
			l.advance()
			continue
		}
		v, okv := hexVal(b)
		if !okv {
			l.errAt(strStart, "bad hex digit in hex string")
			return l.idbuf[start:], false
		}
		if havehi {
			if !l.appendLit(start, hi<<4|v) {
				return l.idbuf[start:], false
			}
			havehi = false
		} else {
			hi, havehi = v, true
		}
		l.advance()
	}
	l.errAt(strStart, "unterminated hex string")
	return l.idbuf[start:], false
}

// readNumber lexes an integer or real literal as raw text.
func (l *Lexer) readNumber() (lit []byte, tok Token, ok bool) {
	start := l.bufstart()
	numStart := l.Pos()
	tok = TokInt
	digits := 0
	if l.ch == '+' || l.ch == '-' {
		if !l.appendLit(start, l.ch) {
			return l.idbuf[start:], tok, false
		}
		l.advance()
	}
	for l.chvalid && (isDigit(l.ch) || l.ch == '.') {
		if l.ch == '.' {
			if tok == TokReal {
				break // Second dot terminates the number.
			}
			tok = TokReal
		} else {
			digits++
		}
		if !l.appendLit(start, l.ch) {
			return l.idbuf[start:], tok, false
		}
		l.advance()
	}
	if digits == 0 && tok == TokInt {
		l.errAt(numStart, "sign without digits")
		return l.idbuf[start:], tok, false
	}
	return l.idbuf[start:], tok, true
}

// readIdent lexes a run of regular bytes (bare keyword candidates).
// A truncated identifier fails the token: silently returning a prefix
// would corrupt keyword recognition downstream.
func (l *Lexer) readIdent() (lit []byte, ok bool) {
	start := l.bufstart()
	for l.chvalid && isRegular(l.ch) {
		if !l.appendLit(start, l.ch) {
			return l.idbuf[start:], false
		}
		l.advance()
	}
	return l.idbuf[start:], true
}

// appendLit appends b to the literal being built, honoring MaxLiteral.
func (l *Lexer) appendLit(litStart int, b byte) bool {
	if l.MaxLiteral > 0 && len(l.idbuf)-litStart >= l.MaxLiteral {
		if l.err == nil {
			l.err = ErrMemoryLimit
		}
		return false
	}
	l.idbuf = append(l.idbuf, b)
	return true
}

func (l *Lexer) bufstart() int {
	if l.ReuseLiteralBuffer {
		l.idbuf = l.idbuf[:0]
		return 0
	}
	free := cap(l.idbuf) - len(l.idbuf)
	if l.idbuf == nil || cap(l.idbuf) >= defaultLitBuflen && free < 32 {
		l.idbuf = make([]byte, 0, defaultLitBuflen) // Make sure to not grow needlessly on appends.
	}
	return len(l.idbuf)
}

func (l *Lexer) errAt(pos Pos, msg string) {
	if l.err == nil || l.err == io.EOF {
		l.err = &SyntaxError{Off: pos, Msg: msg}
	}
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

// Pos is an absolute byte offset into the PDF source.
type Pos int64

// String returns the offset formatted as "@0x<hex>".
func (pos Pos) String() string {
	var buf [7 + 3]byte
	return string(pos.AppendString(buf[:0]))
}

func (pos Pos) AppendString(dst []byte) []byte {
	dst = append(dst, "@0x"...)
	dst = strconv.AppendInt(dst, int64(pos), 16)
	return dst
}

// ToLineCol converts a byte offset to 1-indexed line:column for diagnostics.
// Also returns the length of the line containing the offset. aux is a scratch
// buffer used for reading; its size determines read chunk size (1024B
// recommended). PDF is a binary format so line:col is only meaningful for
// the textual structural regions, which is where lexing errors occur.
func (pos Pos) ToLineCol(r io.ReaderAt, aux []byte) (line, col, lineLength int, err error) {
	offset := int(pos)
	if r == nil || offset < 0 {
		return 0, 0, 0, errors.New("invalid reader or offset")
	}

	line = 1
	lastNewlinePos := -1 // byte position of last newline seen (-1 means before start of file)

	// Read source up to offset to count newlines
	for readPos := 0; readPos < offset; {
		toRead := min(len(aux), offset-readPos)
		n, rerr := r.ReadAt(aux[:toRead], int64(readPos))
		if n == 0 && rerr != nil {
			return 0, 0, 0, rerr
		}

		// Count newlines and find last newline position in this chunk
		chunk := aux[:n]
		for {
			idx := bytes.IndexByte(chunk, '\n')
			if idx < 0 {
				break
			}
			line++
			lastNewlinePos = readPos + (n - len(chunk)) + idx
			chunk = chunk[idx+1:]
		}

		readPos += n
		if rerr == io.EOF {
			break
		}
	}

	col = offset - lastNewlinePos

	// Find line length by reading until next newline or EOF
	lineLength = col - 1 // at minimum, the portion before offset
	for readPos := offset; ; {
		n, rerr := r.ReadAt(aux[:], int64(readPos))
		if n == 0 && rerr != nil {
			break
		}
		idx := bytes.IndexByte(aux[:n], '\n')
		if idx >= 0 {
			lineLength = (readPos - lastNewlinePos - 1) + idx
			break
		}
		readPos += n
		if rerr == io.EOF {
			lineLength = readPos - lastNewlinePos - 1
			break
		}
	}

	return line, col, lineLength, nil
}

// SyntaxError describes a lexing or parsing failure at an exact byte offset.
type SyntaxError struct {
	Off Pos
	Msg string
}

func (e *SyntaxError) Error() string {
	return "piudf: syntax error " + e.Off.String() + ": " + e.Msg
}
