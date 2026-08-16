package piupage

import "github.com/soypat/piudf"

// Font provides text metrics and byte encoding for one typeface. The
// standard-14 implementation ([Standard14]) needs no embedded font program; a
// future lefevre-backed TrueType implementation would embed one. writeObjects
// is unexported by design: only this package supplies fonts, and the doc layer
// consumes them as opaque interface values (see [WriteFont]).
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
	writeObjects(enc *piudf.Encoder) (piudf.ObjectID, error)
}

// StringWidth returns the rendered width of s in points at the given size.
func StringWidth(f Font, s string, size float64) float64 {
	var w float64
	for _, r := range s {
		w += f.Width(r)
	}
	return w * size
}

// WriteFont emits f's PDF object(s) into enc and returns the font dictionary's
// id. It is the exported door onto the unexported Font.writeObjects, so the doc
// layer can materialize shared font objects.
func WriteFont(enc *piudf.Encoder, f Font) (piudf.ObjectID, error) {
	return f.writeObjects(enc)
}

// Standard14 returns one of the built-in PDF fonts by /BaseFont name
// (WinAnsi-encoded, no embedded program). ok is false for an unknown name. Only
// the Helvetica family carries real metrics in this first cut; other names
// resolve with Helvetica widths as a stand-in.
func Standard14(name string) (f Font, ok bool) {
	for b := FontHelvetica; b <= FontZapfDingbats; b++ {
		if b.String() == name {
			return b, true
		}
	}
	return nil, false
}

var _ Font = FontBuiltin(0)
