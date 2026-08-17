package piudoc

import (
	"bytes"
	"testing"

	"github.com/soypat/piudf/piupage"
)

// A registered family is what a Style names: the face reaches the page through
// the style layer, with no Drawer of the caller's own setting the font by hand.
func TestStyleReachesRegisteredFamily(t *testing.T) {
	f := testFont(t)
	var bld Builder
	bld.SetFamily("DejaVu", Family{Regular: f})

	d := &Doc{Size: SizeA4(), Margins: Margins{72, 72, 72, 72}}
	out := build(t, d, []Drawer{
		bld.Text("hello", Style{Font: "DejaVu", Size: 12}),
	}, 1)

	if err := bld.Err(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/FontFile2", "/Identity-H", "/ToUnicode"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the registered face carries no %s", want)
		}
	}
}

// Weight and slant come off the family name and out of the markup, so one
// registration serves every span a document opens within it.
func TestFamilyPicksWeightAndSlant(t *testing.T) {
	reg, bold, ital := piupage.FontCourier, piupage.FontHelveticaBold, piupage.FontTimesItalic
	fam := Family{Regular: reg, Bold: bold, Italic: ital}

	for _, tc := range []struct {
		bold, ital bool
		want       piupage.Font
	}{
		{false, false, reg},
		{true, false, bold},
		{false, true, ital},
		// No BoldItalic registered, so bold outranks slant rather than
		// dropping both and landing back on Regular.
		{true, true, bold},
	} {
		if got := fam.pick(tc.bold, tc.ital); got != tc.want {
			t.Errorf("pick(%v,%v) = %v, want %v", tc.bold, tc.ital, got, tc.want)
		}
	}
	if got := (Family{Regular: reg}).pick(true, true); got != reg {
		t.Errorf("a family of one face resolved to %v, want %v", got, reg)
	}
}

// An unregistered name still resolves to its standard-14 face, so registering
// nothing leaves every existing document exactly as it was.
func TestUnregisteredFamilyKeepsStandard14(t *testing.T) {
	var ap atomParser
	if got := ap.resolveFont("Times", true, false); got != piupage.FontTimesBold {
		t.Errorf("Times bold resolved to %v", got)
	}
	ap.families = []namedFamily{{name: "Times", fam: Family{Regular: piupage.FontCourier}}}
	if got := ap.resolveFont("Times", true, false); got != piupage.FontCourier {
		t.Errorf("a registered family did not shadow the standard-14 name: %v", got)
	}
}

// A family with no name or no Regular face is a mistake at registration, not a
// blank page later.
func TestSetFamilyRejectsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		fam  Family
	}{
		{"", Family{Regular: piupage.FontCourier}},
		{"Nothing", Family{}},
	} {
		var bld Builder
		bld.SetFamily(tc.name, tc.fam)
		if bld.Err() == nil {
			t.Errorf("SetFamily(%q, %+v) reported no error", tc.name, tc.fam)
		}
	}
}
