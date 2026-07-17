package internal

import (
	"errors"
	"io"
	"math"
	"unsafe"
)

// defaultWindowSize is the fill size used when the caller supplies no window
// buffer through SetWindow.
const defaultWindowSize = 4096

// Window is a byte Window over an io.ReaderAt: buf holds the file bytes
// [base, base+n). Unlike a bufio.Reader it knows which file range it holds,
// so seeking to an offset already inside the Window costs no read at all.
// PDF lexing is a sequence of jumps — to xref-provided object offsets, past
// stream payloads, back to a dictionary span — and those jumps overwhelmingly
// land within a few hundred bytes of each other.
type Window struct {
	base int64 // File offset of buf[0].
	end  int64 // First offset known to be at or past EOF.
	r    io.ReaderAt
	buf  []byte // Fill buffer; caller-owned when set through SetWindow.
	n    int    // Valid bytes in buf.
	err  error  // Sticky read error other than EOF.
}

// Reset binds w to r, optionally replacing the fill buffer; a nil buf keeps
// the current one, allocating a default-sized buffer on first use. Resetting
// to the reader already bound keeps the buffered bytes, which is what makes
// repeated jumps around one file cost no reads.
//
// r's dynamic type must be comparable — every stdlib io.ReaderAt is, as is
// any pointer — because Reset tests r against the bound reader to decide
// whether the buffered bytes still describe the same file.
func (w *Window) Reset(r io.ReaderAt, buf []byte) {
	w.err = nil
	if buf != nil && (len(buf) != len(w.buf) || unsafe.SliceData(buf) != unsafe.SliceData(w.buf)) {
		// A different buffer: base and n describe the old one's contents.
		// Handing back the same slice must stay a no-op — callers pass it on
		// every Reset, and dropping the bytes each time would defeat the point.
		w.buf = buf
		w.n = 0
	}
	if w.r == r {
		return // Same ReaderAt: buffered bytes and known EOF still apply.
	}
	w.r = r
	w.n = 0
	w.end = math.MaxInt64
}

func (w *Window) Err() error { return w.err }

// Drop discards the resident bytes and any recorded end, so the next ByteAt
// reads. It is for a reader whose bytes at a given offset changed under the
// window — a cursor over one decompressed stream pointed at another — which
// the window cannot notice on its own: it keys what it holds by reader
// identity, and that identity did not change.
func (w *Window) Drop() {
	w.n = 0
	w.end = math.MaxInt64
	w.err = nil
}

// ByteAt returns the file byte at off, refilling only when off falls outside
// the resident window.
func (w *Window) ByteAt(off int64) (byte, bool) {
	if i := off - w.base; i >= 0 && i < int64(w.n) {
		return w.buf[i], true
	}
	return w.fillByteAt(off)
}

// FillByteAt is byteAt's slow path: off lies outside the window, so refill
// starting at off. Filling forward from the requested byte (rather than
// centering on it) keeps a forward scan to one read per len(buf) bytes.
func (w *Window) fillByteAt(off int64) (byte, bool) {
	if w.r == nil {
		w.err = errors.New("piudf: readerAt unititialized")
		return 0, false
	}
	if off < 0 || off >= w.end || w.err != nil {
		return 0, false
	}
	if w.buf == nil {
		w.buf = make([]byte, defaultWindowSize)
	}
	n, err := w.r.ReadAt(w.buf, off)
	switch {
	case err == io.EOF:
		// The file ends inside this fill: record it so EOF costs no further
		// reads. Inferring this from n < len(buf) instead would trust the
		// ReadAt contract; a reader that breaks it would truncate the file.
		w.end = off + int64(n)
	case err != nil:
		w.err = err
	}
	w.base, w.n = off, n
	if n == 0 {
		return 0, false
	}
	return w.buf[0], true
}
