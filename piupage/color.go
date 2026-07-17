// Package canvas is an imperative PDF drawing surface: text, lines and
// rectangles on a single page, with color and standard-14 font support. It is
// the low-level half of a reportlab-style report generator (the flowable layer
// lives in github.com/soypat/piudf/doc) and emits PDF content-stream operators
// through a piulex.Emitter into an in-memory buffer.
package canvas

// Units expressed in PDF points (1/72 inch, the native content-stream unit).
const (
	Pt   = 1.0
	Inch = 72.0
	MM   = Inch / 25.4
	CM   = 10 * MM
)

// Color is a DeviceRGB color with components in [0,1].
type Color struct {
	R, G, B float64
}

// HexColor parses an "#rrggbb" or "rrggbb" hex string into a Color.
func HexColor(s string) Color { panic("todo: canvas.HexColor") }

// Gray returns a neutral color with all components set to v.
func Gray(v float64) Color { return Color{v, v, v} }
