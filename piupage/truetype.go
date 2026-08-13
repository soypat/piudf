package piupage

import (
	"errors"
	"slices"
	"strconv"

	"github.com/soypat/lefevre"
	"github.com/soypat/lefevre/sfnt"
	"github.com/soypat/piudf"
)

// TrueType wraps a TrueType-outline font program for embedding, using lefevre
// to read the font's cmap and metrics. The returned Font encodes text as
// two-byte glyph ids under Identity-H, so it is not limited to the 256 codes
// [Standard14] can reach and carries no encoding assumptions of its own.
//
// data is retained, not copied: it must stay unmodified until the document is
// written. Only the glyphs actually drawn are recorded, and only those are
// embedded — the font program written into the PDF is a subset built at write
// time (see [Font] and Encoder use in the piudoc layer, which writes fonts
// after the whole story has been drawn).
//
// OpenType/CFF fonts (an 'OTTO' sfnt) have no glyf table to subset and are
// rejected; feed a .ttf.
func TrueType(data []byte) (Font, error) {
	fnt, err := lefevre.FontFromMemory(data, 0)
	if err != nil {
		return nil, err
	} else if !fnt.IsValid() {
		return nil, errors.New("piupage: font parsed but invalid")
	}
	info := fnt.Info()
	if info.UnitsPerEm == 0 {
		return nil, errors.New("piupage: font declares no units per em")
	}
	if fnt.OutlineFormat() != lefevre.OutlineGlyf {
		return nil, sfnt.ErrNoOutlines
	}
	f := &ttfFont{
		fnt:  fnt,
		info: info,
		upem: float64(info.UnitsPerEm),
	}
	f.name = info.PostScriptName
	if f.name == "" {
		f.name = info.FullName
	}
	if f.name == "" {
		f.name = "Embedded"
	}
	return f, nil
}

// ttfFont is an embedded TrueType font written as a Type0/Identity-H composite
// font over a CIDFontType2 descendant. [sfnt.Subsetter] preserves glyph ids
// from the source program, which is what lets the descendant declare
// /CIDToGIDMap /Identity and spares this layer any renumbering.
type ttfFont struct {
	fnt  *lefevre.Font
	info lefevre.FontInfo
	name string
	upem float64
	// used holds the glyphs Encode has been asked for, sorted by glyph id: the
	// /W array, the /ToUnicode CMap and the subsetter all walk it in order.
	used []ttfGlyph
	// gids and sub are the subsetter's inputs and scratch, kept so that a
	// document embedding several faces reuses one set of buffers per face.
	gids []uint16
	sub  sfnt.Subsetter
}

// ttfGlyph is one referenced glyph and the rune that reached it, which is the
// pair /ToUnicode needs to make the drawn text extractable again.
type ttfGlyph struct {
	gid uint16
	r   rune
}

var _ Font = (*ttfFont)(nil)

func (f *ttfFont) BaseName() string { return f.name }

// Width reports r's advance as a fraction of the em.
func (f *ttfFont) Width(r rune) float64 {
	return float64(f.fnt.GlyphAdvance(f.fnt.GlyphID(r))) / f.upem
}

// Encode appends r's big-endian glyph id and records the glyph as used.
func (f *ttfFont) Encode(dst []byte, r rune) []byte {
	gid := f.fnt.GlyphID(r)
	i, found := slices.BinarySearchFunc(f.used, gid, func(g ttfGlyph, gid uint16) int {
		return int(g.gid) - int(gid)
	})
	if !found {
		f.used = slices.Insert(f.used, i, ttfGlyph{gid: gid, r: r})
	}
	return append(dst, byte(gid>>8), byte(gid))
}

// hexCodes reports that this font's codes are multi-byte, so text is better
// written as a hex string than as a literal one.
func (f *ttfFont) hexCodes() bool { return true }

// has reports whether the font's cmap resolves r to a glyph of its own.
func (f *ttfFont) has(r rune) bool { return f.fnt.GlyphID(r) != 0 }

// scale converts a font-unit measure to PDF glyph space (1/1000 em).
func (f *ttfFont) scale(v float64) float64 { return v * 1000 / f.upem }

// subsetTag derives the six-uppercase-letter subset prefix PDF requires on an
// embedded subset's /BaseFont, hashing the glyph set so the same document
// always produces the same tag.
func (f *ttfFont) subsetTag() string {
	var h uint32 = 2166136261
	for _, g := range f.used {
		h = (h ^ uint32(g.gid)) * 16777619
	}
	var tag [6]byte
	for i := range tag {
		tag[i] = 'A' + byte(h%26)
		h /= 26
	}
	return string(tag[:])
}

func (f *ttfFont) writeObjects(enc *piudf.Encoder) (piudf.ObjectID, error) {
	f.gids = f.gids[:0]
	for _, g := range f.used {
		f.gids = append(f.gids, g.gid)
	}
	program, err := f.sub.AppendSubset(nil, f.fnt, f.gids)
	if err != nil {
		return piudf.ObjectID{}, err
	}
	baseName := f.subsetTag() + "+" + f.name

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
	enc.Int(int64(f.descriptorFlags()))
	enc.Name("FontBBox")
	enc.ArrayOpen()
	enc.Real(f.scale(float64(f.info.XMin)))
	enc.Real(f.scale(float64(f.info.YMin)))
	enc.Real(f.scale(float64(f.info.XMax)))
	enc.Real(f.scale(float64(f.info.YMax)))
	enc.ArrayClose()
	enc.Name("ItalicAngle")
	enc.Real(f.info.ItalicAngle)
	enc.Name("Ascent")
	enc.Real(f.scale(float64(f.info.Ascent)))
	enc.Name("Descent")
	enc.Real(f.scale(float64(f.info.Descent)))
	enc.Name("CapHeight")
	enc.Real(f.capHeight())
	enc.Name("StemV")
	enc.Int(int64(f.stemV()))
	enc.Name("FontFile2")
	enc.Ref(fileID.Num, fileID.Gen)
	enc.DictClose()
	enc.EndObject()

	toUnicodeID, err := f.writeToUnicode(enc)
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
	enc.String([]byte("Adobe"))
	enc.Name("Ordering")
	enc.String([]byte("Identity"))
	enc.Name("Supplement")
	enc.Int(0)
	enc.DictClose()
	enc.Name("FontDescriptor")
	enc.Ref(descID.Num, descID.Gen)
	enc.Name("DW")
	enc.Int(1000)
	enc.Name("W")
	f.writeWidths(enc)
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

// writeWidths emits the /W array, collapsing consecutive glyph ids into one
// "first [w w w]" run so the array stays short.
func (f *ttfFont) writeWidths(enc *piudf.Encoder) {
	enc.ArrayOpen()
	for i := 0; i < len(f.used); {
		j := i + 1
		for j < len(f.used) && f.used[j].gid == f.used[j-1].gid+1 {
			j++
		}
		enc.Int(int64(f.used[i].gid))
		enc.ArrayOpen()
		for _, g := range f.used[i:j] {
			enc.Real(f.scale(float64(f.fnt.GlyphAdvance(g.gid))))
		}
		enc.ArrayClose()
		i = j
	}
	enc.ArrayClose()
}

// writeToUnicode emits the CMap that maps this font's glyph ids back to the
// runes they came from. Without it the drawn text is a bag of glyph indices:
// copy-paste and full-text search out of the finished PDF both fail.
func (f *ttfFont) writeToUnicode(enc *piudf.Encoder) (piudf.ObjectID, error) {
	var b []byte
	b = append(b, "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n"...)
	b = append(b, "/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n"...)
	b = append(b, "/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n"...)
	b = append(b, "1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n"...)
	// A bfchar section may hold at most 100 mappings (ISO 32000-1 9.10.3).
	const perSection = 100
	for i := 0; i < len(f.used); i += perSection {
		sec := f.used[i:min(i+perSection, len(f.used))]
		b = strconv.AppendInt(b, int64(len(sec)), 10)
		b = append(b, " beginbfchar\n"...)
		for _, g := range sec {
			b = append(b, '<')
			b = appendHex16(b, g.gid)
			b = append(b, "> <"...)
			b = appendUTF16BE(b, g.r)
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

// descriptorFlags builds the /Flags bitfield: fixed pitch (1), nonsymbolic
// (32) and italic (64) are the bits that matter for a text font.
func (f *ttfFont) descriptorFlags() int {
	const (
		flagFixedPitch  = 1 << 0
		flagNonsymbolic = 1 << 5
		flagItalic      = 1 << 6
	)
	flags := flagNonsymbolic
	if f.info.IsFixedPitch {
		flags |= flagFixedPitch
	}
	if f.info.StyleFlags&lefevre.FontStyleItalic != 0 {
		flags |= flagItalic
	}
	return flags
}

// capHeight reports the cap height in glyph space, falling back to the 'H'
// bounding box for fonts whose OS/2 table leaves the field zero.
func (f *ttfFont) capHeight() float64 {
	if f.info.CapHeight != 0 {
		return f.scale(float64(f.info.CapHeight))
	}
	_, _, _, yMax := f.fnt.GlyphBounds(f.fnt.GlyphID('H'))
	if yMax != 0 {
		return f.scale(float64(yMax))
	}
	return 700
}

// stemV estimates the dominant vertical stem width, which no sfnt table
// records. Viewers use it only to synthesize a substitute face, which an
// embedded font never needs, so a weight-derived estimate is enough.
func (f *ttfFont) stemV() int {
	if f.info.Weight >= lefevre.FontWeightBold {
		return 165
	}
	return 80
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
