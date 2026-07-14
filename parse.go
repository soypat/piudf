package piudf

import (
	"fmt"
	"math"
)

// tokval is a token with its literal already converted to a Value, since
// lexer literals are only valid until the next NextToken call.
type tokval struct {
	val Value
	pos Pos
	tok Token
}

// next returns the next token as a tokval, honoring the pushback queue.
// Names intern into p's arena; other conversions are self-contained.
func (d *Decoder) next(p *PDF) (tokval, error) {
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
		ref, err := p.names.intern(lit)
		if err != nil {
			p.stats.Dropped++
			return tv, fmt.Errorf("%w: interning name at %v", ErrMemoryLimit, pos)
		}
		tv.val = Value{Kind: KindName, Name: ref}
	case TokString, TokHexString:
		// Store the raw payload span; unescaped bytes are re-lexed on
		// demand by AppendString. Payload sits between the delimiters.
		kind := KindString
		if tok == TokHexString {
			kind = KindHexString
		}
		rawLen := int64(d.lx.Pos()) - int64(pos) - 2
		if rawLen < 0 {
			rawLen = 0
		}
		tv.val = Value{Kind: kind, I: int64(pos) + 1, N: uint32(rawLen)}
	case TokTrue:
		tv.val = Value{Kind: KindBool, I: 1}
	case TokFalse:
		tv.val = Value{Kind: KindBool, I: 0}
	case TokNull:
		tv.val = Value{Kind: KindNull}
	}
	return tv, nil
}

// unread pushes tv back; up to len(d.pb) tokens may be pending.
func (d *Decoder) unread(tv tokval) {
	d.pb[d.npb] = tv
	d.npb++
}

// parseNext parses the next complete PDF object from the token stream into
// a Value backed by p's arena. Composites are assembled on d.stack and
// moved into p.values when closed, so a composite's elements are contiguous.
func (d *Decoder) parseNext(p *PDF, depth int) (Value, error) {
	if depth > p.lim.MaxParseDepth {
		return Value{}, fmt.Errorf("%w: nesting deeper than %d", ErrMemoryLimit, p.lim.MaxParseDepth)
	}
	tv, err := d.next(p)
	if err != nil {
		return Value{}, err
	}
	switch tv.tok {
	case TokInt:
		// Lookahead for an indirect reference: <int> <int> R.
		tv2, err := d.next(p)
		if err != nil {
			return Value{}, err
		}
		if tv2.tok == TokInt {
			tv3, err := d.next(p)
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

	case TokArrayOpen:
		stackStart := len(d.stack)
		for {
			nt, err := d.next(p)
			if err != nil {
				return Value{}, err
			}
			if nt.tok == TokArrayClose {
				break
			}
			if nt.tok == TokEOF {
				return Value{}, &SyntaxError{Off: nt.pos, Msg: "unterminated array"}
			}
			d.unread(nt)
			v, err := d.parseNext(p, depth+1)
			if err != nil {
				return Value{}, err
			}
			if err := d.pushStack(p, v); err != nil {
				return Value{}, err
			}
		}
		return d.closeComposite(p, KindArray, stackStart, 1)

	case TokDictOpen:
		stackStart := len(d.stack)
		for {
			nt, err := d.next(p)
			if err != nil {
				return Value{}, err
			}
			if nt.tok == TokDictClose {
				break
			}
			if nt.tok != TokName {
				return Value{}, &SyntaxError{Off: nt.pos, Msg: "expected name as dictionary key"}
			}
			v, err := d.parseNext(p, depth+1)
			if err != nil {
				return Value{}, err
			}
			if err := d.pushStack(p, nt.val); err != nil {
				return Value{}, err
			}
			if err := d.pushStack(p, v); err != nil {
				return Value{}, err
			}
		}
		return d.closeComposite(p, KindDict, stackStart, 2)

	case TokEOF:
		return Value{}, &SyntaxError{Off: tv.pos, Msg: "unexpected end of input"}
	}
	return Value{}, &SyntaxError{Off: tv.pos, Msg: "unexpected token " + tv.tok.String()}
}

// pushStack adds a pending composite element, enforcing p's arena budget:
// stack elements are counted because they move verbatim into the arena.
func (d *Decoder) pushStack(p *PDF, v Value) error {
	if !p.lim.Grow && len(p.values)+len(d.stack) >= p.lim.ValueArena {
		p.stats.Dropped++
		return fmt.Errorf("%w: value arena capacity %d", ErrMemoryLimit, p.lim.ValueArena)
	}
	d.stack = append(d.stack, v)
	return nil
}

// closeComposite moves the elements accumulated since stackStart into p's
// values arena and returns the composite Value spanning them. perElem is 1
// for arrays and 2 for dictionaries (key+value pairs).
func (d *Decoder) closeComposite(p *PDF, kind Kind, stackStart, perElem int) (Value, error) {
	n := len(d.stack) - stackStart
	start := len(p.values)
	p.values = append(p.values, d.stack[stackStart:]...)
	d.stack = d.stack[:stackStart]
	if hw := len(p.values) + len(d.stack); hw > p.stats.ValuesHighWater {
		p.stats.ValuesHighWater = hw
	}
	return Value{Kind: kind, I: int64(start), N: uint32(n / perElem)}, nil
}

// resetScratch recycles the machine scratch and p's per-parse arena.
// Interned names persist.
func (d *Decoder) resetScratch(p *PDF) {
	p.values = p.values[:0]
	d.stack = d.stack[:0]
	d.npb = 0
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
