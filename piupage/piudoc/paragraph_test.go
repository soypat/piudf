package piudoc

import "testing"

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

func TestParseAtomsReusesBuffer(t *testing.T) {
	buf := parseAtoms("one two three", Normal, nil)
	buf = parseAtoms("four five", Normal, buf[:0])
	if len(buf) != 2 || buf[0].word != "four" || buf[1].word != "five" {
		t.Fatalf("reuse gave %v", buf)
	}
}
