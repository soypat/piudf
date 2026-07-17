package piudf

import (
	"errors"
	"testing"
)

func TestExploreClassic(t *testing.T) {
	b := basicPDF()
	firstXref := b.xrefTrailer(8, " /Root 1 0 R") // 7, 8 free.
	b.obj(6, "5678")
	b.update([]uint32{6}, 9, " /Root 1 0 R", firstXref)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	// Two revisions, newest first, both classic.
	if n := p.NumRevisions(); n != 2 {
		t.Fatalf("NumRevisions = %d, want 2", n)
	}
	newest, oldest := p.Revision(0), p.Revision(1)
	if !newest.Classic || !oldest.Classic {
		t.Errorf("revisions not classic: %+v %+v", newest, oldest)
	}
	if oldest.XrefOff != firstXref {
		t.Errorf("oldest XrefOff = %#x, want %#x", oldest.XrefOff, firstXref)
	}
	if newest.XrefOff <= firstXref {
		t.Errorf("newest XrefOff = %#x not after oldest %#x", newest.XrefOff, firstXref)
	}
	// The update covers exactly one subsection (object 6); revision section
	// ranges must tile the section list.
	if got := newest.EndSection - newest.FirstSection; got != 1 {
		t.Errorf("newest revision spans %d sections, want 1", got)
	}
	if oldest.EndSection != p.NumXrefSections() {
		t.Errorf("oldest EndSection = %d, want %d", oldest.EndSection, p.NumXrefSections())
	}
	s := p.XrefSection(newest.FirstSection)
	if s.First != 6 || s.Count != 1 || !s.Classic || s.FileOff <= 0 {
		t.Errorf("newest section = %+v", s)
	}
	// Each revision's trailer parses at its recorded offset.
	for i := range 2 {
		tr, err := d.ParseValueAt(r, p.Revision(i).TrailerOff)
		if err != nil || tr.Kind != KindDict {
			t.Fatalf("revision %d trailer: %+v, %v", i, tr, err)
		}
		sz, err := d.DictGet(p, r, tr, "Size")
		if err != nil || sz.Kind != KindInt {
			t.Fatalf("revision %d trailer /Size: %+v, %v", i, sz, err)
		}
	}

	// Lookup agrees with Resolve and reports free entries.
	e, err := p.Lookup(r, 1)
	if err != nil || e.Kind != XrefNormal || e.ID != (ObjectID{Num: 1}) {
		t.Fatalf("Lookup(1) = %+v, %v", e, err)
	}
	v, err := d.ParseValueAt(r, e.Offset+len32("1 0 obj\n"))
	if err != nil || v.Kind != KindDict {
		t.Errorf("value at Lookup offset: %+v, %v", v, err)
	}
	if e, err := p.Lookup(r, 7); err != nil || e.Kind != XrefFree {
		t.Errorf("Lookup(7) = %+v, %v; want free", e, err)
	}
	if _, err := p.Lookup(r, 100); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("Lookup(100) = %v, want ErrObjectNotFound", err)
	}
}

func len32(s string) int64 { return int64(len(s)) }

func TestExploreXrefStream(t *testing.T) {
	b, xrefOff := objStmPDF(true, xsFlatePred)
	d, p, r := decodeBuilder(t, b, DecodeLimits{})

	if n := p.NumRevisions(); n != 1 {
		t.Fatalf("NumRevisions = %d, want 1", n)
	}
	rev := p.Revision(0)
	if rev.Classic {
		t.Error("revision reported classic for xref stream")
	}
	if rev.XrefOff != xrefOff {
		t.Errorf("XrefOff = %#x, want %#x", rev.XrefOff, xrefOff)
	}
	// The trailer is the stream dictionary itself.
	tr, err := d.ParseValueAt(r, rev.TrailerOff)
	if err != nil || tr.Kind != KindDict {
		t.Fatalf("trailer: %+v, %v", tr, err)
	}
	if w, err := d.DictGet(p, r, tr, "W"); err != nil || w.Kind != KindArray {
		t.Errorf("trailer /W: %+v, %v", w, err)
	}
	// Stream sections have no file coordinates.
	for i := rev.FirstSection; i < rev.EndSection; i++ {
		if s := p.XrefSection(i); s.Classic || s.FileOff != -1 {
			t.Errorf("section %d = %+v, want stream form", i, s)
		}
	}
	// Compressed entries expose their stream and index.
	e, err := p.Lookup(r, 1)
	if err != nil || e.Kind != XrefCompressed || e.Stream != 3 || e.Index != 0 {
		t.Fatalf("Lookup(1) = %+v, %v", e, err)
	}
	e, err = p.Lookup(r, 5)
	if err != nil || e.Kind != XrefCompressed || e.Stream != 3 || e.Index != 2 {
		t.Fatalf("Lookup(5) = %+v, %v", e, err)
	}
	if e, err := p.Lookup(r, 2); err != nil || e.Kind != XrefNormal {
		t.Fatalf("Lookup(2) = %+v, %v", e, err)
	}
}
