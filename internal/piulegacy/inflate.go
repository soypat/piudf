package piudf

import (
	"bufio"
	"compress/zlib"
	"fmt"
	"io"
)

// zlibReader is the reusable inflater held by a Decoder: a zlib stream
// reader that can be pointed at a new source without reallocating its
// window.
type zlibReader interface {
	io.ReadCloser
	zlib.Resetter
}

// spanReader is a reusable io.Reader over one file span, the source fed to
// the inflater. It exists so internal decompression does not construct an
// io.SectionReader per call.
type spanReader struct {
	r   io.ReaderAt
	off int64
	end int64
}

func (s *spanReader) set(r io.ReaderAt, off, length int64) {
	s.r, s.off, s.end = r, off, off+length
}

func (s *spanReader) Read(b []byte) (int, error) {
	if s.off >= s.end {
		return 0, io.EOF
	}
	if max := s.end - s.off; int64(len(b)) > max {
		b = b[:max]
	}
	n, err := s.r.ReadAt(b, s.off)
	s.off += int64(n)
	return n, err
}

// resetInflater points the Decoder's zlib reader at src, allocating it on
// first use. FlateDecode data in PDF is a zlib stream (RFC 1950). src is
// routed through the Decoder's reusable bufio.Reader: zlib would otherwise
// wrap any source without ReadByte in a fresh one per call.
func (d *Decoder) resetInflater(src io.Reader) (io.Reader, error) {
	if d.zsrc == nil {
		d.zsrc = bufio.NewReader(src)
	} else {
		d.zsrc.Reset(src)
	}
	if d.zr == nil {
		zr, err := zlib.NewReader(d.zsrc)
		if err != nil {
			return nil, fmt.Errorf("%w: bad zlib header: %v", ErrCorrupt, err)
		}
		d.zr = zr.(zlibReader)
		return d.zr, nil
	}
	if err := d.zr.Reset(d.zsrc, nil); err != nil {
		return nil, fmt.Errorf("%w: bad zlib header: %v", ErrCorrupt, err)
	}
	return d.zr, nil
}

// inflate appends the FlateDecode-decompressed bytes of src to dst. max is
// a hard cap on the decompressed output regardless of grow — it is the
// guard against compressed data expanding without bound. grow only governs
// whether dst may be reallocated below that cap; with grow false the output
// must fit in dst's spare capacity.
func (d *Decoder) inflate(dst []byte, src io.Reader, max int, grow bool) ([]byte, error) {
	zr, err := d.resetInflater(src)
	if err != nil {
		return dst, err
	}
	start := len(dst)
	for {
		if len(dst) == cap(dst) {
			// Full buffer: distinguish "stream exactly done" from
			// "needs more room" with a one-byte probe. The probe lives on
			// the Decoder so the interface Read cannot make it escape.
			probe := d.iobuf[:1]
			n, perr := zr.Read(probe)
			if n == 0 {
				if perr == io.EOF {
					return dst, nil
				}
				if perr != nil {
					return dst, fmt.Errorf("%w: inflate: %v", ErrCorrupt, perr)
				}
				continue
			}
			if !grow {
				return dst, fmt.Errorf("%w: decompressed data over buffer capacity %d", ErrMemoryLimit, cap(dst)-start)
			}
			if len(dst)-start >= max {
				return dst, fmt.Errorf("%w: decompressed data over %d bytes", ErrMemoryLimit, max)
			}
			dst = append(dst, probe[0])
			if perr == io.EOF {
				return dst, nil
			}
			if perr != nil {
				return dst, fmt.Errorf("%w: inflate: %v", ErrCorrupt, perr)
			}
			continue
		}
		// Never read more than one byte past the max budget: that byte,
		// if it arrives, proves the limit is exceeded.
		room := cap(dst) - len(dst)
		if budget := max - (len(dst) - start) + 1; room > budget {
			room = budget
		}
		n, rerr := zr.Read(dst[len(dst) : len(dst)+room])
		dst = dst[:len(dst)+n]
		if len(dst)-start > max {
			return dst, fmt.Errorf("%w: decompressed data over %d bytes", ErrMemoryLimit, max)
		}
		if rerr == io.EOF {
			return dst, nil
		}
		if rerr != nil {
			return dst, fmt.Errorf("%w: inflate: %v", ErrCorrupt, rerr)
		}
	}
}

// undoPredictor reverses the /DecodeParms predictor transform on
// decompressed data in place and returns the (possibly shorter) result.
// Predictor 1 or absent is the identity; 10-15 are the PNG row filters;
// TIFF predictor 2 is unsupported.
func undoPredictor(data []byte, predictor, columns, colors, bpc int) ([]byte, error) {
	switch {
	case predictor <= 1:
		return data, nil
	case predictor == 2:
		return nil, fmt.Errorf("%w: TIFF predictor", ErrUnsupported)
	case predictor >= 10 && predictor <= 15:
		return applyPNGPredictor(data, columns, colors, bpc)
	}
	return nil, fmt.Errorf("%w: predictor %d", ErrUnsupported, predictor)
}

// applyPNGPredictor undoes the PNG row filters (None, Sub, Up, Average,
// Paeth — the actual filter is chosen per row by its leading byte, whatever
// /Predictor 10-15 declared) and returns the packed rows without the filter
// bytes. The transform is done in place: output rows land earlier in the
// same buffer than they are read from.
func applyPNGPredictor(rows []byte, columns, colors, bpc int) ([]byte, error) {
	if columns <= 0 || colors <= 0 || bpc <= 0 {
		return nil, fmt.Errorf("%w: bad predictor parameters", ErrCorrupt)
	}
	bpp := (colors*bpc + 7) / 8 // Bytes per pixel, minimum 1.
	rowlen := (columns*colors*bpc + 7) / 8
	if len(rows)%(rowlen+1) != 0 {
		return nil, fmt.Errorf("%w: predictor data of %d bytes is not whole %d-byte rows", ErrCorrupt, len(rows), rowlen+1)
	}
	out := 0
	for r := 0; r*(rowlen+1) < len(rows); r++ {
		ft := rows[r*(rowlen+1)]
		src := rows[r*(rowlen+1)+1 : (r+1)*(rowlen+1)]
		dst := rows[out : out+rowlen]
		for i := 0; i < rowlen; i++ {
			x := src[i]
			var a, b, c byte // Left, up, up-left reconstructed bytes.
			if i >= bpp {
				a = dst[i-bpp]
			}
			if r > 0 {
				b = rows[out-rowlen+i]
				if i >= bpp {
					c = rows[out-rowlen+i-bpp]
				}
			}
			switch ft {
			case 0: // None
			case 1: // Sub
				x += a
			case 2: // Up
				x += b
			case 3: // Average
				x += byte((int(a) + int(b)) / 2)
			case 4: // Paeth
				x += paeth(a, b, c)
			default:
				return nil, fmt.Errorf("%w: PNG row filter %d", ErrCorrupt, ft)
			}
			dst[i] = x
		}
		out += rowlen
	}
	return rows[:out], nil
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
