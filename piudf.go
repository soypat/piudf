// Package piudf implements a lazy, memory-constrained PDF decoder.
//
// The package splits responsibilities between two types. PDF is the lazily
// decoded state of one document: it owns all per-document memory and holds
// no reference to the file or the decoder. Decoder is the reusable decoding
// machine (lexer and parser scratch) that decodes into PDF structs without
// retaining them.
//
// The io.ReaderAt supplying file bytes is never stored: methods that need
// the file take it as an explicit parameter. This makes the lazy contract
// visible in signatures — after Decode you may close the file, keep the PDF
// struct around, and later pass a reader over the identical bytes to
// Resolve and friends.
//
// Memory use is bounded by DecodeLimits set at Decode time; hitting a limit
// degrades softly by failing that operation with ErrMemoryLimit while the
// document stays usable. Compressed payloads (images, content streams) are
// never decoded: RawStream exposes the raw bytes and the declared filter,
// and the caller decides.
package piudf

import (
	"errors"
	"fmt"
	"io"
	"math"
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

	errStaleValue = errors.New("piudf: stale Value: arena was recycled since it was produced")
	errNotDecoded = errors.New("piudf: PDF empty: did Decode succeed?")
)

// DecodeLimits bounds per-document memory. The zero value of a field
// selects its default; a zero DecodeLimits selects DefaultDecodeLimits.
// With Grow false the PDF never allocates beyond these bounds after Decode:
// operations that would exceed them fail with ErrMemoryLimit instead.
type DecodeLimits struct {
	// ValueArena is the maximum number of Values held across the composite
	// arena and parse stack during a single Resolve.
	ValueArena int
	// NameArena is the maximum total bytes of interned names.
	NameArena int
	// MaxLiteral is the maximum byte length of a single lexed literal.
	MaxLiteral int
	// MaxParseDepth guards against deeply nested dictionaries and arrays.
	MaxParseDepth int
	// Grow permits exceeding the limits above by reallocating.
	Grow bool
}

// DefaultDecodeLimits returns limits suitable for typical documents, with
// growth enabled. Constrained callers should set explicit bounds and Grow
// false.
func DefaultDecodeLimits() DecodeLimits {
	return DecodeLimits{
		ValueArena:    2048,
		NameArena:     4096,
		MaxLiteral:    1 << 16,
		MaxParseDepth: 64,
		Grow:          true,
	}
}

func (lim DecodeLimits) withDefaults() DecodeLimits {
	def := DefaultDecodeLimits()
	if lim == (DecodeLimits{}) {
		return def
	}
	if lim.ValueArena <= 0 {
		lim.ValueArena = def.ValueArena
	}
	if lim.NameArena <= 0 {
		lim.NameArena = def.NameArena
	}
	if lim.MaxLiteral <= 0 {
		lim.MaxLiteral = def.MaxLiteral
	}
	if lim.MaxParseDepth <= 0 {
		lim.MaxParseDepth = def.MaxParseDepth
	}
	return lim
}

// Stats reports a document's resource usage.
type Stats struct {
	// Dropped counts operations refused due to ErrMemoryLimit.
	Dropped int
	// ValuesHighWater is the peak Value count reached in the arena.
	ValuesHighWater int
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
	// Filter is the declared filter name (first of the chain if /Filter
	// is an array), or zero if the payload is unfiltered.
	Filter NameRef
}

// PDF is the lazily decoded state of one document. It owns all per-document
// memory: slice capacity bounds allocation per DecodeLimits, length is
// usage, and element types contain no pointers. A PDF holds no reference to
// the file or the Decoder that filled it, so it may outlive both; lazy
// methods take an io.ReaderAt over the identical file bytes.
//
// Values produced from a PDF reference its internal arena and are
// invalidated by the next Resolve, Trailer or Decode into that PDF.
// A PDF is not safe for concurrent use.
type PDF struct {
	size     int64
	lim      DecodeLimits
	sections []xrefSection // Cross-reference sections, newest revision first.
	trailer  trailerInfo
	names    nameArena
	values   []Value // Composite arena; spans referenced by Values.
	stats    Stats
	// recbuf backs single xref record reads; a struct field so the slice
	// passed to the io.ReaderAt interface does not escape per lookup.
	recbuf [classicRecLen]byte
}

// Decoder is the decoding machine: lexer, token pushback and parse stack.
// The zero value is ready for use. A Decoder retains no reference to any
// PDF it decoded and is immediately reusable for another document; its
// memory stays constant no matter how many documents it processes.
// A Decoder is not safe for concurrent use.
type Decoder struct {
	lx    Lexer
	stack []Value // Pending elements of open composites during parse.
	pb    [2]tokval
	npb   int
}

// Reset empties the document, keeping allocated capacity for reuse.
func (p *PDF) Reset() {
	p.size = 0
	p.sections = p.sections[:0]
	p.trailer = trailerInfo{}
	p.values = p.values[:0]
	p.stats = Stats{}
}

// Decode initializes dst from r, which holds size bytes of PDF data. Only
// the trailer and cross-reference chain are parsed: no object contents are
// read. dst is reset first; its already-allocated buffers are reused, so
// recycling one PDF struct across documents reuses its memory. r is not
// retained by dst or d.
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
	dst.names.reset(lim.NameArena, lim.Grow)
	if cap(dst.values) < lim.ValueArena {
		dst.values = make([]Value, 0, lim.ValueArena)
	}
	d.resetScratch(dst)

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

// Trailer lazily re-parses and returns the newest trailer dictionary of p.
// The returned Value is invalidated by the next Resolve, Trailer or Decode
// into p.
func (d *Decoder) Trailer(p *PDF, r io.ReaderAt) (Value, error) {
	if p == nil || p.size == 0 {
		return Value{}, errNotDecoded
	}
	d.resetScratch(p)
	if err := d.lx.Reset(r, p.trailer.off); err != nil {
		return Value{}, err
	}
	v, err := d.parseNext(p, 0)
	if err != nil {
		return Value{}, err
	}
	if v.Kind != KindDict {
		return Value{}, fmt.Errorf("%w: trailer is not a dictionary", ErrCorrupt)
	}
	return v, nil
}

// Resolve looks up id in p's cross-reference table, seeks to its offset in
// r and parses the object. Object contents are parsed anew on every call
// (pure lazy): the returned Value and any composite it spans are only valid
// until the next Resolve, Trailer or Decode into p.
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
		return Value{}, fmt.Errorf("%w: %v is in an object stream", ErrUnsupported, id)
	}
	if rec.gen != id.Gen {
		return Value{}, fmt.Errorf("%w: %v generation mismatch (have %d)", ErrObjectNotFound, id, rec.gen)
	}
	return d.parseObjectAt(p, r, rec.offset, id)
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
func (d *Decoder) parseObjectAt(p *PDF, r io.ReaderAt, off int64, id ObjectID) (Value, error) {
	d.resetScratch(p)
	if err := d.lx.Reset(r, off); err != nil {
		return Value{}, err
	}
	numTv, err := d.next(p)
	if err != nil {
		return Value{}, err
	}
	genTv, err := d.next(p)
	if err != nil {
		return Value{}, err
	}
	objTv, err := d.next(p)
	if err != nil {
		return Value{}, err
	}
	if numTv.tok != TokInt || genTv.tok != TokInt || objTv.tok != TokObj {
		return Value{}, fmt.Errorf("%w: no object header at %#x for %v", ErrCorrupt, off, id)
	}
	if numTv.val.I != int64(id.Num) || genTv.val.I != int64(id.Gen) {
		return Value{}, fmt.Errorf("%w: xref points %v at object %d %d", ErrCorrupt, id, numTv.val.I, genTv.val.I)
	}
	v, err := d.parseNext(p, 0)
	if err != nil {
		return Value{}, err
	}
	// A dictionary followed by the 'stream' keyword is a stream object.
	tv, err := d.next(p)
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
	dataStart, err := d.lx.StreamDataStart()
	if err != nil {
		return Value{}, err
	}
	// The dict span is the arena tail; append one pseudo-entry holding the
	// payload offset so KindStream carries it without extra fields.
	if int64(v.I)+2*int64(v.N) != int64(len(p.values)) {
		return Value{}, fmt.Errorf("%w: stream dict span not at arena tail", ErrCorrupt)
	}
	if !p.lim.Grow && len(p.values) >= p.lim.ValueArena {
		p.stats.Dropped++
		return Value{}, fmt.Errorf("%w: value arena capacity %d", ErrMemoryLimit, p.lim.ValueArena)
	}
	p.values = append(p.values, Value{Kind: KindInt, I: int64(dataStart)})
	return Value{Kind: KindStream, I: v.I, N: v.N}, nil
}

// DictGet returns the value for key name in dictionary (or stream dict) v.
// A missing key yields a null Value and no error, matching PDF semantics.
func (p *PDF) DictGet(v Value, name string) (Value, error) {
	if v.Kind != KindDict && v.Kind != KindStream {
		return Value{}, errKindMismatch
	}
	start, n, _ := v.span()
	if start+2*n > len(p.values) {
		return Value{}, errStaleValue
	}
	ref := p.names.lookup(name)
	if ref == 0 {
		return Value{Kind: KindNull}, nil // Name never seen: key cannot exist.
	}
	for i := range n {
		if p.values[start+2*i].Name == ref {
			return p.values[start+2*i+1], nil
		}
	}
	return Value{Kind: KindNull}, nil
}

// ArrayIndex returns element i of array v.
func (p *PDF) ArrayIndex(v Value, i int) (Value, error) {
	if v.Kind != KindArray {
		return Value{}, errKindMismatch
	}
	start, n, _ := v.span()
	if start+n > len(p.values) {
		return Value{}, errStaleValue
	}
	if i < 0 || i >= n {
		return Value{}, errors.New("piudf: array index out of range")
	}
	return p.values[start+i], nil
}

// NameBytes returns the interned bytes of a name reference. The returned
// slice aliases the name arena; do not modify it.
func (p *PDF) NameBytes(ref NameRef) []byte { return p.names.bytes(ref) }

// NameIs reports whether v is a name equal to s, without allocating.
func (p *PDF) NameIs(v Value, s string) bool {
	return v.Kind == KindName && p.names.is(v.Name, s)
}

// AppendString re-reads and unescapes the payload of a string Value,
// appending the decoded bytes to dst. Strings are stored as raw file spans
// at parse time, so this is the only point that copies string data.
func (d *Decoder) AppendString(dst []byte, r io.ReaderAt, v Value) ([]byte, error) {
	if v.Kind != KindString && v.Kind != KindHexString {
		return dst, errKindMismatch
	}
	if err := d.lx.Reset(r, v.I-1); err != nil {
		return dst, err
	}
	tok, _, lit := d.lx.NextToken()
	if tok != TokString && tok != TokHexString {
		return dst, fmt.Errorf("%w: string span does not lex as string", ErrCorrupt)
	}
	return append(dst, lit...), nil
}

// RawStream returns a reader over the raw, still-encoded payload of stream
// object v along with its StreamInfo. The payload is never decompressed:
// Filter tells the caller what encoding the bytes are in. If /Length is an
// indirect reference it is resolved, which invalidates v.
func (*Decoder) RawStream(p *PDF, r io.ReaderAt, v Value) (*io.SectionReader, StreamInfo, error) {
	var info StreamInfo
	if v.Kind != KindStream {
		return nil, info, errKindMismatch
	}
	start, n, _ := v.span()
	if start+2*n+1 > len(p.values) {
		return nil, info, errStaleValue
	}
	info.Offset = p.values[start+2*n].I

	filterV, err := p.DictGet(v, "Filter")
	if err != nil {
		return nil, info, err
	}
	switch filterV.Kind {
	case KindName:
		info.Filter = filterV.Name
	case KindArray:
		if filterV.N > 0 {
			first, err := p.ArrayIndex(filterV, 0)
			if err == nil && first.Kind == KindName {
				info.Filter = first.Name
			}
		}
	}
	lengthV, err := p.DictGet(v, "Length")
	if err != nil {
		return nil, info, err
	}
	// Resolve indirect /Length last: Resolve recycles the arena backing v.
	lengthV, err = d.ResolveRef(p, r, lengthV, 1)
	if err != nil {
		return nil, info, fmt.Errorf("resolving stream /Length: %w", err)
	}
	length, err := lengthV.Int()
	if err != nil {
		return nil, info, fmt.Errorf("%w: stream /Length is %v", ErrCorrupt, lengthV.Kind)
	}
	if length < 0 || info.Offset < 0 || info.Offset > math.MaxInt64-length || info.Offset+length > p.size {
		return nil, info, fmt.Errorf("%w: stream payload [%#x, +%d) outside file", ErrCorrupt, info.Offset, length)
	}
	info.Length = length
	return io.NewSectionReader(r, info.Offset, length), info, nil
}
