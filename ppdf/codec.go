package ppdf

import (
	"fmt"
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
		case tokDict:
			return Value{Tok: piulex.TokNull}, nil
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

// lexValueSpan positions the lexer at span v's first byte: in the file via
// r, or — for Values tagged with an object stream (see Value.Ref) — inside
// that stream's decompressed data, loading p's cache if needed.
func (d *Codec) lexValueSpan(r io.ReaderAt, v Value) error {
	if !v.isSpan() || v.ObjectID().Num == 0 {
		return d.lexAt(r, v.I)
	}
	if err := d.loadObjStm(p, r, v.Ref.Num); err != nil {
		return fmt.Errorf("in object stream %d: %w", v.Ref.Num, err)
	}
	return d.lexStmAt(p, v.I)
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
		end, err := d.skipComposite(pos)
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

// maxObjStmDepth bounds object streams whose /Length references chase
// through further object streams, against reference cycles.
const maxObjStmDepth = 4

// loadObjStm ensures p's object-stream cache holds the decompressed
// contents of object stream num. A cache hit is free; a miss resolves the
// stream object, inflates its payload (capped by MaxDecompress) and records
// its /N and /First. One stream is cached at a time — sequential access to
// objects of the same stream costs one decompression.
func (d *Codec) loadObjStm(p *PDF, r io.ReaderAt, num uint32) error {

	if d.stmDepth >= maxObjStmDepth {
		return fmt.Errorf("%w: object streams nested deeper than %d", ErrCorrupt, maxObjStmDepth)
	}
	d.stmDepth++
	defer func() { d.stmDepth-- }()

	p.stmNum = 0 // Invalid while rebuilding.
	rec, err := p.lookupXref(r, num)
	if err != nil {
		return err
	}
	if rec.kind != recordNormal {
		return fmt.Errorf("%w: object stream %d is not a plain stream object", ErrCorrupt, num)
	}
	sv, err := d.parseObjectAt(r, rec.offset, ObjectID{Num: num, Gen: rec.gen})
	if err != nil {
		return err
	}
	if sv.Kind != KindStream {
		return fmt.Errorf("%w: object %d is not a stream", ErrCorrupt, num)
	}
	if tv, err := d.DictGet(p, r, sv, "Type"); err != nil {
		return err
	} else if tv.Kind == KindName && !d.NameIs(p, r, tv, "ObjStm") {
		return fmt.Errorf("%w: object %d is not /Type /ObjStm", ErrCorrupt, num)
	}
	// /N and /First must be direct (ISO 32000-1 7.5.7).
	getInt := func(key string) (int64, error) {
		v, err := d.DictGet(p, r, sv, key)
		if err != nil {
			return 0, err
		}
		n, err := v.Int()
		if err != nil {
			return 0, fmt.Errorf("%w: object stream /%s is not an integer", ErrCorrupt, key)
		}
		return n, nil
	}
	nObjs, err := getInt("N")
	if err != nil {
		return err
	}
	first, err := getInt("First")
	if err != nil {
		return err
	}
	if nObjs < 0 || first < 0 {
		return fmt.Errorf("%w: object stream %d /N %d /First %d", ErrCorrupt, num, nObjs, first)
	}
	// streamInfo resolves /Length, which may itself be a reference into
	// another object stream; the cache is marked invalid above exactly so
	// that such a detour cannot be mistaken for this stream's data.
	info, err := d.streamInfo(p, r, sv)
	if err != nil {
		return err
	}
	codec, err := d.readCodec(p, r, sv)
	if err != nil {
		return err
	}

	p.stmbuf = p.stmbuf[:0]
	if codec.flate {
		d.spanRdr.set(r, info.Offset, info.Length)
		p.stmbuf, err = d.inflate(p.stmbuf, &d.spanRdr, p.lim.MaxDecompress, p.lim.Grow)
		if err != nil {
			return fmt.Errorf("object stream %d: %w", num, err)
		}
	} else {
		if info.Length > int64(p.lim.MaxDecompress) {
			return fmt.Errorf("%w: object stream of %d bytes", ErrMemoryLimit, info.Length)
		}
		if !p.lim.Grow && info.Length > int64(cap(p.stmbuf)) {
			return fmt.Errorf("%w: object stream of %d bytes over buffer capacity", ErrMemoryLimit, info.Length)
		}
		p.stmbuf = append(p.stmbuf, make([]byte, info.Length)...)
		if n, err := r.ReadAt(p.stmbuf, info.Offset); int64(n) != info.Length {
			return fmt.Errorf("piudf: reading object stream %d/%d: %w", n, info.Length, err)
		}
	}
	if codec.predictor > 1 {
		p.stmbuf, err = undoPredictor(p.stmbuf, codec.predictor, codec.columns, codec.colors, codec.bpc)
		if err != nil {
			return err
		}
	}
	if first > int64(len(p.stmbuf)) {
		return fmt.Errorf("%w: object stream %d /First %d beyond %d data bytes", ErrCorrupt, num, first, len(p.stmbuf))
	}
	p.stmN = int(nObjs)
	p.stmFirst = first
	p.stmNum = num
	return nil
}

// lexStmAt positions the lexer at off inside p's decompressed object-stream
// cache, through the Decoder's reused bytes.Reader so nothing allocates.
func (d *Codec) lexStmAt(p *PDF, off int64) error {
	d.npb = 0
	d.stmRdr.Reset(p.stmbuf)
	return d.lx.Reset(&d.stmRdr, off)
}

// resolveCompressed resolves object id stored at index rec.offset inside
// object stream rec.stream: the stream is decompressed into p's cache, the
// leading pair table locates the object, and the object is shallow-parsed
// from the decompressed bytes. Span Values it returns are tagged with the
// object stream (see Value.Ref) so the accessors read them from the cache.
func (d *Codec) resolveCompressed(p *PDF, r io.ReaderAt, id ObjectID, rec xrefRecord) (Value, error) {
	if err := d.loadObjStm(p, r, rec.stream); err != nil {
		return Value{}, fmt.Errorf("in object stream %d: %w", rec.stream, err)
	}
	idx := rec.offset
	if idx < 0 || idx >= int64(p.stmN) {
		return Value{}, fmt.Errorf("%w: %v at index %d of object stream %d holding %d objects", ErrCorrupt, id, idx, rec.stream, p.stmN)
	}
	// Pair table: /N pairs of "objnum offset" integers at the data start.
	if err := d.lexStmAt(p, 0); err != nil {
		return Value{}, err
	}
	var numTv, offTv tokval
	for range idx + 1 {
		var err error
		if numTv, err = d.next(); err != nil {
			return Value{}, err
		}
		if offTv, err = d.next(); err != nil {
			return Value{}, err
		}
	}
	if numTv.tok != TokInt || offTv.tok != TokInt {
		return Value{}, fmt.Errorf("%w: malformed pair table in object stream %d", ErrCorrupt, rec.stream)
	}
	if numTv.val.I != int64(id.Num) {
		return Value{}, fmt.Errorf("%w: object stream %d index %d holds object %d, xref wants %v", ErrCorrupt, rec.stream, idx, numTv.val.I, id)
	}
	off := p.stmFirst + offTv.val.I
	if offTv.val.I < 0 || off >= int64(len(p.stmbuf)) {
		return Value{}, fmt.Errorf("%w: object %v outside object stream %d data", ErrCorrupt, id, rec.stream)
	}
	if err := d.lexStmAt(p, off); err != nil {
		return Value{}, err
	}
	v, err := d.parseShallow()
	if err != nil {
		return Value{}, err
	}
	return tagObjStm(v, rec.stream), nil
}
