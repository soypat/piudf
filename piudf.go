// Package piudf implements a lazy, memory-constrained PDF decoder.
//
// The package splits responsibilities between two types. PDF is the lazy
// index of one document — its cross-reference sections and trailer
// metadata — and owns no other memory. Decoder is the reusable decoding
// machine (lexer and parser scratch) that operates on PDF structs without
// retaining them.
//
// Values are file coordinates: an object is either an inline scalar or the
// {offset, length} of its raw text in the file. Composites (dictionaries,
// arrays) and text are re-lexed from their span each time they are
// accessed, so a Value never goes stale — it remains usable for as long as
// a reader over the identical file bytes exists.
//
// The io.ReaderAt supplying file bytes is never stored: methods that need
// the file take it as an explicit parameter. After Decode you may close the
// file, keep the PDF struct and any Values, and later pass a reader over
// the identical bytes to continue reading lazily.
//
// PDF 1.5+ cross-reference streams and compressed object streams are
// supported: those structural streams are inflated internally, bounded by
// DecodeLimits.MaxDecompress. Compressed payloads (images, content
// streams) are never decoded: RawStream exposes the raw bytes and the
// declared filter, and the caller decides.
package piudf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"unsafe"
)

// Sentinel errors. Wrapped errors are matched with errors.Is.
var (
	// ErrMemoryLimit reports that an operation needed more memory than
	// DecodeLimits permit. The document remains usable for other operations.
	ErrMemoryLimit = errors.New("piudf: memory limit reached")
	// ErrObjectNotFound reports a reference to a missing or free object.
	ErrObjectNotFound = errors.New("piudf: object not found")
	// ErrUnsupported reports a PDF feature outside the decoder's support,
	// such as cross-reference streams or object streams (PDF 1.5+).
	ErrUnsupported = errors.New("piudf: unsupported PDF feature")
	// ErrCorrupt reports structurally invalid PDF data.
	ErrCorrupt = errors.New("piudf: corrupt PDF")

	errNotDecoded = errors.New("piudf: PDF empty: did Decode succeed?")
)

// DecodeLimits bounds decoder and per-document memory. The zero value of a
// field selects its default; a zero DecodeLimits selects
// DefaultDecodeLimits. With Grow false nothing allocates beyond these
// bounds after Decode: operations that would exceed them fail with
// ErrMemoryLimit instead.
type DecodeLimits struct {
	// MaxLiteral is the maximum byte length of a single lexed literal.
	MaxLiteral int
	// MaxParseDepth guards against pathologically nested dictionaries and
	// arrays. It costs a counter, not memory.
	MaxParseDepth int
	// MaxXrefSections caps the cross-reference subsection descriptors
	// recorded per document (~32 bytes each). Files rarely exceed a dozen
	// even after many incremental updates.
	MaxXrefSections int
	// MaxEntries caps the machine-representation tables built by
	// DecodeEager: the entry pool (~32 bytes per dictionary pair or array
	// element) and the object table. Unused by the lazy Decode path.
	MaxEntries int
	// MaxDecompress caps each internal FlateDecode output: the decoded
	// cross-reference stream records kept per document, the object-stream
	// cache and DecodeEager's interned string pool. It is a hard cap —
	// Grow does not lift it — so compressed data cannot expand without
	// bound. Unused for documents with classic xref tables.
	MaxDecompress int
	// Grow permits exceeding MaxXrefSections by reallocating.
	Grow bool
}

// DefaultDecodeLimits returns limits suitable for typical documents, with
// growth enabled. Constrained callers should set explicit bounds and Grow
// false.
func DefaultDecodeLimits() DecodeLimits {
	return DecodeLimits{
		MaxLiteral:      1 << 16,
		MaxParseDepth:   64,
		MaxXrefSections: 128,
		MaxEntries:      1 << 16,
		MaxDecompress:   1 << 22,
		Grow:            true,
	}
}

func (lim DecodeLimits) withDefaults() DecodeLimits {
	def := DefaultDecodeLimits()
	if lim == (DecodeLimits{}) {
		return def
	}
	if lim.MaxLiteral <= 0 {
		lim.MaxLiteral = def.MaxLiteral
	}
	if lim.MaxParseDepth <= 0 {
		lim.MaxParseDepth = def.MaxParseDepth
	}
	if lim.MaxXrefSections <= 0 {
		lim.MaxXrefSections = def.MaxXrefSections
	}
	if lim.MaxEntries <= 0 {
		lim.MaxEntries = def.MaxEntries
	}
	if lim.MaxDecompress <= 0 {
		lim.MaxDecompress = def.MaxDecompress
	}
	return lim
}

// Stats reports a document's resource usage.
type Stats struct {
	// Dropped counts operations refused due to ErrMemoryLimit.
	Dropped int
}

// trailerInfo caches scalar trailer metadata captured during Decode; the
// trailer dictionary itself is re-parsed lazily by Decoder.Trailer.
type trailerInfo struct {
	off  int64 // Offset just past the newest 'trailer' keyword.
	size int64 // /Size of newest trailer.
	root ObjectID
	info ObjectID
}

// StreamInfo describes a stream object's payload without reading it.
type StreamInfo struct {
	// Offset is the absolute file offset of the first payload byte.
	Offset int64
	// Length is the payload byte length as declared by /Length.
	Length int64
	// Filter is the declared filter (a KindName Value; first of the chain
	// if /Filter is an array), or a null Value if the payload is
	// unfiltered.
	Filter Value
}

// PDF is the lazy index of one decoded document: its cross-reference
// section descriptors and trailer metadata. It owns no object data — all
// Values are file coordinates resolved against a reader passed per call.
// A PDF holds no reference to the file or the Decoder that filled it.
//
// For documents with classic xref tables, memory is O(number of xref
// subsections), independent of object count. Documents using
// cross-reference streams (PDF 1.5+) additionally keep the decoded xref
// records (compressed data has no random access) and a cache holding one
// decompressed object stream at a time, both capped by
// DecodeLimits.MaxDecompress. A PDF is not safe for concurrent use.
type PDF struct {
	size     int64
	lim      DecodeLimits
	sections []xrefSection // Cross-reference sections, newest revision first.
	trailer  trailerInfo
	stats    Stats
	// xbuf holds the decoded records of all cross-reference streams in the
	// chain; sectStream sections address rows in it. Empty for classic
	// tables.
	xbuf []byte
	// Object-stream cache: the decompressed contents of object stream
	// stmNum, one stream at a time. stmNum 0 means empty.
	stmNum   uint32
	stmN     int   // /N: number of objects in the cached stream.
	stmFirst int64 // /First: offset of the first object in stmbuf.
	stmbuf   []byte
	// recbuf backs single xref record reads; a struct field so the slice
	// passed to the io.ReaderAt interface does not escape per lookup.
	recbuf [classicRecLen]byte
}

// SizeOnRAM returns the total bytes of memory held by p: the struct itself
// plus all memory referenced by its slices. Slice capacity is counted, not
// length, since the backing memory is allocated either way.
func (p *PDF) SizeOnRAM() int {
	return int(unsafe.Sizeof(*p)) +
		cap(p.sections)*int(unsafe.Sizeof(xrefSection{})) +
		cap(p.xbuf) +
		cap(p.stmbuf)
}

// Decoder is the decoding machine: lexer, token pushback and inflate
// state. The zero value is ready for use. A Decoder retains no reference
// to any PDF it decoded and is immediately reusable for another document;
// its memory stays constant no matter how many documents it processes
// (the zlib window, ~50 KB, is allocated once on first contact with
// FlateDecode data). A Decoder is not safe for concurrent use.
type Decoder struct {
	lx       Lexer
	pb       [2]tokval // Parser token pushback queue.
	npb      int
	maxDepth int          // Nesting guard for span skipping; set by Decode.
	stmDepth int          // Object-stream /Length reference cycle guard.
	zr       zlibReader   // Reusable inflater; nil until first use.
	stmRdr   bytes.Reader // Reusable reader over a PDF's object-stream cache.
}

// Reset empties the document index, keeping allocated capacity for reuse.
func (p *PDF) Reset() {
	p.size = 0
	p.sections = p.sections[:0]
	p.trailer = trailerInfo{}
	p.stats = Stats{}
	p.xbuf = p.xbuf[:0]
	p.stmNum = 0
	p.stmN = 0
	p.stmFirst = 0
	p.stmbuf = p.stmbuf[:0]
}

// Decode initializes dst from r, which holds size bytes of PDF data. Only
// the trailer and cross-reference chain are parsed: no object contents are
// read. dst is reset first; its section buffer is reused, so recycling one
// PDF struct across documents reuses its memory. r is not retained by dst
// or d. The machine guards (MaxLiteral, MaxParseDepth) of the most recent
// Decode govern subsequent lazy calls on d.
func (d *Decoder) Decode(dst *PDF, r io.ReaderAt, size int64, lim DecodeLimits) error {
	if dst == nil {
		return errors.New("piudf: nil destination PDF")
	} else if r == nil {
		return errors.New("piudf: nil reader")
	} else if size <= 0 {
		return errors.New("piudf: non-positive size")
	}
	lim = lim.withDefaults()
	dst.Reset()
	dst.size = size
	dst.lim = lim
	d.lx.ReuseLiteralBuffer = true
	d.lx.MaxLiteral = lim.MaxLiteral
	d.maxDepth = lim.MaxParseDepth
	d.npb = 0

	var header [8]byte
	n, err := r.ReadAt(header[:], 0)
	if n < 5 || string(header[:5]) != "%PDF-" {
		return fmt.Errorf("%w: missing %%PDF- header (read %d: %v)", ErrCorrupt, n, err)
	}
	xrefOff, err := findStartXref(r, size)
	if err != nil {
		return err
	}
	return d.readXrefChain(dst, r, xrefOff)
}

// Stats returns resource usage counters accumulated since Decode.
func (p *PDF) Stats() Stats { return p.stats }

// Root returns the reference to the document catalog from the trailer.
func (p *PDF) Root() ObjectID { return p.trailer.root }

// Info returns the reference to the document info dictionary, if any.
func (p *PDF) Info() ObjectID { return p.trailer.info }

// lexAt positions the lexer at an absolute offset and clears pending
// pushback.
func (d *Decoder) lexAt(r io.ReaderAt, off int64) error {
	d.npb = 0
	return d.lx.Reset(r, off)
}

// lexValueSpan positions the lexer at span v's first byte: in the file via
// r, or — for Values tagged with an object stream (see Value.Ref) — inside
// that stream's decompressed data, loading p's cache if needed.
func (d *Decoder) lexValueSpan(p *PDF, r io.ReaderAt, v Value) error {
	if !v.isSpan() || v.Ref.Num == 0 {
		return d.lexAt(r, v.I)
	}
	if p == nil || p.size == 0 {
		return errNotDecoded
	}
	if err := d.loadObjStm(p, r, v.Ref.Num); err != nil {
		return fmt.Errorf("in object stream %d: %w", v.Ref.Num, err)
	}
	return d.lexStmAt(p, v.I)
}

// tagObjStm marks span Values parsed inside object stream stm so accessors
// know their coordinates address its decompressed data, not the file.
// Scalars and references pass through untouched; stm 0 (file space) is the
// identity.
func tagObjStm(v Value, stm uint32) Value {
	if stm != 0 && v.isSpan() {
		v.Ref = ObjectID{Num: stm}
	}
	return v
}

// Trailer lazily re-parses and returns the newest trailer dictionary of p
// as a file-span Value.
func (d *Decoder) Trailer(p *PDF, r io.ReaderAt) (Value, error) {
	if p == nil || p.size == 0 {
		return Value{}, errNotDecoded
	}
	if err := d.lexAt(r, p.trailer.off); err != nil {
		return Value{}, err
	}
	v, err := d.parseShallow()
	if err != nil {
		return Value{}, err
	}
	if v.Kind != KindDict {
		return Value{}, fmt.Errorf("%w: trailer is not a dictionary", ErrCorrupt)
	}
	return v, nil
}

// Resolve looks up id in p's cross-reference table, seeks to its offset in
// r and shallow-parses the object: scalars are returned inline, composites
// and text as raw file spans. The returned Value is a set of file
// coordinates and never goes stale.
func (d *Decoder) Resolve(p *PDF, r io.ReaderAt, id ObjectID) (Value, error) {
	if p == nil || p.size == 0 {
		return Value{}, errNotDecoded
	} else if id.Num == 0 {
		return Value{}, fmt.Errorf("%w: object number 0", ErrObjectNotFound)
	}
	rec, err := p.lookupXref(r, id.Num)
	if err != nil {
		return Value{}, err
	}
	switch rec.kind {
	case recFree:
		return Value{}, fmt.Errorf("%w: %v is free", ErrObjectNotFound, id)
	case recCompressed:
		// Objects in object streams always have generation 0.
		if id.Gen != 0 {
			return Value{}, fmt.Errorf("%w: %v generation mismatch (have 0)", ErrObjectNotFound, id)
		}
		return d.resolveCompressed(p, r, id, rec)
	}
	if rec.gen != id.Gen {
		return Value{}, fmt.Errorf("%w: %v generation mismatch (have %d)", ErrObjectNotFound, id, rec.gen)
	}
	return d.parseObjectAt(r, rec.offset, id)
}

// ResolveRef resolves v if it is an indirect reference, at most depth hops
// deep, and returns any other Value unchanged. PDF permits references to
// references; depth 1 suffices for well-formed files.
func (d *Decoder) ResolveRef(p *PDF, r io.ReaderAt, v Value, depth int) (Value, error) {
	var err error
	for range depth {
		if v.Kind != KindRef {
			return v, nil
		}
		v, err = d.Resolve(p, r, v.Ref)
		if err != nil {
			return Value{}, err
		}
	}
	if v.Kind == KindRef {
		return Value{}, fmt.Errorf("%w: reference chain deeper than %d", ErrCorrupt, depth)
	}
	return v, nil
}

// parseObjectAt parses the indirect object "N G obj ... endobj" at off.
func (d *Decoder) parseObjectAt(r io.ReaderAt, off int64, id ObjectID) (Value, error) {
	if err := d.lexAt(r, off); err != nil {
		return Value{}, err
	}
	numTv, err := d.next()
	if err != nil {
		return Value{}, err
	}
	genTv, err := d.next()
	if err != nil {
		return Value{}, err
	}
	objTv, err := d.next()
	if err != nil {
		return Value{}, err
	}
	if numTv.tok != TokInt || genTv.tok != TokInt || objTv.tok != TokObj {
		return Value{}, fmt.Errorf("%w: no object header at %#x for %v", ErrCorrupt, off, id)
	}
	if numTv.val.I != int64(id.Num) || genTv.val.I != int64(id.Gen) {
		return Value{}, fmt.Errorf("%w: xref points %v at object %d %d", ErrCorrupt, id, numTv.val.I, genTv.val.I)
	}
	v, err := d.parseShallow()
	if err != nil {
		return Value{}, err
	}
	// A dictionary followed by the 'stream' keyword is a stream object.
	tv, err := d.next()
	if err != nil {
		return v, nil // Tolerate EOF/lex errors after a complete object.
	}
	if tv.tok != TokStream {
		// 'endobj' or anything else: object is complete. Be lenient about
		// a missing endobj keyword.
		return v, nil
	}
	if v.Kind != KindDict {
		return Value{}, &SyntaxError{Off: tv.pos, Msg: "stream keyword without dictionary"}
	}
	return Value{Kind: KindStream, I: v.I, N: v.N}, nil
}

// DictGet returns the value for key in dictionary (or stream dict) dictVal
// by re-lexing its span — from r, or from p's object-stream cache when the
// dictionary lives inside an object stream. A missing key yields a null
// Value and no error, matching PDF semantics. Cost is one scan of the raw
// dict text.
func (d *Decoder) DictGet(p *PDF, r io.ReaderAt, dictVal Value, key string) (Value, error) {
	v := dictVal
	if v.Kind != KindDict && v.Kind != KindStream {
		return Value{}, errKindMismatch
	}
	if err := d.lexValueSpan(p, r, v); err != nil {
		return Value{}, err
	}
	tok, pos, _ := d.lx.NextToken()
	if tok != TokDictOpen {
		return Value{}, &SyntaxError{Off: pos, Msg: "dict span does not start with <<"}
	}
	for {
		// Keys must come through next(): parseShallow's reference
		// lookahead may have pushed the following key back onto the
		// pushback queue.
		tv, err := d.next()
		if err != nil {
			return Value{}, err
		}
		switch tv.tok {
		case TokDictClose:
			return Value{Kind: KindNull}, nil
		case TokName:
			match := string(tv.lit) == key // Comparison only: does not allocate.
			val, err := d.parseShallow()
			if err != nil {
				return Value{}, err
			}
			if match {
				return tagObjStm(val, v.Ref.Num), nil
			}
		default:
			return Value{}, &SyntaxError{Off: tv.pos, Msg: "expected name as dictionary key"}
		}
	}
}

// ArrayIndex returns element i of array arrVal by scanning its span: cost
// O(i).
func (d *Decoder) ArrayIndex(p *PDF, r io.ReaderAt, arrVal Value, i int) (Value, error) {
	v := arrVal
	if v.Kind != KindArray {
		return Value{}, errKindMismatch
	}
	if i < 0 {
		return Value{}, errors.New("piudf: negative array index")
	}
	if err := d.lexValueSpan(p, r, v); err != nil {
		return Value{}, err
	}
	tok, pos, _ := d.lx.NextToken()
	if tok != TokArrayOpen {
		return Value{}, &SyntaxError{Off: pos, Msg: "array span does not start with ["}
	}
	for n := 0; ; n++ {
		nt, err := d.next()
		if err != nil {
			return Value{}, err
		}
		if nt.tok == TokArrayClose {
			return Value{}, errors.New("piudf: array index out of range")
		}
		d.unread(nt)
		elem, err := d.parseShallow()
		if err != nil {
			return Value{}, err
		}
		if n == i {
			return tagObjStm(elem, v.Ref.Num), nil
		}
	}
}

// ArrayLen returns the element count of array arrVal by scanning its span.
func (d *Decoder) ArrayLen(p *PDF, r io.ReaderAt, arrVal Value) (int, error) {
	v := arrVal
	if v.Kind != KindArray {
		return 0, errKindMismatch
	}
	if err := d.lexValueSpan(p, r, v); err != nil {
		return 0, err
	}
	tok, pos, _ := d.lx.NextToken()
	if tok != TokArrayOpen {
		return 0, &SyntaxError{Off: pos, Msg: "array span does not start with ["}
	}
	for n := 0; ; n++ {
		nt, err := d.next()
		if err != nil {
			return 0, err
		}
		if nt.tok == TokArrayClose {
			return n, nil
		}
		d.unread(nt)
		if _, err := d.parseShallow(); err != nil {
			return 0, err
		}
	}
}

// NameIs reports whether nameVal is a name equal to s, decoding #xx
// escapes on the fly. It allocates nothing.
func (d *Decoder) NameIs(p *PDF, r io.ReaderAt, nameVal Value, s string) bool {
	v := nameVal
	if v.Kind != KindName {
		return false
	}
	if err := d.lexValueSpan(p, r, v); err != nil {
		return false
	}
	tok, _, lit := d.lx.NextToken()
	return tok == TokName && string(lit) == s
}

// AppendString re-reads and unescapes the payload of a string, hex string
// or name Value, appending the decoded bytes to dst. Values store raw
// spans, so this is the only point that copies text data.
func (d *Decoder) AppendString(dst []byte, p *PDF, r io.ReaderAt, strVal Value) ([]byte, error) {
	v := strVal
	switch v.Kind {
	case KindString, KindHexString, KindName:
	default:
		return dst, errKindMismatch
	}
	if err := d.lexValueSpan(p, r, v); err != nil {
		return dst, err
	}
	tok, _, lit := d.lx.NextToken()
	switch tok {
	case TokString, TokHexString, TokName:
		return append(dst, lit...), nil
	}
	return dst, fmt.Errorf("%w: span does not lex as text", ErrCorrupt)
}

// RawStream returns a reader over the raw, still-encoded payload of stream
// object v along with its StreamInfo. The payload is never decompressed:
// Filter tells the caller what encoding the bytes are in. The payload
// offset is re-derived by lexing the 'stream' keyword after the dict span.
func (d *Decoder) RawStream(p *PDF, r io.ReaderAt, v Value) (*io.SectionReader, StreamInfo, error) {
	var info StreamInfo
	info.Filter = Value{Kind: KindNull}
	if v.Kind != KindStream {
		return nil, info, errKindMismatch
	}
	filterV, err := d.DictGet(p, r, v, "Filter")
	if err != nil {
		return nil, info, err
	}
	switch filterV.Kind {
	case KindName:
		info.Filter = filterV
	case KindArray:
		first, err := d.ArrayIndex(p, r, filterV, 0)
		if err == nil && first.Kind == KindName {
			info.Filter = first
		}
	}
	lengthV, err := d.DictGet(p, r, v, "Length")
	if err != nil {
		return nil, info, err
	}
	lengthV, err = d.ResolveRef(p, r, lengthV, 1)
	if err != nil {
		return nil, info, fmt.Errorf("resolving stream /Length: %w", err)
	}
	length, err := lengthV.Int()
	if err != nil {
		return nil, info, fmt.Errorf("%w: stream /Length is %v", ErrCorrupt, lengthV.Kind)
	}
	// Locate the payload: the 'stream' keyword follows the dict span.
	if err := d.lexAt(r, v.I+int64(v.N)); err != nil {
		return nil, info, err
	}
	tok, pos, _ := d.lx.NextToken()
	if tok != TokStream {
		return nil, info, &SyntaxError{Off: pos, Msg: "expected 'stream' keyword after stream dictionary"}
	}
	dataStart, err := d.lx.StreamDataStart()
	if err != nil {
		return nil, info, err
	}
	info.Offset = int64(dataStart)
	if length < 0 || info.Offset > math.MaxInt64-length || info.Offset+length > p.size {
		return nil, info, fmt.Errorf("%w: stream payload [%#x, +%d) outside file", ErrCorrupt, info.Offset, length)
	}
	info.Length = length
	return io.NewSectionReader(r, info.Offset, length), info, nil
}
