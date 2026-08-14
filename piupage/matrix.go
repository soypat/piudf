package piupage

import "math"

// Matrix is a PDF transformation matrix, the six operands of the cm operator.
// It stands for the 3x3 affine
//
//	| A B 0 |
//	| C D 0 |
//	| E F 1 |
//
// so that a point is mapped by (x·A + y·C + E, x·B + y·D + F). The zero Matrix
// is degenerate, not the identity; use [Identity].
type Matrix struct {
	A, B, C, D, E, F float64
}

// Identity is the transform that maps every point to itself.
func Identity() Matrix { return Matrix{A: 1, D: 1} }

func (m Matrix) Translate(tx, ty float64) Matrix {
	return m.Mul(mattranslate(tx, ty))
}
func (m Matrix) Scale(sx, sy float64) Matrix {
	return m.Mul(matscale(sx, sy))
}

// Rotate returns the transform that rotates counterclockwise by rad radians
// about the origin.
func (m Matrix) Rotate(rad float64) Matrix { return m.Mul(matrotate(rad)) }

func matrotate(rad float64) Matrix {
	s, c := math.Sincos(rad)
	return Matrix{A: c, B: s, C: -s, D: c}
}

// Translate returns the transform that moves the origin to (tx, ty).
func mattranslate(tx, ty float64) Matrix { return Matrix{A: 1, D: 1, E: tx, F: ty} }

func matscale(sx, sy float64) Matrix {
	return Matrix{A: sx, D: sy}
}

// Mul returns m applied first, then n — the composition n∘m, which is the order
// PDF's cm concatenates in: the new matrix premultiplies the one in effect.
func (m Matrix) Mul(n Matrix) Matrix {
	return Matrix{
		A: m.A*n.A + m.B*n.C,
		B: m.A*n.B + m.B*n.D,
		C: m.C*n.A + m.D*n.C,
		D: m.C*n.B + m.D*n.D,
		E: m.E*n.A + m.F*n.C + n.E,
		F: m.E*n.B + m.F*n.D + n.F,
	}
}

// Apply maps the point (x, y) through m.
func (m Matrix) Apply(x, y float64) (float64, float64) {
	return x*m.A + y*m.C + m.E, x*m.B + y*m.D + m.F
}

// IsIdentity reports whether m maps every point to itself.
func (m Matrix) IsIdentity() bool { return m == Identity() }
