package piupage

import (
	"os"
	"testing"

	"github.com/soypat/lefevre"
)

const testFontPath = "../testdata/DejaVuSansMono.ttf"

func testFontData(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(testFontPath)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTrueTypeMetricsMatchTheFont(t *testing.T) {
	data := testFontData(t)
	f, err := TrueType(data)
	if err != nil {
		t.Fatal(err)
	}
	src, err := lefevre.FontFromMemory(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	upem := float64(src.Info().UnitsPerEm)
	for _, r := range "Encapsulate(carrierData []byte) — µ×²·é" {
		want := float64(src.GlyphAdvance(src.GlyphID(r))) / upem
		if got := f.Width(r); got != want {
			t.Errorf("Width(%q) = %v, want %v", r, got, want)
		}
	}
	// The width of a string is the sum of its runes' advances, scaled.
	const s, size = "Demux", 11.0
	var want float64
	for _, r := range s {
		want += f.Width(r)
	}
	if got := StringWidth(f, s, size); got != want*size {
		t.Errorf("StringWidth = %v, want %v", got, want*size)
	}
}

func TestTrueTypeEncodesGlyphIDs(t *testing.T) {
	data := testFontData(t)
	f, err := TrueType(data)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := lefevre.FontFromMemory(data, 0)
	for _, r := range "Go é" {
		gid := src.GlyphID(r)
		got := f.Encode(nil, r)
		if len(got) != 2 || got[0] != byte(gid>>8) || got[1] != byte(gid) {
			t.Errorf("Encode(%q) = % x, want glyph id %d big-endian", r, got, gid)
		}
	}
}

func TestMissingGlyphs(t *testing.T) {
	f, err := TrueType(testFontData(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := MissingGlyphs(f, "plain ASCII, é and µ"); len(got) != 0 {
		t.Errorf("MissingGlyphs reported %q for text the face covers", got)
	}
	// U+10FFFF is unassigned, so no font has a glyph for it. Repeating it must
	// not repeat the report.
	if got := MissingGlyphs(f, "a\U0010FFFFb\U0010FFFF"); len(got) != 1 || got[0] != '\U0010FFFF' {
		t.Errorf("MissingGlyphs = %q, want one unmapped rune", got)
	}
}

// embed drives the path a document takes: draw text through Encode, then ask
// the font for the program it would write, and parse that back.
func embed(t *testing.T, text string) (src, sub *lefevre.Font, program []byte) {
	t.Helper()
	data := testFontData(t)
	f, err := TrueType(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range text {
		f.Encode(nil, r)
	}
	ttf := f.(*ttfFont)
	for _, g := range ttf.used {
		ttf.gids = append(ttf.gids, g.gid)
	}
	program, err = ttf.sub.AppendSubset(nil, ttf.fnt, ttf.gids)
	if err != nil {
		t.Fatal(err)
	}
	src, err = lefevre.FontFromMemory(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	sub, err = lefevre.FontFromMemory(program, 0)
	if err != nil {
		t.Fatalf("embedded program does not parse as a font: %v", err)
	}
	return src, sub, program
}

// TestEmbeddedProgramKeepsGlyphsAndIDs is the property the whole embedding
// rests on: what Encode wrote into the content stream is a glyph id, and that
// same id must still name the same outline in the program the PDF carries.
func TestEmbeddedProgramKeepsGlyphsAndIDs(t *testing.T) {
	const used = "The quick brown fox — éàü 0123456789"
	src, sub, program := embed(t, used)

	if got, want := sub.Info().UnitsPerEm, src.Info().UnitsPerEm; got != want {
		t.Fatalf("embedded unitsPerEm = %d, want %d", got, want)
	}
	for _, r := range used {
		gid := src.GlyphID(r)
		if got, want := sub.GlyphAdvance(gid), src.GlyphAdvance(gid); got != want {
			t.Errorf("glyph %d (%q): advance %d, want %d", gid, r, got, want)
		}
		wx0, wy0, wx1, wy1 := src.GlyphBounds(gid)
		gx0, gy0, gx1, gy1 := sub.GlyphBounds(gid)
		if gx0 != wx0 || gy0 != wy0 || gx1 != wx1 || gy1 != wy1 {
			t.Errorf("glyph %d (%q): bounds (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				gid, r, gx0, gy0, gx1, gy1, wx0, wy0, wx1, wy1)
		}
		if len(sub.GlyphOutline(nil, gid)) != len(src.GlyphOutline(nil, gid)) {
			t.Errorf("glyph %d (%q): outline segment count changed", gid, r)
		}
	}
	if len(program) >= len(testFontData(t))/2 {
		t.Errorf("embedded program is %d bytes of an %d byte font: it is not subsetting",
			len(program), len(testFontData(t)))
	}
}

// TestEmbeddedProgramDropsUndrawnGlyphs checks the other half of the bargain:
// a glyph the document never drew keeps its id but loses its outline.
func TestEmbeddedProgramDropsUndrawnGlyphs(t *testing.T) {
	src, sub, _ := embed(t, "A")
	kept, dropped := src.GlyphID('A'), src.GlyphID('Z')
	if kept == 0 || dropped == 0 || kept == dropped {
		t.Fatal("test font does not distinguish A from Z")
	}
	if len(sub.GlyphOutline(nil, kept)) == 0 {
		t.Error("a drawn glyph lost its outline")
	}
	if n := len(sub.GlyphOutline(nil, dropped)); n != 0 {
		t.Errorf("an undrawn glyph kept %d outline segments", n)
	}
}

func TestTrueTypeRejectsNonTrueType(t *testing.T) {
	if _, err := TrueType([]byte("not a font at all")); err == nil {
		t.Error("TrueType accepted data that is not a font")
	}
	otto := append([]byte("OTTO"), make([]byte, 64)...)
	if _, err := TrueType(otto); err == nil {
		t.Error("TrueType accepted a CFF-outline font it cannot subset")
	}
}
