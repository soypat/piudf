package ppdf

import (
	"io"
	"math"

	"github.com/soypat/piudf/ppdf/piulex"
)

const (
	tokDict   = piulex.TokDictOpen
	tokArray  = piulex.TokArrayOpen
	tokObj    = piulex.TokObj
	tokStream = piulex.TokStream
)

type Codec struct {
	npb      int
	pb       [2]Value
	stmDepth int
	lex      piulex.Lexer

	buf []byte

	accumErr        error
	MaxLazySections int
	MaxDepth        int
	auxcounter      int
}

func (d *Codec) DictGet(r io.ReaderAt, dictVal Value, key string) (Value, error) {
	v := dictVal
	if v.Tok != tokDict && v.Tok != tokStream {
		return Value{}, errUnexpectedToken
	}
	if err := d.lexValueSpan(r, v); err != nil {
		return Value{}, err
	}
	tok, _, _ := d.lex.NextToken()
	if tok != tokDict {
		return Value{}, errUnexpectedToken
	}
	for {
		// Keys must come through next(): parseShallow's reference
		// lookahead may have pushed the following key back onto the
		// pushback queue.
		tv, lit, err := d.nextRaw()
		if err != nil {
			return Value{}, err
		}
		switch tv.Tok {
		case piulex.TokDictClose:
			return Value{Tok: piulex.TokNull}, nil // Key absent.
		case piulex.TokName:
			match := bequal(lit, key)
			val, err := d.decodeShallow()
			if err != nil {
				return Value{}, err
			}
			if match {
				return tagObjStm(val, v.Stm), nil
			}
		default:
			return Value{}, errUnexpectedToken // expected name as dictionary key
		}
	}
}

func (d *Codec) dictGetAccum(r io.ReaderAt, dictVal Value, key string, want piulex.Token) Value {
	if d.accumErr != nil {
		return Value{}
	}
	v, err := d.DictGet(r, dictVal, key)
	if err != nil {
		d.accumErr = err
	} else if v.Tok != want {
		d.accumErr = errUnexpectedToken
	}
	return v
}

// lexValueSpan positions the lexer at span v's first byte: in the file via
// r, or — for Values tagged with an object stream (see Value.Stm) — inside
// that stream's decompressed data.
func (d *Codec) lexValueSpan(r io.ReaderAt, v Value) error {
	if !v.isSpan() || v.Stm == 0 {
		return d.lexAt(r, v.I)
	}
	return errTODO // TODO: load object stream v.Stm and lex within its decompressed data.
}

func (c *Codec) lexAt(r io.ReaderAt, off int64) error {
	c.npb = 0
	return c.lex.Reset(r, off, c.lexbuf())
}

// SetBuffer hands c the arena it works out of. c keeps b; it allocates
// nothing else and never grows it.
func (c *Codec) SetBuffer(b []byte) { c.buf = b }

// lexbuf and recbuf split the caller's arena into the two windows the Codec
// reads through. They are separate because object bodies and the xref record
// array are two distinct access streams: resolving an object alternates
// between them, so one shared window would evict on every hop.
func (c *Codec) lexbuf() []byte { return c.buf[:len(c.buf)/2] }
func (c *Codec) recbuf() []byte { return c.buf[len(c.buf)/2:] }

func (c *Codec) Validate() (err error) {
	// Two windows out of one arena, so twice the single-window minimum.
	const minbufsize = 2048
	switch {
	case len(c.buf) < minbufsize:
		err = io.ErrShortBuffer
	case c.MaxLazySections < 1:
		err = ErrInvalidCodecConfig
	}
	return err
}

func (d *Codec) nextValue(tok piulex.Token, elseErr error) (v Value) {
	if d.accumErr != nil {
		return Value{}
	}
	v, _, err := d.nextRawExpect(tok, elseErr)
	if err != nil && d.accumErr == nil {
		d.accumErr = err
	}
	return v
}

func (d *Codec) nextRawExpect(tok piulex.Token, elseErr error) (v Value, lit []byte, err error) {
	v, lit, err = d.nextRaw()
	if err != nil {
		return v, lit, err
	} else if tok != v.Tok {
		return v, lit, elseErr
	}
	return v, lit, nil
}

// nextRaw returns the next token value honoring the pushback queue.
// Span-kind tokens (strings, names) convert to file-coordinate Values;
// nothing is copied or interned.
func (d *Codec) nextRaw() (v Value, lit []byte, err error) {
	if d.npb > 0 {
		d.npb--
		return d.pb[d.npb], nil, nil
	}
	var pos piulex.Pos
	v.Tok, pos, lit = d.lex.NextToken()
	switch v.Tok {
	case piulex.TokIllegal:
		err = d.lex.Err()
		if err == nil {
			err = ErrIllegalToken
		}
	case piulex.TokInt:
		v.I, err = parseInt(lit)
	case piulex.TokReal:
		var f float64
		f, err = parseReal(lit)
		v.I = int64(math.Float64bits(f))
	case piulex.TokName, piulex.TokString, piulex.TokHexString:
		v.I = int64(pos)
		v.N = uint32(max(0, d.lex.Pos()-pos))
	case piulex.TokTrue, piulex.TokFalse, piulex.TokNull:
		// Truthiness in Value.Tok==TokTrue.
	case tokDict, tokArray, tokObj:
		v.I = int64(pos)
	}
	return v, lit, err
}

func (d *Codec) decodeShallow() (_ Value, _ error) {
	tv, _, err := d.nextRaw()
	if err != nil {
		return Value{}, err
	}
	switch tv.Tok {
	case piulex.TokInt:
		// Lookahead for an indirect reference: <int> <int> R.
		tv2, _, err := d.nextRaw()
		if err != nil {
			return Value{}, err
		}
		if tv2.Tok == piulex.TokInt {
			tv3, _, err := d.nextRaw()
			if err != nil {
				return Value{}, err
			}
			if tv3.Tok == piulex.TokR && tv.I > 0 && tv.I <= math.MaxUint32 &&
				tv2.I >= 0 && tv2.I <= math.MaxUint16 {
				return Value{N: uint32(tv.I), I: tv2.I, Tok: piulex.TokR}, nil
			}
			d.unread(tv3)
		}
		d.unread(tv2)
		return tv, nil
	case piulex.TokReal, piulex.TokName, piulex.TokString, piulex.TokHexString, piulex.TokTrue, piulex.TokFalse, piulex.TokNull:
		return tv, nil
	case tokDict, tokArray:
		pos := piulex.Pos(tv.I)
		end, err := d.skipComposite()
		if err != nil {
			return Value{}, err
		}
		return Value{Tok: tv.Tok, I: tv.I, N: uint32(max(0, end-pos))}, nil
	case piulex.TokEOF:
		return Value{}, errUnexpectedEOF
	}
	return Value{}, errUnexpectedEOF
}

// unread pushes tv back; up to len(d.pb) tokens may be pending.
func (d *Codec) unread(tv Value) {
	d.pb[d.npb] = tv
	d.npb++
}

// skipComposite scans past the body of a composite whose opening token was
// already consumed, and returns the offset just past the matching close. The
// scan is token-level so parentheses, strings and comments cannot fool the
// nesting count. Cost is a bounded counter; no memory is consumed regardless
// of nesting.
func (d *Codec) skipComposite() (end piulex.Pos, err error) {
	depth := 1
	for {
		tv, _, err := d.nextRaw()
		if err != nil {
			return 0, err
		}
		switch tv.Tok {
		case piulex.TokDictOpen, piulex.TokArrayOpen:
			depth++
			if depth > d.MaxDepth {
				return 0, ErrCodecDepthLimit // fmt.Errorf("%w: nesting deeper than %d at %v", ErrCorrupt, d.maxDepth, tv.pos)
			}
		case piulex.TokDictClose, piulex.TokArrayClose:
			depth--
			if depth == 0 {
				return d.lex.Pos(), nil
			}
		case piulex.TokEOF:
			return 0, errUnexpectedEOF
		}
	}
}
