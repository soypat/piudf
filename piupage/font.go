package piupage

import (
	"errors"
	"strconv"

	"github.com/soypat/lefevre/sfnt"
	"github.com/soypat/piudf"
)

var (
	errNotEmbeddable = errors.New("piupage: font carries no program and is not one of the standard 14")
	errNoUnitsPerEm  = errors.New("piupage: font declares no units per em")
)

// Font is one typeface described in its own terms: a design grid, a character
// map and advances. Every metric is in font units.
type Font interface {
	// PostScriptName is the /BaseFont name, e.g. "Helvetica-Bold".
	PostScriptName() string
	// UnitsPerEm is the design grid the advances below are expressed in
	UnitsPerEm() int
	// GlyphID is the glyph the face draws for r, or 0 if it has none.
	GlyphID(r rune) uint16
	// GlyphAdvance is glyph's horizontal advance in font units.
	GlyphAdvance(glyph uint16) int32
}

// GlyphUse is one glyph a canvas drew and the rune it was reached through: the
// pair a /ToUnicode map is built from, and the set a subset is cut to.
type GlyphUse struct {
	// Font indexes the canvas's [Canvas.Fonts], or the document's own font list
	// once a [Doc] has merged the pages.
	Font  uint16
	Glyph uint16
	Rune  rune
}

// StringWidth returns the rendered width of s in points at the given size.
func StringWidth(f Font, s string, size float64) float64 {
	upem := float64(f.UnitsPerEm())
	var w float64
	for _, r := range s {
		// Scaled per glyph rather than summed and scaled once. The two differ in
		// the last bit, and this is the order every position in the corpus was
		// laid out with.
		w += float64(f.GlyphAdvance(f.GlyphID(r))) / upem
	}
	return w * size
}

// Standard14 returns one of the built-in PDF fonts by /BaseFont name
// (WinAnsi-encoded, no embedded program).
func Standard14(name string) (f Font, ok bool) {
	for b := FontHelvetica; b <= FontZapfDingbats; b++ {
		if b.String() == name {
			return b, true
		}
	}
	return nil, false
}

var _ Font = FontBuiltin(0)

// WriteFont emits f's PDF object(s) into enc and returns the font dictionary's id.
func WriteFont(enc *piudf.Encoder, f Font, glyphs []GlyphUse) (piudf.ObjectID, error) {
	if src, ok := f.(sfnt.Source); ok {
		return writeEmbeddedFont(enc, f, src, glyphs)
	}
	return writeBuiltinFont(enc, f)
}

// writeEmbeddedFont emits a Type0/Identity-H composite font over a
// CIDFontType2 descendant. [sfnt.Subsetter] preserves glyph ids from the source
// program, which is what lets the descendant declare /CIDToGIDMap /Identity and
// spares this layer any renumbering.
func writeEmbeddedFont(enc *piudf.Encoder, f Font, src sfnt.Source, glyphs []GlyphUse) (piudf.ObjectID, error) {
	d := readDescriptor(src, f.UnitsPerEm())
	gids := make([]uint16, len(glyphs))
	for i := range glyphs {
		gids[i] = glyphs[i].Glyph
	}
	// The subsetter is local: cutting a subset is a write-time act, and two
	// documents written at once must not share its scratch.
	var sub sfnt.Subsetter
	program, err := sub.AppendSubset(nil, src, gids)
	if err != nil {
		return piudf.ObjectID{}, err
	}
	name := f.PostScriptName()
	if name == "" {
		// A face whose name table carries no PostScript name still needs a
		// /BaseFont: an empty one is a valid PDF name that no reader can report
		// or match against.
		name = "Embedded"
	}
	baseName := subsetTag(gids) + "+" + name

	// The font program itself. /Length1 is the program's own length, which is
	// what a consumer extracting the font back out needs.
	fileID := enc.NewID()
	enc.BeginObject(fileID)
	enc.DictOpen()
	enc.Name("Length")
	enc.Int(int64(len(program)))
	enc.Name("Length1")
	enc.Int(int64(len(program)))
	enc.DictClose()
	enc.BeginStreamPayload()
	if _, err := enc.PayloadWriter().Write(program); err != nil {
		return piudf.ObjectID{}, err
	}
	enc.EndStreamPayload()
	enc.EndObject()

	descID := enc.NewID()
	enc.BeginObject(descID)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("FontDescriptor")
	enc.Name("FontName")
	enc.Name(baseName)
	enc.Name("Flags")
	enc.Int(int64(d.flags()))
	enc.Name("FontBBox")
	enc.ArrayOpen()
	enc.Real(d.scale(float64(d.xMin)))
	enc.Real(d.scale(float64(d.yMin)))
	enc.Real(d.scale(float64(d.xMax)))
	enc.Real(d.scale(float64(d.yMax)))
	enc.ArrayClose()
	enc.Name("ItalicAngle")
	enc.Real(d.italicAngle)
	enc.Name("Ascent")
	enc.Real(d.scale(float64(d.ascent)))
	enc.Name("Descent")
	enc.Real(d.scale(float64(d.descent)))
	enc.Name("CapHeight")
	enc.Real(d.capHeightOf(f, src))
	enc.Name("StemV")
	enc.Int(int64(d.stemV()))
	enc.Name("FontFile2")
	enc.Ref(fileID.Num, fileID.Gen)
	enc.DictClose()
	enc.EndObject()

	toUnicodeID, err := writeToUnicode(enc, glyphs)
	if err != nil {
		return piudf.ObjectID{}, err
	}

	cidID := enc.NewID()
	enc.BeginObject(cidID)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("Font")
	enc.Name("Subtype")
	enc.Name("CIDFontType2")
	enc.Name("BaseFont")
	enc.Name(baseName)
	enc.Name("CIDSystemInfo")
	enc.DictOpen()
	enc.Name("Registry")
	enc.StringBytes([]byte("Adobe"))
	enc.Name("Ordering")
	enc.StringBytes([]byte("Identity"))
	enc.Name("Supplement")
	enc.Int(0)
	enc.DictClose()
	enc.Name("FontDescriptor")
	enc.Ref(descID.Num, descID.Gen)
	enc.Name("DW")
	enc.Int(1000)
	enc.Name("W")
	writeWidths(enc, f, d, gids)
	enc.Name("CIDToGIDMap")
	enc.Name("Identity")
	enc.DictClose()
	enc.EndObject()

	id := enc.NewID()
	enc.BeginObject(id)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("Font")
	enc.Name("Subtype")
	enc.Name("Type0")
	enc.Name("BaseFont")
	enc.Name(baseName)
	enc.Name("Encoding")
	enc.Name("Identity-H")
	enc.Name("DescendantFonts")
	enc.ArrayOpen()
	enc.Ref(cidID.Num, cidID.Gen)
	enc.ArrayClose()
	enc.Name("ToUnicode")
	enc.Ref(toUnicodeID.Num, toUnicodeID.Gen)
	enc.DictClose()
	enc.EndObject()
	return id, enc.Err()
}

// subsetTag derives the six-uppercase-letter subset prefix PDF requires on an
// embedded subset's /BaseFont, hashing the glyph set so the same document
// always produces the same tag.
func subsetTag(gids []uint16) string {
	var h uint32 = 2166136261
	for _, gid := range gids {
		h = (h ^ uint32(gid)) * 16777619
	}
	var tag [6]byte
	for i := range tag {
		tag[i] = 'A' + byte(h%26)
		h /= 26
	}
	return string(tag[:])
}

// writeWidths emits the /W array, collapsing consecutive glyph ids into one
// "first [w w w]" run so the array stays short.
func writeWidths(enc *piudf.Encoder, f Font, d descriptor, gids []uint16) {
	enc.ArrayOpen()
	for i := 0; i < len(gids); {
		j := i + 1
		for j < len(gids) && gids[j] == gids[j-1]+1 {
			j++
		}
		enc.Int(int64(gids[i]))
		enc.ArrayOpen()
		for _, gid := range gids[i:j] {
			enc.Real(d.scale(float64(f.GlyphAdvance(gid))))
		}
		enc.ArrayClose()
		i = j
	}
	enc.ArrayClose()
}

// writeToUnicode emits the CMap that maps this font's glyph ids back to the
// runes they came from. Without it the drawn text is a bag of glyph indices:
// copy-paste and full-text search out of the finished PDF both fail.
func writeToUnicode(enc *piudf.Encoder, glyphs []GlyphUse) (piudf.ObjectID, error) {
	var b []byte
	b = append(b, "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n"...)
	b = append(b, "/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n"...)
	b = append(b, "/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n"...)
	b = append(b, "1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n"...)
	// A bfchar section may hold at most 100 mappings (ISO 32000-1 9.10.3).
	const perSection = 100
	for i := 0; i < len(glyphs); i += perSection {
		sec := glyphs[i:min(i+perSection, len(glyphs))]
		b = strconv.AppendInt(b, int64(len(sec)), 10)
		b = append(b, " beginbfchar\n"...)
		for _, g := range sec {
			b = append(b, '<')
			b = appendHex16(b, g.Glyph)
			b = append(b, "> <"...)
			b = appendUTF16BE(b, g.Rune)
			b = append(b, ">\n"...)
		}
		b = append(b, "endbfchar\n"...)
	}
	b = append(b, "endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n"...)

	id := enc.NewID()
	enc.BeginObject(id)
	enc.DictOpen()
	enc.Name("Length")
	enc.Int(int64(len(b)))
	enc.DictClose()
	enc.BeginStreamPayload()
	if _, err := enc.PayloadWriter().Write(b); err != nil {
		return piudf.ObjectID{}, err
	}
	enc.EndStreamPayload()
	enc.EndObject()
	return id, enc.Err()
}

func appendHex16(dst []byte, v uint16) []byte {
	const hexdigits = "0123456789ABCDEF"
	return append(dst, hexdigits[v>>12&0xf], hexdigits[v>>8&0xf], hexdigits[v>>4&0xf], hexdigits[v&0xf])
}

// appendUTF16BE writes r as big-endian UTF-16, surrogate pair included, which
// is the encoding a /ToUnicode destination string uses.
func appendUTF16BE(dst []byte, r rune) []byte {
	if r > 0xffff {
		r -= 0x10000
		dst = appendHex16(dst, uint16(0xd800+(r>>10)))
		return appendHex16(dst, uint16(0xdc00+(r&0x3ff)))
	}
	return appendHex16(dst, uint16(r))
}

func writeBuiltinFont(enc *piudf.Encoder, f Font) (piudf.ObjectID, error) {
	b, ok := f.(FontBuiltin)
	if !ok || !b.IsValid() {
		return piudf.ObjectID{}, errNotEmbeddable
	}
	id := enc.NewID()
	enc.BeginObject(id)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("Font")
	enc.Name("Subtype")
	enc.Name("Type1")
	enc.Name("BaseFont")
	enc.Name(b.String())
	enc.Name("Encoding")
	enc.Name("WinAnsiEncoding")
	enc.DictClose()
	enc.EndObject()
	return id, enc.Err()
}

//go:generate go tool stringer -linecomment -type=FontBuiltin -output=stringers.go

// FontBuiltin is one of the 14 fonts every conforming PDF reader carries, so
// none is embedded. The zero value names no font.
type FontBuiltin uint8

const (
	_                        FontBuiltin = iota
	FontHelvetica                        // Helvetica
	FontHelveticaOblique                 // Helvetica-Oblique
	FontHelveticaBold                    // Helvetica-Bold
	FontHelveticaBoldOblique             // Helvetica-BoldOblique
	FontTimesRoman                       // Times-Roman
	FontTimesBold                        // Times-Bold
	FontTimesItalic                      // Times-Italic
	FontTimesBoldItalic                  // Times-BoldItalic
	FontCourier                          // Courier
	FontCourierBold                      // Courier-Bold
	FontCourierOblique                   // Courier-Oblique
	FontCourierBoldOblique               // Courier-BoldOblique
	FontSymbol                           // Symbol
	FontZapfDingbats                     // ZapfDingbats
)

// IsValid reports whether b names one of the 14, which the zero value does not.
func (b FontBuiltin) IsValid() bool { return b >= FontHelvetica && b <= FontZapfDingbats }

// PostScriptName is the font's /BaseFont name, e.g. "Helvetica-Bold".
func (b FontBuiltin) PostScriptName() string { return b.String() }

// UnitsPerEm is the design grid, 1000 for every Type1 face.
func (b FontBuiltin) UnitsPerEm() int { return 1000 }

// GlyphID is the WinAnsi code b draws r as, 0 if the encoding has none — which
// is the same "cannot spell this" a glyf font reports as .notdef.
func (b FontBuiltin) GlyphID(r rune) uint16 {
	c, ok := winansiByte(r)
	if !ok {
		return 0
	}
	return uint16(c)
}

// GlyphAdvance is a WinAnsi code's advance in font units. A code the width
// table does not carry falls back to '?', as an unencodable rune does.
func (b FontBuiltin) GlyphAdvance(glyph uint16) int32 {
	if glyph > 0xff {
		return 0
	}
	w := b.widths()
	adv := w[glyph]
	if adv == 0 {
		adv = w['?']
	}
	return int32(adv)
}

// widths is b's AFM advance table. Only the two Helvetica weights carry real
// metrics in this first cut; the rest stand in with Helvetica's.
func (b FontBuiltin) widths() *[256]int16 {
	switch b {
	case FontHelveticaBold, FontHelveticaBoldOblique:
		return &helveticaBoldWidths
	}
	return &helveticaWidths
}

// Adobe AFM advance-width tables for the Helvetica family, indexed by WinAnsi
// byte, in units of 1/1000 em. First cut ships the two weights the invoice
// uses; the rest of the standard-14 tables are purely additive.

var helveticaWidths = [256]int16{
	0x20: 278, 0x21: 278, 0x22: 355, 0x23: 556, 0x24: 556, 0x25: 889, 0x26: 667, 0x27: 191,
	0x28: 333, 0x29: 333, 0x2a: 389, 0x2b: 584, 0x2c: 278, 0x2d: 333, 0x2e: 278, 0x2f: 278,
	0x30: 556, 0x31: 556, 0x32: 556, 0x33: 556, 0x34: 556, 0x35: 556, 0x36: 556, 0x37: 556,
	0x38: 556, 0x39: 556, 0x3a: 278, 0x3b: 278, 0x3c: 584, 0x3d: 584, 0x3e: 584, 0x3f: 556,
	0x40: 1015, 0x41: 667, 0x42: 667, 0x43: 722, 0x44: 722, 0x45: 667, 0x46: 611, 0x47: 778,
	0x48: 722, 0x49: 278, 0x4a: 500, 0x4b: 667, 0x4c: 556, 0x4d: 833, 0x4e: 722, 0x4f: 778,
	0x50: 667, 0x51: 778, 0x52: 722, 0x53: 667, 0x54: 611, 0x55: 722, 0x56: 667, 0x57: 944,
	0x58: 667, 0x59: 667, 0x5a: 611, 0x5b: 278, 0x5c: 278, 0x5d: 278, 0x5e: 469, 0x5f: 556,
	0x60: 333, 0x61: 556, 0x62: 556, 0x63: 500, 0x64: 556, 0x65: 556, 0x66: 278, 0x67: 556,
	0x68: 556, 0x69: 222, 0x6a: 222, 0x6b: 500, 0x6c: 222, 0x6d: 833, 0x6e: 556, 0x6f: 556,
	0x70: 556, 0x71: 556, 0x72: 333, 0x73: 500, 0x74: 278, 0x75: 556, 0x76: 500, 0x77: 722,
	0x78: 500, 0x79: 500, 0x7a: 500, 0x7b: 334, 0x7c: 260, 0x7d: 334, 0x7e: 584,
	0x96: 556, 0x97: 1000, 0xa0: 278,
	0xdf: 611, 0xe1: 556, 0xe4: 556, 0xe9: 556, 0xed: 278, 0xf1: 556,
	0xf3: 556, 0xf6: 556, 0xfa: 556, 0xfc: 556,
}

var helveticaBoldWidths = [256]int16{
	0x20: 278, 0x21: 333, 0x22: 474, 0x23: 556, 0x24: 556, 0x25: 889, 0x26: 722, 0x27: 238,
	0x28: 333, 0x29: 333, 0x2a: 389, 0x2b: 584, 0x2c: 278, 0x2d: 333, 0x2e: 278, 0x2f: 278,
	0x30: 556, 0x31: 556, 0x32: 556, 0x33: 556, 0x34: 556, 0x35: 556, 0x36: 556, 0x37: 556,
	0x38: 556, 0x39: 556, 0x3a: 333, 0x3b: 333, 0x3c: 584, 0x3d: 584, 0x3e: 584, 0x3f: 611,
	0x40: 975, 0x41: 722, 0x42: 722, 0x43: 722, 0x44: 722, 0x45: 667, 0x46: 611, 0x47: 778,
	0x48: 722, 0x49: 278, 0x4a: 556, 0x4b: 722, 0x4c: 611, 0x4d: 833, 0x4e: 722, 0x4f: 778,
	0x50: 667, 0x51: 778, 0x52: 722, 0x53: 667, 0x54: 611, 0x55: 722, 0x56: 667, 0x57: 944,
	0x58: 667, 0x59: 667, 0x5a: 611, 0x5b: 333, 0x5c: 278, 0x5d: 333, 0x5e: 584, 0x5f: 556,
	0x60: 333, 0x61: 556, 0x62: 611, 0x63: 556, 0x64: 611, 0x65: 556, 0x66: 333, 0x67: 611,
	0x68: 611, 0x69: 278, 0x6a: 278, 0x6b: 556, 0x6c: 278, 0x6d: 889, 0x6e: 611, 0x6f: 611,
	0x70: 611, 0x71: 611, 0x72: 389, 0x73: 556, 0x74: 333, 0x75: 611, 0x76: 556, 0x77: 778,
	0x78: 556, 0x79: 556, 0x7a: 500, 0x7b: 389, 0x7c: 280, 0x7d: 389, 0x7e: 584,
	0x96: 556, 0x97: 1000, 0xa0: 278,
	0xdf: 611, 0xe1: 556, 0xe4: 556, 0xe9: 556, 0xed: 278, 0xf1: 611,
	0xf3: 611, 0xf6: 611, 0xfa: 611, 0xfc: 611,
}

// winansiByte maps a rune to its WinAnsi (CP1252) code.
func winansiByte(r rune) (b byte, ok bool) {
	switch {
	case r >= 0x20 && r <= 0x7e:
		return byte(r), true
	case r >= 0xa0 && r <= 0xff:
		return byte(r), true
	}
	switch r {
	case 0x20ac:
		return 0x80, true
	case 0x201a:
		return 0x82, true
	case 0x0192:
		return 0x83, true
	case 0x201e:
		return 0x84, true
	case 0x2026:
		return 0x85, true
	case 0x2020:
		return 0x86, true
	case 0x2021:
		return 0x87, true
	case 0x02c6:
		return 0x88, true
	case 0x2030:
		return 0x89, true
	case 0x0160:
		return 0x8a, true
	case 0x2039:
		return 0x8b, true
	case 0x0152:
		return 0x8c, true
	case 0x017d:
		return 0x8e, true
	case 0x2018:
		return 0x91, true
	case 0x2019:
		return 0x92, true
	case 0x201c:
		return 0x93, true
	case 0x201d:
		return 0x94, true
	case 0x2022:
		return 0x95, true
	case 0x2013:
		return 0x96, true
	case 0x2014:
		return 0x97, true
	case 0x02dc:
		return 0x98, true
	case 0x2122:
		return 0x99, true
	case 0x0161:
		return 0x9a, true
	case 0x203a:
		return 0x9b, true
	case 0x0153:
		return 0x9c, true
	case 0x017e:
		return 0x9e, true
	case 0x0178:
		return 0x9f, true
	}
	return 0, false
}
