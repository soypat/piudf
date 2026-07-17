package piudf

import (
	"errors"
	"fmt"
	"io"
)

// This file is the read-only introspection API: exported views of the
// cross-reference machinery for tools that examine file layout (see
// cmd/piudf) rather than document content.

// XrefKind discriminates cross-reference record types.
type XrefKind uint8

const (
	// XrefFree marks a free-list entry: the object does not exist.
	XrefFree XrefKind = iota
	// XrefNormal marks an uncompressed object stored at a file offset.
	XrefNormal
	// XrefCompressed marks an object stored inside an object stream.
	XrefCompressed
)

// String returns the record type name.
func (k XrefKind) String() string {
	switch k {
	case XrefFree:
		return "free"
	case XrefNormal:
		return "normal"
	case XrefCompressed:
		return "compressed"
	}
	return "invalid"
}

// XrefEntry is the decoded cross-reference record of one object.
type XrefEntry struct {
	// ID is the object number looked up and the generation recorded for it.
	ID ObjectID
	// Offset is the absolute file offset of the object header for
	// XrefNormal entries.
	Offset int64
	// Stream is the object stream containing the object for
	// XrefCompressed entries.
	Stream uint32
	// Index is the object's position within that stream.
	Index int64
	// Kind discriminates which of the fields above are meaningful.
	Kind XrefKind
}

// Lookup returns the newest cross-reference record for object num, reading
// record bytes from r for classic tables and from the decoded stream rows
// otherwise. Unlike Resolve it does not touch the object itself and also
// reports free entries instead of failing.
func (p *PDF) Lookup(r io.ReaderAt, num uint32) (XrefEntry, error) {
	if p.size == 0 {
		return XrefEntry{}, errNotDecoded
	}
	rec, err := p.lookupXref(r, num)
	if err != nil {
		return XrefEntry{}, err
	}
	e := XrefEntry{ID: ObjectID{Num: num, Gen: rec.gen}}
	switch rec.kind {
	case recNormal:
		e.Kind = XrefNormal
		e.Offset = rec.offset
	case recCompressed:
		e.Kind = XrefCompressed
		e.Stream = rec.stream
		e.Index = rec.offset
	default:
		e.Kind = XrefFree
	}
	return e, nil
}

// XrefSection is the exported view of one cross-reference subsection
// descriptor, covering object numbers [First, First+Count).
type XrefSection struct {
	First uint32
	Count uint32
	// Classic reports a classic table subsection; FileOff is then the
	// absolute file offset of its first 20-byte record. Subsections of
	// cross-reference streams (Classic false) have no file coordinates —
	// their records exist only decoded in memory — and FileOff is -1.
	Classic bool
	FileOff int64
}

// NumXrefSections returns the number of cross-reference subsections,
// ordered newest revision first.
func (p *PDF) NumXrefSections() int { return len(p.sections) }

// XrefSection returns subsection i; see NumXrefSections for the order.
func (p *PDF) XrefSection(i int) XrefSection {
	s := &p.sections[i]
	x := XrefSection{First: s.firstObj, Count: s.count, Classic: s.kind == sectClassic, FileOff: s.fileOff}
	if !x.Classic {
		x.FileOff = -1
	}
	return x
}

// Revision describes one step of the cross-reference chain: a classic
// table or cross-reference stream together with its trailer. Revisions are
// ordered newest first, mirroring how incremental updates shadow older
// ones; the oldest revision is the original document.
type Revision struct {
	// XrefOff is the absolute file offset of the 'xref' keyword or the
	// cross-reference stream's object header.
	XrefOff int64
	// TrailerOff is the offset of the trailer dictionary text: past the
	// 'trailer' keyword for classic tables, the dictionary itself for
	// cross-reference streams. Parse it with Decoder.ParseValueAt.
	TrailerOff int64
	// Classic reports a classic table revision.
	Classic bool
	// FirstSection and EndSection delimit this revision's subsections as
	// indices for PDF.XrefSection: [FirstSection, EndSection).
	FirstSection int
	EndSection   int
}

// NumRevisions returns the length of the cross-reference chain.
func (p *PDF) NumRevisions() int { return len(p.revs) }

// Revision returns chain step i, 0 being the newest revision.
func (p *PDF) Revision(i int) Revision {
	rv := &p.revs[i]
	end := len(p.sections)
	if i+1 < len(p.revs) {
		end = p.revs[i+1].firstSection
	}
	return Revision{
		XrefOff:      rv.xrefOff,
		TrailerOff:   rv.trailerOff,
		Classic:      rv.classic,
		FirstSection: rv.firstSection,
		EndSection:   end,
	}
}

// Size returns the file size in bytes passed to Decode.
func (p *PDF) Size() int64 { return p.size }

// DictLen returns the number of key-value pairs of dictionary (or stream
// dict) dictVal by scanning its span. Exploration API: content lookups
// should use DictGet.
func (d *Decoder) DictLen(p *PDF, r io.ReaderAt, dictVal Value) (int, error) {
	n := -1
	_, _, err := d.dictScan(p, r, dictVal, -1, &n)
	return n, err
}

// DictIndex returns key-value pair i of dictVal in span order: cost O(i).
func (d *Decoder) DictIndex(p *PDF, r io.ReaderAt, dictVal Value, i int) (key, val Value, err error) {
	if i < 0 {
		return Value{}, Value{}, errors.New("piudf: negative dictionary index")
	}
	return d.dictScan(p, r, dictVal, i, nil)
}

// dictScan walks dictVal's pairs; with want >= 0 it returns pair want,
// with count != nil it scans to the end storing the pair count.
func (d *Decoder) dictScan(p *PDF, r io.ReaderAt, dictVal Value, want int, count *int) (key, val Value, err error) {
	if dictVal.Kind != KindDict && dictVal.Kind != KindStream {
		return Value{}, Value{}, errKindMismatch
	}
	if err := d.lexValueSpan(p, r, dictVal); err != nil {
		return Value{}, Value{}, err
	}
	tok, pos, _ := d.lx.NextToken()
	if tok != TokDictOpen {
		return Value{}, Value{}, &SyntaxError{Off: pos, Msg: "dict span does not start with <<"}
	}
	for n := 0; ; n++ {
		tv, err := d.next()
		if err != nil {
			return Value{}, Value{}, err
		}
		if tv.tok == TokDictClose {
			if count != nil {
				*count = n
			}
			return Value{}, Value{}, errIfMissing(want >= 0)
		}
		if tv.tok != TokName {
			return Value{}, Value{}, &SyntaxError{Off: tv.pos, Msg: "expected name as dictionary key"}
		}
		k := tv.val
		v, err := d.parseShallow()
		if err != nil {
			return Value{}, Value{}, err
		}
		if n == want {
			return tagObjStm(k, dictVal.Ref.Num), tagObjStm(v, dictVal.Ref.Num), nil
		}
	}
}

func errIfMissing(missing bool) error {
	if missing {
		return errors.New("piudf: dictionary index out of range")
	}
	return nil
}

// ParseValueAt shallow-parses the single value whose text starts at
// absolute file offset off: scalars are returned inline, composites and
// text as raw file spans, exactly like Resolve does for object bodies.
func (d *Decoder) ParseValueAt(r io.ReaderAt, off int64) (Value, error) {
	if err := d.lexAt(r, off); err != nil {
		return Value{}, err
	}
	v, err := d.parseShallow()
	if err != nil {
		return Value{}, fmt.Errorf("parsing value at %#x: %w", off, err)
	}
	return v, nil
}
