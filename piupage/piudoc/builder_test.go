package piudoc

import "testing"

// words returns what a paragraph will draw, word by word, which is the only
// thing a reader ever sees of it.
func words(p *Paragraph) []string {
	out := make([]string, 0, len(p.line))
	for i := range p.line {
		if p.line[i].brk {
			out = append(out, "|")
			continue
		}
		out = append(out, string(p.line[i].text))
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// Text is data: whatever markup characters it carries have to reach the page as
// themselves, not as formatting, even though the composed paragraph as a whole
// is parsed as markup.
func TestParaTextIsData(t *testing.T) {
	var b Builder
	var pb ParagraphBuilder
	p := pb.
		Text("Tom & Jerry ").
		Text("<b>not bold</b> ").
		Text("5 > 4").
		Build(&b, Normal)
	eq(t, words(p), []string{"Tom", "&", "Jerry", "<b>not", "bold</b>", "5", ">", "4"})
}

// Markup is the caller's own formatting and is read as such, in the same
// paragraph as the data beside it.
func TestParaMixesDataAndMarkup(t *testing.T) {
	var b Builder
	var pb ParagraphBuilder
	p := pb.
		Text("a & b ").
		Markup("<b>bold</b>").
		Break().
		Text("next").
		Build(&b, Normal)
	eq(t, words(p), []string{"a", "&", "b", "bold", "|", "next"})
	if got := p.line[3].font.PostScriptName(); got != "Helvetica-Bold" {
		t.Errorf("the markup half did not apply: %s", got)
	}
}

// A link's two halves are escaped by different rules, and both have to survive
// the round trip: the URI back to exactly what was passed in, the text drawn as
// itself.
func TestParaLinkRoundTrips(t *testing.T) {
	var b Builder
	var pb ParagraphBuilder
	const uri = "https://x.test/search?a=1&b=2"
	p := pb.Text("see ").Link("the docs", uri).Build(&b, Normal)
	eq(t, words(p), []string{"see", "the", "docs"})
	for _, pc := range p.line[1:] {
		if got := string(pc.href); got != uri {
			t.Errorf("href = %q, want %q", got, uri)
		}
	}
	if len(p.line[0].href) != 0 {
		t.Error("the link swallowed the word before it")
	}
}

// The half of the escaping that is not obvious: a quote or a ">" inside a URI
// would close the attribute or the tag early, and an unescaped "<" in link text
// would open a span that runs to the end of the paragraph.
func TestParaLinkEscapesHostileData(t *testing.T) {
	var b Builder
	var pb ParagraphBuilder
	const uri = `https://x.test/?q="><b>&z=1`
	p := pb.Link("<b>name", uri).Text(" after").Build(&b, Normal)
	eq(t, words(p), []string{"<b>name", "after"})
	// The tag-breaking bytes are percent-encoded; the query separator is not.
	const want = `https://x.test/?q=%22%3E%3Cb%3E&z=1`
	if got := string(p.line[0].href); got != want {
		t.Errorf("href = %q, want %q", got, want)
	}
	if len(p.line[1].href) != 0 {
		t.Error("the link escaped its own tag and swallowed what followed")
	}
	if got := p.line[0].font.PostScriptName(); got != "Helvetica" {
		t.Errorf("link text opened a span: font = %s", got)
	}
}

// A ParagraphBuilder is reused across paragraphs, so a second one must not
// carry any of the first.
func TestParaReusesTheBuffer(t *testing.T) {
	var b Builder
	var pb ParagraphBuilder
	first := pb.Text("one two").Build(&b, Normal)
	second := pb.Text("three").Build(&b, Normal)
	eq(t, words(first), []string{"one", "two"})
	eq(t, words(second), []string{"three"})
}

func TestBuilderRow(t *testing.T) {
	var b Builder
	a, c := b.Text("a", Normal), b.Text("c", Normal)
	row := b.Row(a, c)
	if len(row) != 2 || row[0].Drawer != Drawer(a) || row[1].Drawer != Drawer(c) {
		t.Fatalf("row = %+v", row)
	}
}
