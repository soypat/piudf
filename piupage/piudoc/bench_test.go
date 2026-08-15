package piudoc

import (
	"io"
	"strings"
	"testing"

	"github.com/soypat/piudf/piupage"
)

var benchText = strings.Repeat("The quick <b>brown</b> fox jumps over the <i>lazy</i> dog. ", 12)

var benchLinkText = strings.Repeat(
	`The quick <a href="https://go.dev/doc">brown fox</a> jumps over the lazy dog. `, 12)

// benchEntityText exercises what benchText does not: entities, which are the
// only bytes a parse writes, and attributes, which are parsed rather than merely
// matched. It stays at zero allocations.
//
// It leaves out <font color=>, which does not: a color.RGBA is four bytes and a
// color.Color is an interface, so naming a color boxes one. That is the tag's
// cost, not the parser's, and no buffer of ours can take it away.
var benchEntityText = strings.Repeat(
	`Tom &amp; Jerry &lt;b&gt; <font size="8">Q&amp;A</font>, `+
		`see <a href="https://go.dev/doc?x=1&amp;y=2">the docs</a>. `, 12)

func BenchmarkParagraphDrawEntities(b *testing.B) {
	var cv [1]piupage.Canvas
	buf := make([]byte, 8192)
	f := Frame{X: 72, Width: 451, Top: 770, Bottom: 72}
	p := P(benchEntityText, Normal)
	b.ReportAllocs()
	for b.Loop() {
		cv[0].Reset(buf)
		_, _, err := p.Draw(cv[:], f, f.Top)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParagraphDrawLinks(b *testing.B) {
	var cv [1]piupage.Canvas
	buf := make([]byte, 8192)
	f := Frame{X: 72, Width: 451, Top: 770, Bottom: 72}
	st := Normal
	st.Link = LinkStyle{Color: piupage.HexColor("#0645AD"), Underline: true}
	p := P(benchLinkText, st)
	b.ReportAllocs()
	for b.Loop() {
		cv[0].Reset(buf)
		_, _, err := p.Draw(cv[:], f, f.Top)
		if err != nil {
			b.Fatal(err)
		}
	}
}

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

func BenchmarkTableDraw(b *testing.B) {
	var cv [1]piupage.Canvas
	buf := make([]byte, 16384)
	f := Frame{X: 72, Width: 450, Top: 770, Bottom: 72}
	rows := make([][]Cell, 12)
	for i := range rows {
		rows[i] = []Cell{TextCell("left"), TextCell("a middle column"), TextCell("42.00")}
	}
	t := &Table{Rows: rows, ColWidths: []float64{150, 200, 100}}
	t.Style.Grid(0, 0, -1, -1, 0.5, nil).Background(0, 0, -1, 0, nil).Valign(0, 0, -1, -1, Middle)
	b.ReportAllocs()
	for b.Loop() {
		cv[0].Reset(buf)
		_, _, err := t.Draw(cv[:], f, f.Top)
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
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}, Title: "bench"}
	b.ReportAllocs()
	for b.Loop() {
		err := d.Build(io.Discard, dst, story, encBuf, canvasBuf)
		if err != nil {
			b.Fatal(err)
		}
	}
}
