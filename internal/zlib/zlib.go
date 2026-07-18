// Package zlib is an allocation-free inflater for zlib (RFC 1950) streams
// wrapping deflate (RFC 1951) data — the FlateDecode filter of PDF.
//
// It exists because compress/flate reallocates its Huffman overflow tables on
// every dynamic block (huffmanDecoder.init), so re-inflating a stream — which
// is what resolving a PDF object inside an object stream does on every access —
// allocates once per block, every time. This package draws all of its working
// memory from buffers the caller supplies up front through Config, following the
// same contract as the repository's Codec.Configure: sizes are validated once,
// and a stream needing more memory than was given returns a typed error rather
// than allocating. After Configure, Reset and Read never allocate.
//
// Only decompression is implemented, only zlib framing (not gzip), and preset
// dictionaries (the FDICT header bit) are rejected — none of which PDF uses.
package zlib

import (
	"encoding/binary"
	"errors"
	"io"
)

// Sizes for Config buffers.
const (
	// MaxWindow is the deflate sliding-window size and the required minimum for
	// Config.Window. RFC 1951 caps a back-reference distance at this value.
	MaxWindow = 1 << 15 // 32768
	// MinInput is the smallest accepted Config.Input; it is only a fill buffer,
	// so a larger one just means fewer reads from the source.
	MinInput = 512
	// RecommendedLinks covers the Huffman overflow tables of every real-world
	// PDF FlateDecode stream (whose codes stay well under the format's 15-bit
	// maximum). Use it for Config.Links unless a stream returns ErrShortLinks.
	RecommendedLinks = 4096
	// MaxLinks is the size at which ErrShortLinks can never occur, whatever the
	// stream. One decoder's flat link storage is at most huffmanNumChunks
	// subtables of maxSubtableLen entries each; the literal/length and distance
	// decoders are live at once, so twice that is the provable ceiling.
	maxSubtableLen = 1 << (15 - huffmanChunkBits) // 64: max code (15) over the chunk width
	MaxLinks       = 2 * huffmanNumChunks * maxSubtableLen
)

var (
	// ErrHeader reports a byte pair that is not a valid zlib header.
	ErrHeader = errors.New("zlib: invalid header")
	// ErrChecksum reports an adler32 trailer that does not match the data.
	ErrChecksum = errors.New("zlib: invalid checksum")
	// ErrPresetDict reports the FDICT header bit, which this package does not
	// support (PDF FlateDecode never sets it).
	ErrPresetDict = errors.New("zlib: preset dictionary not supported")
	// ErrCorrupt reports malformed deflate data.
	ErrCorrupt = errors.New("zlib: corrupt deflate data")
	// ErrShortLinks reports that Config.Links was exhausted building a Huffman
	// table; supply a larger buffer (up to MaxLinks always suffices).
	ErrShortLinks = errors.New("zlib: Config.Links too small for this stream")
	// ErrShortWindow and ErrShortInput report Config buffers below their minimum.
	ErrShortWindow = errors.New("zlib: Config.Window smaller than MaxWindow")
	ErrShortInput  = errors.New("zlib: Config.Input smaller than MinInput")
	// ErrNotConfigured reports Reset called before a successful Configure.
	ErrNotConfigured = errors.New("zlib: reader not configured")
)

// Config is the memory a Reader works out of. Every slice stays the caller's;
// the Reader keeps them and allocates nothing else.
type Config struct {
	// Window is the sliding-window/output buffer; must be at least MaxWindow.
	Window []byte
	// Links is the Huffman overflow-table scratch; see RecommendedLinks and
	// MaxLinks. May be nil, in which case any stream with codes longer than 9
	// bits returns ErrShortLinks.
	Links []uint32
	// Input is the source fill buffer; must be at least MinInput.
	Input []byte
	// SkipChecksum disables adler32 trailer verification. Some real PDFs carry
	// a wrong checksum yet decode correctly; set this to accept them.
	SkipChecksum bool
}

// IsZero reports whether c is the zero Config: no buffers and no flags set. It
// is the signal a caller uses to mean "give me defaults" — see DefaultConfig.
func (c Config) IsZero() bool {
	return c.Window == nil && c.Links == nil && c.Input == nil && !c.SkipChecksum
}

// DefaultInput is the fill-buffer size DefaultConfig allocates. A few KB keeps
// reads infrequent without holding meaningful memory.
const DefaultInput = 4096

// DefaultConfig returns a Config with freshly allocated buffers sized so no
// stream is ever rejected: a full MaxWindow history, MaxLinks of overflow-table
// scratch (so ErrShortLinks cannot occur), and a DefaultInput fill buffer. It
// allocates; call it once per Reader.
func DefaultConfig() Config {
	return Config{
		Window: make([]byte, MaxWindow),
		Links:  make([]uint32, MaxLinks),
		Input:  make([]byte, DefaultInput),
	}
}

// adlerMod is the largest prime below 65536, the modulus of adler32.
const adlerMod = 65521

// Reader inflates one zlib stream at a time. Configure it once, then Reset it to
// each source. It is not safe for concurrent use.
type Reader struct {
	inf          inflate
	skipChecksum bool
	configured   bool
	a1, a2       uint32 // running adler32 state
	scratch      [4]byte
	err          error
}

// Configure hands z the memory of cfg and validates its sizes, building the
// fixed Huffman table once. It must be called before Reset.
func (z *Reader) Configure(cfg Config) error {
	if cfg.IsZero() && len(z.inf.br.buf) == 0 {
		cfg = DefaultConfig()
	}
	if len(cfg.Window) < MaxWindow {
		return ErrShortWindow
	}
	if len(cfg.Input) < MinInput {
		return ErrShortInput
	}
	z.inf.win.hist = cfg.Window[:MaxWindow]
	z.inf.br.buf = cfg.Input
	z.inf.links = cfg.Links
	z.skipChecksum = cfg.SkipChecksum
	if err := z.inf.buildFixed(); err != nil {
		return err
	}
	z.configured = true
	return nil
}

// Reset binds z to src, reads and validates the 2-byte zlib header, and clears
// decode state. It allocates nothing. The source is read sequentially; z buffers
// ahead, so src must serve this stream alone.
func (z *Reader) Reset(src io.Reader) error {
	if !z.configured {
		return ErrNotConfigured
	}
	z.err = nil
	z.a1, z.a2 = 1, 0

	// RFC 1950 section 2.2: CMF, FLG. Read straight from src before the bit
	// reader wraps it, so exactly two bytes are consumed. scratch is a field so
	// the slice handed to src.Read does not escape and allocate.
	h := z.scratch[:2]
	if _, err := io.ReadFull(src, h); err != nil {
		return noEOF(err)
	}
	cm := h[0] & 0x0F
	cinfo := h[0] >> 4
	if cm != 8 || cinfo > 7 || (uint16(h[0])<<8|uint16(h[1]))%31 != 0 {
		return ErrHeader
	}
	if h[1]&0x20 != 0 { // FDICT
		return ErrPresetDict
	}
	z.inf.resetFlate(src)
	return nil
}

// Read inflates into p. It returns io.EOF once the deflate stream ends and, if
// checksums are enabled, only after the adler32 trailer verifies.
func (z *Reader) Read(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	n, err := z.inf.Read(p)
	if !z.skipChecksum && n > 0 {
		z.updateAdler(p[:n])
	}
	if err != io.EOF {
		z.err = err
		return n, err
	}
	// Deflate done: the adler32 sits at the next byte boundary.
	if err := z.inf.br.readAligned(z.scratch[:4]); err != nil {
		z.err = err
		return n, err
	}
	if !z.skipChecksum {
		want := binary.BigEndian.Uint32(z.scratch[:4])
		if got := z.a2<<16 | z.a1; got != want {
			z.err = ErrChecksum
			return n, ErrChecksum
		}
	}
	z.err = io.EOF
	return n, io.EOF
}

// updateAdler folds p into the running adler32, deferring the modulo to the end
// of each 5552-byte run — the most that can be summed before a1 could overflow.
func (z *Reader) updateAdler(p []byte) {
	a1, a2 := z.a1, z.a2
	for len(p) > 0 {
		chunk := p
		if len(chunk) > 5552 {
			chunk = chunk[:5552]
		}
		p = p[len(chunk):]
		for _, b := range chunk {
			a1 += uint32(b)
			a2 += a1
		}
		a1 %= adlerMod
		a2 %= adlerMod
	}
	z.a1, z.a2 = a1, a2
}
