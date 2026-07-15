package ppdf

import (
	"io"
	"math"

	"github.com/soypat/piudf/ppdf/piulex"
)

type Codec struct {
	npb int
	pb  [2]Value

	lex piulex.Lexer

	buf []byte

	accumErr        error
	MaxLazySections int
	MaxDepth        int
}

func (c *Codec) lexAt(r io.ReaderAt, off int64) error {
	c.npb = 0
	return c.lex.Reset(r, off)
}

func (c *Codec) Validate() (err error) {
	const minbufsize = 1024
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
		v.N = uint32(max(0, pos-d.lex.Pos()))
	case piulex.TokTrue, piulex.TokFalse, piulex.TokNull:
		// Truthiness in Value.Tok==TokTrue.
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
	case piulex.TokDictOpen, piulex.TokArrayOpen:
		end, err := d.skipComposite(piulex.Pos(tv.I))
		if err != nil {
			return Value{}, err
		}
		kind := KindDict
		if tv.tok == TokArrayOpen {
			kind = KindArray
		}
		return Value{Kind: kind, I: int64(tv.pos), N: spanLen(tv.pos, end)}, nil
	case piulex.TokEOF:
		return Value{}, errUnexpectedEOF
	}
}

// unread pushes tv back; up to len(d.pb) tokens may be pending.
func (d *Codec) unread(tv Value) {
	d.pb[d.npb] = tv
	d.npb++
}

// skipComposite scans past the body of a composite whose opening token
// (starting at start) was already consumed, and returns the offset just
// past the matching close. The scan is token-level so parentheses, strings
// and comments cannot fool the nesting count. Cost is a bounded counter;
// no memory is consumed regardless of nesting.
func (d *Codec) skipComposite(start piulex.Pos) (end piulex.Pos, err error) {
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
