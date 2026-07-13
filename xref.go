package piudf

import (
	"bytes"
	"fmt"
	"io"
)

const (
	sectClassic = iota // 20-byte fixed records in the file itself.
	sectStream         // PDF 1.5+ cross-reference stream (not yet supported).
)

const (
	recFree       = iota // Free-list entry: object does not exist.
	recNormal            // Uncompressed object at a file offset.
	recCompressed        // Object inside an object stream (not yet supported).
)

// classicRecLen is the record length mandated by ISO 32000-1 7.5.4:
// 10-digit offset, space, 5-digit generation, space, keyword, 2-byte EOL.
const classicRecLen = 20

// xrefSection describes one cross-reference subsection without materializing
// its entries: the on-disk table is already a random-access array of
// fixed-width records, so lookups read single records via ReadAt. Memory
// cost is O(number of subsections), independent of object count.
type xrefSection struct {
	firstObj uint32
	count    uint32
	fileOff  int64    // Absolute offset of the first record.
	w        [3]uint8 // Field widths; classic is {10, 5, 1}. Ready for /W of xref streams.
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
// Sections are stored newest-revision-first so the first section containing
// num shadows all older ones.
func (d *Decoder) lookupXref(num uint32) (xrefRecord, error) {
	rec := d.recbuf[:classicRecLen]
	for i := range d.sections {
		s := &d.sections[i]
		if num < s.firstObj || num >= s.firstObj+s.count {
			continue
		}
		recOff := s.fileOff + classicRecLen*int64(num-s.firstObj)
		n, err := d.r.ReadAt(rec, recOff)
		if n != len(rec) {
			return xrefRecord{}, fmt.Errorf("piudf: reading xref record %d/%d at %#x: %w", n, len(rec), recOff, err)
		}
		return parseClassicRecord(rec, recOff)
	}
	return xrefRecord{}, ErrObjectNotFound
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
// startxref, recording section descriptors and trailer metadata. Only
// classic tables are supported; encountering a cross-reference stream
// (PDF 1.5+) fails with ErrUnsupported.
func (d *Decoder) readXrefChain(startOff int64) error {
	off := startOff
	for range maxXrefChain {
		if off < 0 || off >= d.size {
			return fmt.Errorf("%w: xref offset %#x out of file bounds", ErrCorrupt, off)
		}
		prev, err := d.readXrefTable(off)
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
// off, appending section descriptors. Returns the /Prev offset or 0.
func (d *Decoder) readXrefTable(off int64) (prev int64, err error) {
	lx := &d.lx
	if err := lx.Reset(d.r, off); err != nil {
		return 0, err
	}
	tok, pos, _ := lx.NextToken()
	if tok == TokInt {
		// An "N G obj" header here means a cross-reference stream.
		return 0, fmt.Errorf("%w: cross-reference streams (PDF 1.5+) at %#x", ErrUnsupported, off)
	}
	if tok != TokXref {
		return 0, &SyntaxError{Off: pos, Msg: "expected 'xref' keyword"}
	}
	for {
		tok, pos, lit := lx.NextToken()
		switch tok {
		case TokTrailer:
			return d.readTrailer()
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
				d.sections = append(d.sections, xrefSection{
					firstObj: uint32(first),
					count:    uint32(count),
					fileOff:  recOff,
					w:        [3]uint8{10, 5, 1},
					kind:     sectClassic,
				})
			}
			// Jump over the fixed-width record area instead of tokenizing it.
			if err := lx.Reset(d.r, recOff+classicRecLen*count); err != nil {
				return 0, err
			}
		default:
			return 0, &SyntaxError{Off: pos, Msg: "expected subsection header or 'trailer'"}
		}
	}
}

// readTrailer parses the trailer dictionary following the 'trailer' keyword
// (lexer already positioned there) and records /Root, /Info, /Size and the
// trailer offset for lazy re-parsing. Returns /Prev or 0.
func (d *Decoder) readTrailer() (prev int64, err error) {
	trailerOff := int64(d.lx.Pos())
	d.resetScratch()
	v, err := d.parseNext(0)
	if err != nil {
		return 0, fmt.Errorf("parsing trailer at %#x: %w", trailerOff, err)
	}
	if v.Kind != KindDict {
		return 0, fmt.Errorf("%w: trailer is not a dictionary at %#x", ErrCorrupt, trailerOff)
	}
	if d.trailer.off == 0 {
		d.trailer.off = trailerOff // Newest trailer wins for Trailer().
	}
	// Capture scalar metadata now: the values arena is scratch and this
	// dict's span dies on the next parse.
	if r, err := d.DictGet(v, "Root"); err == nil && r.IsRef() && d.trailer.root.IsZero() {
		d.trailer.root = r.Ref
	}
	if r, err := d.DictGet(v, "Info"); err == nil && r.IsRef() && d.trailer.info.IsZero() {
		d.trailer.info = r.Ref
	}
	if r, err := d.DictGet(v, "Size"); err == nil && r.Kind == KindInt && d.trailer.size == 0 {
		d.trailer.size = r.I
	}
	r, err := d.DictGet(v, "Prev")
	if err != nil {
		return 0, err
	}
	switch r.Kind {
	case KindInt:
		return r.I, nil
	case KindNull:
		return 0, nil
	}
	return 0, fmt.Errorf("%w: trailer /Prev is not an integer", ErrCorrupt)
}
