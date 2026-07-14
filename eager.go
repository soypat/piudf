package piudf

import (
	"errors"
	"fmt"
	"io"
	"math"
	"unsafe"
)

// PDFEager holds the machine representation of a document's structure,
// built in one pass by Decoder.DecodeEager. Structure navigation (Resolve,
// DictGet, ArrayIndex, NameIs) never touches the file; only bulk data —
// string payloads and stream payloads — stays behind as {offset, size}
// spans read from the io.ReaderAt on demand.
//
// Values produced by a PDFEager are handles into its tables and are only
// meaningful with the PDFEager that produced them, unlike the lazy PDF's
// Values which are pure file coordinates. Within one PDFEager they are
// stable forever: the tables are filled once by DecodeEager and immutable
// afterwards.
//
// Memory is O(document structure): ~32 bytes per dictionary pair or array
// element plus the deduplicated name pool, all bounded by
// DecodeLimits.MaxEntries. A PDFEager is not safe for concurrent use.
type PDFEager struct {
	pdf        PDF          // Internal lazy index; drives the decode-time sweep.
	objs       []eagerObj   // Indexed by object number; O(1) Resolve.
	entries    []eagerEntry // Flat pool; composite Value = {first, count} here.
	names      []byte       // Interned name text (keys and name values).
	nameSpans  []Value      // One KindName Value per unique name, for dedup.
	streams    []eagerStream
	scratch    []eagerEntry // Composite parse frames, reused across objects.
	strbuf     []byte       // AppendString raw-span read buffer, reused.
	strs       []byte       // Decoded strings from object streams (no file span exists).
	objstms    []uint32     // Unique object stream numbers seen in the sweep.
	objstmCnt  []uint32     // Objects per stream in objstms, for drop accounting.
	pairs      []int64      // Pair-table scratch for one object stream.
	inObjStm   uint32       // Object stream being parsed, 0 in file space.
	trailerVal Value
	stats      Stats
}

// eagerObj is one slot of the object table.
type eagerObj struct {
	val     Value
	gen     uint16
	present bool
}

// eagerEntry is one dictionary pair or array element in the entry pool.
// Array elements have a null key.
type eagerEntry struct {
	key Value // Interned KindName for dictionary pairs; KindNull otherwise.
	val Value
}

// eagerStream carries the per-stream data that has no home in a Value:
// the payload location and the declared /Length (int or ref). A KindStream
// Value reaches it through the handle stashed in Value.Ref.Num.
type eagerStream struct {
	payloadOff int64
	length     Value
}

// Reset empties all tables, keeping allocated capacity for reuse.
func (p *PDFEager) Reset() {
	p.pdf.Reset()
	p.objs = p.objs[:0]
	p.entries = p.entries[:0]
	p.names = p.names[:0]
	p.nameSpans = p.nameSpans[:0]
	p.streams = p.streams[:0]
	p.scratch = p.scratch[:0]
	p.strs = p.strs[:0]
	p.objstms = p.objstms[:0]
	p.objstmCnt = p.objstmCnt[:0]
	p.pairs = p.pairs[:0]
	p.inObjStm = 0
	p.trailerVal = Value{}
	p.stats = Stats{}
}

// Stats returns resource usage counters accumulated by DecodeEager,
// including objects dropped due to limits or per-object parse errors.
func (p *PDFEager) Stats() Stats {
	return Stats{Dropped: p.stats.Dropped + p.pdf.stats.Dropped}
}

// Root returns the reference to the document catalog from the trailer.
func (p *PDFEager) Root() ObjectID { return p.pdf.Root() }

// Info returns the reference to the document info dictionary, if any.
func (p *PDFEager) Info() ObjectID { return p.pdf.Info() }

// DecodeEager initializes dst from r, which holds size bytes of PDF data,
// converting the entire document structure to machine representation in one
// sweep: every live object is deep-parsed into dst's tables. After it
// returns, navigating the document needs no lexing and no reader; r is only
// needed again for bulk data (AppendString, RawStream). Decode cost is
// proportional to the document's structural size.
//
// An object that fails to parse or would exceed lim.MaxEntries is dropped
// (counted in Stats().Dropped, ErrObjectNotFound on access) while the rest
// of the document remains usable; structural failures (header,
// cross-reference chain, trailer) fail DecodeEager itself.
func (d *Decoder) DecodeEager(dst *PDFEager, r io.ReaderAt, size int64, lim DecodeLimits) error {
	if dst == nil {
		return errors.New("piudf: nil destination PDFEager")
	}
	dst.Reset()
	if err := d.Decode(&dst.pdf, r, size, lim); err != nil {
		return err
	}
	lim = dst.pdf.lim // Defaults applied by Decode.

	// The object table spans the numbers covered by the trailer /Size and
	// the cross-reference sections, whichever reaches further.
	maxObj := dst.pdf.trailer.size
	for i := range dst.pdf.sections {
		s := &dst.pdf.sections[i]
		if end := int64(s.firstObj) + int64(s.count); end > maxObj {
			maxObj = end
		}
	}
	if maxObj <= 0 {
		return fmt.Errorf("%w: empty cross-reference table", ErrCorrupt)
	}
	if maxObj > math.MaxUint32 {
		return fmt.Errorf("%w: object table of %d entries", ErrCorrupt, maxObj)
	}
	if !lim.Grow && maxObj > int64(lim.MaxEntries) {
		return fmt.Errorf("%w: object table of %d exceeds MaxEntries %d", ErrMemoryLimit, maxObj, lim.MaxEntries)
	}
	if cap(dst.objs) < int(maxObj) {
		dst.objs = make([]eagerObj, maxObj)
	} else {
		dst.objs = dst.objs[:maxObj]
		clear(dst.objs)
	}

	// The trailer dictionary is structure too: parse it into the tables so
	// Trailer needs no reader.
	if err := d.lexAt(r, dst.pdf.trailer.off); err != nil {
		return err
	}
	tv, err := dst.parseDeep(d, 0)
	if err != nil {
		return fmt.Errorf("parsing trailer at %#x: %w", dst.pdf.trailer.off, err)
	}
	if tv.Kind != KindDict {
		return fmt.Errorf("%w: trailer is not a dictionary", ErrCorrupt)
	}
	dst.trailerVal = tv

	dst.objstms = dst.objstms[:0]
	dst.objstmCnt = dst.objstmCnt[:0]
	for num := uint32(1); num < uint32(maxObj); num++ {
		rec, err := dst.pdf.lookupXref(r, num)
		if err != nil || rec.kind == recFree {
			continue // Absent objects are not an error.
		}
		if rec.kind == recCompressed {
			// Collect unique stream numbers; the second pass below
			// decompresses each stream exactly once.
			found := false
			for i, s := range dst.objstms {
				if s == rec.stream {
					dst.objstmCnt[i]++
					found = true
					break
				}
			}
			if !found {
				if !lim.Grow && len(dst.objstms) >= lim.MaxEntries {
					dst.stats.Dropped++
					continue
				}
				dst.objstms = append(dst.objstms, rec.stream)
				dst.objstmCnt = append(dst.objstmCnt, 1)
			}
			continue
		}
		if err := dst.decodeObject(d, r, num, rec); err != nil {
			dst.stats.Dropped++
		}
	}
	for i, stm := range dst.objstms {
		if err := dst.decodeObjStm(d, r, stm); err != nil {
			dst.stats.Dropped += int(dst.objstmCnt[i])
		}
	}
	return nil
}

// decodeObjStm deep-parses every object of object stream stm that the
// cross-reference table still attributes to it (newer revisions may have
// moved individual objects elsewhere). The stream is decompressed once;
// objects that fail to parse are dropped individually, the rest survive.
// An error return means the whole stream was unusable.
func (p *PDFEager) decodeObjStm(d *Decoder, r io.ReaderAt, stm uint32) error {
	if err := d.loadObjStm(&p.pdf, r, stm); err != nil {
		return err
	}
	n := p.pdf.stmN
	lim := p.pdf.lim
	if n > lim.MaxEntries {
		return fmt.Errorf("%w: object stream of %d objects", ErrMemoryLimit, n)
	}
	if cap(p.pairs) < 2*n {
		if !lim.Grow && cap(p.pairs) > 0 {
			return fmt.Errorf("%w: pair table of %d objects over buffer capacity", ErrMemoryLimit, n)
		}
		p.pairs = make([]int64, 2*n)
	}
	pairs := p.pairs[:2*n]
	// The pair table ("objnum offset" per object) heads the data.
	if err := d.lexStmAt(&p.pdf, 0); err != nil {
		return err
	}
	for i := range pairs {
		tv, err := d.next()
		if err != nil {
			return err
		}
		if tv.tok != TokInt {
			return fmt.Errorf("%w: malformed pair table in object stream %d", ErrCorrupt, stm)
		}
		pairs[i] = tv.val.I
	}
	first := p.pdf.stmFirst
	for i := 0; i < n; i++ {
		objNum, objOff := pairs[2*i], pairs[2*i+1]
		if objNum <= 0 || objNum >= int64(len(p.objs)) || objOff < 0 || first+objOff >= int64(len(p.pdf.stmbuf)) {
			p.stats.Dropped++
			continue
		}
		rec, err := p.pdf.lookupXref(r, uint32(objNum))
		if err != nil || rec.kind != recCompressed || rec.stream != stm || rec.offset != int64(i) {
			continue // Shadowed: the live revision of this object lives elsewhere.
		}
		if err := d.lexStmAt(&p.pdf, first+objOff); err != nil {
			p.stats.Dropped++
			continue
		}
		p.scratch = p.scratch[:0]
		p.inObjStm = stm
		v, err := p.parseDeep(d, 0)
		p.inObjStm = 0
		if err != nil {
			p.stats.Dropped++
			continue
		}
		p.objs[objNum] = eagerObj{val: v, gen: 0, present: true}
	}
	return nil
}

// decodeObject deep-parses the indirect object "N G obj ... endobj" at
// rec.offset into p's tables. On error the object is simply not recorded;
// entries already committed by nested composites remain in the pool as
// unreferenced garbage, which is bounded by the same MaxEntries budget.
func (p *PDFEager) decodeObject(d *Decoder, r io.ReaderAt, num uint32, rec xrefRecord) error {
	p.scratch = p.scratch[:0]
	if err := d.lexAt(r, rec.offset); err != nil {
		return err
	}
	numTv, err := d.next()
	if err != nil {
		return err
	}
	genTv, err := d.next()
	if err != nil {
		return err
	}
	objTv, err := d.next()
	if err != nil {
		return err
	}
	if numTv.tok != TokInt || genTv.tok != TokInt || objTv.tok != TokObj {
		return fmt.Errorf("%w: no object header at %#x for object %d", ErrCorrupt, rec.offset, num)
	}
	if numTv.val.I != int64(num) || genTv.val.I != int64(rec.gen) {
		return fmt.Errorf("%w: xref points object %d %d at object %d %d", ErrCorrupt, num, rec.gen, numTv.val.I, genTv.val.I)
	}
	v, err := p.parseDeep(d, 0)
	if err != nil {
		return err
	}
	// A dictionary followed by the 'stream' keyword is a stream object.
	// Tolerate lex errors after a complete object, like the lazy path.
	if tv, err := d.next(); err == nil && tv.tok == TokStream {
		if v.Kind != KindDict {
			return &SyntaxError{Off: tv.pos, Msg: "stream keyword without dictionary"}
		}
		dataStart, err := d.lx.StreamDataStart()
		if err != nil {
			return err
		}
		lengthVal, err := p.DictGet(v, "Length") // Entries already committed.
		if err != nil {
			return err
		}
		lim := p.pdf.lim
		if !lim.Grow && len(p.streams) >= lim.MaxEntries {
			return fmt.Errorf("%w: more than %d streams", ErrMemoryLimit, lim.MaxEntries)
		}
		p.streams = append(p.streams, eagerStream{payloadOff: int64(dataStart), length: lengthVal})
		v = Value{Kind: KindStream, I: v.I, N: v.N, Ref: ObjectID{Num: uint32(len(p.streams))}}
	}
	p.objs[num] = eagerObj{val: v, gen: rec.gen, present: true}
	return nil
}

// parseDeep parses the next complete PDF object into machine
// representation: scalars inline, names interned, composites appended to
// the entry pool as contiguous {first, count} runs.
func (p *PDFEager) parseDeep(d *Decoder, depth int) (Value, error) {
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

	case TokReal, TokTrue, TokFalse, TokNull:
		return tv.val, nil

	case TokString, TokHexString:
		if p.inObjStm != 0 {
			// No file span exists for a string inside a compressed
			// object stream: intern its decoded payload instead.
			return p.internStr(tv.val.Kind, tv.lit)
		}
		return tv.val, nil

	case TokName:
		return p.internName(tv.lit)

	case TokDictOpen, TokArrayOpen:
		return p.parseComposite(d, depth, tv.tok == TokDictOpen, tv.pos)

	case TokEOF:
		return Value{}, &SyntaxError{Off: tv.pos, Msg: "unexpected end of input"}
	}
	return Value{}, &SyntaxError{Off: tv.pos, Msg: "unexpected token " + tv.tok.String()}
}

// parseComposite parses a dictionary or array body (opening token already
// consumed) into a contiguous run of the entry pool. Children are collected
// in a scratch frame first — nested composites commit their own runs to the
// pool before the parent does — so a composite's direct children always end
// up adjacent.
func (p *PDFEager) parseComposite(d *Decoder, depth int, isDict bool, start Pos) (Value, error) {
	if depth >= p.pdf.lim.MaxParseDepth {
		return Value{}, fmt.Errorf("%w: nesting deeper than %d at %v", ErrCorrupt, p.pdf.lim.MaxParseDepth, start)
	}
	frame := len(p.scratch)
	for {
		tv, err := d.next()
		if err != nil {
			return Value{}, err
		}
		key := Value{Kind: KindNull}
		if isDict {
			if tv.tok == TokDictClose {
				break
			}
			if tv.tok != TokName {
				return Value{}, &SyntaxError{Off: tv.pos, Msg: "expected name as dictionary key"}
			}
			if key, err = p.internName(tv.lit); err != nil {
				return Value{}, err
			}
		} else {
			if tv.tok == TokArrayClose {
				break
			}
			d.unread(tv)
		}
		val, err := p.parseDeep(d, depth+1)
		if err != nil {
			return Value{}, err
		}
		p.scratch = append(p.scratch, eagerEntry{key: key, val: val})
	}
	n := len(p.scratch) - frame
	lim := p.pdf.lim
	if !lim.Grow && len(p.entries)+n > lim.MaxEntries {
		return Value{}, fmt.Errorf("%w: more than %d entries", ErrMemoryLimit, lim.MaxEntries)
	}
	first := len(p.entries)
	p.entries = append(p.entries, p.scratch[frame:]...)
	p.scratch = p.scratch[:frame]
	kind := KindDict
	if !isDict {
		kind = KindArray
	}
	return Value{Kind: kind, I: int64(first), N: uint32(n)}, nil
}

// internName returns the interned Value for name text lit (leading '/'
// excluded, #xx escapes already decoded), adding it to the pool on first
// sight. Documents reuse a small set of unique names — ISO 32000-1 Annex C
// caps a name at 127 bytes — so a linear scan is cheap and the pool small.
func (p *PDFEager) internName(lit []byte) (Value, error) {
	for _, nv := range p.nameSpans {
		if string(p.names[nv.I:nv.I+int64(nv.N)]) == string(lit) { // Comparison only: does not allocate.
			return nv, nil
		}
	}
	lim := p.pdf.lim
	if !lim.Grow && len(p.nameSpans) >= lim.MaxEntries {
		return Value{}, fmt.Errorf("%w: more than %d unique names", ErrMemoryLimit, lim.MaxEntries)
	}
	nv := Value{Kind: KindName, I: int64(len(p.names)), N: uint32(len(lit))}
	p.names = append(p.names, lit...)
	p.nameSpans = append(p.nameSpans, nv)
	return nv, nil
}

// internStr copies the decoded payload of a string lexed inside an object
// stream into the string pool. The pool handle is marked by a nonzero
// Ref.Num (file-span strings have zero Ref), which AppendString serves
// from memory without a reader. The pool shares the MaxDecompress budget:
// it exists only because the data has no uncompressed home in the file.
func (p *PDFEager) internStr(kind Kind, lit []byte) (Value, error) {
	lim := p.pdf.lim
	if len(p.strs)+len(lit) > lim.MaxDecompress {
		return Value{}, fmt.Errorf("%w: string pool over %d bytes", ErrMemoryLimit, lim.MaxDecompress)
	}
	if !lim.Grow && len(p.strs)+len(lit) > cap(p.strs) {
		return Value{}, fmt.Errorf("%w: string pool over buffer capacity", ErrMemoryLimit)
	}
	v := Value{Kind: kind, I: int64(len(p.strs)), N: uint32(len(lit)), Ref: ObjectID{Num: 1}}
	p.strs = append(p.strs, lit...)
	return v, nil
}

// name returns the pool text of an interned KindName Value.
func (p *PDFEager) name(nameVal Value) ([]byte, error) {
	if nameVal.Kind != KindName {
		return nil, errKindMismatch
	}
	end := nameVal.I + int64(nameVal.N)
	if nameVal.I < 0 || end > int64(len(p.names)) {
		return nil, fmt.Errorf("%w: dangling name handle", ErrCorrupt)
	}
	return p.names[nameVal.I:end], nil
}

// composite returns the entry run of a dictionary, stream dictionary or
// array Value.
func (p *PDFEager) composite(v Value, want Kind) ([]eagerEntry, error) {
	if v.Kind != want && !(want == KindDict && v.Kind == KindStream) {
		return nil, errKindMismatch
	}
	end := v.I + int64(v.N)
	if v.I < 0 || end > int64(len(p.entries)) {
		return nil, fmt.Errorf("%w: dangling composite handle", ErrCorrupt)
	}
	return p.entries[v.I:end], nil
}

// Resolve returns the machine representation of object id: an O(1) table
// lookup, no reader involved.
func (p *PDFEager) Resolve(id ObjectID) (Value, error) {
	if len(p.objs) == 0 {
		return Value{}, errNotDecoded
	}
	if id.Num == 0 || int(id.Num) >= len(p.objs) {
		return Value{}, fmt.Errorf("%w: %v", ErrObjectNotFound, id)
	}
	o := &p.objs[id.Num]
	if !o.present {
		return Value{}, fmt.Errorf("%w: %v", ErrObjectNotFound, id)
	}
	if o.gen != id.Gen {
		return Value{}, fmt.Errorf("%w: %v generation mismatch (have %d)", ErrObjectNotFound, id, o.gen)
	}
	return o.val, nil
}

// ResolveRef resolves refVal if it is an indirect reference, at most depth
// hops deep, and returns any other Value unchanged.
func (p *PDFEager) ResolveRef(refVal Value, depth int) (Value, error) {
	var err error
	for range depth {
		if refVal.Kind != KindRef {
			return refVal, nil
		}
		refVal, err = p.Resolve(refVal.Ref)
		if err != nil {
			return Value{}, err
		}
	}
	if refVal.Kind == KindRef {
		return Value{}, fmt.Errorf("%w: reference chain deeper than %d", ErrCorrupt, depth)
	}
	return refVal, nil
}

// Trailer returns the newest trailer dictionary as a dict Value.
func (p *PDFEager) Trailer() (Value, error) {
	if len(p.objs) == 0 {
		return Value{}, errNotDecoded
	}
	return p.trailerVal, nil
}

// DictGet returns the value for key in dictionary (or stream dict) dictVal.
// A missing key yields a null Value and no error, matching PDF semantics.
func (p *PDFEager) DictGet(dictVal Value, key string) (Value, error) {
	ents, err := p.composite(dictVal, KindDict)
	if err != nil {
		return Value{}, err
	}
	for i := range ents {
		k := ents[i].key
		if string(p.names[k.I:k.I+int64(k.N)]) == key { // Comparison only: does not allocate.
			return ents[i].val, nil
		}
	}
	return Value{Kind: KindNull}, nil
}

// ArrayLen returns the element count of array arrVal in O(1).
func (p *PDFEager) ArrayLen(arrVal Value) (int, error) {
	ents, err := p.composite(arrVal, KindArray)
	if err != nil {
		return 0, err
	}
	return len(ents), nil
}

// ArrayIndex returns element i of array arrVal in O(1).
func (p *PDFEager) ArrayIndex(arrVal Value, i int) (Value, error) {
	ents, err := p.composite(arrVal, KindArray)
	if err != nil {
		return Value{}, err
	}
	if i < 0 {
		return Value{}, errors.New("piudf: negative array index")
	}
	if i >= len(ents) {
		return Value{}, errors.New("piudf: array index out of range")
	}
	return ents[i].val, nil
}

// NameIs reports whether nameVal is a name equal to s. Memory-only, no
// allocation.
func (p *PDFEager) NameIs(nameVal Value, s string) bool {
	lit, err := p.name(nameVal)
	return err == nil && string(lit) == s
}

// AppendName appends the text of name nameVal (without the leading '/',
// #xx escapes decoded) to dst.
func (p *PDFEager) AppendName(dst []byte, nameVal Value) ([]byte, error) {
	lit, err := p.name(nameVal)
	if err != nil {
		return dst, err
	}
	return append(dst, lit...), nil
}

// AppendString appends the decoded payload of a string, hex string or name
// Value to dst. Strings live in the file as raw {offset, size} spans, so r
// supplies the bytes; names come from the pool and need no reader (nil is
// accepted then).
func (p *PDFEager) AppendString(dst []byte, r io.ReaderAt, strVal Value) ([]byte, error) {
	switch strVal.Kind {
	case KindName:
		return p.AppendName(dst, strVal)
	case KindString, KindHexString:
	default:
		return dst, errKindMismatch
	}
	if strVal.Ref.Num != 0 {
		// Pool-interned (the string came from a compressed object
		// stream): already decoded, no reader needed.
		end := strVal.I + int64(strVal.N)
		if strVal.I < 0 || end > int64(len(p.strs)) {
			return dst, fmt.Errorf("%w: dangling string handle", ErrCorrupt)
		}
		return append(dst, p.strs[strVal.I:end]...), nil
	}
	n := int(strVal.N)
	maxLit := p.pdf.lim.MaxLiteral
	// Escapes and line continuations make the raw span longer than the
	// decoded payload, at most 4x (\ddd); the decoded length is checked
	// exactly during unescaping below.
	if maxLit > 0 && n > 4*maxLit+2 {
		return dst, fmt.Errorf("%w: string span of %d raw bytes", ErrMemoryLimit, n)
	}
	if cap(p.strbuf) < n {
		p.strbuf = make([]byte, n)
	}
	raw := p.strbuf[:n]
	if nn, err := r.ReadAt(raw, strVal.I); nn != n {
		return dst, fmt.Errorf("piudf: reading string span %d/%d at %#x: %w", nn, n, strVal.I, err)
	}
	if strVal.Kind == KindString {
		return unescapeLiteralString(dst, raw, maxLit)
	}
	return unescapeHexString(dst, raw, maxLit)
}

// RawStream returns a reader over the raw, still-encoded payload of stream
// object streamVal along with its StreamInfo. The payload location was
// recorded at decode time and /Length resolves through the object table:
// no lexing, r is only the byte source for the returned reader.
func (p *PDFEager) RawStream(r io.ReaderAt, streamVal Value) (*io.SectionReader, StreamInfo, error) {
	var info StreamInfo
	info.Filter = Value{Kind: KindNull}
	if streamVal.Kind != KindStream {
		return nil, info, errKindMismatch
	}
	idx := int(streamVal.Ref.Num) - 1
	if idx < 0 || idx >= len(p.streams) {
		return nil, info, fmt.Errorf("%w: dangling stream handle", ErrCorrupt)
	}
	st := p.streams[idx]
	filterV, err := p.DictGet(streamVal, "Filter")
	if err != nil {
		return nil, info, err
	}
	switch filterV.Kind {
	case KindName:
		info.Filter = filterV
	case KindArray:
		if first, err := p.ArrayIndex(filterV, 0); err == nil && first.Kind == KindName {
			info.Filter = first
		}
	}
	lengthV, err := p.ResolveRef(st.length, 1)
	if err != nil {
		return nil, info, fmt.Errorf("resolving stream /Length: %w", err)
	}
	length, err := lengthV.Int()
	if err != nil {
		return nil, info, fmt.Errorf("%w: stream /Length is %v", ErrCorrupt, lengthV.Kind)
	}
	info.Offset = st.payloadOff
	if length < 0 || info.Offset > math.MaxInt64-length || info.Offset+length > p.pdf.size {
		return nil, info, fmt.Errorf("%w: stream payload [%#x, +%d) outside file", ErrCorrupt, info.Offset, length)
	}
	info.Length = length
	return io.NewSectionReader(r, info.Offset, length), info, nil
}

// unescapeLiteralString decodes the raw span of a (string) token — outer
// parentheses included — appending the payload to dst. Semantics mirror the
// lexer's readLiteralString exactly: standard escapes, up to three octal
// digits, dropped line continuations, unknown escapes keep the byte, and
// EOLs normalize to \n. maxLit bounds the decoded length (<=0: unlimited).
func unescapeLiteralString(dst, raw []byte, maxLit int) ([]byte, error) {
	if len(raw) < 2 || raw[0] != '(' || raw[len(raw)-1] != ')' {
		return dst, fmt.Errorf("%w: span is not a literal string", ErrCorrupt)
	}
	b := raw[1 : len(raw)-1]
	start := len(dst)
	put := func(c byte) bool {
		if maxLit > 0 && len(dst)-start >= maxLit {
			return false
		}
		dst = append(dst, c)
		return true
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		ok := true
		switch c {
		case '\\':
			i++
			if i >= len(b) {
				break // Trailing backslash before the closing ')': dropped.
			}
			switch e := b[i]; e {
			case 'n':
				ok = put('\n')
			case 'r':
				ok = put('\r')
			case 't':
				ok = put('\t')
			case 'b':
				ok = put('\b')
			case 'f':
				ok = put('\f')
			case '\r':
				// Line continuation: backslash + EOL is dropped.
				if i+1 < len(b) && b[i+1] == '\n' {
					i++
				}
			case '\n':
			default:
				if e >= '0' && e <= '7' {
					v := e - '0'
					for range 2 {
						if i+1 >= len(b) || b[i+1] < '0' || b[i+1] > '7' {
							break
						}
						i++
						v = v<<3 | (b[i] - '0')
					}
					ok = put(v)
				} else {
					ok = put(e) // Unknown escape: backslash ignored, byte kept.
				}
			}
		case '\r':
			// EOL inside string normalizes to \n; \r\n counts as one.
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			ok = put('\n')
		default:
			ok = put(c)
		}
		if !ok {
			return dst, ErrMemoryLimit
		}
	}
	return dst, nil
}

// unescapeHexString decodes the raw span of a <hex> token — angle brackets
// included — appending the bytes to dst. Whitespace between digits is
// allowed; an odd final digit is padded with zero, mirroring the lexer's
// readHexString. maxLit bounds the decoded length (<=0: unlimited).
func unescapeHexString(dst, raw []byte, maxLit int) ([]byte, error) {
	if len(raw) < 2 || raw[0] != '<' || raw[len(raw)-1] != '>' {
		return dst, fmt.Errorf("%w: span is not a hex string", ErrCorrupt)
	}
	start := len(dst)
	var hi byte
	havehi := false
	for _, c := range raw[1 : len(raw)-1] {
		if isWhitespace(c) {
			continue
		}
		v, ok := hexVal(c)
		if !ok {
			return dst, fmt.Errorf("%w: bad hex digit in hex string", ErrCorrupt)
		}
		if havehi {
			if maxLit > 0 && len(dst)-start >= maxLit {
				return dst, ErrMemoryLimit
			}
			dst = append(dst, hi<<4|v)
			havehi = false
		} else {
			hi, havehi = v, true
		}
	}
	if havehi {
		if maxLit > 0 && len(dst)-start >= maxLit {
			return dst, ErrMemoryLimit
		}
		dst = append(dst, hi<<4)
	}
	return dst, nil
}

// SizeOnRAM returns the total bytes of memory held by p: the struct itself
// plus all memory referenced by its slices, including the internal lazy
// index and the machine-representation tables. Slice capacity is counted,
// not length, since the backing memory is allocated either way.
func (p *PDFEager) SizeOnRAM() int {
	sz := int(unsafe.Sizeof(*p)) // Includes the inline part of p.pdf.
	sz += p.pdf.SizeOnRAM() - int(unsafe.Sizeof(p.pdf))
	sz += cap(p.objs) * int(unsafe.Sizeof(eagerObj{}))
	sz += cap(p.entries) * int(unsafe.Sizeof(eagerEntry{}))
	sz += cap(p.names)
	sz += cap(p.nameSpans) * int(unsafe.Sizeof(Value{}))
	sz += cap(p.streams) * int(unsafe.Sizeof(eagerStream{}))
	sz += cap(p.scratch) * int(unsafe.Sizeof(eagerEntry{}))
	sz += cap(p.strbuf)
	sz += cap(p.strs)
	sz += cap(p.objstms) * 4
	sz += cap(p.objstmCnt) * 4
	sz += cap(p.pairs) * 8
	return sz
}
