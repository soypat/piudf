package piulex

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"
)

// tokLit is one lexed token with its literal, the unit the round-trip tests
// compare in.
type tokLit struct {
	tok Token
	lit string
}

// lexAll tokenizes data completely. The lexer is the emitter's oracle: emitted
// bytes are correct iff they lex back to the intended tokens.
func emitLexAll(t *testing.T, data []byte) []tokLit {
	t.Helper()
	var lx Lexer
	lx.ReuseLiteralBuffer = true
	if err := lx.Reset(bytes.NewReader(data), 0, nil); err != nil {
		t.Fatal(err)
	}
	var out []tokLit
	for {
		tok, _, lit := lx.NextToken()
		if tok == TokEOF {
			return out
		}
		if tok == TokIllegal {
			t.Fatalf("illegal token lexing %q: %v", data, lx.Err())
		}
		out = append(out, tokLit{tok, string(lit)})
	}
}

// emitOp is one self-balanced emission and the tokens it must lex back as.
// Self-balanced so any sequence of ops keeps dict/array nesting legal, which
// is what lets the pair test below try every adjacency.
type emitOp struct {
	name string
	emit func(e *Emitter)
	want []tokLit
}

var emitOps = []emitOp{
	{"int", func(e *Emitter) { e.Int(42) }, []tokLit{{TokInt, "42"}}},
	{"negint", func(e *Emitter) { e.Int(-7) }, []tokLit{{TokInt, "-7"}}},
	{"real", func(e *Emitter) { e.Real(0.5) }, []tokLit{{TokReal, "0.5"}}},
	{"negreal", func(e *Emitter) { e.Real(-1.25) }, []tokLit{{TokReal, "-1.25"}}},
	{"name", func(e *Emitter) { e.Name("Type") }, []tokLit{{TokName, "Type"}}},
	{"string", func(e *Emitter) { e.String([]byte("hi")) }, []tokLit{{TokString, "hi"}}},
	{"hexstring", func(e *Emitter) { e.HexString([]byte{0xde, 0xad}) }, []tokLit{{TokHexString, "\xde\xad"}}},
	{"true", func(e *Emitter) { e.Bool(true) }, []tokLit{{TokTrue, "true"}}},
	{"false", func(e *Emitter) { e.Bool(false) }, []tokLit{{TokFalse, "false"}}},
	{"null", func(e *Emitter) { e.Null() }, []tokLit{{TokNull, "null"}}},
	{"ident", func(e *Emitter) { e.Ident("BT") }, []tokLit{{TokIdent, "BT"}}},
	{"ref", func(e *Emitter) { e.Ref(3, 0) }, []tokLit{{TokInt, "3"}, {TokInt, "0"}, {TokR, "R"}}},
	{"dict", func(e *Emitter) { e.DictOpen(); e.DictClose() }, []tokLit{{TokDictOpen, ""}, {TokDictClose, ""}}},
	{"array", func(e *Emitter) { e.ArrayOpen(); e.ArrayClose() }, []tokLit{{TokArrayOpen, ""}, {TokArrayClose, ""}}},
	{"eol", func(e *Emitter) { e.EOL() }, nil},
}

// TestEmitterPairs emits every ordered pair of ops back to back and checks the
// output lexes as their concatenated tokens: the separator rule is exactly the
// property that no adjacency merges or splits a token.
func TestEmitterPairs(t *testing.T) {
	var buf [MinEmitBuffer]byte // Minimum size, so flush boundaries are exercised too.
	for _, a := range emitOps {
		for _, b := range emitOps {
			var out bytes.Buffer
			var e Emitter
			if err := e.Reset(&out, buf[:]); err != nil {
				t.Fatal(err)
			}
			a.emit(&e)
			b.emit(&e)
			if err := e.Flush(); err != nil {
				t.Fatalf("%s+%s: %v", a.name, b.name, err)
			}
			want := append(append([]tokLit{}, a.want...), b.want...)
			got := emitLexAll(t, out.Bytes())
			if len(got) != len(want) {
				t.Fatalf("%s+%s: emitted %q lexed to %d tokens, want %d: %v", a.name, b.name, out.Bytes(), len(got), len(want), got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s+%s: token %d = %v, want %v (emitted %q)", a.name, b.name, i, got[i], want[i], out.Bytes())
				}
			}
		}
	}
}

// TestEmitterStringRoundTrip checks that arbitrary bytes survive
// String -> Lexer exactly, including the bytes the lexer normalizes or
// interprets when unescaped.
func TestEmitterStringRoundTrip(t *testing.T) {
	cases := [][]byte{
		[]byte("plain"),
		[]byte("with (parens) inside"),
		[]byte(`back\slash`),
		[]byte("cr\rlf\ncrlf\r\n"),
		{0, 1, 2, 0xff, '(', ')', '\\', '\r'},
		[]byte("\x0005"), // NUL then digit: octal escape must not swallow the 5.
		{},
	}
	var buf [MinEmitBuffer]byte
	for _, want := range cases {
		var out bytes.Buffer
		var e Emitter
		if err := e.Reset(&out, buf[:]); err != nil {
			t.Fatal(err)
		}
		e.String(want)
		if err := e.Flush(); err != nil {
			t.Fatal(err)
		}
		got := emitLexAll(t, out.Bytes())
		if len(got) != 1 || got[0].tok != TokString || got[0].lit != string(want) {
			t.Errorf("String(%q) emitted %q, lexed back %v", want, out.Bytes(), got)
		}
	}
}

// TestEmitterNameRoundTrip checks #xx escaping: every byte a name may not
// contain bare must come back decoded by the lexer.
func TestEmitterNameRoundTrip(t *testing.T) {
	cases := []string{
		"Type",
		"with space",
		"has#hash",
		"paren(",
		"slash/inside",
		"A;Name_With-Various***Chars?",
		"\x01\xfe",
	}
	var buf [MinEmitBuffer]byte
	for _, want := range cases {
		var out bytes.Buffer
		var e Emitter
		if err := e.Reset(&out, buf[:]); err != nil {
			t.Fatal(err)
		}
		e.Name(want)
		if err := e.Flush(); err != nil {
			t.Fatal(err)
		}
		got := emitLexAll(t, out.Bytes())
		if len(got) != 1 || got[0].tok != TokName || got[0].lit != want {
			t.Errorf("Name(%q) emitted %q, lexed back %v", want, out.Bytes(), got)
		}
	}
}

func TestEmitterReal(t *testing.T) {
	cases := []struct {
		in   float64
		want string // Empty means an error is expected.
	}{
		{0, "0"},
		{0.5, "0.5"},
		{-1.25, "-1.25"},
		{72.0, "72"},
		{841.89, "841.89"},
		{1e-30, "0"},  // Below any meaningful PDF unit: clamps, no zero-parade.
		{-1e-30, "0"}, // Negative zero still reads back as real 0.
		{5e-324, "0"},
		{math.MaxFloat32, ""}, // In range but 'f' form is 39 digits; boundary must not error.
		{math.NaN(), ""},
		{math.Inf(1), ""},
		{math.MaxFloat64, ""},
	}
	// MaxFloat32 is valid; its text is the shortest float64 round-trip form
	// (17 significant digits), not the exact decimal expansion.
	cases[8].want = "340282346638528860000000000000000000000"
	var buf [MinEmitBuffer]byte
	for _, tc := range cases {
		var out bytes.Buffer
		var e Emitter
		if err := e.Reset(&out, buf[:]); err != nil {
			t.Fatal(err)
		}
		e.Real(tc.in)
		err := e.Flush()
		if tc.want == "" {
			if err == nil {
				t.Errorf("Real(%v) = %q, want error", tc.in, out.Bytes())
			}
			continue
		}
		if err != nil {
			t.Errorf("Real(%v): %v", tc.in, err)
			continue
		}
		if s := out.String(); s != tc.want {
			t.Errorf("Real(%v) = %q, want %q", tc.in, s, tc.want)
		}
		if strings.ContainsAny(out.String(), "eE") {
			t.Errorf("Real(%v) = %q uses exponent notation, forbidden in PDF", tc.in, out.Bytes())
		}
	}
}

func TestEmitterBalance(t *testing.T) {
	var buf [MinEmitBuffer]byte
	var e Emitter
	if err := e.Reset(io.Discard, buf[:]); err != nil {
		t.Fatal(err)
	}
	e.DictOpen()
	e.ArrayOpen()
	if d := e.Depth(); d != 2 {
		t.Errorf("Depth = %d, want 2", d)
	}
	e.ArrayClose()
	e.DictClose()
	if d := e.Depth(); d != 0 {
		t.Errorf("Depth = %d, want 0", d)
	}
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	e.ArrayClose() // Nothing open: must latch an error.
	if err := e.Err(); err == nil {
		t.Error("close without open: no error")
	}
	e.Int(1) // Sticky: later emissions no-op rather than write.
	if e.Pos() != 6 {
		t.Errorf("emissions after error moved Pos to %d", e.Pos())
	}
}

func TestEmitterPos(t *testing.T) {
	var buf [MinEmitBuffer]byte
	var out bytes.Buffer
	var e Emitter
	if err := e.Reset(&out, buf[:]); err != nil {
		t.Fatal(err)
	}
	for i := range 100 { // Crosses buffer boundary several times.
		if e.Pos() != int64(out.Len()+eBuffered(&e)) {
			t.Fatalf("iteration %d: Pos %d disagrees with written+buffered", i, e.Pos())
		}
		e.Int(int64(i))
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	if e.Pos() != int64(out.Len()) {
		t.Errorf("after flush Pos = %d, output has %d bytes", e.Pos(), out.Len())
	}
}

func eBuffered(e *Emitter) int { return e.n }

// TestEmitterRawVerbatim pins Raw's contract: bytes pass through untouched —
// no separator is injected even between chunks whose boundary would merge
// tokens — and the separator state after Raw follows its last byte.
func TestEmitterRawVerbatim(t *testing.T) {
	var buf [MinEmitBuffer]byte
	var out bytes.Buffer
	var e Emitter
	if err := e.Reset(&out, buf[:]); err != nil {
		t.Fatal(err)
	}
	e.Int(7)
	e.EOL()
	// Chunked payload with a regular/regular chunk boundary: any injected
	// byte here would corrupt a stream payload.
	e.Raw([]byte("abc"))
	e.Raw([]byte("def"))
	e.EOL()
	e.Raw([]byte("xyz")) // Ends regular: the following Int must be separated.
	e.Int(9)
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "7\nabcdef\nxyz 9"; got != want {
		t.Errorf("Raw output %q, want %q", got, want)
	}
}

func TestEmitterSetPos(t *testing.T) {
	var buf [MinEmitBuffer]byte
	var out bytes.Buffer
	var e Emitter
	if err := e.Reset(&out, buf[:]); err != nil {
		t.Fatal(err)
	}
	if err := e.SetPos(1000); err != nil {
		t.Fatal(err)
	}
	e.Int(5)
	if e.Pos() != 1001 {
		t.Errorf("Pos after SetPos(1000)+1 byte = %d, want 1001", e.Pos())
	}
	if err := e.SetPos(0); err == nil {
		t.Error("SetPos with buffered bytes: no error")
	}
}

func TestEmitterIdentRejectsIrregular(t *testing.T) {
	var buf [MinEmitBuffer]byte
	var e Emitter
	if err := e.Reset(io.Discard, buf[:]); err != nil {
		t.Fatal(err)
	}
	e.Ident("no spaces")
	if e.Err() == nil {
		t.Error("Ident with space: no error")
	}
}

func TestEmitterAllocs(t *testing.T) {
	var buf [256]byte
	var e Emitter
	if err := e.Reset(io.Discard, buf[:]); err != nil {
		t.Fatal(err)
	}
	payload := []byte("stream payload bytes")
	allocs := testing.AllocsPerRun(100, func() {
		e.DictOpen()
		e.Name("Type")
		e.Name("Page")
		e.Name("MediaBox")
		e.ArrayOpen()
		e.Int(0)
		e.Int(0)
		e.Real(595.28)
		e.Real(841.89)
		e.ArrayClose()
		e.Name("Contents")
		e.Ref(5, 0)
		e.DictClose()
		e.Ident("BT")
		e.String(payload)
		e.Ident("Tj")
		e.Raw(payload)
		e.EOL()
	})
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	if allocs != 0 {
		t.Errorf("steady-state emission: %v allocs/op, want 0", allocs)
	}
}
