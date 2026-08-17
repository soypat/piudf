package piudoc

import (
	"testing"

	"github.com/soypat/piudf/piupage"
)

// gap is the distance between the end of one run and the start of the next,
// each measured in the face it was actually set in.
func gap(a, b run) float64 { return b.x - a.end() }

// near reports whether two lengths agree to within a printer's indifference.
func near(a, b float64) bool { return a-b < 0.01 && b-a < 0.01 }

// A tag is not a separator. Text that runs straight onto a link, or onto bold,
// is drawn with nothing between it — the source said so.
func TestNoSpaceIsInventedAtATagBoundary(t *testing.T) {
	var bld Builder
	st := Style{Font: "Helvetica", Size: 10, Leading: 12}
	for _, tc := range []struct {
		name, markup string
	}{
		{"punctuation after a link", `<a href="https://x.test">lneto</a>, next`},
		{"punctuation after italic", `<i>time.Now()</i>, next`},
		{"punctuation after bold", `<b>seqs</b>. next`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst, f := frame(t, 1)
			if _, _, err := bld.P(tc.markup, st).Draw(dst, f, f.Top); err != nil {
				t.Fatal(err)
			}
			runs := runsOf(t, &dst[0])
			if len(runs) != 3 {
				t.Fatalf("drew %d runs, want 3", len(runs))
			}
			// The glued piece abuts its neighbour, and the ordinary word after
			// it still gets its space.
			if g := gap(runs[0], runs[1]); !near(g, 0) {
				t.Errorf("a space of %v was invented before %q", g, runs[1].text)
			}
			if g := gap(runs[1], runs[2]); g <= 0 {
				t.Errorf("the real space before %q was dropped (gap %v)", runs[2].text, g)
			}
		})
	}
}

// Whitespace on either side of a tag is a separator, wherever it sits.
func TestSpaceAroundATagSurvives(t *testing.T) {
	var bld Builder
	st := Style{Font: "Helvetica", Size: 10, Leading: 12}
	for _, markup := range []string{
		`one <b>two</b>`,  // space before the tag
		`one<b> two</b>`,  // space inside it
		`one <b> two</b>`, // both
		"one\n<b>two</b>", // a newline is whitespace too
	} {
		dst, f := frame(t, 1)
		if _, _, err := bld.P(markup, st).Draw(dst, f, f.Top); err != nil {
			t.Fatal(err)
		}
		runs := runsOf(t, &dst[0])
		if len(runs) != 2 {
			t.Fatalf("%q drew %d runs, want 2", markup, len(runs))
		}
		if g := gap(runs[0], runs[1]); g <= 0 {
			t.Errorf("%q lost the space between its words (gap %v)", markup, g)
		}
	}
}

// Runs of whitespace still collapse to one space: that is what lets a caller
// write a paragraph across several Go string literals.
func TestWhitespaceStillCollapses(t *testing.T) {
	var bld Builder
	st := Style{Font: "Helvetica", Size: 10, Leading: 12}
	dst, f := frame(t, 1)
	if _, _, err := bld.Text("one \n\t  two", st).Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	runs := runsOf(t, &dst[0])
	if len(runs) != 2 {
		t.Fatalf("drew %d runs, want 2", len(runs))
	}
	want := piupage.StringWidth(runs[0].font, " ", st.Size)
	if g := gap(runs[0], runs[1]); !near(g, want) {
		t.Errorf("five whitespace bytes drew as %v, want one space of %v", g, want)
	}
}

// A justified line spreads its slack over the gaps it actually has, so a glued
// piece must not be counted as one — the line would fall short of the measure.
func TestJustifyIgnoresGluedGaps(t *testing.T) {
	var bld Builder
	st := Style{Font: "Helvetica", Size: 10, Leading: 12, Align: Justify}
	dst, f := frame(t, 2)
	// Wraps several times, and every line carries glued commas.
	markup := ""
	for range 20 {
		markup += `the <a href="https://x.test">quick</a>, brown <b>fox</b>, jumps over `
	}
	if _, _, err := bld.P(markup, st).Draw(dst, f, f.Top); err != nil {
		t.Fatal(err)
	}
	runs := runsOf(t, &dst[0])
	want := f.X + f.Width
	lines := 0
	for i, r := range runs {
		if i+1 == len(runs) || runs[i+1].x >= r.x {
			continue // Not the end of a line.
		}
		lines++
		if end := r.end(); !near(end, want) {
			t.Errorf("line %d ends at %v, want the measure %v", lines, end, want)
		}
	}
	if lines < 3 {
		t.Fatalf("only %d full lines; the text did not wrap enough to test", lines)
	}
}
