// Package piudf implements a lazy, memory-constrained PDF decoder.
//
// The decoder is driven entirely by an io.ReaderAt: it records file offsets
// instead of contents and parses indirect objects on demand. Memory use is
// bounded by Limits set at Decode time; hitting a limit degrades softly by
// failing that operation with ErrMemoryLimit while the decoder stays usable.
// Compressed payloads (images, content streams) are never decoded: RawStream
// exposes the raw bytes and the declared filter, and the caller decides.
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
	// Limits permit. The decoder remains usable for other operations.
	ErrMemoryLimit = errors.New("piudf: memory limit reached")
	// ErrObjectNotFound reports a reference to a missing or free object.
	ErrObjectNotFound = errors.New("piudf: object not found")
	// ErrUnsupported reports a PDF feature outside the decoder's support,
	// such as cross-reference streams or object streams (PDF 1.5+).
	ErrUnsupported = errors.New("piudf: unsupported PDF feature")
	// ErrCorrupt reports structurally invalid PDF data.
	ErrCorrupt = errors.New("piudf: corrupt PDF")

	errStaleValue = errors.New("piudf: stale Value: arena was recycled since it was produced")
	errNotDecoded = errors.New("piudf: decoder empty: did Decode succeed?")
)

// Limits bounds decoder memory. The zero value of a field selects its
// default; a zero Limits selects DefaultLimits. With Grow false the decoder
// never allocates beyond these bounds after Decode: operations that would
// exceed them fail with ErrMemoryLimit instead.
type Limits struct {
	// ValueArena is the maximum number of Values held across the composite
	// scratch arena and parse stack during a single Resolve.
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

// DefaultLimits returns limits suitable for typical documents, with growth
// enabled. Constrained callers should set explicit bounds and Grow false.
func DefaultLimits() Limits {
	return Limits{
		ValueArena:    2048,
		NameArena:     4096,
		MaxLiteral:    1 << 16,
		MaxParseDepth: 64,
		Grow:          true,
	}
}

func (lim Limits) withDefaults() Limits {
	def := DefaultLimits()
	if lim == (Limits{}) {
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

// Stats reports decoder resource usage.
type Stats struct {
	// Dropped counts operations refused due to ErrMemoryLimit.
	Dropped int
	// ValuesHighWater is the peak Value count reached in the arena.
	ValuesHighWater int
}

// trailerInfo caches scalar trailer metadata captured during Decode; the
// trailer dictionary itself is re-parsed lazily by Trailer.
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

// Decoder is a lazy PDF decoder over an io.ReaderAt. The zero value is
// ready for Decode. A Decoder is not safe for concurrent use.
//
// Values returned by Resolve and Trailer reference the Decoder's internal
// arena and are invalidated by the next Resolve, Trailer or Decode call.
type Decoder struct {
	r        io.ReaderAt
	size     int64
	lim      Limits
	sections []xrefSection // Cross-reference sections, newest revision first.
	trailer  trailerInfo
	names    nameArena
	values   []Value // Composite scratch arena; spans referenced by Values.
	stack    []Value // Pending elements of open composites during parse.
	lx       Lexer
	pb       [2]tokval // Parser token pushback queue.
	npb      int
	stats    Stats
	// recbuf backs single xref record reads; a struct field so the slice
	// passed to the io.ReaderAt interface does not escape per lookup.
	recbuf [classicRecLen]byte
}

// Decode initializes the decoder from r, which holds size bytes of PDF
// data. Only the trailer and cross-reference chain are parsed: no object
// contents are read. Internal buffers are reused across Decode calls.
func (d *Decoder) Decode(r io.ReaderAt, size int64, lim Limits) error {
	if r == nil {
		return errors.New("piudf: nil reader")
	} else if size <= 0 {
		return errors.New("piudf: non-positive size")
	}
	lim = lim.withDefaults()
	*d = Decoder{
		r:        r,
		size:     size,
		lim:      lim,
		sections: d.sections[:0],
		names:    d.names,
		values:   d.values,
		stack:    d.stack,
		lx:       d.lx,
	}
	d.lx.ReuseLiteralBuffer = true
	d.lx.MaxLiteral = lim.MaxLiteral
	d.names.reset(lim.NameArena, lim.Grow)
	if cap(d.values) < lim.ValueArena {
		d.values = make([]Value, 0, lim.ValueArena)
	}
	d.resetScratch()

	var header [8]byte
	n, err := r.ReadAt(header[:], 0)
	if n < 5 || string(header[:5]) != "%PDF-" {
		return fmt.Errorf("%w: missing %%PDF- header (read %d: %v)", ErrCorrupt, n, err)
	}
	xrefOff, err := findStartXref(r, size)
	if err != nil {
		return err
	}
	return d.readXrefChain(xrefOff)
}

// Stats returns resource usage counters accumulated since Decode.
func (d *Decoder) Stats() Stats { return d.stats }

// Root returns the reference to the document catalog from the trailer.
func (d *Decoder) Root() ObjectID { return d.trailer.root }

// Info returns the reference to the document info dictionary, if any.
func (d *Decoder) Info() ObjectID { return d.trailer.info }

// Trailer lazily re-parses and returns the newest trailer dictionary.
// The returned Value is invalidated by the next Resolve, Trailer or Decode.
func (d *Decoder) Trailer() (Value, error) {
	if d.r == nil {
		return Value{}, errNotDecoded
	}
	d.resetScratch()
	if err := d.lx.Reset(d.r, d.trailer.off); err != nil {
		return Value{}, err
	}
	v, err := d.parseNext(0)
	if err != nil {
		return Value{}, err
	}
	if v.Kind != KindDict {
		return Value{}, fmt.Errorf("%w: trailer is not a dictionary", ErrCorrupt)
	}
	return v, nil
}

// Resolve looks up id in the cross-reference table, seeks to its offset and
// parses the object. Object contents are parsed anew on every call (pure
// lazy): the returned Value and any composite it spans are only valid until
// the next Resolve, Trailer or Decode call on d.
func (d *Decoder) Resolve(id ObjectID) (Value, error) {
	if d.r == nil {
		return Value{}, errNotDecoded
	} else if id.Num == 0 {
		return Value{}, fmt.Errorf("%w: object number 0", ErrObjectNotFound)
	}
	rec, err := d.lookupXref(id.Num)
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
	return d.parseObjectAt(rec.offset, id)
}

// ResolveRef resolves v if it is an indirect reference, at most depth hops
// deep, and returns any other Value unchanged. PDF permits references to
// references; depth 1 suffices for well-formed files.
func (d *Decoder) ResolveRef(v Value, depth int) (Value, error) {
	var err error
	for range depth {
		if v.Kind != KindRef {
			return v, nil
		}
		v, err = d.Resolve(v.Ref)
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
func (d *Decoder) parseObjectAt(off int64, id ObjectID) (Value, error) {
	d.resetScratch()
	if err := d.lx.Reset(d.r, off); err != nil {
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
	v, err := d.parseNext(0)
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
	dataStart, err := d.lx.StreamDataStart()
	if err != nil {
		return Value{}, err
	}
	// The dict span is the arena tail; append one pseudo-entry holding the
	// payload offset so KindStream carries it without extra fields.
	if int64(v.I)+2*int64(v.N) != int64(len(d.values)) {
		return Value{}, fmt.Errorf("%w: stream dict span not at arena tail", ErrCorrupt)
	}
	if !d.lim.Grow && len(d.values) >= d.lim.ValueArena {
		d.stats.Dropped++
		return Value{}, fmt.Errorf("%w: value arena capacity %d", ErrMemoryLimit, d.lim.ValueArena)
	}
	d.values = append(d.values, Value{Kind: KindInt, I: int64(dataStart)})
	return Value{Kind: KindStream, I: v.I, N: v.N}, nil
}

// DictGet returns the value for key name in dictionary (or stream dict) v.
// A missing key yields a null Value and no error, matching PDF semantics.
func (d *Decoder) DictGet(v Value, name string) (Value, error) {
	if v.Kind != KindDict && v.Kind != KindStream {
		return Value{}, errKindMismatch
	}
	start, n, _ := v.span()
	if start+2*n > len(d.values) {
		return Value{}, errStaleValue
	}
	ref := d.names.lookup(name)
	if ref == 0 {
		return Value{Kind: KindNull}, nil // Name never seen: key cannot exist.
	}
	for i := range n {
		if d.values[start+2*i].Name == ref {
			return d.values[start+2*i+1], nil
		}
	}
	return Value{Kind: KindNull}, nil
}

// ArrayIndex returns element i of array v.
func (d *Decoder) ArrayIndex(v Value, i int) (Value, error) {
	if v.Kind != KindArray {
		return Value{}, errKindMismatch
	}
	start, n, _ := v.span()
	if start+n > len(d.values) {
		return Value{}, errStaleValue
	}
	if i < 0 || i >= n {
		return Value{}, errors.New("piudf: array index out of range")
	}
	return d.values[start+i], nil
}

// NameBytes returns the interned bytes of a name Value or NameRef holder.
// The returned slice aliases the name arena; do not modify it.
func (d *Decoder) NameBytes(ref NameRef) []byte { return d.names.bytes(ref) }

// NameIs reports whether v is a name equal to s, without allocating.
func (d *Decoder) NameIs(v Value, s string) bool {
	return v.Kind == KindName && d.names.is(v.Name, s)
}

// AppendString re-reads and unescapes the payload of a string Value,
// appending the decoded bytes to dst. Strings are stored as raw file spans
// at parse time, so this is the only point that copies string data.
func (d *Decoder) AppendString(dst []byte, v Value) ([]byte, error) {
	if v.Kind != KindString && v.Kind != KindHexString {
		return dst, errKindMismatch
	}
	if err := d.lx.Reset(d.r, v.I-1); err != nil {
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
func (d *Decoder) RawStream(v Value) (*io.SectionReader, StreamInfo, error) {
	var info StreamInfo
	if v.Kind != KindStream {
		return nil, info, errKindMismatch
	}
	start, n, _ := v.span()
	if start+2*n+1 > len(d.values) {
		return nil, info, errStaleValue
	}
	info.Offset = d.values[start+2*n].I

	filterV, err := d.DictGet(v, "Filter")
	if err != nil {
		return nil, info, err
	}
	switch filterV.Kind {
	case KindName:
		info.Filter = filterV.Name
	case KindArray:
		if filterV.N > 0 {
			first, err := d.ArrayIndex(filterV, 0)
			if err == nil && first.Kind == KindName {
				info.Filter = first.Name
			}
		}
	}
	lengthV, err := d.DictGet(v, "Length")
	if err != nil {
		return nil, info, err
	}
	// Resolve indirect /Length last: Resolve recycles the arena backing v.
	lengthV, err = d.ResolveRef(lengthV, 1)
	if err != nil {
		return nil, info, fmt.Errorf("resolving stream /Length: %w", err)
	}
	length, err := lengthV.Int()
	if err != nil {
		return nil, info, fmt.Errorf("%w: stream /Length is %v", ErrCorrupt, lengthV.Kind)
	}
	if length < 0 || info.Offset < 0 || info.Offset > math.MaxInt64-length || info.Offset+length > d.size {
		return nil, info, fmt.Errorf("%w: stream payload [%#x, +%d) outside file", ErrCorrupt, info.Offset, length)
	}
	info.Length = length
	return io.NewSectionReader(d.r, info.Offset, length), info, nil
}
