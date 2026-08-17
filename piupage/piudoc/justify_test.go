package piudoc

import (
	"strconv"
	"strings"
	"testing"

	"github.com/soypat/piudf/piupage"
)

// run is one text run a canvas emitted: where it was set, what it says, and the
// face it was set in — without which its width cannot be measured.
type run struct {
	x    float64
	text string
	font piupage.Font
	size float64
}

// width is the run's rendered width, and end the x it reaches.
func (r run) width() float64 { return piupage.StringWidth(r.font, r.text, r.size) }
func (r run) end() float64   { return r.x + r.width() }

// runsOf reads the "/Fn size Tf … x y Td(text)Tj" runs out of a content stream,
// in order. Operators run together with their operands, hence the prefix matches.
func runsOf(t testing.TB, cv *piupage.Canvas) []run {
	t.Helper()
	var out []run
	var cur piupage.Font
	var size float64
	fonts := cv.Fonts()
	f := strings.Fields(string(cv.Bytes()))
	for i, tok := range f {
		if j := strings.Index(tok, "/F"); j >= 0 && i+2 < len(f) && f[i+2] == "Tf" {
			n, err := strconv.Atoi(tok[j+2:])
			if err != nil || n < 1 || n > len(fonts) {
				t.Fatalf("bad font resource %q (have %d)", tok, len(fonts))
			}
			if size, err = strconv.ParseFloat(f[i+1], 64); err != nil {
				t.Fatalf("bad Tf size %q: %v", f[i+1], err)
			}
			cur = fonts[n-1]
			continue
		}
		if i < 2 || !strings.HasPrefix(tok, "Td(") {
			continue
		}
		x, err := strconv.ParseFloat(f[i-2], 64)
		if err != nil {
			t.Fatalf("bad Td operand %q: %v", f[i-2], err)
		}
		out = append(out, run{x: x, text: pdfString(tok[len("Td("):]), font: cur, size: size})
	}
	return out
}

// pdfString reads a literal string's content up to its unescaped ')'. The
// emitter backslash-escapes '(', ')' and '\', so "time.Now()" arrives as
// "time.Now\(\)" and a scan for a bare ')' would stop inside the word.
func pdfString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case s[i] == ')':
			return b.String()
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// runXs is runsOf reduced to the positions, for tests that only place lines.
func runXs(t testing.TB, cv *piupage.Canvas) []float64 {
	t.Helper()
	rs := runsOf(t, cv)
	xs := make([]float64, len(rs))
	for i, r := range rs {
		xs[i] = r.x
	}
	return xs
}

// A justified line ends flush on the measure: its last word's right edge is the
// frame's right edge, whatever the natural spacing would have left over.
func TestJustifyFillsTheMeasure(t *testing.T) {
	var bld Builder
	st := Style{Font: "Helvetica", Size: 10, Leading: 12, Align: Justify}
	// Long enough to wrap several times, so there are interior lines to stretch.
	text := strings.Repeat("the quick brown fox jumps over the lazy dog ", 6)

	dst, f := frame(t, 2)
	p := bld.Text(text, st)
	if _, _, err := p.Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	// A run set left of the one before it begins a new line, which is what lets
	// the ends of the lines be picked out of a flat list of runs.
	runs := runsOf(t, &dst[0])
	if len(runs) < 20 {
		t.Fatalf("only %d runs drawn; the text did not wrap", len(runs))
	}
	var ends []float64
	for i, r := range runs {
		if i+1 == len(runs) || runs[i+1].x < r.x {
			ends = append(ends, r.end())
		}
		if i == 0 || runs[i].x < runs[i-1].x {
			if r.x != f.X {
				t.Fatalf("a line begins at %v, not at the frame's left edge %v", r.x, f.X)
			}
		}
	}
	if len(ends) < 3 {
		t.Fatalf("found %d lines, want several", len(ends))
	}
	// Every line but the last ends exactly on the measure.
	want := f.X + f.Width
	for i, end := range ends[:len(ends)-1] {
		if diff := end - want; diff > 0.01 || diff < -0.01 {
			t.Errorf("line %d ends at %v, want the measure %v", i, end, want)
		}
	}
	if ends[len(ends)-1] >= want {
		t.Errorf("the last line ends at %v, at or past the measure — it was stretched", ends[len(ends)-1])
	}
}

// The last line of a paragraph is never stretched: it sets flush left, as does
// a line ended by an explicit break.
func TestJustifyLeavesLastLineAlone(t *testing.T) {
	var bld Builder
	st := Style{Font: "Helvetica", Size: 10, Leading: 12, Align: Justify}

	dst, f := frame(t, 2)
	// One short line, which is both the first and the last.
	p := bld.Text("three short words", st)
	if _, _, err := p.Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	xs := runXs(t, &dst[0])
	if len(xs) != 3 {
		t.Fatalf("drew %d runs, want 3", len(xs))
	}
	// Natural spacing: the three words sit within the first fifth of a 400pt
	// measure. Stretched, the last would be near f.X+f.Width.
	if xs[2] > f.X+f.Width/2 {
		t.Errorf("the last word of a final line is at %v — it was stretched", xs[2])
	}
}

// Justify is the only alignment that stretches; the other three place the line
// as a whole and leave its internal spacing alone.
func TestAlignmentsPlaceTheLine(t *testing.T) {
	var bld Builder
	base := Style{Font: "Helvetica", Size: 10, Leading: 12}
	const text = "three short words"

	first := func(a Align) float64 {
		dst, f := frame(t, 2)
		st := base
		st.Align = a
		if _, _, err := bld.Text(text, st).Draw(dst, f, f.Top); err != nil {
			t.Fatal(err)
		}
		return runXs(t, &dst[0])[0]
	}
	left, center, right := first(Left), first(Center), first(Right)
	if !(left < center && center < right) {
		t.Errorf("left %v, center %v, right %v are not in ascending order", left, center, right)
	}
	if got := first(Justify); got != left {
		t.Errorf("a justified single line starts at %v, want the left-aligned %v", got, left)
	}
}
