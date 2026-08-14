package piudoc2

import (
	"io"
	"strings"
	"testing"

	"github.com/soypat/piudf/piupage"
)

var benchText = strings.Repeat("The quick <b>brown</b> fox jumps over the <i>lazy</i> dog. ", 12)

func BenchmarkParagraphDraw(b *testing.B) {
	var cv [1]piupage.Canvas
	buf := make([]byte, 4096)
	f := Frame{X: 72, Width: 451, Top: 770, Bottom: 72}
	p := P(benchText, Normal)
	b.ReportAllocs()
	for b.Loop() {
		cv[0].Reset(buf)
		_, _, err := p.Draw(cv[:], f, f.Top)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParagraphMeasure(b *testing.B) {
	var cv [1]piupage.Canvas
	buf := make([]byte, 4096)
	cv[0].Reset(buf)
	f := Frame{X: 72, Width: 451, Top: 770, Bottom: 72}
	p := P(benchText, Normal)
	b.ReportAllocs()
	for b.Loop() {
		_, err := Measure(cv[:], p, f, f.Top)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDocBuild(b *testing.B) {
	const npage = 8
	dst := make([]piupage.Canvas, npage)
	canvasBuf := make([]byte, npage*512)
	encBuf := make([]byte, 4096)
	story := make([]Drawer, 24)
	for i := range story {
		story[i] = P(benchText, Normal)
	}
	d := &Doc{Size: A4, Margins: Margins{72, 72, 72, 72}, Title: "bench"}
	b.ReportAllocs()
	for b.Loop() {
		err := d.Build(io.Discard, dst, story, encBuf, canvasBuf)
		if err != nil {
			b.Fatal(err)
		}
	}
}
