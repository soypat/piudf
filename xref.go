package piudf

import (
	"bytes"
	"fmt"
	"io"
	"math"
)

const (
	sectClassic = iota // 20-byte fixed records in the file itself.
	sectStream         // PDF 1.5+ cross-reference stream: decoded rows in PDF.xbuf.
)

const (
	recFree       = iota // Free-list entry: object does not exist.
	recNormal            // Uncompressed object at a file offset.
	recCompressed        // Object inside an object stream.
)

// classicRecLen is the record length mandated by ISO 32000-1 7.5.4:
// 10-digit offset, space, 5-digit generation, space, keyword, 2-byte EOL.
const classicRecLen = 20

// xrefSection describes one cross-reference subsection without materializing
// its entries. Classic on-disk tables are already random-access arrays of
// fixed-width records, so lookups read single records via ReadAt; the
// decoded rows of a cross-reference stream live in PDF.xbuf and are read
// from memory. Memory cost is O(number of subsections) plus, for streams,
// the decoded rows.
type xrefSection struct {
	firstObj uint32
	count    uint32
	fileOff  int64    // sectClassic: file offset of the first record. sectStream: offset into PDF.xbuf.
	w        [3]uint8 // Field widths; classic is {10, 5, 1}, streams use /W.
	kind     uint8
}

// xrefRecord is a single decoded cross-reference entry.
type xrefRecord struct {
	offset int64 // recNormal: absolute object offset. recCompressed: index in stream.
	stream uint32
	gen    uint16
	kind   uint8
}

// lookupXref finds the newest cross-reference record for object num.
// Classic sections read the record bytes from r; stream sections read the
// decoded rows in p.xbuf. Sections are stored newest-revision-first so the
// first section containing num shadows all older ones.
func (p *PDF) lookupXref(r io.ReaderAt, num uint32) (xrefRecord, error) {
	rec := p.recbuf[:classicRecLen]
	for i := range p.sections {
		s := &p.sections[i]
		if num < s.firstObj || num >= s.firstObj+s.count {
			continue
		}
		if s.kind == sectStream {
			rowlen := int64(s.w[0]) + int64(s.w[1]) + int64(s.w[2])
			off := s.fileOff + rowlen*int64(num-s.firstObj)
			if off < 0 || off+rowlen > int64(len(p.xbuf)) {
				return xrefRecord{}, fmt.Errorf("%w: xref stream row outside decoded data", ErrCorrupt)
			}
			return parseStreamRecord(p.xbuf[off:off+rowlen], s.w)
		}
		recOff := s.fileOff + classicRecLen*int64(num-s.firstObj)
		n, err := r.ReadAt(rec, recOff)
		if n != len(rec) {
			return xrefRecord{}, fmt.Errorf("piudf: reading xref record %d/%d at %#x: %w", n, len(rec), recOff, err)
		}
		return parseClassicRecord(rec, recOff)
	}
	return xrefRecord{}, ErrObjectNotFound
}

// parseStreamRecord decodes one row of a cross-reference stream: up to
// three big-endian fields of the widths declared by /W. A zero-width type
// field defaults to type 1 (ISO 32000-1 7.5.8.3); unknown types read as
// free, which the spec equates with references to the null object.
func parseStreamRecord(rec []byte, w [3]uint8) (xrefRecord, error) {
	be := func(b []byte) int64 {
		var v int64
		for _, c := range b {
			v = v<<8 | int64(c)
		}
		return v
	}
	typ := int64(1)
	i := int(w[0])
	if i > 0 {
		typ = be(rec[:i])
	}
	f2 := be(rec[i : i+int(w[1])])
	f3 := be(rec[i+int(w[1]) : i+int(w[1])+int(w[2])])
	switch typ {
	case 1:
		if f3 < 0 || f3 > math.MaxUint16 {
			return xrefRecord{}, fmt.Errorf("%w: xref stream generation %d", ErrCorrupt, f3)
		}
		return xrefRecord{kind: recNormal, offset: f2, gen: uint16(f3)}, nil
	case 2:
		if f2 <= 0 || f2 > math.MaxUint32 {
			return xrefRecord{}, fmt.Errorf("%w: xref stream object stream number %d", ErrCorrupt, f2)
		}
		return xrefRecord{kind: recCompressed, stream: uint32(f2), offset: f3}, nil
	}
	return xrefRecord{kind: recFree}, nil
}

// parseClassicRecord decodes a 20-byte "nnnnnnnnnn ggggg n" record.
func parseClassicRecord(rec []byte, recOff int64) (xrefRecord, error) {
	off, ok1 := atoiFixed(rec[0:10])
	gen, ok2 := atoiFixed(rec[11:16])
	kw := rec[17]
	if !ok1 || !ok2 || rec[10] != ' ' || rec[16] != ' ' || (kw != 'n' && kw != 'f') {
		return xrefRecord{}, fmt.Errorf("%w: malformed xref record at %#x", ErrCorrupt, recOff)
	}
	r := xrefRecord{offset: off, gen: uint16(gen)}
	if kw == 'f' {
		r.kind = recFree
	} else {
		r.kind = recNormal
	}
	return r, nil
}

// atoiFixed parses a fixed-width decimal field.
func atoiFixed(b []byte) (int64, bool) {
	var v int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int64(c-'0')
	}
	return v, true
}

// findStartXref locates the cross-reference offset advertised in the file
// tail: ... startxref <offset> %%EOF.
func findStartXref(r io.ReaderAt, size int64) (int64, error) {
	var tail [1024]byte
	n := int64(len(tail))
	if size < n {
		n = size
	}
	nn, err := r.ReadAt(tail[:n], size-n)
	if int64(nn) != n {
		return 0, fmt.Errorf("piudf: reading file tail %d/%d: %w", nn, n, err)
	}
	buf := tail[:n]
	i := bytes.LastIndex(buf, []byte("startxref"))
	if i < 0 {
		return 0, fmt.Errorf("%w: no startxref in last %d bytes", ErrCorrupt, n)
	}
	i += len("startxref")
	for i < len(buf) && isWhitespace(buf[i]) {
		i++
	}
	start := i
	for i < len(buf) && isDigit(buf[i]) {
		i++
	}
	off, ok := atoiFixed(buf[start:i])
	if !ok || start == i {
		return 0, fmt.Errorf("%w: malformed startxref offset", ErrCorrupt)
	}
	return off, nil
}

// maxXrefChain bounds the /Prev chain walk against reference loops.
const maxXrefChain = 64

// readXrefChain walks the cross-reference chain starting at the offset from
// startxref, recording section descriptors and trailer metadata into p.
// Only classic tables are supported; encountering a cross-reference stream
// (PDF 1.5+) fails with ErrUnsupported.
func (d *Decoder) readXrefChain(p *PDF, r io.ReaderAt, startOff int64) error {
	off := startOff
	for range maxXrefChain {
		if off < 0 || off >= p.size {
			return fmt.Errorf("%w: xref offset %#x out of file bounds", ErrCorrupt, off)
		}
		prev, err := d.readXrefTable(p, r, off)
		if err != nil {
			return err
		}
		if prev == 0 {
			return nil
		}
		if prev == off {
			return fmt.Errorf("%w: xref /Prev loop at %#x", ErrCorrupt, off)
		}
		off = prev
	}
	return fmt.Errorf("%w: xref /Prev chain longer than %d", ErrCorrupt, maxXrefChain)
}

// readXrefTable parses one classic cross-reference table and its trailer at
// off, appending section descriptors to p. Returns the /Prev offset or 0.
func (d *Decoder) readXrefTable(p *PDF, r io.ReaderAt, off int64) (prev int64, err error) {
	lx := &d.lx
	if err := lx.Reset(r, off); err != nil {
		return 0, err
	}
	tok, pos, _ := lx.NextToken()
	if tok == TokInt {
		// An "N G obj" header here means a cross-reference stream.
		return d.readXrefStream(p, r, off)
	}
	if tok != TokXref {
		return 0, &SyntaxError{Off: pos, Msg: "expected 'xref' keyword"}
	}
	for {
		tok, pos, lit := lx.NextToken()
		switch tok {
		case TokTrailer:
			return d.readTrailer(p, r)
		case TokInt:
			first, ok := atoiFixed(lit)
			if !ok {
				return 0, &SyntaxError{Off: pos, Msg: "bad subsection start"}
			}
			tok, pos, lit = lx.NextToken()
			count, ok := atoiFixed(lit)
			if tok != TokInt || !ok {
				return 0, &SyntaxError{Off: pos, Msg: "expected subsection entry count"}
			}
			// Records begin at the next non-whitespace byte. The lexer's
			// current byte is just past the count literal.
			lx.skipWhitespace()
			recOff := int64(lx.Pos())
			if count > 0 {
				if !p.lim.Grow && len(p.sections) >= p.lim.MaxXrefSections {
					p.stats.Dropped++
					return 0, fmt.Errorf("%w: more than %d xref subsections", ErrMemoryLimit, p.lim.MaxXrefSections)
				}
				p.sections = append(p.sections, xrefSection{
					firstObj: uint32(first),
					count:    uint32(count),
					fileOff:  recOff,
					w:        [3]uint8{10, 5, 1},
					kind:     sectClassic,
				})
			}
			// Jump over the fixed-width record area instead of tokenizing it.
			if err := lx.Reset(r, recOff+classicRecLen*count); err != nil {
				return 0, err
			}
		default:
			return 0, &SyntaxError{Off: pos, Msg: "expected subsection header or 'trailer'"}
		}
	}
}

// streamCodec is the decoded /Filter + /DecodeParms of an internal (xref
// or object) stream. Only FlateDecode, optionally behind a predictor, is
// accepted there.
type streamCodec struct {
	flate     bool
	predictor int
	columns   int
	colors    int
	bpc       int
}

// readCodec extracts the codec of internal stream dict dictVal. A filter
// other than FlateDecode (or a chain) fails with ErrUnsupported.
func (d *Decoder) readCodec(p *PDF, r io.ReaderAt, dictVal Value) (streamCodec, error) {
	c := streamCodec{predictor: 1, columns: 1, colors: 1, bpc: 8}
	fv, err := d.DictGet(p, r, dictVal, "Filter")
	if err != nil {
		return c, err
	}
	if fv.Kind == KindArray {
		n, err := d.ArrayLen(p, r, fv)
		if err != nil {
			return c, err
		}
		if n > 1 {
			return c, fmt.Errorf("%w: filter chain on internal stream", ErrUnsupported)
		}
		arr := fv
		fv = Value{Kind: KindNull}
		if n == 1 {
			if fv, err = d.ArrayIndex(p, r, arr, 0); err != nil {
				return c, err
			}
		}
	}
	switch fv.Kind {
	case KindNull:
	case KindName:
		if !d.NameIs(p, r, fv, "FlateDecode") {
			return c, fmt.Errorf("%w: filter on internal stream is not FlateDecode", ErrUnsupported)
		}
		c.flate = true
	default:
		return c, fmt.Errorf("%w: internal stream /Filter is %v", ErrCorrupt, fv.Kind)
	}
	pv, err := d.DictGet(p, r, dictVal, "DecodeParms")
	if err != nil {
		return c, err
	}
	if pv.Kind == KindArray {
		if pv, err = d.ArrayIndex(p, r, pv, 0); err != nil {
			return c, err
		}
	}
	if pv.Kind != KindDict {
		return c, nil
	}
	for _, f := range [...]struct {
		key string
		dst *int
	}{
		{"Predictor", &c.predictor},
		{"Columns", &c.columns},
		{"Colors", &c.colors},
		{"BitsPerComponent", &c.bpc},
	} {
		v, err := d.DictGet(p, r, pv, f.key)
		if err != nil {
			return c, err
		}
		if v.Kind == KindNull {
			continue
		}
		n, err := v.Int()
		if err != nil {
			return c, fmt.Errorf("%w: /DecodeParms /%s is not an integer", ErrCorrupt, f.key)
		}
		*f.dst = int(n)
	}
	return c, nil
}

// readXrefStream parses the cross-reference stream whose object header
// starts at off (PDF 1.5+). The stream dictionary doubles as the trailer;
// the decompressed payload holds the records and is appended to p.xbuf —
// compressed data has no random access, so this is the one place the lazy
// index materializes xref records. Returns the /Prev offset or 0.
func (d *Decoder) readXrefStream(p *PDF, r io.ReaderAt, off int64) (prev int64, err error) {
	if err := d.lexAt(r, off); err != nil {
		return 0, err
	}
	numTv, err := d.next()
	if err != nil {
		return 0, err
	}
	genTv, err := d.next()
	if err != nil {
		return 0, err
	}
	objTv, err := d.next()
	if err != nil {
		return 0, err
	}
	if numTv.tok != TokInt || genTv.tok != TokInt || objTv.tok != TokObj {
		return 0, fmt.Errorf("%w: no object header for cross-reference stream at %#x", ErrCorrupt, off)
	}
	dictV, err := d.parseShallow()
	if err != nil {
		return 0, err
	}
	if dictV.Kind != KindDict {
		return 0, fmt.Errorf("%w: cross-reference stream without dictionary at %#x", ErrCorrupt, off)
	}
	tv, err := d.next()
	if err != nil {
		return 0, err
	}
	if tv.tok != TokStream {
		return 0, &SyntaxError{Off: tv.pos, Msg: "expected 'stream' keyword in cross-reference stream"}
	}
	dataStart, err := d.lx.StreamDataStart()
	if err != nil {
		return 0, err
	}

	// Scalar entries; ISO 32000-1 7.5.8.2 requires them to be direct.
	getInt := func(key string) (int64, bool, error) {
		v, err := d.DictGet(p, r, dictV, key)
		if err != nil {
			return 0, false, err
		}
		if v.Kind == KindNull {
			return 0, false, nil
		}
		n, err := v.Int()
		if err != nil {
			return 0, false, fmt.Errorf("%w: xref stream /%s is not an integer", ErrCorrupt, key)
		}
		return n, true, nil
	}
	size, ok, err := getInt("Size")
	if err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("%w: xref stream without /Size at %#x", ErrCorrupt, off)
	}
	length, ok, err := getInt("Length")
	if err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("%w: xref stream without /Length at %#x", ErrCorrupt, off)
	}
	wV, err := d.DictGet(p, r, dictV, "W")
	if err != nil {
		return 0, err
	}
	if wV.Kind != KindArray {
		return 0, fmt.Errorf("%w: xref stream /W is not an array", ErrCorrupt)
	}
	if n, err := d.ArrayLen(p, r, wV); err != nil {
		return 0, err
	} else if n != 3 {
		return 0, fmt.Errorf("%w: xref stream /W has %d fields", ErrCorrupt, n)
	}
	var w [3]uint8
	for i := range w {
		el, err := d.ArrayIndex(p, r, wV, i)
		if err != nil {
			return 0, err
		}
		n, err := el.Int()
		if err != nil || n < 0 || n > 8 {
			return 0, fmt.Errorf("%w: xref stream /W width %v", ErrCorrupt, el.Kind)
		}
		w[i] = uint8(n)
	}
	if w[1] == 0 {
		return 0, fmt.Errorf("%w: xref stream /W has no offset field", ErrCorrupt)
	}
	rowlen := int(w[0]) + int(w[1]) + int(w[2])
	codec, err := d.readCodec(p, r, dictV)
	if err != nil {
		return 0, err
	}
	if length < 0 || int64(dataStart) > p.size-length {
		return 0, fmt.Errorf("%w: xref stream payload [%#x, +%d) outside file", ErrCorrupt, dataStart, length)
	}

	// Decode the records into xbuf; MaxDecompress caps its total.
	xstart := len(p.xbuf)
	budget := p.lim.MaxDecompress - xstart
	if codec.flate {
		sr := io.NewSectionReader(r, int64(dataStart), length)
		p.xbuf, err = d.inflate(p.xbuf, sr, budget, p.lim.Grow)
		if err != nil {
			return 0, fmt.Errorf("xref stream at %#x: %w", off, err)
		}
	} else {
		if length > int64(budget) {
			return 0, fmt.Errorf("%w: xref records of %d bytes", ErrMemoryLimit, length)
		}
		if !p.lim.Grow && length > int64(cap(p.xbuf)-xstart) {
			return 0, fmt.Errorf("%w: xref records of %d bytes over buffer capacity", ErrMemoryLimit, length)
		}
		p.xbuf = append(p.xbuf, make([]byte, length)...)
		if n, err := r.ReadAt(p.xbuf[xstart:], int64(dataStart)); int64(n) != length {
			return 0, fmt.Errorf("piudf: reading xref records %d/%d: %w", n, length, err)
		}
	}
	rows, err := undoPredictor(p.xbuf[xstart:], codec.predictor, codec.columns, codec.colors, codec.bpc)
	if err != nil {
		return 0, err
	}
	p.xbuf = p.xbuf[:xstart+len(rows)]
	if rowlen == 0 || len(rows)%rowlen != 0 {
		return 0, fmt.Errorf("%w: xref stream data of %d bytes is not whole %d-byte rows", ErrCorrupt, len(rows), rowlen)
	}
	nrows := len(rows) / rowlen

	// Subsections come from /Index pairs; the default covers [0, /Size).
	row := 0
	add := func(first, count int64) error {
		if first < 0 || count < 0 || first+count > math.MaxUint32 {
			return fmt.Errorf("%w: xref stream /Index range [%d, +%d)", ErrCorrupt, first, count)
		}
		if row+int(count) > nrows {
			return fmt.Errorf("%w: xref stream /Index needs %d rows, have %d", ErrCorrupt, row+int(count), nrows)
		}
		if count > 0 {
			if !p.lim.Grow && len(p.sections) >= p.lim.MaxXrefSections {
				p.stats.Dropped++
				return fmt.Errorf("%w: more than %d xref subsections", ErrMemoryLimit, p.lim.MaxXrefSections)
			}
			p.sections = append(p.sections, xrefSection{
				firstObj: uint32(first),
				count:    uint32(count),
				fileOff:  int64(xstart + row*rowlen),
				w:        w,
				kind:     sectStream,
			})
		}
		row += int(count)
		return nil
	}
	idxV, err := d.DictGet(p, r, dictV, "Index")
	if err != nil {
		return 0, err
	}
	switch idxV.Kind {
	case KindNull:
		if err := add(0, size); err != nil {
			return 0, err
		}
	case KindArray:
		n, err := d.ArrayLen(p, r, idxV)
		if err != nil {
			return 0, err
		}
		if n%2 != 0 {
			return 0, fmt.Errorf("%w: xref stream /Index has %d elements", ErrCorrupt, n)
		}
		for k := 0; k < n; k += 2 {
			fV, err := d.ArrayIndex(p, r, idxV, k)
			if err != nil {
				return 0, err
			}
			first, err := fV.Int()
			if err != nil {
				return 0, fmt.Errorf("%w: xref stream /Index element", ErrCorrupt)
			}
			cV, err := d.ArrayIndex(p, r, idxV, k+1)
			if err != nil {
				return 0, err
			}
			count, err := cV.Int()
			if err != nil {
				return 0, fmt.Errorf("%w: xref stream /Index element", ErrCorrupt)
			}
			if err := add(first, count); err != nil {
				return 0, err
			}
		}
	default:
		return 0, fmt.Errorf("%w: xref stream /Index is %v", ErrCorrupt, idxV.Kind)
	}

	// The stream dictionary is this revision's trailer.
	if p.trailer.off == 0 {
		p.trailer.off = dictV.I
	}
	if rv, err := d.DictGet(p, r, dictV, "Root"); err == nil && rv.IsRef() && p.trailer.root.IsZero() {
		p.trailer.root = rv.Ref
	}
	if rv, err := d.DictGet(p, r, dictV, "Info"); err == nil && rv.IsRef() && p.trailer.info.IsZero() {
		p.trailer.info = rv.Ref
	}
	if p.trailer.size == 0 {
		p.trailer.size = size
	}
	prevV, err := d.DictGet(p, r, dictV, "Prev")
	if err != nil {
		return 0, err
	}
	switch prevV.Kind {
	case KindInt:
		return prevV.I, nil
	case KindNull:
		return 0, nil
	}
	return 0, fmt.Errorf("%w: xref stream /Prev is not an integer", ErrCorrupt)
}

// readTrailer parses the trailer dictionary following the 'trailer' keyword
// (lexer already positioned there) and records /Root, /Info, /Size and the
// trailer offset in p for lazy re-parsing. Returns /Prev or 0.
func (d *Decoder) readTrailer(p *PDF, r io.ReaderAt) (prev int64, err error) {
	trailerOff := int64(d.lx.Pos())
	d.npb = 0
	v, err := d.parseShallow()
	if err != nil {
		return 0, fmt.Errorf("parsing trailer at %#x: %w", trailerOff, err)
	}
	if v.Kind != KindDict {
		return 0, fmt.Errorf("%w: trailer is not a dictionary at %#x", ErrCorrupt, trailerOff)
	}
	if p.trailer.off == 0 {
		p.trailer.off = trailerOff // Newest trailer wins for Trailer().
	}
	// Capture scalar metadata now so Root/Info/Size need no file later.
	if rv, err := d.DictGet(p, r, v, "Root"); err == nil && rv.IsRef() && p.trailer.root.IsZero() {
		p.trailer.root = rv.Ref
	}
	if rv, err := d.DictGet(p, r, v, "Info"); err == nil && rv.IsRef() && p.trailer.info.IsZero() {
		p.trailer.info = rv.Ref
	}
	if rv, err := d.DictGet(p, r, v, "Size"); err == nil && rv.Kind == KindInt && p.trailer.size == 0 {
		p.trailer.size = rv.I
	}
	rv, err := d.DictGet(p, r, v, "Prev")
	if err != nil {
		return 0, err
	}
	switch rv.Kind {
	case KindInt:
		return rv.I, nil
	case KindNull:
		return 0, nil
	}
	return 0, fmt.Errorf("%w: trailer /Prev is not an integer", ErrCorrupt)
}
