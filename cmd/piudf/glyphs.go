package main

import (
	"strconv"
	"strings"
)

// glyphRune maps a PostScript glyph name to the character it draws. A font's
// /Differences names glyphs, not characters, so this is what turns a subset
// font's codes back into text.
//
// Most names are one character long and stand for themselves. The rest are
// the Adobe Glyph List, of which this covers the ASCII range plus the
// punctuation a text extractor meets constantly. A name outside it yields
// zero: unknown is better reported than guessed.
func glyphRune(name string) rune {
	if name == "" {
		return 0
	}
	if r := []rune(name); len(r) == 1 {
		return r[0] // i.e. /B, /a, /7.
	}
	if u, ok := uniName(name); ok {
		return u
	}
	return aglASCII[name]
}

// uniName decodes the uniXXXX and uXXXX[XX] forms, which spell a code point
// out in hex instead of naming it.
func uniName(name string) (rune, bool) {
	var hex string
	switch {
	case strings.HasPrefix(name, "uni") && len(name) == 7:
		hex = name[3:]
	case strings.HasPrefix(name, "u") && len(name) >= 5 && len(name) <= 7:
		hex = name[1:]
	default:
		return 0, false
	}
	n, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(n), true
}

// aglASCII is the multi-character part of the Adobe Glyph List that a Latin
// text stream actually uses. Single-character names are handled by glyphRune
// and absent here.
var aglASCII = map[string]rune{
	"space":            ' ',
	"exclam":           '!',
	"quotedbl":         '"',
	"numbersign":       '#',
	"dollar":           '$',
	"percent":          '%',
	"ampersand":        '&',
	"quotesingle":      '\'',
	"parenleft":        '(',
	"parenright":       ')',
	"asterisk":         '*',
	"plus":             '+',
	"comma":            ',',
	"hyphen":           '-',
	"period":           '.',
	"slash":            '/',
	"zero":             '0',
	"one":              '1',
	"two":              '2',
	"three":            '3',
	"four":             '4',
	"five":             '5',
	"six":              '6',
	"seven":            '7',
	"eight":            '8',
	"nine":             '9',
	"colon":            ':',
	"semicolon":        ';',
	"less":             '<',
	"equal":            '=',
	"greater":          '>',
	"question":         '?',
	"at":               '@',
	"bracketleft":      '[',
	"backslash":        '\\',
	"bracketright":     ']',
	"asciicircum":      '^',
	"underscore":       '_',
	"grave":            '`',
	"braceleft":        '{',
	"bar":              '|',
	"braceright":       '}',
	"asciitilde":       '~',
	"quoteleft":        '‘',
	"quoteright":       '’',
	"quotedblleft":     '“',
	"quotedblright":    '”',
	"quotesinglbase":   '‚',
	"quotedblbase":     '„',
	"endash":           '–',
	"emdash":           '—',
	"bullet":           '•',
	"ellipsis":         '…',
	"dagger":           '†',
	"daggerdbl":        '‡',
	"perthousand":      '‰',
	"guilsinglleft":    '‹',
	"guilsinglright":   '›',
	"guillemotleft":    '«',
	"guillemotright":   '»',
	"fi":               'ﬁ',
	"fl":               'ﬂ',
	"degree":           '°',
	"plusminus":        '±',
	"multiply":         '×',
	"divide":           '÷',
	"copyright":        '©',
	"registered":       '®',
	"trademark":        '™',
	"currency":         '¤',
	"cent":             '¢',
	"sterling":         '£',
	"yen":              '¥',
	"euro":             '€',
	"section":          '§',
	"paragraph":        '¶',
	"periodcentered":   '·',
	"nbspace":          ' ',
	"minus":            '−',
	"fraction":         '⁄',
	"nonbreakingspace": ' ',
}
