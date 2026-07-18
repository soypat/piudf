package ppdf

import (
	"io"
	"math"

	"github.com/soypat/piudf/internal/zlib"
	"github.com/soypat/piudf/piulex"
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
	maxLazySections int
	maxDepth        int
	auxcounter      int
}

// MinBufferSize is the smallest [DecoderConfig.Buffer] a Codec accepts. The
// arena is split into two windows, so it is twice what one window needs to
// hold a dictionary worth of tokens.
const MinBufferSize = 2048

// DecoderConfig is the memory a Codec works out of and the bounds it works
// within. It is the whole of the Codec's configuration: the fields are
// unexported so that what a Codec may spend is decided once, by the caller,
// and cannot drift underneath a decode in progress.
//
// Every buffer here stays the caller's. The Codec keeps the slices, never
// grows them, and allocates nothing else of its own beyond the inflate windows
// compress/flate insists on owning.
type DecoderConfig struct {
	// Buffer is the arena the Codec reads through: the lexer's window over the
	// file, and the window over the cross-reference records. At least
	// [MinBufferSize] bytes. Larger spans more of the file per read and bounds
	// nothing else — literals build in the lexer's own buffer.
	Buffer []byte

	// XrefCache is optional storage for decoded cross-reference rows, and is
	// the difference between a lookup costing a bounds check and costing a
	// decode.
	//
	// It matters only for documents whose cross-reference table is a stream
	// (PDF 1.5 and later). Such a table is compressed, and compression has no
	// random access: a row is reachable only by decoding every row before it.
	// Without a cache the Codec keeps its place and decodes forward, which
	// makes an ascending sweep free and a backward jump cost the stream from
	// its first byte. With one, rows are kept as they are decoded and any row
	// already seen is a bounds check.
	//
	// The cache holds a prefix of one stream's rows — as many as fit — so a
	// partial buffer is not wasted: it serves the rows it covers and the
	// cursor serves the rest. [PDF.XrefCacheSize] reports what covering the
	// whole document takes. A nil XrefCache is the cursor alone, which holds
	// two rows and is what keeps [PDF.SizeOnRAM] flat.
	XrefCache []byte

	// MaxLazySections caps the cross-reference subsections a document may
	// record. Each costs ~40 bytes of PDF and files rarely exceed a dozen,
	// even after many incremental updates.
	MaxLazySections int

	// MaxDepth caps how deeply dictionaries and arrays may nest. It costs a
	// counter, not memory.
	MaxDepth int

	// XrefStreamConfig and ObjectStreamConfig are the inflate memory for the two
	// decompression cursors a Codec keeps: the cross-reference stream decoder
	// (PDF 1.5 xref streams) and the object stream decoder. They must be
	// distinct buffer sets — the two cursors can be live at once, so sharing
	// memory would silently corrupt one. A zero Config for either is replaced
	// with [zlib.DefaultConfig] (which allocates); pass your own to own the
	// memory or to set [zlib.Config.SkipChecksum].
	XrefStreamConfig   zlib.Config
	ObjectStreamConfig zlib.Config
}

// Configure hands c the memory and bounds of cfg, replacing any earlier
// configuration. It reports what is wrong with cfg rather than discovering it
// mid-decode.
func (c *Codec) Configure(cfg DecoderConfig) error {
	switch {
	case len(cfg.Buffer) < MinBufferSize:
		return io.ErrShortBuffer
	case cfg.MaxLazySections < 1, cfg.MaxDepth < 1:
		return ErrInvalidCodecConfig
	}
	if err := c.rows.data.Configure(cfg.XrefStreamConfig); err != nil {
		return err
	}
	if err := c.stm.data.Configure(cfg.ObjectStreamConfig); err != nil {
		return err
	}

	c.buf = cfg.Buffer
	c.maxLazySections = cfg.MaxLazySections
	c.maxDepth = cfg.MaxDepth
	// Literals are consumed before the next token is lexed, so one reused
	// buffer serves them all. This is configuration, so it is set here rather
	// than by Decode: a Codec must lex the same way before a document is
	// indexed as after.
	c.lex.ReuseLiteralBuffer = true
	c.lex.MaxLiteral = len(cfg.Buffer)
	// The cursor holds the cache rather than the Codec: it is the only thing
	// that fills it, and it alone knows when what it holds stops being true.
	c.rows.setCache(cfg.XrefCache)

	return nil
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

// DictGet returns the value for key in dictionary (or stream dictionary)
// dictVal, or a null Value when the key is absent. Cost is one scan of the
// dictionary's span, so reading n keys costs n scans; [Codec.DictForEach] reads
// them all in one.
func (d *Codec) DictGet(pdf *PDF, r io.ReaderAt, dictVal Value, key string) (ret Value, err error) {
	ret = Value{Tok: piulex.TokNull}
	// key is captured rather than parked on the Codec. A field would be one
	// Codec-wide slot for a value this call needs across the walk, and the walk
	// can re-enter DictGet: reading a span inside an object stream loads that
	// stream, which reads its /N and /First. The inner call would leave its key
	// in the slot and this one would go looking for /First.
	err = d.DictForEach(pdf, r, dictVal, func(bkey []byte, v Value) bool {
		if bequal(bkey, key) {
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

// AppendString appends the text of string, hex string or name v to dst.
//
// The text is the value's, decoded: escape sequences of a literal string, the
// digit pairs of a hex string and the #xx of a name all resolve, and a name's
// leading slash is punctuation rather than text and is left out. Which of the
// three v is does not change the result's meaning — PDF spells the same bytes
// three ways.
//
// The span is re-read rather than remembered. A Value is a coordinate and this
// package holds no file bytes, so this is the seam where text leaves it and
// dst is who owns it. Callers that only need to know whether a name matches
// should use [Codec.NameIs], which compares without copying.
//
// It lexes, so it must not be called from inside an [Codec.ArrayForEach] or
// [Codec.DictForEach] callback: one Codec has one lexer, and moving it mid-walk
// loses the caller's place. Collect the Values, then decode them.
func (d *Codec) AppendString(dst []byte, pdf *PDF, r io.ReaderAt, v Value) ([]byte, error) {
	switch v.Tok {
	case piulex.TokString, piulex.TokHexString, piulex.TokName:
	default:
		return dst, errValueMismatch
	}
	if err := d.lexValueSpan(pdf, r, v); err != nil {
		return dst, err
	}
	tok, _, lit := d.lex.NextToken()
	if tok != v.Tok {
		// The span does not lex as what v says it is, so the coordinate and
		// the file disagree — or the literal outran MaxLiteral, which the
		// lexer reports rather than truncate.
		if err := d.lex.Err(); err != nil {
			return dst, err
		}
		return dst, errValueMismatch
	}
	return append(dst, lit...), nil
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

// lexbuf and recbuf split the caller's arena into the two windows the Codec
// reads through. They are separate because object bodies and the xref record
// array are two distinct access streams: resolving an object alternates
// between them, so one shared window would evict on every hop.
func (c *Codec) lexbuf() []byte { return c.buf[:len(c.buf)/2] }
func (c *Codec) recbuf() []byte { return c.buf[len(c.buf)/2:] }

// Reset drops the scratch a decode leaves behind — pushed-back tokens, the
// error accumulator, the lexer window's resident bytes and both stream cursors
// — while keeping the configuration Configure set (buffer, bounds, caches). It
// is what makes a Codec reusable: [PDF.Decode] calls it, so one Codec may
// decode any number of documents. Call it directly to reuse a Codec whose last
// decode or resolve failed partway and left a cursor mid-stream.
func (c *Codec) Reset() {
	c.npb = 0
	c.stmDepth = 0
	c.accumErr = nil
	c.auxcounter = 0
	c.lex.DropBuffered() // TODO: see if Reset(nil, nil) gets the job done.
	c.rows.reset()
	c.stm.reset()
}

// validate reports a Codec that Configure never accepted, which is the only
// way one reaches a decode unusable.
func (c *Codec) validate() error {
	if len(c.buf) < MinBufferSize || c.maxLazySections < 1 || c.maxDepth < 1 {
		return ErrInvalidCodecConfig
	}
	return nil
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
			if depth > d.maxDepth {
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
