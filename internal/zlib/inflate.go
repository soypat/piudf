package zlib

import (
	"io"
	"math/bits"
)

// This is compress/flate's inflate loop, ported to draw every buffer from
// caller memory (see the package piudoc). The block state machine, the resumable
// step function, and huffSym are unchanged in behaviour; only the sources of
// memory differ: bits come from bitReader, output from window, and the Huffman
// overflow tables from the shared links buffer instead of a per-block make.
const (
	maxNumLit      = 286
	maxNumDist     = 30
	numCodes       = 19  // symbols in the code-length meta-code
	endBlockMarker = 256 // literal/length symbol that ends a block
)

// codeOrder is the permutation RFC 1951 section 3.2.7 stores the code-length
// code lengths in.
var codeOrder = [...]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// inflate is the deflate (RFC 1951) decoder. All of its working memory is
// caller-owned and reused across resetFlate, so re-inflating a stream — the
// operation a PDF object-stream rewind repeats — allocates nothing.
type inflate struct {
	br  bitReader
	win window

	h1, h2 huffmanDecoder // literal/length and distance, for dynamic blocks
	fixed  huffmanDecoder // the RFC 1951 fixed literal/length table, built once
	links  []uint32       // caller buffer the dynamic tables draw overflow from

	// Code-length scratch, inline (bounded by the format, never allocated).
	bitsArr  [maxNumLit + maxNumDist]int
	codebits [numCodes]int

	step      func(*inflate) // next state; a method expression, so no closure alloc
	stepState int
	final     bool
	err       error
	toRead    []byte
	hl, hd    *huffmanDecoder
	copyLen   int
	copyDist  int
	// hdr backs the stored-block length header. It is a field, not a local,
	// because readRaw hands it to the source's Read across an interface: a
	// local would escape and allocate on every stored block.
	hdr [4]byte
}

// buildFixed constructs the fixed Huffman literal/length decoder once. Its codes
// are at most 9 bits, so it needs no overflow tables and no link buffer.
func (i *inflate) buildFixed() error {
	var lengths [288]int
	for j := 0; j < 144; j++ {
		lengths[j] = 8
	}
	for j := 144; j < 256; j++ {
		lengths[j] = 9
	}
	for j := 256; j < 280; j++ {
		lengths[j] = 7
	}
	for j := 280; j < 288; j++ {
		lengths[j] = 8
	}
	_, err := i.fixed.init(lengths[:], nil)
	return err
}

// resetFlate rebinds the decoder to src and clears its state, keeping every
// buffer. It allocates nothing.
func (i *inflate) resetFlate(src io.Reader) {
	i.br.reset(src)
	i.win.reset(i.win.hist)
	i.step = (*inflate).nextBlock
	i.stepState = 0
	i.final = false
	i.err = nil
	i.toRead = nil
	i.hl, i.hd = nil, nil
	i.copyLen, i.copyDist = 0, 0
}

func (i *inflate) moreBits() error {
	c, err := i.br.readByte()
	if err != nil {
		return noEOF(err)
	}
	i.br.b |= uint32(c) << i.br.nb
	i.br.nb += 8
	return nil
}

// Read decodes into b, driving the step machine until output is available or
// the stream ends. It is compress/flate's Read verbatim in shape.
func (i *inflate) Read(b []byte) (int, error) {
	for {
		if len(i.toRead) > 0 {
			n := copy(b, i.toRead)
			i.toRead = i.toRead[n:]
			if len(i.toRead) == 0 {
				return n, i.err
			}
			return n, nil
		}
		if i.err != nil {
			return 0, i.err
		}
		i.step(i)
		if i.err != nil && len(i.toRead) == 0 {
			i.toRead = i.win.readFlush() // Flush what remains on error.
		}
	}
}

func (i *inflate) nextBlock() {
	for i.br.nb < 1+2 {
		if i.err = i.moreBits(); i.err != nil {
			return
		}
	}
	i.final = i.br.b&1 == 1
	i.br.b >>= 1
	typ := i.br.b & 3
	i.br.b >>= 2
	i.br.nb -= 1 + 2
	switch typ {
	case 0:
		i.dataBlock()
	case 1: // fixed Huffman tables
		i.hl = &i.fixed
		i.hd = nil
		i.huffmanBlock()
	case 2: // dynamic Huffman tables
		if i.err = i.readHuffman(); i.err != nil {
			break
		}
		i.hl = &i.h1
		i.hd = &i.h2
		i.huffmanBlock()
	default: // 3 is reserved
		i.err = ErrCorrupt
	}
}

// readHuffman reads the dynamic-block header and builds h1 (literal/length) and
// h2 (distance). Both tables' overflow rows come from i.links: h1 takes the
// front, h2 the remainder. This is where the stdlib would allocate.
func (i *inflate) readHuffman() error {
	for i.br.nb < 5+5+4 {
		if err := i.moreBits(); err != nil {
			return err
		}
	}
	nlit := int(i.br.b&0x1F) + 257
	if nlit > maxNumLit {
		return ErrCorrupt
	}
	i.br.b >>= 5
	ndist := int(i.br.b&0x1F) + 1
	if ndist > maxNumDist {
		return ErrCorrupt
	}
	i.br.b >>= 5
	nclen := int(i.br.b&0xF) + 4
	i.br.b >>= 4
	i.br.nb -= 5 + 5 + 4

	for j := 0; j < nclen; j++ {
		for i.br.nb < 3 {
			if err := i.moreBits(); err != nil {
				return err
			}
		}
		i.codebits[codeOrder[j]] = int(i.br.b & 0x7)
		i.br.b >>= 3
		i.br.nb -= 3
	}
	for j := nclen; j < len(codeOrder); j++ {
		i.codebits[codeOrder[j]] = 0
	}
	// The code-length meta-code: at most 7 bits, so no overflow tables.
	if _, err := i.h1.init(i.codebits[:], i.links); err != nil {
		return err
	}

	// Decode the nlit+ndist code lengths using the meta-code.
	for n, k := nlit+ndist, 0; k < n; {
		x, err := i.huffSym(&i.h1)
		if err != nil {
			return err
		}
		if x < 16 {
			i.bitsArr[k] = x
			k++
			continue
		}
		var rep int
		var nb uint
		var b int
		switch x {
		default:
			return ErrCorrupt
		case 16:
			rep, nb = 3, 2
			if k == 0 {
				return ErrCorrupt
			}
			b = i.bitsArr[k-1]
		case 17:
			rep, nb, b = 3, 3, 0
		case 18:
			rep, nb, b = 11, 7, 0
		}
		for i.br.nb < nb {
			if err := i.moreBits(); err != nil {
				return err
			}
		}
		rep += int(i.br.b & uint32(1<<nb-1))
		i.br.b >>= nb
		i.br.nb -= nb
		if k+rep > n {
			return ErrCorrupt
		}
		for j := 0; j < rep; j++ {
			i.bitsArr[k] = b
			k++
		}
	}

	// Build both live tables into disjoint runs of the shared link buffer.
	usedL, err := i.h1.init(i.bitsArr[0:nlit], i.links)
	if err != nil {
		return err
	}
	if _, err := i.h2.init(i.bitsArr[nlit:nlit+ndist], i.links[usedL:]); err != nil {
		return err
	}

	// Every block ends with the EOB marker, so the literal decoder never needs
	// to read fewer bits than that code is long.
	if i.h1.min < i.bitsArr[endBlockMarker] {
		i.h1.min = i.bitsArr[endBlockMarker]
	}
	return nil
}

func (i *inflate) huffmanBlock() {
	const (
		stateInit = iota
		stateDict
	)
	switch i.stepState {
	case stateInit:
		goto readLiteral
	case stateDict:
		goto copyHistory
	}

readLiteral:
	{
		v, err := i.huffSym(i.hl)
		if err != nil {
			i.err = err
			return
		}
		var n uint
		var length int
		switch {
		case v < 256:
			i.win.writeByte(byte(v))
			if i.win.availWrite() == 0 {
				i.toRead = i.win.readFlush()
				i.step = (*inflate).huffmanBlock
				i.stepState = stateInit
				return
			}
			goto readLiteral
		case v == 256:
			i.finishBlock()
			return
		case v < 265:
			length = v - (257 - 3)
			n = 0
		case v < 269:
			length = v*2 - (265*2 - 11)
			n = 1
		case v < 273:
			length = v*4 - (269*4 - 19)
			n = 2
		case v < 277:
			length = v*8 - (273*8 - 35)
			n = 3
		case v < 281:
			length = v*16 - (277*16 - 67)
			n = 4
		case v < 285:
			length = v*32 - (281*32 - 131)
			n = 5
		case v < maxNumLit:
			length = 258
			n = 0
		default:
			i.err = ErrCorrupt
			return
		}
		if n > 0 {
			for i.br.nb < n {
				if err = i.moreBits(); err != nil {
					i.err = err
					return
				}
			}
			length += int(i.br.b & uint32(1<<n-1))
			i.br.b >>= n
			i.br.nb -= n
		}

		var dist int
		if i.hd == nil {
			for i.br.nb < 5 {
				if err = i.moreBits(); err != nil {
					i.err = err
					return
				}
			}
			dist = int(bits.Reverse8(uint8(i.br.b & 0x1F << 3)))
			i.br.b >>= 5
			i.br.nb -= 5
		} else {
			if dist, err = i.huffSym(i.hd); err != nil {
				i.err = err
				return
			}
		}

		switch {
		case dist < 4:
			dist++
		case dist < maxNumDist:
			nb := uint(dist-2) >> 1
			extra := (dist & 1) << nb
			for i.br.nb < nb {
				if err = i.moreBits(); err != nil {
					i.err = err
					return
				}
			}
			extra |= int(i.br.b & uint32(1<<nb-1))
			i.br.b >>= nb
			i.br.nb -= nb
			dist = 1<<(nb+1) + 1 + extra
		default:
			i.err = ErrCorrupt
			return
		}

		if dist > i.win.histSize() {
			i.err = ErrCorrupt
			return
		}

		i.copyLen, i.copyDist = length, dist
		goto copyHistory
	}

copyHistory:
	{
		cnt := i.win.tryWriteCopy(i.copyDist, i.copyLen)
		if cnt == 0 {
			cnt = i.win.writeCopy(i.copyDist, i.copyLen)
		}
		i.copyLen -= cnt
		if i.win.availWrite() == 0 || i.copyLen > 0 {
			i.toRead = i.win.readFlush()
			i.step = (*inflate).huffmanBlock
			i.stepState = stateDict
			return
		}
		goto readLiteral
	}
}

func (i *inflate) dataBlock() {
	// Stored blocks are byte-aligned: drop the partial byte, then read the
	// length and its ones-complement.
	if err := i.br.readAligned(i.hdr[:]); err != nil {
		i.err = err
		return
	}
	n := int(i.hdr[0]) | int(i.hdr[1])<<8
	nn := int(i.hdr[2]) | int(i.hdr[3])<<8
	if uint16(nn) != uint16(^n) {
		i.err = ErrCorrupt
		return
	}
	if n == 0 {
		i.toRead = i.win.readFlush()
		i.finishBlock()
		return
	}
	i.copyLen = n
	i.copyData()
}

func (i *inflate) copyData() {
	buf := i.win.writeSlice()
	if len(buf) > i.copyLen {
		buf = buf[:i.copyLen]
	}
	cnt, err := i.br.readRaw(buf)
	i.copyLen -= cnt
	i.win.writeMark(cnt)
	if err != nil {
		i.err = noEOF(err)
		return
	}
	if i.win.availWrite() == 0 || i.copyLen > 0 {
		i.toRead = i.win.readFlush()
		i.step = (*inflate).copyData
		return
	}
	i.finishBlock()
}

func (i *inflate) finishBlock() {
	if i.final {
		if i.win.availRead() > 0 {
			i.toRead = i.win.readFlush()
		}
		i.err = io.EOF
	}
	i.step = (*inflate).nextBlock
}

// huffSym decodes one symbol using h. It is the hot loop and mirrors
// compress/flate, reading from the bit reader through the flat link table.
func (i *inflate) huffSym(h *huffmanDecoder) (int, error) {
	n := uint(h.min)
	br := &i.br
	nb, b := br.nb, br.b
	for {
		for nb < n {
			c, err := br.readByte()
			if err != nil {
				br.b, br.nb = b, nb
				return 0, noEOF(err)
			}
			b |= uint32(c) << (nb & 31)
			nb += 8
		}
		chunk := h.chunks[b&(huffmanNumChunks-1)]
		n = uint(chunk & huffmanCountMask)
		if n > huffmanChunkBits {
			chunk = h.links[(chunk>>huffmanValueShift)*h.numLinks+((b>>huffmanChunkBits)&h.linkMask)]
			n = uint(chunk & huffmanCountMask)
		}
		if n <= nb {
			if n == 0 {
				br.b, br.nb = b, nb
				return 0, ErrCorrupt
			}
			br.b = b >> (n & 31)
			br.nb = nb - n
			return int(chunk >> huffmanValueShift), nil
		}
	}
}
