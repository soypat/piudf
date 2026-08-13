// package piupage is an imperative PDF drawing surface: text, lines and
// rectangles on a single page, with color and standard-14 font support. It is
// the low-level half of a reportlab-style report generator (the flowable layer
// lives in github.com/soypat/piudf/piupage/piudoc) and emits PDF content-stream
// operators through a piulex.Emitter into an in-memory buffer.
//
// Colors are stdlib image/color values; DeviceRGB is derived from RGBA().
package piupage

import "image/color"

// Units expressed in PDF points (1/72 inch, the native content-stream unit).
const (
	Pt   = 1.0
	Inch = 72.0 * Pt
	MM   = Inch / 25.4
	CM   = 10 * MM
)

// HexColor parses an "#rrggbb" or "rrggbb" hex string into an opaque color. An
// unparseable string yields black.
func HexColor(s string) color.RGBA {
	if len(s) > 0 && s[0] == '#' {
		s = s[1:]
	}
	c := color.RGBA{A: 0xff}
	if len(s) != 6 {
		return color.RGBA{A: 0xff}
	}
	var v [3]uint8
	for i := 0; i < 3; i++ {
		hi, ok1 := hexNibble(s[i*2])
		lo, ok2 := hexNibble(s[i*2+1])
		if !ok1 || !ok2 {
			return color.RGBA{A: 0xff}
		}
		v[i] = uint8(hi<<4 | lo)
	}
	c.R, c.G, c.B = v[0], v[1], v[2]
	return c
}

func hexNibble(b byte) (int, bool) {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0'), true
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10, true
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10, true
	}
	return 0, false
}

// rgb converts a color.Color to DeviceRGB components in [0,1]. A nil color is
// treated as opaque black.
func rgb(c color.Color) (r, g, b float64) {
	if c == nil {
		return 0, 0, 0
	}
	ri, gi, bi, _ := c.RGBA() // 16-bit, alpha-premultiplied
	return float64(ri) / 0xffff, float64(gi) / 0xffff, float64(bi) / 0xffff
}
