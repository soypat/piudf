package piupage

import "testing"

// Every one of the 14 has to answer to its own /BaseFont name, since that name
// is the only thing a reader has to find the font by.
func TestFontBuiltinNamesRoundTrip(t *testing.T) {
	for b := FontHelvetica; b <= FontZapfDingbats; b++ {
		got, ok := Standard14(b.PostScriptName())
		if !ok {
			t.Errorf("%s does not resolve", b)
			continue
		}
		if got != Font(b) {
			t.Errorf("%s resolved to a different font", b)
		}
	}
	if FontBuiltin(0).IsValid() {
		t.Error("the zero value names a font")
	}
}

// GlyphID is the WinAnsi code, and zero means the face cannot spell the rune —
// the same answer a glyf font gives with .notdef.
func TestFontBuiltinGlyphID(t *testing.T) {
	for _, tt := range []struct {
		r    rune
		want uint16
	}{
		{'A', 0x41},
		{' ', 0x20},
		{'é', 0xe9},
		{'€', 0x80}, // a WinAnsi code that is not its code point
		{'中', 0},    // no WinAnsi representation at all
		{'☃', 0},
	} {
		if got := FontHelvetica.GlyphID(tt.r); got != tt.want {
			t.Errorf("GlyphID(%q) = %#x, want %#x", tt.r, got, tt.want)
		}
	}
}

// The advance a glyph reports and the width its rune reports are the same
// measurement on two scales; drifting apart would mis-space every line.
func TestFontBuiltinAdvanceMatchesWidth(t *testing.T) {
	for _, b := range []FontBuiltin{FontHelvetica, FontHelveticaBold, FontTimesRoman} {
		for _, r := range "Aa fi.—é" {
			adv := float64(b.GlyphAdvance(b.GlyphID(r))) / float64(b.UnitsPerEm())
			if got := b.Width(r); got != adv {
				t.Errorf("%s %q: Width = %v, GlyphAdvance/upem = %v", b, r, got, adv)
			}
		}
	}
}

// The bold weight is the one that carries metrics of its own; if it silently
// fell back to Helvetica's, bold text would lay out as if it were regular.
func TestFontBuiltinBoldIsWider(t *testing.T) {
	// 'b' is one of the codes the two tables genuinely disagree on; plenty of
	// others, 'W' among them, are identical in both weights.
	const b = 'b'
	if FontHelveticaBold.GlyphAdvance(b) <= FontHelvetica.GlyphAdvance(b) {
		t.Error("Helvetica-Bold is no wider than Helvetica")
	}
	if FontHelveticaBoldOblique.GlyphAdvance(b) != FontHelveticaBold.GlyphAdvance(b) {
		t.Error("the bold oblique does not share the bold metrics")
	}
}

// Resolving a font sits on the layout path, so it must not reach the heap.
func TestFontBuiltinBoxesWithoutAllocating(t *testing.T) {
	var sink Font
	got := testing.AllocsPerRun(100, func() {
		sink = FontHelveticaBold
	})
	if got != 0 {
		t.Errorf("boxing a builtin allocates %v times per run", got)
	}
	if sink == nil {
		t.Fatal("unreachable")
	}
}
