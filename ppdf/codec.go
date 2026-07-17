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
	npb int
	pb  [2]Value
	// pblit preserves the literal of a pushed-back name. The lexer reuses its
	// literal buffer every token, so a name crossing the pushback queue would
	// come back empty — which is exactly what a dictionary key does whenever
	// decodeShallow's reference lookahead reads past an integer value.
	pblit    [2][maxNameLen]byte
	pblitLen [2]uint8
	stmDepth int
	lex      piulex.Lexer

	buf []byte

	// keybuf holds the current dictionary key while its value is lexed. It
	// cannot live in buf: that arena is the lexer's window, so a key copied
	// there overwrites the very bytes the value is read from.
	keybuf [maxNameLen]byte

	// rows decodes cross-reference stream records and stm decodes the object
	// stream currently being read. Both are scratch, not index: each holds
	// where it stopped rather than what it decoded, so the Codec pays for two
	// cursors and the PDF still pays nothing per object.
	rows xrefRows
	stm  objStm

	accumErr        error
	MaxLazySections int
	MaxDepth        int
	auxcounter      int
	auxkey          string
}

// maxNameLen is the name length ISO 32000-1 Annex C.2 requires an
// implementation to support. Longer keys are out of spec, and treating them
// as such is what keeps a key in fixed storage.
const maxNameLen = 127

// ArrayForEach calls push with each element of array arrVal in order,
// stopping early when push returns false. Elements come through
// decodeShallow, so "1 0 R" arrives as one reference Value and a nested
// array or dictionary as one span Value; nothing is materialized.
func (codec *Codec) ArrayForEach(pdf *PDF, r io.ReaderAt, arrVal Value, push func(Value) bool) error {
	v := arrVal
	if !v.IsArray() {
		return errValueMismatch
	} else if err := codec.lexValueSpan(pdf, r, v); err != nil {
		return err
	}
	tok, _, _ := codec.lex.NextToken()
	if tok != piulex.TokArrayOpen {
		return errUnexpectedToken
	}
	for {
		// decodeShallow does not know the array's terminator, so the closing
		// bracket is recognized here and every other token handed back to it.
		nt, nlit, err := codec.nextRaw()
		if err != nil {
			return err
		}
		switch nt.Tok {
		case piulex.TokArrayClose:
			return nil
		case piulex.TokEOF:
			return errUnexpectedEOF
		}
		if err = codec.unread(nt, nlit); err != nil {
			return err
		}
		ev, err := codec.decodeShallow()
		if err != nil {
			return err
		}
		if !push(tagObjStm(ev, v.Stm)) {
			return nil
		}
	}
}

func (d *Codec) DictGet(pdf *PDF, r io.ReaderAt, dictVal Value, key string) (ret Value, err error) {
	ret = Value{Tok: piulex.TokNull}
	d.auxkey = key
	err = d.DictForEach(pdf, r, dictVal, func(bkey []byte, v Value) bool {
		if bequal(bkey, d.auxkey) {
			ret = v
			return false
		}
		return true
	})
	return ret, err
}

func (d *Codec) DictForEach(pdf *PDF, r io.ReaderAt, dictVal Value, push func(key []byte, v Value) bool) error {
	v := dictVal
	if v.Tok != tokDict && v.Tok != tokStream {
		return errUnexpectedToken
	}
	if err := d.lexValueSpan(pdf, r, v); err != nil {
		return err
	}
	tok, _, _ := d.lex.NextToken()
	if tok != tokDict {
		return errUnexpectedToken
	}
	for {
		// Keys must come through next(): parseShallow's reference
		// lookahead may have pushed the following key back onto the
		// pushback queue.
		tv, lit, err := d.nextRaw()
		if err != nil {
			return err
		}
		switch tv.Tok {
		case piulex.TokDictClose:
			return nil // Dictionary done.
		case piulex.TokName:
			if len(lit) > len(d.keybuf) {
				return errNameTooLong
			}
			// lit points into the lexer's literal buffer and dies at the next
			// token, which decodeShallow below lexes.
			key := d.keybuf[:copy(d.keybuf[:], lit)]
			val, err := d.decodeShallow()
			if err != nil {
				return err
			}
			ok := push(key, tagObjStm(val, v.Stm))
			if !ok {
				return nil // User requested end of iteration.
			}
		default:
			return errUnexpectedToken // expected name as dictionary key
		}
	}
}

// dictPrev returns the /Prev offset of trailer dictionary dictV. A trailer
// without /Prev is the oldest revision and ends the cross-reference chain.
func (d *Codec) dictPrev(pdf *PDF, r io.ReaderAt, dictV Value) (prev int64, err error) {
	v, err := d.DictGet(pdf, r, dictV, "Prev")
	if err != nil {
		return 0, err
	}
	prev, ok := v.Int()
	if !ok {
		return 0, nil
	}
	return prev, nil
}

// NameIs reports whether name span v is the PDF name s.
func (d *Codec) NameIs(pdf *PDF, r io.ReaderAt, v Value, s string) (bool, error) {
	if v.Tok != piulex.TokName {
		return false, errValueMismatch
	} else if err := d.lexValueSpan(pdf, r, v); err != nil {
		return false, err
	}
	tok, _, lit := d.lex.NextToken()
	if tok != piulex.TokName {
		return false, errUnexpectedToken
	}
	return bequal(lit, s), nil
}

// arrayFirst returns the sole element of array arrV, or null when it is
// empty. Internal streams take a single filter, so a longer array is a filter
// chain and unsupported rather than silently truncated.
func (d *Codec) arrayFirst(pdf *PDF, r io.ReaderAt, arrV Value) (v Value, err error) {
	v = Value{Tok: piulex.TokNull}
	d.auxcounter = 0
	err = d.ArrayForEach(pdf, r, arrV, func(el Value) bool {
		if d.auxcounter == 0 {
			v = el
		}
		d.auxcounter++
		return true
	})
	if err != nil {
		return v, err
	} else if d.auxcounter > 1 {
		return v, errTODO // TODO: filter chains on internal streams.
	}
	return v, nil
}

// readCodec extracts the /Filter and /DecodeParms of internal stream dictV.
// Only a lone FlateDecode, optionally behind a predictor, is valid on one.
func (d *Codec) readCodec(pdf *PDF, r io.ReaderAt, dictV Value) (sc streamCodec, err error) {
	// Defaults mandated by ISO 32000-1 Table 10; predictor 1 means none.
	sc = streamCodec{predictor: 1, columns: 1, colors: 1, bpc: 8}
	fv, err := d.DictGet(pdf, r, dictV, "Filter")
	if err != nil {
		return sc, err
	}
	if fv.IsArray() {
		if fv, err = d.arrayFirst(pdf, r, fv); err != nil {
			return sc, err
		}
	}
	switch fv.Tok {
	case piulex.TokNull:
		return sc, nil // Unfiltered: no /DecodeParms to read.
	case piulex.TokName:
		is, err := d.NameIs(pdf, r, fv, "FlateDecode")
		if err != nil {
			return sc, err
		} else if !is {
			return sc, errTODO // TODO: filters other than FlateDecode.
		}
		sc.flate = true
	default:
		return sc, errXrefStreamBad
	}
	pv, err := d.DictGet(pdf, r, dictV, "DecodeParms")
	if err != nil {
		return sc, err
	}
	if pv.IsArray() {
		if pv, err = d.arrayFirst(pdf, r, pv); err != nil {
			return sc, err
		}
	}
	if !pv.IsDict() {
		return sc, nil // No parameters: defaults stand.
	}
	// Read through an array rather than pointers into sc, which would force
	// the returned struct to escape on every call.
	keys := [4]string{"Predictor", "Columns", "Colors", "BitsPerComponent"}
	lims := [4]int64{math.MaxUint8, math.MaxUint16, math.MaxUint8, math.MaxUint8}
	vals := [4]int64{int64(sc.predictor), int64(sc.columns), int64(sc.colors), int64(sc.bpc)}
	for i, key := range keys {
		v, err := d.DictGet(pdf, r, pv, key)
		if err != nil {
			return sc, err
		}
		if v.IsNull() {
			continue // Absent: keep the default.
		}
		n, ok := v.Int()
		if !ok || n < 1 || n > lims[i] {
			return sc, errXrefStreamBad
		}
		vals[i] = n
	}
	sc.predictor, sc.columns = uint8(vals[0]), uint16(vals[1])
	sc.colors, sc.bpc = uint8(vals[2]), uint8(vals[3])
	return sc, nil
}

func (d *Codec) dictGetAccum(pdf *PDF, r io.ReaderAt, dictVal Value, key string, want piulex.Token) Value {
	if d.accumErr != nil {
		return Value{}
	}
	v, err := d.DictGet(pdf, r, dictVal, key)
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
//
// pdf is what locates that stream, and is needed only for a tagged v: a span
// in file space is already a file coordinate and needs no index to reach, so
// callers lexing raw source may pass nil.
func (d *Codec) lexValueSpan(pdf *PDF, r io.ReaderAt, v Value) error {
	if !v.isSpan() || v.Stm == 0 {
		return d.lexAt(r, v.I)
	}
	if err := pdf.loadObjStm(r, v.Stm, d); err != nil {
		return err
	}
	return d.lexAt(&d.stm.data, v.I)
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
		return d.pb[d.npb], d.pblit[d.npb][:d.pblitLen[d.npb]], nil
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
		tv2, lit2, err := d.nextRaw()
		if err != nil {
			return Value{}, err
		}
		if tv2.Tok == piulex.TokInt {
			tv3, lit3, err := d.nextRaw()
			if err != nil {
				return Value{}, err
			}
			if tv3.Tok == piulex.TokR && tv.I > 0 && tv.I <= math.MaxUint32 &&
				tv2.I >= 0 && tv2.I <= math.MaxUint16 {
				return Value{N: uint32(tv.I), I: tv2.I, Tok: piulex.TokR}, nil
			}
			if err = d.unread(tv3, lit3); err != nil {
				return Value{}, err
			}
		}
		if err = d.unread(tv2, lit2); err != nil {
			return Value{}, err
		}
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

// unread pushes tv back; up to len(d.pb) tokens may be pending. lit is kept
// for names only: numbers arrive already parsed into Value.I, and every other
// literal-bearing token is a span the Value locates on its own.
func (d *Codec) unread(tv Value, lit []byte) error {
	n := 0
	if tv.Tok == piulex.TokName {
		if len(lit) > maxNameLen {
			return errNameTooLong
		}
		n = copy(d.pblit[d.npb][:], lit)
	}
	d.pblitLen[d.npb] = uint8(n)
	d.pb[d.npb] = tv
	d.npb++
	return nil
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
