package zlib

// window is the LZ77 sliding dictionary of the decoder, ported from
// compress/flate's dictDecoder with the one change that matters here: its
// history buffer is caller-owned (Config.Window), never allocated. There is no
// preset-dictionary support — PDF FlateDecode never uses one — so init takes
// only the backing slice.
//
// The buffer doubles as the output staging area: decoded bytes are written into
// it and handed to the reader in runs by readFlush, which is why a single
// MaxWindow-sized slice serves both the back-reference history and the output.
//
// For speed this mirrors the source and performs no per-call bounds sanity
// checks; the invariants on each method must hold.
type window struct {
	hist  []byte
	wrPos int  // next write position
	rdPos int  // bytes up to here have been flushed to the reader
	full  bool // a full window has been written at least once
}

// reset points the window at hist and empties it.
func (w *window) reset(hist []byte) {
	w.hist = hist
	w.wrPos, w.rdPos, w.full = 0, 0, false
}

// histSize reports how much history is available to copy from.
func (w *window) histSize() int {
	if w.full {
		return len(w.hist)
	}
	return w.wrPos
}

// availRead reports bytes ready for readFlush.
func (w *window) availRead() int { return w.wrPos - w.rdPos }

// availWrite reports free space before the buffer must be flushed.
func (w *window) availWrite() int { return len(w.hist) - w.wrPos }

// writeSlice returns the free tail of the buffer; len <= availWrite().
func (w *window) writeSlice() []byte { return w.hist[w.wrPos:] }

// writeMark advances the write cursor after a direct write into writeSlice.
func (w *window) writeMark(cnt int) { w.wrPos += cnt }

// writeByte appends one byte; availWrite() must be > 0.
func (w *window) writeByte(c byte) {
	w.hist[w.wrPos] = c
	w.wrPos++
}

// writeCopy performs a back-reference copy of length bytes from dist behind the
// write cursor, returning how many it wrote (short if the buffer filled).
// 0 < dist <= histSize() must hold.
func (w *window) writeCopy(dist, length int) int {
	dstBase := w.wrPos
	dstPos := dstBase
	srcPos := dstPos - dist
	endPos := dstPos + length
	if endPos > len(w.hist) {
		endPos = len(w.hist)
	}

	// Non-overlapping wrap-around section behind the buffer start.
	if srcPos < 0 {
		srcPos += len(w.hist)
		dstPos += copy(w.hist[dstPos:endPos], w.hist[srcPos:])
		srcPos = 0
	}

	// Possibly overlapping forward copy (run-length when length > dist).
	for dstPos < endPos {
		dstPos += copy(w.hist[dstPos:endPos], w.hist[srcPos:dstPos])
	}

	w.wrPos = dstPos
	return dstPos - dstBase
}

// tryWriteCopy is the fast path of writeCopy for copies that neither wrap nor
// fill the buffer; it returns 0 when it cannot apply, so the caller falls back.
func (w *window) tryWriteCopy(dist, length int) int {
	dstPos := w.wrPos
	endPos := dstPos + length
	if dstPos < dist || endPos > len(w.hist) {
		return 0
	}
	dstBase := dstPos
	srcPos := dstPos - dist
	for dstPos < endPos {
		dstPos += copy(w.hist[dstPos:endPos], w.hist[srcPos:dstPos])
	}
	w.wrPos = dstPos
	return dstPos - dstBase
}

// readFlush returns the decoded bytes ready to emit. They must be consumed
// before any other window call. When the buffer is full it wraps to the start.
func (w *window) readFlush() []byte {
	toRead := w.hist[w.rdPos:w.wrPos]
	w.rdPos = w.wrPos
	if w.wrPos == len(w.hist) {
		w.wrPos, w.rdPos = 0, 0
		w.full = true
	}
	return toRead
}
