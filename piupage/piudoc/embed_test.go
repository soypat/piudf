package piudoc

import (
	"bytes"
	"os"
	"strconv"
	"testing"

	"github.com/soypat/lefevre"
	"github.com/soypat/piudf/piupage"
)

// setter is a Drawer that writes one string in one font. The style layer has no
// way to name an embedded face yet, so this is how a document gets one.
type setter struct {
	font piupage.Font
	text string
}

func (s setter) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	cv := &dst[0]
	cv.SetFont(s.font, 12)
	cv.Text(f.X, yTop, s.text, nil)
	return 0, yTop - 15, nil
}

func testFont(t *testing.T) piupage.Font {
	t.Helper()
	data, err := os.ReadFile("../../testdata/DejaVuSans.ttf")
	if err != nil {
		t.Skipf("test font not available: %v", err)
	}
	fnt := new(lefevre.Font)
	if err := fnt.LoadBytes(data, 0); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return fnt
}

// A face the reader does not have must arrive with the document: the program
// itself, the composite encoding that reaches past 256 codes, and the map back
// to Unicode without which the text cannot be copied out of the finished page.
func TestEmbeddedFontReachesTheFile(t *testing.T) {
	f := testFont(t)
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{setter{font: f, text: "hello"}}, 1)

	for _, want := range []string{"/FontFile2", "/Type0", "/Identity-H", "/ToUnicode", "/CIDFontType2"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the embedded font carries no %s", want)
		}
	}
	if bytes.Contains(out, []byte("/WinAnsiEncoding")) {
		t.Error("an embedded font was written as a simple WinAnsi one")
	}
}

// The subset is cut from what the document drew, merged across its elements and
// deduplicated: one entry per distinct glyph, however many times it was set.
// Over-counting embeds glyphs nobody reads; under-counting drops letters.
func TestSubsetHoldsEachGlyphOnce(t *testing.T) {
	f := testFont(t)
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	// "banana" repeats every letter but 'b', and the second line repeats the
	// whole word, so a count that survives is a count that deduplicated.
	out := build(t, d, []Drawer{
		setter{font: f, text: "banana"},
		setter{font: f, text: "bananabanana"},
	}, 1)

	// b, a and n. .notdef is kept in the program but is not mapped: /ToUnicode
	// describes what was drawn, not what the subset carries.
	const distinct = 3
	want := []byte(strconv.Itoa(distinct) + " beginbfchar")
	if !bytes.Contains(out, want) {
		t.Errorf("want %q; the /ToUnicode map is not one entry per distinct glyph:\n%s",
			want, toUnicodeExcerpt(out))
	}
}

// A document setting both kinds must write each in its own shape, and must not
// confuse which glyphs belong to which face.
func TestBuiltinAndEmbeddedInOneDocument(t *testing.T) {
	f := testFont(t)
	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{
		setter{font: piupage.FontHelvetica, text: "wxyz"},
		setter{font: f, text: "ab"},
	}, 1)

	if !bytes.Contains(out, []byte("/WinAnsiEncoding")) {
		t.Error("the built-in font lost its simple encoding")
	}
	if !bytes.Contains(out, []byte("/FontFile2")) {
		t.Error("the embedded font lost its program")
	}
	// Only the embedded face is mapped, and only its own two letters: the
	// built-in font's glyphs must not have been folded into its subset.
	if !bytes.Contains(out, []byte("2 beginbfchar")) {
		t.Errorf("the embedded subset is not exactly its own glyphs:\n%s", toUnicodeExcerpt(out))
	}
}

// toUnicodeExcerpt pulls the bfchar section out of a failing document so the
// error says what was written rather than only what was not.
func toUnicodeExcerpt(out []byte) []byte {
	i := bytes.Index(out, []byte("beginbfchar"))
	if i < 0 {
		return []byte("(no bfchar section at all)")
	}
	j := bytes.Index(out[i:], []byte("endbfchar"))
	if j < 0 {
		j = len(out) - i
	}
	return out[max(i-4, 0) : i+j]
}
