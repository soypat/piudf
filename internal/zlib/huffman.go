package zlib

import "math/bits"

// The Huffman lookup structure is the one zlib and compress/flate use: a
// direct table indexed by the low huffmanChunkBits of the pending bits, with
// codes longer than that width redirected through an overflow ("link") table.
//
//	chunk & huffmanCountMask is the code's bit length.
//	chunk >> huffmanValueShift is the decoded symbol, or a link-table index.
//
// The one departure from compress/flate: the overflow tables are not
// [][]uint32 allocated per Huffman-table build (the source of every allocation
// on a rewind). Every subtable of one decoder has the same length numLinks, so
// they are laid end to end in a single flat []uint32 drawn from caller memory:
// subtable k occupies links[k*numLinks : (k+1)*numLinks]. init returns how many
// uint32 it took so the caller can hand the next decoder the rest.
const (
	huffmanChunkBits  = 9
	huffmanNumChunks  = 1 << huffmanChunkBits // 512
	huffmanCountMask  = 15
	huffmanValueShift = 4
	maxCodeLen        = 16 // RFC 1951: a code is at most 15 bits; the table is sized one wider.
)

type huffmanDecoder struct {
	min      int
	chunks   [huffmanNumChunks]uint32
	links    []uint32 // flat overflow tables, from caller memory; see file comment
	numLinks uint32   // length of one subtable, a power of two
	linkMask uint32   // numLinks - 1
}

// init builds h from the per-symbol code lengths, drawing any overflow tables
// from the front of linkbuf. It returns the number of uint32 taken from linkbuf
// (zero when every code fits in huffmanChunkBits), or an error: ErrShortLinks
// when linkbuf is too small, ErrCorrupt when the lengths do not form a complete
// tree. An empty tree (all lengths zero) is valid and returns 0, nil.
func (h *huffmanDecoder) init(lengths []int, linkbuf []uint32) (used int, err error) {
	h.min = 0
	h.numLinks, h.linkMask, h.links = 0, 0, nil
	clear(h.chunks[:])

	// Count codes of each length and find the extremes.
	var count [maxCodeLen]int
	var min, max int
	for _, n := range lengths {
		if n == 0 {
			continue
		}
		if min == 0 || n < min {
			min = n
		}
		if n > max {
			max = n
		}
		count[n]++
	}
	if max == 0 {
		return 0, nil // Empty tree; huffSym fails if it is ever used.
	}

	// First code of each length.
	code := 0
	var nextcode [maxCodeLen]int
	for i := min; i <= max; i++ {
		code <<= 1
		nextcode[i] = code
		code += count[i]
	}
	// The tree must be complete, save for the degenerate single-code coding
	// zlib tolerates.
	if code != 1<<uint(max) && !(code == 1 && max == 1) {
		return 0, ErrCorrupt
	}

	h.min = min
	if max > huffmanChunkBits {
		numLinks := 1 << (uint(max) - huffmanChunkBits)
		h.numLinks = uint32(numLinks)
		h.linkMask = uint32(numLinks - 1)

		link := nextcode[huffmanChunkBits+1] >> 1
		nsub := huffmanNumChunks - link
		used = nsub * numLinks
		if used > len(linkbuf) {
			return 0, ErrShortLinks
		}
		h.links = linkbuf[:used]
		clear(h.links)
		for j := link; j < huffmanNumChunks; j++ {
			reverse := int(bits.Reverse16(uint16(j)))
			reverse >>= uint(16 - huffmanChunkBits)
			off := j - link
			h.chunks[reverse] = uint32(off<<huffmanValueShift | (huffmanChunkBits + 1))
		}
	}

	for i, n := range lengths {
		if n == 0 {
			continue
		}
		code := nextcode[n]
		nextcode[n]++
		chunk := uint32(i<<huffmanValueShift | n)
		reverse := int(bits.Reverse16(uint16(code)))
		reverse >>= uint(16 - n)
		if n <= huffmanChunkBits {
			for off := reverse; off < len(h.chunks); off += 1 << uint(n) {
				h.chunks[off] = chunk
			}
		} else {
			j := reverse & (huffmanNumChunks - 1)
			value := h.chunks[j] >> huffmanValueShift
			base := int(value) * numLinksOf(h)
			reverse >>= huffmanChunkBits
			for off := reverse; off < numLinksOf(h); off += 1 << uint(n-huffmanChunkBits) {
				h.links[base+off] = chunk
			}
		}
	}
	return used, nil
}

func numLinksOf(h *huffmanDecoder) int { return int(h.numLinks) }
