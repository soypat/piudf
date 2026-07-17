package internal

import (
	"bufio"
	"compress/zlib"
	"errors"
	"io"
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
	// bsrc buffers span for the inflater, which reads a byte at a time.
	bsrc *bufio.Reader
	zr   zlibReader
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

// inflate points s's zlib reader at src, allocating its window on first use
// and reusing it for every span after. src goes through a bufio.Reader because
// compress/flate reads a byte at a time and wraps any source lacking ReadByte
// in a bufio.Reader of its own, per call.
func (s *Stream) inflate(src io.Reader) (io.Reader, error) {
	if s.bsrc == nil {
		s.bsrc = bufio.NewReader(src)
	} else {
		s.bsrc.Reset(src)
	}
	if s.zr == nil {
		zr, err := zlib.NewReader(s.bsrc)
		if err != nil {
			return nil, err
		}
		s.zr = zr.(zlibReader)
		return s.zr, nil
	}
	return s.zr, s.zr.Reset(s.bsrc, nil)
}

// zlibReader is an inflater that can be pointed at a new source without
// reallocating its window.
type zlibReader interface {
	io.ReadCloser
	zlib.Resetter
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
