package piupage

import (
	"math"
	"os"
	"testing"

	"github.com/soypat/lefevre"
	"github.com/soypat/lefevre/sfnt"
)

func testTTF(t *testing.T) Font {
	t.Helper()
	data, err := os.ReadFile(testFontPath)
	if err != nil {
		t.Skipf("test font not available: %v", err)
	}
	fnt := new(lefevre.Font)
	if err := fnt.LoadBytes(data, 0); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return fnt
}

// The descriptor is read out of the raw tables so that any sfnt.Source can be
// embedded, not just the one implementation. This is what says that reading
// agrees with a parser written independently of it — every disagreement is a
// wrong number in a /FontDescriptor, which is invisible until a viewer
// synthesizes a substitute face from it.
func TestDescriptorMatchesTheFontsOwnReading(t *testing.T) {
	f := testTTF(t)
	src, ok := f.(sfnt.Source)
	if !ok {
		t.Fatal("an embedded font is not an sfnt.Source")
	}
	d := readDescriptor(src, f.UnitsPerEm())

	var info lefevre.FontInfo
	f.(*lefevre.Font).ReadInfo(&info)

	for _, tt := range []struct {
		name      string
		got, want int
	}{
		{"xMin", int(d.xMin), int(info.XMin)},
		{"yMin", int(d.yMin), int(info.YMin)},
		{"xMax", int(d.xMax), int(info.XMax)},
		{"yMax", int(d.yMax), int(info.YMax)},
		{"ascent", int(d.ascent), int(info.Ascent)},
		{"descent", int(d.descent), int(info.Descent)},
		{"capHeight", int(d.capHeight), int(info.CapHeight)},
		{"weight", d.weight, int(info.WeightClass)},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
	if d.italicAngle != info.ItalicAngle {
		t.Errorf("italicAngle = %v, want %v", d.italicAngle, info.ItalicAngle)
	}
	if d.fixedPitch != info.IsFixedPitch {
		t.Errorf("fixedPitch = %v, want %v", d.fixedPitch, info.IsFixedPitch)
	}
	if want := info.StyleFlags&lefevre.FontStyleItalic != 0; d.italic != want {
		t.Errorf("italic = %v, want %v", d.italic, want)
	}
	if d.upem == 0 {
		t.Error("descriptor carries no design grid, so every scaled metric is +Inf")
	}
}

// A row is measured by drawing it and retracting, so every cell is drawn twice.
// If a retracted draw left its glyphs behind, the subset would carry glyphs the
// page never shows — and would keep growing with each measure.
func TestRewindTakesBackGlyphs(t *testing.T) {
	f := testTTF(t)
	var c Canvas
	if err := c.Reset(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	c.SetFont(f, 12)
	c.Text(0, 0, "kept", nil)
	m := c.Mark()
	before := len(c.Glyphs())
	c.Text(0, 20, "retracted", nil)
	if len(c.Glyphs()) <= before {
		t.Fatal("drawing recorded no glyphs")
	}
	c.Rewind(m)
	if got := len(c.Glyphs()); got != before {
		t.Errorf("after Rewind the canvas holds %d glyphs, want %d", got, before)
	}
	for _, g := range c.Glyphs() {
		if g.Rune == 'r' || g.Rune == 'c' {
			t.Errorf("a retracted rune %q survived the rewind", g.Rune)
		}
	}
}

// zeroGrid reports no design grid, which is the one kind of broken face that
// lays out silently instead of failing: every advance is divided by it.
type zeroGrid struct{}

func (zeroGrid) PostScriptName() string    { return "Broken" }
func (zeroGrid) UnitsPerEm() int           { return 0 }
func (zeroGrid) GlyphID(rune) uint16       { return 1 }
func (zeroGrid) GlyphAdvance(uint16) int32 { return 100 }

// Anyone may supply a Font now, so the check that used to sit in a constructor
// has to sit where a font arrives instead.
func TestFontWithNoGridIsRefused(t *testing.T) {
	var c Canvas
	if err := c.Reset(make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	c.SetFont(zeroGrid{}, 12)
	if err := c.Err(); err != errNoUnitsPerEm {
		t.Errorf("Err = %v, want errNoUnitsPerEm", err)
	}
	if got := StringWidth(zeroGrid{}, "x", 10); !math.IsInf(got, 1) {
		t.Errorf("width = %v; the guard is guarding nothing", got)
	}
}

// A built-in font is neither subset nor mapped, so recording what it drew would
// be so much memory spent on nothing.
func TestBuiltinRecordsNoGlyphs(t *testing.T) {
	var c Canvas
	if err := c.Reset(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	c.SetFont(FontHelvetica, 12)
	c.Text(0, 0, "no record needed", nil)
	if got := len(c.Glyphs()); got != 0 {
		t.Errorf("a built-in font recorded %d glyphs", got)
	}
}

// The two encodings are the visible half of the Font contract: an embedded face
// writes two bytes per glyph id under Identity-H, a built-in one writes one
// WinAnsi byte, and a rune neither can spell must not silently vanish.
func TestEncodingWidthFollowsTheFontKind(t *testing.T) {
	var c Canvas
	if err := c.Reset(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	c.curFont = FontHelvetica
	if got := c.encode("abc", 0); len(got) != 3 {
		t.Errorf("built-in encoded %q in %d bytes, want 3", got, len(got))
	}
	if got := c.encode("中", 0); len(got) != 1 || got[0] != '?' {
		t.Errorf("an unencodable rune became %q, want %q", got, "?")
	}
	c.curFont = testTTF(t)
	if got := c.encode("abc", 0); len(got) != 6 {
		t.Errorf("embedded encoded %q in %d bytes, want 6", got, len(got))
	}
}
