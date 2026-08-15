package piudoc

import (
	"testing"

	"github.com/soypat/piudf/piupage"
)

func TestParseAtoms(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string // word, or "|" for an explicit break
	}{
		{"plain", "hello world", []string{"hello", "world"}},
		{"collapses space", "  a \t\n b  ", []string{"a", "b"}},
		{"empty", "", nil},
		{"only space", "   ", nil},
		{"tags dropped", "a <b>bold</b> c", []string{"a", "bold", "c"}},
		{"break", "a<br/>b", []string{"a", "|", "b"}},
		{"entities", "a &amp; b &lt;x&gt;", []string{"a", "&", "b", "<x>"}},
		{"unterminated tag is text", "a <b c", []string{"a", "<b", "c"}},
		{"unknown tag dropped", "a <blink>b</blink>", []string{"a", "b"}},
		{"leading tag", "<b>a</b>", []string{"a"}},
		{"adjacent tags", "<b><i>a</i></b>", []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAtoms(tc.text, Normal, nil)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d atoms, want %d: %v", len(got), len(tc.want), got)
			}
			for i, a := range got {
				w := a.word
				if a.brk {
					w = "|"
				}
				if w != tc.want[i] {
					t.Errorf("atom %d = %q, want %q", i, w, tc.want[i])
				}
			}
		})
	}
}

func TestParseAtomsStyle(t *testing.T) {
	atoms := parseAtoms(`a <b>b</b> <i>c</i> <font size="20">d</font>`, Normal, nil)
	if len(atoms) != 4 {
		t.Fatalf("got %d atoms, want 4", len(atoms))
	}
	for i, want := range []string{"Helvetica", "Helvetica-Bold", "Helvetica-Oblique", "Helvetica"} {
		if got := atoms[i].font.BaseName(); got != want {
			t.Errorf("atom %d font = %s, want %s", i, got, want)
		}
	}
	if atoms[3].size != 20 {
		t.Errorf("font size = %v, want 20", atoms[3].size)
	}
	if atoms[0].size != Normal.Size {
		t.Errorf("size leaked out of the font span: %v", atoms[0].size)
	}
}

func TestParseAtomsHref(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string // one href per atom, "" for unlinked
	}{
		{"spans exactly the tag", `a <a href="u">b c</a> d`, []string{"", "u", "u", ""}},
		{"reportlab link alias", `<link href="u">b</link> c`, []string{"u", ""}},
		{"entity in query", `<a href="u?x=1&amp;y=2">b</a>`, []string{"u?x=1&y=2"}},
		{"nests inside style", `<b><a href="u">b</a></b> c`, []string{"u", ""}},
		{"style nests inside", `<a href="u"><b>b</b> c</a>`, []string{"u", "u"}},
		{"no href", `<a>b</a>`, []string{""}},
		// A tag merely starting with the same letters is not a link.
		{"blink is not a link", `<blink>b</blink>`, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAtoms(tc.text, Normal, nil)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d atoms, want %d", len(got), len(tc.want))
			}
			for i, a := range got {
				if a.href != tc.want[i] {
					t.Errorf("atom %d (%q) href = %q, want %q", i, a.word, a.href, tc.want[i])
				}
			}
		})
	}
}

func TestParagraphLinkWrapsToOneRectPerLine(t *testing.T) {
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 200, Top: 500, Bottom: 0}
	// The break forces the link onto two lines; PDF has no multi-line
	// annotation, so it must become two rects and not one tall or one wide.
	p := P(`<a href="https://go.dev">first<br/>second</a>`, Normal)
	if _, _, err := p.Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	links := dst[0].Links()
	if len(links) != 2 {
		t.Fatalf("got %d rects for a link across two lines, want 2", len(links))
	}
	if links[0].Y <= links[1].Y {
		t.Error("rects are not one per line, descending")
	}
	for i, ln := range links {
		if ln.URI != "https://go.dev" {
			t.Errorf("rect %d uri = %q", i, ln.URI)
		}
		if ln.W <= 0 || ln.H <= 0 {
			t.Errorf("rect %d is degenerate: %+v", i, ln)
		}
	}
}

func TestParagraphLinkCoalescesWords(t *testing.T) {
	dst := newCanvases(t, 1)
	f := Frame{X: 0, Width: 400, Top: 500, Bottom: 0}
	p := P(`x <a href="u">one two three</a> y`, Normal)
	if _, _, err := p.Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	links := dst[0].Links()
	if len(links) != 1 {
		t.Fatalf("got %d rects for three linked words on one line, want 1", len(links))
	}
	// The rect must start at the "o" of "one", not at the space in front of it.
	x := f.X + piupage.StringWidth(mustFont(t, "Helvetica"), "x ", Normal.Size)
	if got := links[0].X; got < x-0.01 {
		t.Errorf("rect starts at %v, before the linked text at %v", got, x)
	}
}

func TestParagraphLinkUnderline(t *testing.T) {
	f := Frame{X: 0, Width: 400, Top: 500, Bottom: 0}
	draw := func(st Style) string {
		dst := newCanvases(t, 1)
		p := P(`<a href="u">linked</a>`, st)
		if _, _, err := p.Draw(dst, f, f.Top); err != nil {
			t.Fatal(err)
		}
		return string(dst[0].Bytes())
	}
	plain := draw(Normal)
	st := Normal
	st.Link = LinkStyle{Underline: true}
	// A zero LinkStyle must leave the stream exactly as it was: the annotation
	// is behavioural and paints nothing on its own.
	if under := draw(st); under == plain {
		t.Error("Underline drew no rule")
	} else if len(under) <= len(plain) {
		t.Error("the underlined stream is not longer than the plain one")
	}
}

func TestParseAtomsReusesBuffer(t *testing.T) {
	buf := parseAtoms("one two three", Normal, nil)
	buf = parseAtoms("four five", Normal, buf[:0])
	if len(buf) != 2 || buf[0].word != "four" || buf[1].word != "five" {
		t.Fatalf("reuse gave %v", buf)
	}
}
