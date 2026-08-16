package piupage

import (
	"image/color"
	"testing"
)

func TestCanvasRewind(t *testing.T) {
	helv, _ := Standard14("Helvetica")
	bold, _ := Standard14("Helvetica-Bold")

	var c Canvas
	if err := c.Reset(make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	c.SetFont(helv, 10)
	c.Text(10, 700, "kept", color.Black)
	c.Link(0, 0, 10, 10, "https://example.com/kept")

	want := string(c.Bytes())
	wantFonts, wantLinks := len(c.Fonts()), len(c.Links())
	m := c.Mark()

	c.SetFont(bold, 14)
	c.Text(10, 680, "retracted", color.Black)
	c.FillRect(0, 0, 100, 100, color.White)
	c.Link(0, 0, 10, 10, "https://example.com/retracted")
	if string(c.Bytes()) == want {
		t.Fatal("drawing after the mark changed nothing")
	}

	c.Rewind(m)
	if got := string(c.Bytes()); got != want {
		t.Errorf("content after rewind:\n got %q\nwant %q", got, want)
	}
	if got := len(c.Fonts()); got != wantFonts {
		t.Errorf("fonts after rewind = %d, want %d", got, wantFonts)
	}
	if got := len(c.Links()); got != wantLinks {
		t.Errorf("links after rewind = %d, want %d", got, wantLinks)
	}

	// The font current at the mark must be current again, or the next Text
	// would silently draw nothing.
	c.Text(10, 660, "after", color.Black)
	if string(c.Bytes()) == want {
		t.Error("drawing after rewind emitted nothing: current font was lost")
	}
}

func TestCanvasRewindToStart(t *testing.T) {
	helv, _ := Standard14("Helvetica")
	var c Canvas
	c.Reset(make([]byte, 512))
	m := c.Mark()
	c.SetFont(helv, 10)
	c.Text(10, 700, "all of it", color.Black)
	c.Rewind(m)
	if got := c.Bytes(); len(got) != 0 {
		t.Errorf("rewind to start left %q", got)
	}
	if got := len(c.Fonts()); got != 0 {
		t.Errorf("rewind to start left %d fonts", got)
	}
}

func TestStandard14Singleton(t *testing.T) {
	a, ok := Standard14("Helvetica")
	if !ok {
		t.Fatal("Helvetica not found")
	}
	b, _ := Standard14("Helvetica")
	if a != b {
		t.Error("Standard14 returns a distinct font per call")
	}
	if _, ok := Standard14("Nonesuch"); ok {
		t.Error("unknown font resolved")
	}
	for _, name := range []string{"Helvetica-Bold", "Times-Roman", "Courier-BoldOblique", "ZapfDingbats"} {
		f, ok := Standard14(name)
		if !ok {
			t.Errorf("%s not found", name)
		} else if f.PostScriptName() != name {
			t.Errorf("%s resolved to %s", name, f.PostScriptName())
		}
	}
}
