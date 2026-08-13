package piupage

import (
	"slices"

	"github.com/soypat/piudf"
)

// Font provides text metrics and byte encoding for one typeface. The
// standard-14 implementation ([Standard14]) needs no embedded font program; a
// future lefevre-backed TrueType implementation would embed one. writeObjects
// is unexported by design: only this package supplies fonts, and the doc layer
// consumes them as opaque interface values (see [WriteFont]).
type Font interface {
	// BaseName is the PDF /BaseFont value, e.g. "Helvetica-Bold".
	BaseName() string
	// Width is the advance of r as a fraction of the em, so that
	// StringWidth(f, s, size) == sum(Width(r)) * size. Being a ratio it
	// carries no unit of its own, which is what lets a size in any unit
	// produce a width in that same unit.
	Width(r rune) float64
	// Encode appends the font's byte code(s) for r to dst. WinAnsi fonts emit
	// one byte; an unencodable rune falls back to '?'.
	Encode(dst []byte, r rune) []byte
	// hexCodes reports whether encoded text should be written as a hex string.
	// Multi-byte codes are unreadable either way, and hex keeps the content
	// stream free of the escaping a literal string would need.
	hexCodes() bool
	// has reports whether the font can draw r with a glyph of its own.
	has(r rune) bool
	// writeObjects emits the font's PDF object(s) and returns the font dict id.
	// It is called once, after every page has been drawn, so a font that
	// embeds a subset of itself knows the full set of glyphs it must carry.
	writeObjects(enc *piudf.Encoder) (piudf.ObjectID, error)
}

// StringWidth returns the rendered width of s at the given size, in whatever
// unit that size is expressed in: the advances it sums are em fractions, so
// the size is the only thing carrying a unit and it passes straight through.
func StringWidth(f Font, s string, size float64) float64 {
	var w float64
	for _, r := range s {
		w += f.Width(r)
	}
	return w * size
}

// MissingGlyphs returns the distinct runes of s that f has no glyph for, in
// order of first appearance. Those runes still draw — as .notdef, the empty
// box — so a document that wants to be sure of its typography checks its text
// against its faces before writing rather than after reading the result.
func MissingGlyphs(f Font, s string) []rune {
	var missing []rune
	for _, r := range s {
		if f.has(r) || slices.Contains(missing, r) {
			continue
		}
		missing = append(missing, r)
	}
	return missing
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
// resolve with Helvetica widths as a stand-in, so a document that wants a
// different typeface — and correct measurements for it — wants [TrueType].
func Standard14(name string) (f Font, ok bool) {
	switch name {
	case "Helvetica", "Helvetica-Oblique":
		return &stdFont{base: name, widths: &helveticaWidths}, true
	case "Helvetica-Bold", "Helvetica-BoldOblique":
		return &stdFont{base: name, widths: &helveticaBoldWidths}, true
	case "Times-Roman", "Times-Bold", "Times-Italic", "Times-BoldItalic",
		"Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique",
		"Symbol", "ZapfDingbats":
		return &stdFont{base: name, widths: &helveticaWidths}, true
	}
	return nil, false
}

// stdFont is a built-in (non-embedded) Type1 font backed by an AFM width table
// and WinAnsi encoding. See afm.go.
type stdFont struct {
	base   string      // /BaseFont name
	widths *[256]int16 // per-1000-em advance keyed by WinAnsi byte
}

var _ Font = (*stdFont)(nil)
