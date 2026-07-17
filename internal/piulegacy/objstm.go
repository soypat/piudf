package piudf

import (
	"fmt"
	"io"
)

// maxObjStmDepth bounds object streams whose /Length references chase
// through further object streams, against reference cycles.
const maxObjStmDepth = 4

// loadObjStm ensures p's object-stream cache holds the decompressed
// contents of object stream num. A cache hit is free; a miss resolves the
// stream object, inflates its payload (capped by MaxDecompress) and records
// its /N and /First. One stream is cached at a time — sequential access to
// objects of the same stream costs one decompression.
func (d *Decoder) loadObjStm(p *PDF, r io.ReaderAt, num uint32) error {
	if p.stmNum == num {
		return nil
	}
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
	if rec.kind != recNormal {
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
func (d *Decoder) lexStmAt(p *PDF, off int64) error {
	d.npb = 0
	d.stmRdr.Reset(p.stmbuf)
	return d.lx.Reset(&d.stmRdr, off)
}

// resolveCompressed resolves object id stored at index rec.offset inside
// object stream rec.stream: the stream is decompressed into p's cache, the
// leading pair table locates the object, and the object is shallow-parsed
// from the decompressed bytes. Span Values it returns are tagged with the
// object stream (see Value.Ref) so the accessors read them from the cache.
func (d *Decoder) resolveCompressed(p *PDF, r io.ReaderAt, id ObjectID, rec xrefRecord) (Value, error) {
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
