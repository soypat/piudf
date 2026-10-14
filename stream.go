package piudf

import (
	"errors"
	"io"

	"github.com/soypat/piudf/internal/zlib"
)

// Stream reads one span of a file as a byte stream, inflating it when the span
// holds FlateDecode data. It is the counterpart of [Window]: a window serves
// bytes that are already there, a stream makes bytes that are not.
//
// Nothing decoded is retained. That is the point — the decompressed data of a
// PDF stream is unbounded and the file it came from is not, so a stream costs
// its inflate window whatever the payload's size. What it keeps instead is
// where it stopped, which is what makes the seek [Stream.ReadAt] promises
// affordable: flate has no random access, so reading forward continues the
// open inflate and reading backward starts it over.
//
// The zero Stream is unbound; Reset binds it. A bound Stream may be Reset to
// another span any number of times and reuses its inflate window across all of
// them: that window is the one allocation it makes, and it makes it once.
type Stream struct {
	span spanReader
	// zr inflates span. It owns its working memory (see zrReady) and is reused
	// across every Reset, so a rewind re-inflates without allocating.
	zr zlib.Reader
	// InflateConfig is the memory zr works out of, set before the first
	// compressed span. Any buffer left nil is allocated with a safe default on
	// first use (see inflate); the SkipChecksum flag is honored regardless.
	// InflateConfig zlib.Config

	// zrReady reports that zr has been configured; its buffers are allocated
	// once, on the first compressed span.
	// zrReady bool

	// rd is span decoded: the inflater, or span itself when uncompressed. A
	// nil rd means unbound.
	rd io.Reader
	r  io.ReaderAt
	// off and length are the span in the file, kept so a rewind can reopen it.
	off, length int64
	compressed  bool
	// pos is the decoded offset of the next byte rd returns.
	pos int64
	// sink absorbs the bytes a forward seek skips. They are decoded and
	// dropped: reaching an offset means decoding everything before it.
	sink [256]byte
	// pred undoes a predictor over the inflated bytes when predicted is set,
	// with the parameters in sc.
	pred      predictReader
	sc        streamCodec
	predicted bool
}

func (strm *Stream) ConfigureDefault() {
	strm.Configure(zlib.DefaultConfig())
}

func (strm *Stream) Configure(config zlib.Config) error {
	return strm.zr.Configure(config)
}

// ErrStreamUnbound is returned by reads on a Stream that Reset has not bound
// to a span, or whose last Reset failed.
var ErrStreamUnbound = errors.New("piudf: read from unbound stream")

// Reset binds s to the span [off, off+length) of r and opens it at its first
// decoded byte. compressed declares the span to be a zlib stream (RFC 1950),
// which is what PDF's FlateDecode filter writes; an uncompressed span is
// passed through.
//
// Resetting to the span already bound reopens it, which is the only way back
// to a byte already read.
func (s *Stream) Reset(r io.ReaderAt, off, length int64, compressed bool) error {
	if r == nil {
		return errors.New("piudf: nil reader")
	} else if off < 0 || length < 0 {
		return errors.New("piudf: negative span")
	}
	s.r, s.off, s.length, s.compressed = r, off, length, compressed
	s.predicted = false
	return s.Rewind()
}

// resetCodec binds s to a payload span decoded per sc, whose chain must be
// empty or a lone FlateDecode: a predictor in sc is undone over the inflated
// bytes.
func (s *Stream) resetCodec(r io.ReaderAt, off, length int64, sc *streamCodec) error {
	if err := s.Reset(r, off, length, sc.flate()); err != nil {
		return err
	}
	if sc.predictor <= 1 {
		return nil
	}
	s.sc, s.predicted = *sc, true
	return s.Rewind()
}

// Rewind reopens the bound span at its first decoded byte.
func (s *Stream) Rewind() error {
	if s.r == nil {
		return ErrStreamUnbound
	}
	s.span = spanReader{r: s.r, off: s.off, end: s.off + s.length}
	s.pos = 0
	if !s.compressed {
		s.rd = &s.span
		return nil
	}
	rd, err := s.inflate(&s.span)
	if err != nil {
		s.rd = nil // Not bound to anything readable.
		return err
	}
	s.rd = rd
	if s.predicted {
		if err = s.pred.reset(rd, &s.sc); err != nil {
			s.rd = nil
			return err
		}
		s.rd = &s.pred
	}
	return nil
}

// Reader returns the file s is bound to, or nil. Offset returns the file
// offset of its span. Together they say what s is reading, so a caller holding
// one Stream for many spans can ask rather than remember. Comparing the
// returned reader requires its dynamic type to be comparable; see
// [Window.Reset].
func (s *Stream) Reader() io.ReaderAt { return s.r }

// Offset returns the file offset of the bound span's first encoded byte. It is
// a file coordinate and unrelated to [Stream.Pos], which counts decoded bytes.
func (s *Stream) Offset() int64 { return s.off }

// Pos returns the decoded offset of the next byte Read returns: how far into
// the decompressed data the stream stands, not where it is in the file.
func (s *Stream) Pos() int64 { return s.pos }

// Read reads the decoded bytes in order, reporting io.EOF at the end of the
// span.
func (s *Stream) Read(b []byte) (int, error) {
	if s.rd == nil {
		return 0, ErrStreamUnbound
	}
	n, err := s.rd.Read(b)
	s.pos += int64(n)
	return n, err
}

// ReadAt reads the decoded bytes at off, seeking there first. The seek is what
// costs: forward, it decodes and discards everything between; backward, it
// rewinds and decodes from the payload's first byte. Callers that read
// forward — which is what lexing a stream does — pay neither.
//
// It satisfies io.ReaderAt with one documented deviation: concurrent calls are
// not safe, because a Stream is a position and there is only one.
func (s *Stream) ReadAt(b []byte, off int64) (int, error) {
	if s.rd == nil {
		return 0, ErrStreamUnbound
	}
	if off < 0 {
		return 0, errors.New("piudf: negative offset")
	}
	if off < s.pos {
		// The bytes are behind us and gone; only the decode can make them
		// again.
		if err := s.Rewind(); err != nil {
			return 0, err
		}
	}
	for s.pos < off {
		skip := s.sink[:]
		if d := off - s.pos; int64(len(skip)) > d {
			skip = skip[:d]
		}
		if _, err := s.Read(skip); err != nil {
			return 0, err
		}
	}
	n, err := io.ReadFull(s, b)
	if err == io.ErrUnexpectedEOF {
		// A short read at the end of the data. ReadAt reports that as io.EOF,
		// which is how a Window learns where the data stops.
		err = io.EOF
	}
	return n, err
}

// inflate points s's zlib reader at src, configuring it (its one allocation)
// on first use and reusing it for every span after. The reader owns its inflate
// window, overflow tables and read buffer, so a rewind re-inflates without
// allocating — unlike compress/zlib, which reallocates its Huffman tables on
// every dynamic block.
func (s *Stream) inflate(src io.Reader) (io.Reader, error) {
	if err := s.zr.Reset(src); err != nil {
		return nil, err
	}
	return &s.zr, nil
}

// spanReader reads one file span as a stream. It is a field of the Stream that
// uses it and never escapes, which is the point: an io.SectionReader per span
// would be an allocation per stream.
type spanReader struct {
	r   io.ReaderAt
	off int64
	end int64
}

func (s *spanReader) Read(b []byte) (int, error) {
	if s.off >= s.end {
		return 0, io.EOF
	}
	if rem := s.end - s.off; int64(len(b)) > rem {
		b = b[:rem]
	}
	n, err := s.r.ReadAt(b, s.off)
	s.off += int64(n)
	return n, err
}

// maxPredictRow bounds one predictor row. It admits every 16-bit RGBA or 8-bit
// CMYK row up to the 65535 columns /Columns can name; a wider row is a
// DeviceN image nobody draws or a corrupt dictionary, and either way not worth
// half a megabyte per row.
const maxPredictRow = 1 << 19

// predictReader undoes a /Predictor (ISO 32000-1 7.4.4.4) over the bytes a
// Flate or LZW filter decoded. Predictors are row transforms: a PNG row is
// expressed against the row above, a TIFF row against the pixel to its left,
// so it holds two rows and nothing more, whatever the image's height.
type predictReader struct {
	src io.Reader
	// bufs holds the previous and current reconstructed rows, rowlen each.
	bufs   []byte
	rowlen int
	// bpp is the PNG bytes-per-pixel, the distance to a pixel's left
	// neighbour, rounded up to one for sub-byte samples (RFC 2083 section 6).
	bpp int
	// which is the index of the current row in bufs, and off..end the part of
	// it Read has yet to return. A short final row has end < rowlen.
	which, off, end int
	png             bool
	colors, bpc     uint8
	// eof is set once the source has ended, possibly mid-row.
	eof bool
	ft  [1]byte
}

// predictRowLen returns the byte length of one predictor row of sc, or zero
// when the parameters cannot describe one.
func predictRowLen(sc *streamCodec) int {
	bits := int(sc.columns) * int(sc.colors) * int(sc.bpc)
	return (bits + 7) / 8
}

// reset binds p to src with sc's parameters, keeping the row storage of an
// earlier binding when it is large enough. That storage is the one allocation
// a predictor makes.
func (p *predictReader) reset(src io.Reader, sc *streamCodec) error {
	rowlen := predictRowLen(sc)
	switch {
	case rowlen <= 0:
		return errBadPredictor
	case rowlen > maxPredictRow:
		return ErrCodecMemLimit
	case sc.predictor == 2:
		if sc.bpc != 8 && sc.bpc != 16 {
			return errTODO // TODO: TIFF predictor on sub-byte samples.
		}
		p.png = false
	case sc.predictor >= 10 && sc.predictor <= 15:
		p.png = true
	default:
		return errBadPredictor // Values the format does not define.
	}
	if cap(p.bufs) < 2*rowlen {
		p.bufs = make([]byte, 2*rowlen)
	}
	p.bufs = p.bufs[:2*rowlen]
	// The row above the first is all zeros (ISO 32000-1 7.4.4.4).
	clear(p.bufs)
	p.src, p.rowlen = src, rowlen
	p.bpp = max(1, int(sc.colors)*int(sc.bpc)/8)
	p.colors, p.bpc = sc.colors, sc.bpc
	p.which, p.off, p.end, p.eof = 0, 0, 0, false
	return nil
}

func (p *predictReader) row(i int) []byte { return p.bufs[i*p.rowlen : (i+1)*p.rowlen] }

// Read returns the reconstructed rows' bytes in order. A final row cut short by
// the source is returned as far as it goes: the samples it holds decode
// correctly, and refusing them would lose a whole image to one missing row.
func (p *predictReader) Read(b []byte) (n int, err error) {
	for n < len(b) {
		if p.off == p.end {
			if err = p.next(); err != nil {
				return n, err
			}
		}
		c := copy(b[n:], p.row(p.which)[p.off:p.end])
		p.off += c
		n += c
	}
	return n, nil
}

// next reconstructs the row after the current one.
func (p *predictReader) next() error {
	if p.eof {
		return io.EOF
	}
	prev := p.row(p.which)
	cur := p.row(1 - p.which)
	if p.png {
		// Each PNG row is preceded by the filter it was written with.
		if _, err := io.ReadFull(p.src, p.ft[:]); err != nil {
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			return err
		}
	}
	n, err := io.ReadFull(p.src, cur)
	switch {
	case err == io.EOF && !p.png:
		return io.EOF // Clean end between rows.
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		p.eof = true
		if n == 0 {
			return io.EOF
		}
	case err != nil:
		return err
	}
	if p.png {
		// A short row is unfiltered against the matching prefix of prev,
		// which is all a prefix of a PNG row depends on.
		if err := unfilterPNG(cur[:n], prev[:n], p.ft[0], p.bpp); err != nil {
			return err
		}
	} else {
		unpredictTIFF(cur[:n], int(p.colors), p.bpc)
	}
	p.which = 1 - p.which
	p.off, p.end = 0, n
	return nil
}

// unpredictTIFF undoes TIFF predictor 2 on row in place: each sample is stored
// as its difference from the same component of the pixel to its left. bpc is 8
// or 16; 16-bit samples are big-endian, as everywhere in PDF.
func unpredictTIFF(row []byte, colors int, bpc uint8) {
	if bpc == 8 {
		for i := colors; i < len(row); i++ {
			row[i] += row[i-colors]
		}
		return
	}
	stride := 2 * colors
	for i := stride; i+1 < len(row); i += 2 {
		v := uint16(row[i])<<8 | uint16(row[i+1])
		left := uint16(row[i-stride])<<8 | uint16(row[i-stride+1])
		v += left
		row[i], row[i+1] = byte(v>>8), byte(v)
	}
}

// unfilterPNG reconstructs row cur in place from the row filter ft and the row
// above it (RFC 2083 section 6.3). bpp is the distance in bytes to the
// corresponding byte of the pixel to the left; a cross-reference stream's rows
// are 8-bit single-component, so it passes one.
//
// The filters chain — each row is expressed against the one above — which is
// why whoever reads them cannot skip.
func unfilterPNG(cur, prev []byte, ft byte, bpp int) error {
	switch ft {
	case 0: // None.
	case 1: // Sub.
		for i := bpp; i < len(cur); i++ {
			cur[i] += cur[i-bpp]
		}
	case 2: // Up.
		for i := range cur {
			cur[i] += prev[i]
		}
	case 3: // Average.
		for i := range cur {
			var a byte
			if i >= bpp {
				a = cur[i-bpp]
			}
			cur[i] += byte((int(a) + int(prev[i])) / 2)
		}
	case 4: // Paeth.
		for i := range cur {
			var a, c byte // Left and up-left reconstructed bytes.
			if i >= bpp {
				a = cur[i-bpp]
				c = prev[i-bpp]
			}
			cur[i] += paeth(a, prev[i], c)
		}
	default:
		return errBadPredictor
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
