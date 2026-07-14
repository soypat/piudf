package piudf

import (
	"fmt"
	"math"
)

// tokval is a token with its literal already converted to a Value.
type tokval struct {
	val Value
	// lit aliases the lexer's literal buffer for TokName, TokString and
	// TokHexString tokens (decoded payload). It is valid until the lexer
	// produces its next literal-bearing token; the pushback queue holds at
	// most one literal-bearing token (the most recently lexed), so a
	// popped lit is always still intact.
	lit []byte
	pos Pos
	tok Token
}

// next returns the next token as a tokval, honoring the pushback queue.
// Span-kind tokens (strings, names) convert to file-coordinate Values;
// nothing is copied or interned.
func (d *Decoder) next() (tokval, error) {
	if d.npb > 0 {
		d.npb--
		return d.pb[d.npb], nil
	}
	tok, pos, lit := d.lx.NextToken()
	tv := tokval{tok: tok, pos: pos}
	switch tok {
	case TokIllegal:
		err := d.lx.Err()
		if err == nil {
			err = &SyntaxError{Off: pos, Msg: "illegal token"}
		}
		return tv, err
	case TokInt:
		v, ok := atoiSigned(lit)
		if !ok {
			return tv, &SyntaxError{Off: pos, Msg: "malformed integer"}
		}
		tv.val = Value{Kind: KindInt, I: v}
	case TokReal:
		f, ok := parseReal(lit)
		if !ok {
			return tv, &SyntaxError{Off: pos, Msg: "malformed real"}
		}
		tv.val = Value{Kind: KindReal, I: int64(math.Float64bits(f))}
	case TokName:
		tv.val = Value{Kind: KindName, I: int64(pos), N: spanLen(pos, d.lx.Pos())}
		tv.lit = lit
	case TokString:
		tv.val = Value{Kind: KindString, I: int64(pos), N: spanLen(pos, d.lx.Pos())}
		tv.lit = lit
	case TokHexString:
		tv.val = Value{Kind: KindHexString, I: int64(pos), N: spanLen(pos, d.lx.Pos())}
		tv.lit = lit
	case TokTrue:
		tv.val = Value{Kind: KindBool, I: 1}
	case TokFalse:
		tv.val = Value{Kind: KindBool, I: 0}
	case TokNull:
		tv.val = Value{Kind: KindNull}
	}
	return tv, nil
}

// spanLen returns the raw byte length of the token spanning [start, end).
func spanLen(start, end Pos) uint32 {
	if end < start {
		return 0
	}
	return uint32(end - start)
}

// unread pushes tv back; up to len(d.pb) tokens may be pending.
func (d *Decoder) unread(tv tokval) {
	d.pb[d.npb] = tv
	d.npb++
}

// parseShallow parses the next complete PDF object into a Value without
// descending into composites: dictionaries and arrays are skipped at token
// level and returned as raw file spans. No recursion, no memory.
func (d *Decoder) parseShallow() (Value, error) {
	tv, err := d.next()
	if err != nil {
		return Value{}, err
	}
	switch tv.tok {
	case TokInt:
		// Lookahead for an indirect reference: <int> <int> R.
		tv2, err := d.next()
		if err != nil {
			return Value{}, err
		}
		if tv2.tok == TokInt {
			tv3, err := d.next()
			if err != nil {
				return Value{}, err
			}
			if tv3.tok == TokR && tv.val.I > 0 && tv.val.I <= math.MaxUint32 &&
				tv2.val.I >= 0 && tv2.val.I <= math.MaxUint16 {
				return Value{Kind: KindRef, Ref: ObjectID{Num: uint32(tv.val.I), Gen: uint16(tv2.val.I)}}, nil
			}
			d.unread(tv3)
		}
		d.unread(tv2)
		return tv.val, nil

	case TokReal, TokName, TokString, TokHexString, TokTrue, TokFalse, TokNull:
		return tv.val, nil

	case TokDictOpen, TokArrayOpen:
		end, err := d.skipComposite(tv.pos)
		if err != nil {
			return Value{}, err
		}
		kind := KindDict
		if tv.tok == TokArrayOpen {
			kind = KindArray
		}
		return Value{Kind: kind, I: int64(tv.pos), N: spanLen(tv.pos, end)}, nil

	case TokEOF:
		return Value{}, &SyntaxError{Off: tv.pos, Msg: "unexpected end of input"}
	}
	return Value{}, &SyntaxError{Off: tv.pos, Msg: "unexpected token " + tv.tok.String()}
}

// skipComposite scans past the body of a composite whose opening token
// (starting at start) was already consumed, and returns the offset just
// past the matching close. The scan is token-level so parentheses, strings
// and comments cannot fool the nesting count. Cost is a bounded counter;
// no memory is consumed regardless of nesting.
func (d *Decoder) skipComposite(start Pos) (end Pos, err error) {
	depth := 1
	for {
		tv, err := d.next()
		if err != nil {
			return 0, err
		}
		switch tv.tok {
		case TokDictOpen, TokArrayOpen:
			depth++
			if depth > d.maxDepth {
				return 0, fmt.Errorf("%w: nesting deeper than %d at %v", ErrCorrupt, d.maxDepth, tv.pos)
			}
		case TokDictClose, TokArrayClose:
			depth--
			if depth == 0 {
				return d.lx.Pos(), nil
			}
		case TokEOF:
			return 0, &SyntaxError{Off: start, Msg: "unterminated dictionary or array"}
		}
	}
}

// atoiSigned parses an optionally signed decimal integer.
func atoiSigned(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	neg := false
	if b[0] == '+' || b[0] == '-' {
		neg = b[0] == '-'
		b = b[1:]
	}
	v, ok := atoiFixed(b)
	if neg {
		v = -v
	}
	return v, ok && len(b) > 0
}

// parseReal parses a PDF real: optional sign, digits, optional dot, digits.
// PDF reals have modest range and no exponent notation, so a simple
// accumulate-and-scale conversion is exact enough and does not allocate.
func parseReal(b []byte) (float64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	neg := false
	if b[0] == '+' || b[0] == '-' {
		neg = b[0] == '-'
		b = b[1:]
	}
	var mant int64
	var scale float64 = 1
	seenDot := false
	for _, c := range b {
		if c == '.' {
			if seenDot {
				return 0, false
			}
			seenDot = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		mant = mant*10 + int64(c-'0')
		if seenDot {
			scale *= 10
		}
	}
	f := float64(mant) / scale
	if neg {
		f = -f
	}
	return f, true
}
