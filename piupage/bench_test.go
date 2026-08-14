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
