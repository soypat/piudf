package piupage

import (
	"image/color"
	"os"
	"testing"
)

func BenchmarkCanvasPage(b *testing.B) {
	std, ok := Standard14("Helvetica")
	if !ok {
		b.Fatal("Helvetica is not available")
	}
	ttf, err := TrueType(testFontData(b))
	if err != nil {
		b.Fatal(err)
	}
	for _, f := range []Font{std, ttf} {
		b.Run(f.BaseName(), func(b *testing.B) {
			var c Canvas
			scratch := make([]byte, 4096)
			c.Reset(scratch)
			// One unmeasured page grows the content buffer and, for an
			// embedded face, records the glyphs so Encode's sorted insert
			// into the used set is warm.
			page(&c, f, scratch)
			b.SetBytes(int64(len(c.Bytes())))
			b.ReportAllocs()
			for b.Loop() {
				page(&c, f, scratch)
			}
		})
	}
}

func BenchmarkPathRecord(b *testing.B) {
	var p Path
	ops, pts := make([]byte, 0, 64), make([]float64, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		p.Reset(ops, pts)
		p.MoveTo(0, 0)
		for i := range 10 {
			p.CubicTo(float64(i), 1, 2, 3, 4, 5)
			p.QuadTo(1, 2, 3, 4)
		}
		p.Close()
		if p.Err() != nil {
			b.Fatal(p.Err())
		}
	}
}

func BenchmarkPathPaint(b *testing.B) {
	var p Path
	p.MoveTo(10, 10)
	for i := range 20 {
		p.CubicTo(float64(i), 1, 2, 3, 4, 5)
	}
	p.Close()
	pen := Pen{Color: color.Black, Width: 2, Cap: RoundCap, Dash: []float64{4, 2}}
	// color.White is a Gray16 value, not an interface, so naming it inside the
	// loop would box it on every call and measure that instead.
	var fill color.Color = color.White

	var c Canvas
	scratch := make([]byte, 4096)
	c.Reset(scratch)
	b.ReportAllocs()
	for b.Loop() {
		c.Reset(scratch)
		s := c.Save()
		c.Translate(72, 720)
		c.Rotate(0.3)
		c.Clip(&p)
		c.FillStroke(&p, fill, pen)
		c.Restore(s)
		c.Bytes()
	}
}

func BenchmarkCanvasMarkRewind(b *testing.B) {
	f, _ := Standard14("Helvetica")
	var c Canvas
	scratch := make([]byte, 4096)
	c.Reset(scratch)
	b.ReportAllocs()
	for b.Loop() {
		m := c.Mark()
		s := c.Save()
		c.Translate(10, 10)
		c.SetFont(f, 10)
		c.Text(0, 0, benchLine, color.Black)
		c.Restore(s)
		c.Rewind(m)
	}
}

// benchLine is text a report might actually carry: mixed case, punctuation and
// accents, which is what makes the encoding path do work.
const benchLine = "The quick brown fox jumps over the lazy dog — résumé, café, 0123456789."

// page draws a page of text, rules and links, and flushes it.
func page(c *Canvas, f Font, scratch []byte) {
	var col color.Color = color.RGBA{R: 0x22, G: 0x22, B: 0x22, A: 0xff}
	c.Reset(scratch)
	c.SetFont(f, 11)
	y := 720.0
	for range 48 {
		c.Text(72, y, benchLine, col)
		c.TextRight(523, y, "1", col)
		c.Line(72, y-2, 523, y-2, 0.5, col)
		c.FillRect(72, y-12, 451, 10, col)
		c.StrokeRect(72, y-12, 451, 10, 0.5, col)
		c.Link(72, y, 120, 12, "https://pkg.go.dev/github.com/soypat/piudf")
		y -= 13.2
	}
	c.Bytes()
	c.Fonts()
}

const testFontPath = "../testdata/DejaVuSans.ttf"

func testFontData(t testing.TB) []byte {
	data, err := os.ReadFile(testFontPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
