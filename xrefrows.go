package ppdf

import (
	"io"

	"github.com/soypat/piudf/internal"
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
	// cache is the caller's, from DecoderConfig, and holds rows [0, cached) of
	// the stream the cursor is on: a prefix, because that is what a forward
	// decode produces and a prefix is what a bounds check can serve. Rows past
	// it — or all of them, when the caller supplied none — cost the decode
	// they always did.
	cache  []byte
	cached int64
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

// setCache hands x the caller's row storage. What is cached describes the
// stream the cursor was on, so replacing the storage forgets it.
func (x *xrefRows) setCache(b []byte) {
	x.cache, x.cached = b, 0
}

// at returns row i of section s's stream. The returned bytes live in x and die
// at the next call.
func (x *xrefRows) at(r io.ReaderAt, s *xrefSection, i int64) ([]byte, error) {
	// The payload offset identifies the stream, since every subsection of one
	// shares it.
	if x.data.Reader() != r || x.data.Offset() != s.fileOff {
		// A different file or a different stream: the cache describes neither.
		x.cached = 0
		if err := x.restart(r, s); err != nil {
			return nil, err
		}
	} else if i < x.cached {
		return x.cache[i*int64(x.rowlen):][:x.rowlen], nil
	} else if i < x.row {
		// A step backward past what is cached: the bytes are gone and only the
		// decode can make them again. The cache survives — it holds the same
		// stream's rows, and they have not changed.
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
	// Keep it if the caller gave room. Only the row extending the prefix is
	// kept: a hole would make a hit a search rather than a bounds check, and
	// the rows arrive in prefix order anyway.
	if x.row == x.cached {
		if off := x.row * int64(x.rowlen); off+int64(x.rowlen) <= int64(len(x.cache)) {
			copy(x.cache[off:], x.buf[x.which][:x.rowlen])
			x.cached++
		}
	}
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

// === Predictor logic ===

// unfilterPNG reconstructs row cur in place from the row filter ft and the row
// above it (RFC 2083 section 6.3). The bytes-per-pixel is one: only 8-bit
// single-component rows reach here, which is every cross-reference stream.
//
// The filters chain — each row is expressed against the one above — which is
// why whoever reads them cannot skip.
func unfilterPNG(cur, prev []byte, ft byte) error {
	const bpp = 1
	for i := range cur {
		var a, c byte // Left and up-left reconstructed bytes.
		if i >= bpp {
			a = cur[i-bpp]
			c = prev[i-bpp]
		}
		b := prev[i] // Up.
		switch ft {
		case 0: // None.
		case 1: // Sub.
			cur[i] += a
		case 2: // Up.
			cur[i] += b
		case 3: // Average.
			cur[i] += byte((int(a) + int(b)) / 2)
		case 4: // Paeth.
			cur[i] += paeth(a, b, c)
		default:
			return errXrefStreamBad
		}
	}
	return nil
}

// paeth is the PNG Paeth predictor function (RFC 2083 section 6.6).
func paeth(a, b, c byte) byte {
	pa := int(b) - int(c)
	pb := int(a) - int(c)
	pc := pa + pb
	if pa < 0 {
		pa = -pa
	}
	if pb < 0 {
		pb = -pb
	}
	if pc < 0 {
		pc = -pc
	}
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}
