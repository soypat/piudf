package ppdf

import (
	"io"

	"github.com/soypat/piudf/ppdf/internal"
)

// maxXrefRowLen bounds one decoded row. readWidths caps each of the three /W
// fields at 8 bytes, so a row cannot exceed this and the cursor's rows need no
// allocation.
const maxXrefRowLen = 24

// xrefRows is a cursor over the decoded rows of one cross-reference stream.
// Flate has no random access and the PNG row filters chain each row to the one
// above it, so a row is reachable only by decoding every row before it. The
// cursor keeps that decode open at the row it stopped on: a forward step
// continues it and a backward one starts the stream over.
//
// It therefore holds two rows and never the table. That is the difference from
// the root package, which decodes every row of every cross-reference stream
// into one buffer and keeps it for the document's life: O(objects) held to make
// lookups O(1). Here a sweep in object order — which is what walking an xref
// costs — is still one decode pass, because object order is row order.
type xrefRows struct {
	data internal.Stream
	// row is the index of the row in buf[which]; -1 before the first is
	// decoded.
	row     int64
	rowlen  int
	predict bool
	// which selects the current row. The other slot holds the row above it,
	// which the PNG filters need, so the two alternate rather than copy.
	which int
	buf   [2][maxXrefRowLen]byte
	// ft holds the row's filter byte. It is a field rather than a local
	// because io.ReadFull takes an io.Reader: the slice crosses an interface
	// call, so a local would escape and cost an allocation per row.
	ft [1]byte
}

// at returns row i of section s's stream. The returned bytes live in x and die
// at the next call.
func (x *xrefRows) at(r io.ReaderAt, s *xrefSection, i int64) ([]byte, error) {
	if x.data.Reader() != r || x.data.Offset() != s.fileOff || i < x.row {
		// A different file, a different stream, or a step backward: none can
		// be served by the open decode. The payload offset identifies the
		// stream, since every subsection of one shares it.
		if err := x.restart(r, s); err != nil {
			return nil, err
		}
	}
	for x.row < i {
		if err := x.next(); err != nil {
			return nil, err
		}
	}
	return x.buf[x.which][:x.rowlen], nil
}

// restart points the cursor at s's payload from its first byte.
func (x *xrefRows) restart(r io.ReaderAt, s *xrefSection) error {
	sc := &s.codec
	rowlen := sc.rowLen()
	if rowlen <= 0 || rowlen > maxXrefRowLen {
		return errXrefStreamBad
	}
	x.row, x.rowlen, x.which, x.predict = -1, int(rowlen), 0, false
	// Cleared, not merely rewound: the row above the first is all zeros
	// (ISO 32000-1 7.4.4.4), which is what buf[which] must hold going in.
	x.buf = [2][maxXrefRowLen]byte{}
	switch {
	case sc.predictor <= 1:
	case sc.predictor >= 10 && sc.predictor <= 15:
		if sc.bpc != 8 || sc.colors != 1 || int64(sc.columns) != rowlen {
			// A predictor row and a table row must be the same bytes; the row
			// index means nothing otherwise. The root package assumes this
			// silently by indexing its decoded buffer at the /W row length.
			return errXrefStreamBad
		}
		x.predict = true
	default:
		return errTODO // TIFF predictor 2, or a value the format does not define.
	}
	return x.data.Reset(r, s.fileOff, s.length, sc.flate)
}

// next decodes the row after the current one, which is the only way forward.
func (x *xrefRows) next() error {
	cur := x.buf[1-x.which][:x.rowlen]
	if x.predict {
		// Each PNG row is preceded by the filter it was written with;
		// /Predictor declares only that they are present, not which.
		if _, err := io.ReadFull(&x.data, x.ft[:]); err != nil {
			return rowReadErr(err)
		}
		if _, err := io.ReadFull(&x.data, cur); err != nil {
			return rowReadErr(err)
		}
		if err := unfilterPNG(cur, x.buf[x.which][:x.rowlen], x.ft[0]); err != nil {
			return err
		}
	} else if _, err := io.ReadFull(&x.data, cur); err != nil {
		return rowReadErr(err)
	}
	x.row++
	x.which = 1 - x.which
	return nil
}

// rowReadErr reports a payload that ran out before the row asked for. The
// section says the row exists, so the file disagrees with its own xref.
func rowReadErr(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return errXrefStreamBad
	}
	return err
}
