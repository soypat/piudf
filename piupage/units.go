package piupage

import "math"

// Unit is a length unit expressed as PDF points per unit: the scale factor
// between a document's coordinates and PDF user space, which is always 1/72
// inch. A Canvas carries one, so every length handed to it — coordinates,
// widths, font sizes — is in that unit and the point is never named again.
//
// Unit is a defined type rather than an untyped constant on purpose: a length
// is a float64 and a Unit is not, so the old idiom of writing 22*MM into a
// length field no longer compiles. The conversion is now the document's job.
type Unit float64

// The units a page can be measured in.
const (
	Pt   Unit = 1
	Inch Unit = 72 * Pt
	MM   Unit = Inch / 25.4
	CM   Unit = 10 * MM
)

// ToPt converts a length in u to PDF points.
func (u Unit) ToPt(v float64) float64 { return v * float64(u) }

// FromPt converts a length in PDF points to u. It is how a caller who thinks
// in typographic points expresses one in a document that does not: a 9.5pt
// body face in a millimetre document is Style{Size: MM.FromPt(9.5)}.
func (u Unit) FromPt(v float64) float64 { return v / float64(u) }

// emitPerPt is the reciprocal of the resolution lengths are rounded to on
// their way into a content stream: ten thousand steps to the point. Emitted
// reals are written at shortest round-trip precision, so an irrational scale
// factor would otherwise turn a two-digit coordinate into seventeen digits of
// noise on every operand of every page. A ten-thousandth of a point is some
// three orders of magnitude finer than anything a rasterizer resolves, so the
// rounding is invisible and only the digits it saves are not.
//
// It is a power of ten held exactly by a float64 and applied as a division,
// which is what keeps the result the nearest float64 to a short decimal — and
// so keeps the shortest round-trip form short.
const emitPerPt = 1e4

// quantize rounds a length in points to the emission resolution.
func quantize(pts float64) float64 {
	if math.IsInf(pts, 0) || math.IsNaN(pts) {
		return pts // Left for the emitter to reject.
	}
	return math.Round(pts*emitPerPt) / emitPerPt
}

// EmitPt converts v, a length in u, to the points that should be written to a
// PDF for it. It is the one conversion between a document's units and the
// file's, used both for content-stream operands and for the page geometry that
// lives outside a content stream — /MediaBox, annotation rectangles and
// destinations — so that the two cannot round differently and drift apart.
func (u Unit) EmitPt(v float64) float64 { return quantize(u.ToPt(v)) }
