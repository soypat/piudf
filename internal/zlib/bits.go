package zlib

import "io"

// bitReader serves the deflate bit stream from a caller-supplied byte buffer,
// bulk-filled from an io.Reader. It replaces the bufio.Reader compress/flate
// wraps every non-ByteReader source in — that wrapper is one of the per-Reset
// allocations this package exists to remove.
//
// Bits accumulate low-to-high in b; nb counts how many are valid. buf[r:w] holds
// bytes fetched from src but not yet drawn into b. The invariant the aligned
// readers below rely on: at every byte-aligned point in the format (a stored
// block's header, its payload, the zlib trailer) nb < 8, so b holds at most the
// tail of one already-consumed byte and buf[r:w] begins exactly at the next
// aligned byte.
type bitReader struct {
	src     io.Reader
	buf     []byte // caller-owned fill buffer
	r, w    int    // buf[r:w] is fetched-but-unconsumed
	b       uint32 // pending bits, low bits first
	nb      uint   // number of valid bits in b
	eofErr  error  // error to report once buf drains (src returned data and an error together)
	roffset int64
}

// reset binds br to src, keeping the fill buffer. It reads no bytes.
func (br *bitReader) reset(src io.Reader) {
	br.src = src
	br.r, br.w = 0, 0
	br.b, br.nb = 0, 0
	br.eofErr = nil
	br.roffset = 0
}

// fill refills buf when empty. It returns io.EOF only when the source is
// exhausted with no bytes delivered; a source that returns data and an error
// together has the error deferred until the delivered bytes are consumed.
func (br *bitReader) fill() error {
	if br.r < br.w {
		return nil
	}
	if br.eofErr != nil {
		err := br.eofErr
		br.eofErr = nil
		return err
	}
	for {
		n, err := br.src.Read(br.buf)
		if n > 0 {
			br.r, br.w = 0, n
			if err != nil {
				br.eofErr = err
			}
			return nil
		}
		if err != nil {
			return err
		}
		// Read returned (0, nil): permitted but unusual; ask again.
	}
}

// readByte returns the next whole byte of the stream.
func (br *bitReader) readByte() (byte, error) {
	if br.r >= br.w {
		if err := br.fill(); err != nil {
			return 0, err
		}
	}
	c := br.buf[br.r]
	br.r++
	br.roffset++
	return c, nil
}

// readRaw fills p with whole bytes, from the buffer first and then the source,
// without touching the bit accumulator. It behaves like io.ReadFull: it returns
// len(p) on success, or a short count with the error that stopped it.
func (br *bitReader) readRaw(p []byte) (int, error) {
	off := 0
	for off < len(p) {
		if br.r < br.w {
			m := copy(p[off:], br.buf[br.r:br.w])
			br.r += m
			off += m
			br.roffset += int64(m)
			continue
		}
		if br.eofErr != nil {
			err := br.eofErr
			br.eofErr = nil
			return off, err
		}
		n, err := br.src.Read(p[off:])
		off += n
		br.roffset += int64(n)
		if err != nil {
			return off, err
		}
		if n == 0 {
			return off, io.ErrNoProgress
		}
	}
	return off, nil
}

// readAligned discards the current partial byte and reads len(p) whole bytes.
// It is for the byte-aligned fields of the format; see the bitReader invariant.
func (br *bitReader) readAligned(p []byte) error {
	br.b, br.nb = 0, 0
	n, err := br.readRaw(p)
	if err != nil {
		return noEOF(err)
	}
	if n < len(p) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// noEOF turns a premature EOF into ErrUnexpectedEOF; other errors pass through.
func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
