package piupage

import (
	"math"
	"testing"
)

func closeXY(t *testing.T, gotX, gotY, wantX, wantY float64) {
	t.Helper()
	const eps = 1e-9
	if math.Abs(gotX-wantX) > eps || math.Abs(gotY-wantY) > eps {
		t.Errorf("got (%v, %v), want (%v, %v)", gotX, gotY, wantX, wantY)
	}
}

func TestMatrixApply(t *testing.T) {
	x, y := Identity().Apply(3, 4)
	closeXY(t, x, y, 3, 4)

	x, y = Identity().Translate(10, -5).Apply(3, 4)
	closeXY(t, x, y, 13, -1)

	x, y = Identity().Scale(2, 3).Apply(3, 4)
	closeXY(t, x, y, 6, 12)

	// A quarter turn counterclockwise takes +x to +y.
	x, y = Identity().Rotate(math.Pi/2).Apply(1, 0)
	closeXY(t, x, y, 0, 1)
	x, y = Identity().Rotate(math.Pi/2).Apply(0, 1)
	closeXY(t, x, y, -1, 0)
}

func TestMatrixMulOrder(t *testing.T) {
	// m.Mul(n) is m first, then n. Scaling then translating moves by the
	// untouched offset; translating then scaling multiplies the offset too.
	scaleThenMove := Identity().Scale(2, 2).Translate(10, 0)
	x, y := scaleThenMove.Apply(1, 1)
	closeXY(t, x, y, 12, 2)

	moveThenScale := Identity().Translate(10, 0).Mul(Identity().Scale(2, 2))
	x, y = moveThenScale.Apply(1, 1)
	closeXY(t, x, y, 22, 2)
}

func TestMatrixMulAssociative(t *testing.T) {
	a, b, c := Identity().Translate(3, 7), Identity().Rotate(0.4), Identity().Scale(2, 5)
	left := a.Mul(b).Mul(c)
	right := a.Mul(b.Mul(c))
	lx, ly := left.Apply(1.5, -2.5)
	rx, ry := right.Apply(1.5, -2.5)
	closeXY(t, lx, ly, rx, ry)
}

func TestMatrixInverseRoundTrip(t *testing.T) {
	// Composing a transform with the reverse operations in reverse order
	// returns to where it started.
	m := Identity().Translate(30, 40).Mul(Identity().Rotate(math.Pi/3)).Scale(2, 2)
	back := Identity().Scale(0.5, 0.5).Rotate(-math.Pi/3).Translate(-30, -40)
	x, y := m.Mul(back).Apply(11, -13)
	closeXY(t, x, y, 11, -13)
}

func TestMatrixIdentity(t *testing.T) {
	if !Identity().IsIdentity() {
		t.Error("Identity is not identity")
	}
	if (Matrix{}).IsIdentity() {
		t.Error("the zero Matrix must not pass as identity: it is degenerate")
	}
	if !Identity().Translate(0, 0).IsIdentity() || !Identity().Scale(1, 1).IsIdentity() {
		t.Error("a null translate or unit scale should be the identity")
	}
}
