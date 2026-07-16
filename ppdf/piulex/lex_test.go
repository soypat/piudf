package piulex

import (
	"io"
	"strings"
	"testing"
)

// countingReaderAt records how many ReadAt calls a lexer makes.
type countingReaderAt struct {
	r     io.ReaderAt
	reads int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	return c.r.ReadAt(p, off)
}

type lexed struct {
	tok Token
	pos Pos
	lit string
}

// lexAll drains r from off through buf, returning every token including the
// terminating TokEOF.
func lexAll(t *testing.T, r io.ReaderAt, off int64, buf []byte) []lexed {
	t.Helper()
	var l Lexer
	if err := l.Reset(r, off, buf); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	var out []lexed
	for {
		tok, pos, lit := l.NextToken()
		out = append(out, lexed{tok, pos, string(lit)})
		if tok == TokEOF || tok == TokIllegal {
			return out
		}
	}
}

const sample = `<</Type/Page/N 3/F 3.14/S(a\)b)/H<4142>/A[1 0 R]>>`

func TestLexerTokens(t *testing.T) {
	want := []lexed{
		{TokDictOpen, 0, ""},
		{TokName, 2, "Type"},
		{TokName, 7, "Page"},
		{TokName, 12, "N"},
		{TokInt, 15, "3"},
		{TokName, 16, "F"},
		{TokReal, 19, "3.14"},
		{TokName, 23, "S"},
		{TokString, 25, "a)b"},
		{TokName, 31, "H"},
		{TokHexString, 33, "AB"},
		{TokName, 39, "A"},
		{TokArrayOpen, 41, ""},
		{TokInt, 42, "1"},
		{TokInt, 44, "0"},
		{TokR, 46, "R"},
		{TokArrayClose, 47, ""},
		{TokDictClose, 48, ""},
		{TokEOF, 50, ""},
	}
	got := lexAll(t, strings.NewReader(sample), 0, nil)
	if len(got) != len(want) {
		t.Fatalf("got %d tokens, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// TestLexerWindowSizeInvariance is the load-bearing test for the offset-
// addressed window: a token straddling a window edge forces a refill mid-token,
// which the old bufio.Reader hid. A one-byte window puts an edge between every
// pair of bytes, so if any read path depends on its token being resident in a
// single fill, these disagree with the default window.
func TestLexerWindowSizeInvariance(t *testing.T) {
	r := strings.NewReader(sample)
	want := lexAll(t, r, 0, nil)
	for size := 1; size <= 64; size++ {
		got := lexAll(t, r, 0, make([]byte, size))
		if len(got) != len(want) {
			t.Fatalf("window %d: got %d tokens, want %d:\n%v", size, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("window %d, token %d: got %v, want %v", size, i, got[i], want[i])
			}
		}
	}
}

// TestLexerResetReusesWindow pins the property the whole rewrite exists for:
// re-lexing a span already inside the window costs no read. Decode's trailer
// handling re-Resets to the same dictionary once per key.
func TestLexerResetReusesWindow(t *testing.T) {
	c := &countingReaderAt{r: strings.NewReader(sample)}
	var l Lexer
	if err := l.Reset(c, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _ = l.NextToken(); c.reads != 1 {
		t.Fatalf("first token took %d reads, want 1", c.reads)
	}
	// Every offset of sample is resident after that one fill.
	for i := range 8 {
		if err := l.Reset(c, int64(i), nil); err != nil {
			t.Fatal(err)
		}
		l.NextToken()
	}
	if c.reads != 1 {
		t.Errorf("re-lexing inside the window took %d reads, want 1", c.reads)
	}
}

// TestLexerEOFCostsNoReads guards the `end` bookkeeping: once a fill has seen
// the file end, offsets past it must be answered from memory.
func TestLexerEOFCostsNoReads(t *testing.T) {
	c := &countingReaderAt{r: strings.NewReader(sample)}
	var l Lexer
	if err := l.Reset(c, 0, nil); err != nil {
		t.Fatal(err)
	}
	for {
		if tok, _, _ := l.NextToken(); tok == TokEOF {
			break
		}
	}
	reads := c.reads
	for range 10 {
		if tok, _, _ := l.NextToken(); tok != TokEOF {
			t.Fatalf("got %v past EOF, want TokEOF", tok)
		}
	}
	if c.reads != reads {
		t.Errorf("lexing past EOF took %d extra reads, want 0", c.reads-reads)
	}
	if err := l.Err(); err != nil {
		t.Errorf("Err at EOF = %v, want nil", err)
	}
}

// TestLexerResetDifferentReader checks the window drops bytes belonging to a
// file it is no longer bound to.
func TestLexerResetDifferentReader(t *testing.T) {
	a := strings.NewReader("/Alpha")
	b := strings.NewReader("/Bravo")
	var l Lexer
	buf := make([]byte, 64)
	if err := l.Reset(a, 0, buf); err != nil {
		t.Fatal(err)
	}
	if _, _, lit := l.NextToken(); string(lit) != "Alpha" {
		t.Fatalf("got %q, want Alpha", lit)
	}
	if err := l.Reset(b, 0, buf); err != nil {
		t.Fatal(err)
	}
	if _, _, lit := l.NextToken(); string(lit) != "Bravo" {
		t.Fatalf("got %q, want Bravo — window served the previous reader's bytes", lit)
	}
}
