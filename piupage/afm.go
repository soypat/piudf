package canvas

import ppdf "github.com/soypat/piudf"

// This file will carry the Adobe AFM advance-width tables for the Helvetica
// family (per-1000-em widths keyed by WinAnsi byte) and the CP1252/WinAnsi
// rune->byte map. First cut ships the Helvetica fonts the invoice uses; the
// remaining standard-14 tables are additive.

// helveticaWidths is the AFM advance table for Helvetica, WinAnsi-keyed.
// TODO: populate from the Adobe AFM metrics.
var helveticaWidths [256]int16

// helveticaBoldWidths is the AFM advance table for Helvetica-Bold.
// TODO: populate from the Adobe AFM metrics.
var helveticaBoldWidths [256]int16

// winansiByte maps a rune to its WinAnsi (CP1252) code, ok=false when the rune
// has no WinAnsi representation.
func winansiByte(r rune) (b byte, ok bool) { panic("todo: canvas.winansiByte") }

func (f *stdFont) BaseName() string { return f.base }

func (f *stdFont) Width(r rune) float64 {
	panic("todo: stdFont.Width")
}

func (f *stdFont) Encode(dst []byte, r rune) []byte {
	panic("todo: stdFont.Encode")
}

func (f *stdFont) writeObjects(enc *ppdf.Encoder) (ppdf.ObjectID, error) {
	// Emits << /Type /Font /Subtype /Type1 /BaseFont f.base
	//          /Encoding /WinAnsiEncoding >>
	panic("todo: stdFont.writeObjects")
}
