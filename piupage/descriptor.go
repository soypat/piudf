package piupage

import "github.com/soypat/lefevre/sfnt"

// descriptor is what a PDF /FontDescriptor needs, in font units, read straight
// out of the sfnt tables.
type descriptor struct {
	upem                   float64
	xMin, yMin, xMax, yMax int16
	ascent, descent        int16
	capHeight              int16
	weight                 int // OS/2 usWeightClass, 100..1000
	italicAngle            float64
	fixedPitch             bool
	italic                 bool
}

func readDescriptor(src sfnt.Source, upem int) descriptor {
	d := descriptor{upem: float64(upem)}
	if head := src.Table("head"); len(head) >= 54 {
		d.xMin = s16(head, 36)
		d.yMin = s16(head, 38)
		d.xMax = s16(head, 40)
		d.yMax = s16(head, 42)
	}
	if os2 := src.Table("OS/2"); len(os2) >= 68 {
		d.weight = int(u16(os2, 4))
		if len(os2) >= 64 {
			d.italic = u16(os2, 62)&(1<<0) != 0
		}
		if len(os2) >= 74 {
			d.ascent = s16(os2, 68)
			d.descent = s16(os2, 70)
		}
		if len(os2) >= 90 {
			d.capHeight = s16(os2, 88)
		}
	}
	// hhea only fills what OS/2 left at zero, which is the same precedence the
	// vertical metrics are read with everywhere else.
	if hhea := src.Table("hhea"); len(hhea) >= 10 {
		if d.ascent == 0 {
			d.ascent = s16(hhea, 4)
		}
		if d.descent == 0 {
			d.descent = s16(hhea, 6)
		}
	}
	if post := src.Table("post"); len(post) >= 16 {
		// italicAngle is a 16.16 fixed-point count of degrees.
		d.italicAngle = float64(int32(u32(post, 4))) / 65536
		d.fixedPitch = u32(post, 12) != 0
	}
	return d
}

// scale converts a font-unit measure to PDF glyph space (1/1000 em).
func (d descriptor) scale(v float64) float64 { return v * 1000 / d.upem }

// flags builds the /Flags bitfield: fixed pitch (1), nonsymbolic (32) and
// italic (64) are the bits that matter for a text font.
func (d descriptor) flags() int {
	const (
		flagFixedPitch  = 1 << 0
		flagNonsymbolic = 1 << 5
		flagItalic      = 1 << 6
	)
	flags := flagNonsymbolic
	if d.fixedPitch {
		flags |= flagFixedPitch
	}
	if d.italic {
		flags |= flagItalic
	}
	return flags
}

// stemV estimates the dominant vertical stem width.
func (d descriptor) stemV() int {
	if d.weight > 600 {
		return 165
	}
	return 80
}

// capHeightOf reports the cap height in glyph space, falling back to the 'H'
// glyph's own bounding box for a font whose OS/2 table leaves the field zero.
func (d descriptor) capHeightOf(f Font, src sfnt.Source) float64 {
	if d.capHeight != 0 {
		return d.scale(float64(d.capHeight))
	}
	// A glyf record opens with the contour count and then its bounding box, so
	// the record itself carries the height without any table walk.
	if rec := src.GlyphData(f.GlyphID('H')); len(rec) >= 10 {
		if yMax := s16(rec, 8); yMax != 0 {
			return d.scale(float64(yMax))
		}
	}
	return 700
}

func u16(b []byte, i int) uint16 { return uint16(b[i])<<8 | uint16(b[i+1]) }

func s16(b []byte, i int) int16 { return int16(u16(b, i)) }

func u32(b []byte, i int) uint32 {
	return uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
}
