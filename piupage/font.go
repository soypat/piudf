package canvas

import ppdf "github.com/soypat/piudf"

// Font provides text metrics and byte encoding for one typeface. The
// standard-14 implementation ([Standard14]) needs no embedded font program; a
// future lefevre-backed TrueType implementation would embed one. writeObjects
// is unexported by design: only this package supplies fonts, and the doc layer
// consumes them as opaque interface values.
type Font interface {
	// BaseName is the PDF /BaseFont value, e.g. "Helvetica-Bold".
	BaseName() string
	// Width is the advance of r at 1pt — a fraction of the em — so that
	// StringWidth(f, s, size) == sum(Width(r)) * size.
	Width(r rune) float64
	// Encode appends the font's byte code(s) for r to dst. WinAnsi fonts emit
	// one byte; an unencodable rune falls back to '?'.
	Encode(dst []byte, r rune) []byte
	// writeObjects emits the font's PDF object(s) and returns the font dict id.
	writeObjects(enc *ppdf.Encoder) (ppdf.ObjectID, error)
}

// StringWidth returns the rendered width of s in points at the given size.
func StringWidth(f Font, s string, size float64) float64 {
	panic("todo: canvas.StringWidth")
}

// Standard14 returns one of the 14 built-in PDF fonts by /BaseFont name
// (WinAnsi-encoded, no embedded program). ok is false for an unknown name.
func Standard14(name string) (f Font, ok bool) {
	panic("todo: canvas.Standard14")
}

// stdFont is a built-in (non-embedded) Type1 font backed by an AFM width table
// and WinAnsi encoding. See afm.go.
type stdFont struct {
	base    string      // /BaseFont name
	widths  *[256]int16 // per-1000-em advance keyed by WinAnsi byte
	written ppdf.ObjectID
}

var _ Font = (*stdFont)(nil)
