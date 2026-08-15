package piupage

import (
	"bytes"
	"strings"
	"testing"

	"github.com/soypat/piudf"
)

func TestLinkRect(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ln             Link
		x0, y0, x1, y1 float64
	}{
		{"ordinary", Link{X: 10, Y: 20, W: 30, H: 40}, 10, 20, 40, 60},
		{"at origin", Link{W: 1, H: 2}, 0, 0, 1, 2},
		// Canvas.Link normalizes before storing, but Rect is arithmetic on
		// whatever it is handed and must not pretend otherwise.
		{"negative size", Link{X: 10, Y: 20, W: -4, H: -5}, 10, 20, 6, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x0, y0, x1, y1 := tc.ln.Rect()
			if x0 != tc.x0 || y0 != tc.y0 || x1 != tc.x1 || y1 != tc.y1 {
				t.Errorf("Rect = (%v %v %v %v), want (%v %v %v %v)",
					x0, y0, x1, y1, tc.x0, tc.y0, tc.x1, tc.y1)
			}
		})
	}
}

// writeLink emits one annotation into a fresh document and returns the bytes.
func writeLink(t *testing.T, ln Link) string {
	t.Helper()
	var buf bytes.Buffer
	var enc piudf.Encoder
	if err := enc.Reset(&buf, make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteLink(&enc, ln); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestWriteLink(t *testing.T) {
	got := writeLink(t, Link{X: 10, Y: 20, W: 30, H: 40, URI: []byte("https://go.dev")})
	for _, want := range []string{
		"/Type/Annot",
		"/Subtype/Link",
		"/Rect[10 20 40 60]",
		"/Border[0 0 0]",
		"/S/URI",
		"(https://go.dev)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("annotation is missing %s:\n%s", want, got)
		}
	}
	// An appearance stream would make the annotation paint over the caller's ink.
	if strings.Contains(got, "/AP") {
		t.Error("wrote an appearance stream")
	}
}

func TestWriteLinkEscapesURI(t *testing.T) {
	// The three bytes a literal string cannot carry raw. A URI is bytes, not a
	// text string, so this escaping is the whole of its protection.
	got := writeLink(t, Link{URI: []byte(`https://x.test/a(b)c\d`)})
	const want = `(https://x.test/a\(b\)c\\d)`
	if !strings.Contains(got, want) {
		t.Errorf("URI not escaped, want %s in:\n%s", want, got)
	}
}
