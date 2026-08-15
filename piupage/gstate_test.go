package piupage

import (
	"image/color"
	"strings"
	"testing"
)

func TestSaveRestoreBalance(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	if err := c.Err(); err != nil {
		t.Fatal(err)
	}
	s := c.Save()
	if c.Err() != errUnclosed {
		t.Error("an open Save must be reported by Err")
	}
	c.Restore(s)
	if err := c.Err(); err != nil {
		t.Errorf("balanced q/Q reported %v", err)
	}
	got := string(c.Bytes())
	if strings.Count(got, "q") != 1 || strings.Count(got, "Q") != 1 {
		t.Errorf("want one q and one Q, got %q", got)
	}
}

func TestRestoreUnwindsToDepth(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	outer := c.Save()
	c.Save()
	c.Save()
	// Restoring the outermost token unwinds the two left open in between,
	// which is what keeps a forgotten Restore from corrupting the stream.
	c.Restore(outer)
	if err := c.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
	got := string(c.Bytes())
	if n := strings.Count(got, "Q"); n != 3 {
		t.Errorf("emitted %d Q for 3 open saves: %q", n, got)
	}
	// Restoring a token already unwound is a no-op, not an underflow.
	c.Restore(outer)
	if n := strings.Count(string(c.Bytes()), "Q"); n != 3 {
		t.Error("restoring twice emitted extra Q")
	}
}

func TestSaveOverflow(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	for range maxGDepth {
		c.Save()
	}
	if err := c.Err(); err != errUnclosed {
		t.Fatalf("Err = %v, want the unclosed-save error", err)
	}
	c.Save()
	if err := c.Err(); err != errGDepth {
		t.Errorf("Err = %v, want errGDepth", err)
	}
}

func TestTransformTracksCTM(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	if !c.CTM().IsIdentity() {
		t.Error("a fresh canvas is not at the identity")
	}
	c.Translate(100, 500)
	x, y := c.DeviceXY(0, 0)
	closeXY(t, x, y, 100, 500)

	c.Scale(2, 2)
	// Scale applies inside the translate: (3,4) -> (6,8) -> (106,508).
	x, y = c.DeviceXY(3, 4)
	closeXY(t, x, y, 106, 508)

	if got := string(c.Bytes()); !strings.Contains(got, "1 0 0 1 100 500 cm") {
		t.Errorf("translate did not emit cm:\n%s", got)
	}
}

func TestRestoreRestoresCTM(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	c.Translate(10, 10)
	before := c.CTM()

	s := c.Save()
	c.Translate(100, 100)
	c.Scale(3, 3)
	if c.CTM() == before {
		t.Fatal("transforms inside the save did not change the CTM")
	}
	c.Restore(s)

	if c.CTM() != before {
		t.Errorf("CTM after restore = %+v, want %+v", c.CTM(), before)
	}
}

func TestRewindRestoresGraphicsState(t *testing.T) {
	// This is the interaction that piudoc.Measure depends on: a measured element
	// may save and transform, and rewinding its bytes must take the canvas's
	// idea of the transform back with them.
	var c Canvas
	c.Reset(make([]byte, 512))
	c.Translate(5, 5)
	m := c.Mark()
	wantCTM := c.CTM()

	c.Save()
	c.Translate(200, 300)
	c.Rotate(1.2)
	if c.CTM() == wantCTM {
		t.Fatal("the measured draw did not change the CTM")
	}

	c.Rewind(m)
	if c.CTM() != wantCTM {
		t.Errorf("CTM after rewind = %+v, want %+v", c.CTM(), wantCTM)
	}
	if err := c.Err(); err != nil {
		t.Errorf("rewind left the state unbalanced: %v", err)
	}
	// The canvas must still be usable, at the transform it had at the mark.
	x, y := c.DeviceXY(0, 0)
	closeXY(t, x, y, 5, 5)
}

func TestLinkMapsThroughCTM(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	c.Link(1, 2, 30, 40, "https://example.com/plain")

	s := c.Save()
	c.Translate(100, 500)
	c.Link(0, 0, 80, 20, "https://example.com/moved")
	c.Restore(s)

	links := c.Links()
	if len(links) != 2 {
		t.Fatalf("got %d links, want 2", len(links))
	}
	if got := links[0]; got.X != 1 || got.Y != 2 || got.W != 30 || got.H != 40 {
		t.Errorf("untransformed link = %+v, want (1,2,30,40)", got)
	}
	// Annotations are page-space: the translate has to be baked into the rect.
	if got := links[1]; got.X != 100 || got.Y != 500 || got.W != 80 || got.H != 20 {
		t.Errorf("translated link = %+v, want (100,500,80,20)", got)
	}
}

func TestLinkUnderRotationIsBounded(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	c.Rotate(3.14159265358979 / 2) // quarter turn: the rect has no axis-aligned image
	c.Link(0, 0, 10, 4, "https://example.com/rotated")

	got := c.Links()[0]
	// The corners land at (0,0), (0,10), (-4,10), (-4,0).
	closeXY(t, got.X, got.Y, -4, 0)
	closeXY(t, got.W, got.H, 4, 10)
}

func TestClipRequiresSaveToUndo(t *testing.T) {
	var p Path
	p.Rect(0, 0, 50, 50)
	got := drawn(t, func(c *Canvas) {
		s := c.Save()
		c.Clip(&p)
		c.FillRect(0, 0, 100, 100, color.Black)
		c.Restore(s)
		c.FillRect(0, 0, 100, 100, color.White)
	})
	// W n establishes the clip without painting the path itself.
	if !strings.Contains(got, "W n") {
		t.Errorf("clip did not end with 'W n':\n%s", got)
	}
	if i, j := strings.Index(got, "q"), strings.Index(got, "W"); i < 0 || j < i {
		t.Error("clip must be bracketed by a save to be undoable")
	}
}

func TestResetClearsGraphicsState(t *testing.T) {
	var c Canvas
	c.Reset(make([]byte, 512))
	c.Save()
	c.Translate(50, 50)
	if err := c.Err(); err == nil {
		t.Fatal("expected an unbalanced state before reset")
	}
	c.Reset(make([]byte, 512))
	if err := c.Err(); err != nil {
		t.Errorf("Reset left %v", err)
	}
	if !c.CTM().IsIdentity() {
		t.Error("Reset did not return the CTM to the identity")
	}
}
