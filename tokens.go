package piudf

import "fmt"

// Token is a lexical token of PDF syntax.
type Token uint8

const (
	TokUndefined Token = iota // undefined
	TokIllegal                // illegal
	TokEOF                    // EOF

	// Literal-carrying tokens.
	TokInt       // <integer>
	TokReal      // <real>
	TokName      // <name>
	TokString    // <string>
	TokHexString // <hexstring>
	TokIdent     // <identifier>

	// Delimiter tokens.
	TokDictOpen   // <<
	TokDictClose  // >>
	TokArrayOpen  // [
	TokArrayClose // ]
	TokBraceOpen  // {
	TokBraceClose // }

	// TokR is the indirect reference keyword as in "12 0 R".
	// Single-character keyword, handled outside the perfect hash.
	TokR // R

	// Add keywords between keywordBeg and keywordEnd. Their linecomment
	// text is the exact keyword as generated into stringers.go.
	keywordBeg
	TokObj       // obj
	TokEndobj    // endobj
	TokStream    // stream
	TokEndstream // endstream
	TokXref      // xref
	TokTrailer   // trailer
	TokStartxref // startxref
	TokTrue      // true
	TokFalse     // false
	TokNull      // null
	keywordEnd
)

var keywordMap [1 << 4]Token

// kwhash is a perfect hash function for the PDF keywords between keywordBeg
// and keywordEnd. It assumes len(s) >= 2. Coefficients found with the search
// harness in perfect_hash_test.go: h = (2*len + s[0] + s[1]) & 15.
func kwhash(s string) uint {
	return (2*uint(len(s)) + uint(s[0]) + uint(s[1])) & uint(len(keywordMap)-1)
}

func init() {
	for tok := keywordBeg + 1; tok < keywordEnd; tok++ {
		h := kwhash(tok.String())
		if keywordMap[h] != 0 {
			panic(fmt.Sprintf("imperfect hash at %#x: %s collides with %s", h, keywordMap[h].String(), tok.String()))
		}
		keywordMap[h] = tok
	}
}

// Lookup maps a bare identifier to its keyword token, or TokIdent if s is
// not a PDF keyword. Call as Lookup(string(literal)); the conversion does
// not allocate.
func Lookup(s string) Token {
	if len(s) < 2 {
		if s == "R" {
			return TokR
		}
		return TokIdent
	}
	tok := keywordMap[kwhash(s)]
	if tok != 0 && s == tok.String() {
		return tok
	}
	return TokIdent
}
