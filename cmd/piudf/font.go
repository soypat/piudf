package main

import (
	"bytes"
	"fmt"
	"io"
	"unicode/utf16"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piulex"
)

// font decodes the codes of one PDF font back into text. A show operator's
// operand is not text: it is a string of codes the font maps to glyphs, and
// nothing requires that map to resemble ASCII. Subset fonts routinely number
// their glyphs from zero in order of first use.
type font struct {
	// width is the bytes per code. Simple fonts use 1; a composite font
	// declares it in its CMap's codespace range, and 2 is universal there.
	width int
	// text maps a code to what it draws. A code absent from it has no known
	// meaning, which is not the same as drawing nothing.
	text map[uint32]string
}

// decode renders a show operator's operand.
func (f *font) decode(dst []byte, s []byte) []byte {
	if f == nil {
		// No font selected, or one whose encoding we could not read. Its
		// codes are bytes and might be text; say so rather than drop them.
		return append(dst, s...)
	}
	for i := 0; i+f.width <= len(s); i += f.width {
		var code uint32
		for j := range f.width {
			code = code<<8 | uint32(s[i+j])
		}
		if t, ok := f.text[code]; ok {
			dst = append(dst, t...)
		} else if f.width == 1 {
			dst = append(dst, s[i]) // Unmapped single byte: assume it stands for itself.
		}
	}
	return dst
}

// pageFonts builds a decoder per /Font resource name, so a Tf operator can
// select one. /Resources is inheritable, so a page without one takes its
// parent's.
func pageFonts(c *ctx, page piudf.Value) (map[string]*font, error) {
	res, err := c.inherited(page, "Resources")
	if err != nil || res.IsNull() {
		return nil, err
	}
	fontsD, err := c.codec.DictGet(c.pdf, c.r, res, "Font")
	if err != nil {
		return nil, err
	}
	if fontsD, err = c.pdf.Deref(c.r, fontsD, c.codec); err != nil || fontsD.IsNull() {
		return nil, err
	}
	// The references are collected before any is resolved: one Codec means
	// one lexer, so resolving inside DictForEach would move the scan.
	type entry struct {
		name string
		id   piudf.ObjectID
	}
	var refs []entry
	err = c.codec.DictForEach(c.pdf, c.r, fontsD, func(k []byte, v piudf.Value) bool {
		if v.Tok == piulex.TokR {
			refs = append(refs, entry{string(k), v.ObjectID()})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	fonts := make(map[string]*font, len(refs))
	for _, e := range refs {
		f, err := loadFont(c, e.id)
		if err != nil {
			return nil, fmt.Errorf("font /%s (%v): %w", e.name, e.id, err)
		}
		fonts[e.name] = f
	}
	return fonts, nil
}

// inherited reads key from dict, walking /Parent when absent. The page tree
// lets a node hand /Resources and /MediaBox down to its kids.
func (c *ctx) inherited(dict piudf.Value, key string) (piudf.Value, error) {
	for depth := 0; depth <= maxPageDepth; depth++ {
		v, err := c.codec.DictGet(c.pdf, c.r, dict, key)
		if err != nil {
			return v, err
		}
		if !v.IsNull() {
			return c.pdf.Deref(c.r, v, c.codec)
		}
		parent, err := c.codec.DictGet(c.pdf, c.r, dict, "Parent")
		if err != nil || parent.IsNull() {
			return piudf.Value{Tok: piulex.TokNull}, err
		}
		if dict, err = c.pdf.Deref(c.r, parent, c.codec); err != nil {
			return dict, err
		}
	}
	return piudf.Value{}, fmt.Errorf("/Parent chain deeper than %d", maxPageDepth)
}

// loadFont reads a font's code-to-text map. /ToUnicode is authoritative and
// says so explicitly; /Encoding /Differences is the older way and names
// glyphs, which only map back to text because the names are conventional.
func loadFont(c *ctx, id piudf.ObjectID) (*font, error) {
	fd, err := c.pdf.Resolve(c.r, id, c.codec)
	if err != nil {
		return nil, err
	}
	tu, err := c.codec.DictGet(c.pdf, c.r, fd, "ToUnicode")
	if err != nil {
		return nil, err
	}
	if !tu.IsNull() {
		tuv, err := c.pdf.Deref(c.r, tu, c.codec)
		if err != nil {
			return nil, err
		}
		cmap, err := readAllStream(c, tuv)
		if err != nil {
			return nil, err
		}
		return parseCMap(cmap)
	}
	f := &font{width: 1, text: map[uint32]string{}}
	enc, err := c.codec.DictGet(c.pdf, c.r, fd, "Encoding")
	if err != nil {
		return nil, err
	}
	if enc, err = c.pdf.Deref(c.r, enc, c.codec); err != nil {
		return nil, err
	}
	if !enc.IsDict() {
		// A base encoding by name, or none: codes are already bytes of text
		// for the Latin ones, which is as far as this goes.
		return f, nil
	}
	diff, err := c.codec.DictGet(c.pdf, c.r, enc, "Differences")
	if err != nil {
		return nil, err
	}
	if diff, err = c.pdf.Deref(c.r, diff, c.codec); err != nil || !diff.IsArray() {
		return f, err
	}
	// /Differences is a code followed by the names of the glyphs at that code
	// onward, repeated: [0 /B /D 65 /a] assigns 0->B, 1->D, 65->a.
	var code uint32
	var names []piudf.Value
	var codes []uint32
	err = c.codec.ArrayForEach(c.pdf, c.r, diff, func(v piudf.Value) bool {
		if n, ok := v.Int(); ok {
			code = uint32(n)
			return true
		}
		if v.Tok != piulex.TokName {
			return true
		}
		// The names are collected, not read: reading one re-lexes, and the
		// walk owns the lexer until it ends.
		names = append(names, v)
		codes = append(codes, code)
		code++
		return true
	})
	if err != nil {
		return nil, err
	}
	var name []byte
	for i, nv := range names {
		if name, err = c.codec.AppendString(name[:0], c.pdf, c.r, nv); err != nil {
			return nil, err
		}
		if r := glyphRune(string(name)); r != 0 {
			f.text[codes[i]] = string(r)
		}
	}
	return f, nil
}

// readAllStream decodes a whole stream into memory. Fine for a CMap, which is
// a few hundred bytes; the library hands back a reader precisely so the
// caller decides this.
func readAllStream(c *ctx, v piudf.Value) ([]byte, error) {
	rd, err := c.pdf.OpenStream(c.r, v, new(piudf.Stream), c.codec)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(rd)
}

// parseCMap reads the bfchar and bfrange sections of a /ToUnicode CMap, which
// is PostScript in name only: the operators that matter are a flat list of
// hex pairs. Destinations are UTF-16BE.
func parseCMap(content []byte) (*font, error) {
	f := &font{width: 2, text: map[uint32]string{}}
	var lx piulex.Lexer
	lx.ReuseLiteralBuffer = true
	buf := make([]byte, 4096)
	lx.MaxLiteral = len(buf)
	if err := lx.Reset(bytes.NewReader(content), 0, buf); err != nil {
		return nil, err
	}
	// operands holds the hex strings since the last operator; an array of
	// them nests one level, which arrayDepth tracks.
	var operands [][]byte
	var arrayDepth int
	var arrayItems [][]byte
	for {
		tok, _, lit := lx.NextToken()
		switch tok {
		case piulex.TokEOF:
			return f, nil
		case piulex.TokIllegal:
			return nil, lx.Err()
		case piulex.TokArrayOpen:
			arrayDepth++
			arrayItems = arrayItems[:0]
			continue
		case piulex.TokArrayClose:
			arrayDepth--
			continue
		case piulex.TokHexString:
			s := append([]byte(nil), lit...)
			if arrayDepth > 0 {
				arrayItems = append(arrayItems, s)
			} else {
				operands = append(operands, s)
			}
			continue
		case piulex.TokIdent:
			switch string(lit) {
			case "endcodespacerange":
				// The first code's byte length is the code width; the format
				// allows mixed widths, which this does not.
				if len(operands) > 0 && len(operands[0]) > 0 {
					f.width = len(operands[0])
				}
			case "endbfchar":
				// Pairs: <src> <dst>.
				for i := 0; i+1 < len(operands); i += 2 {
					f.text[beCode(operands[i])] = utf16BE(operands[i+1])
				}
			case "endbfrange":
				parseBFRange(f, operands, arrayItems)
			}
		case piulex.TokInt, piulex.TokReal, piulex.TokName, piulex.TokString,
			piulex.TokDictOpen, piulex.TokDictClose:
			continue // Counts, CMap metadata: not part of the mapping.
		}
		operands = operands[:0]
		arrayItems = arrayItems[:0]
	}
}

// parseBFRange handles both forms: "<lo> <hi> <dst>" walks dst upward across
// the range, and "<lo> <hi> [<d0> <d1> ...]" gives one destination per code.
func parseBFRange(f *font, operands, arrayItems [][]byte) {
	if len(arrayItems) > 0 && len(operands) >= 2 {
		lo := beCode(operands[0])
		for i, item := range arrayItems {
			f.text[lo+uint32(i)] = utf16BE(item)
		}
		return
	}
	for i := 0; i+2 < len(operands); i += 3 {
		lo, hi := beCode(operands[i]), beCode(operands[i+1])
		dst := operands[i+2]
		if hi < lo || hi-lo > 0xffff {
			continue // Nonsense range; a CMap is not a reason to allocate 4 GB.
		}
		for code := lo; code <= hi; code++ {
			// Only the last UTF-16 unit advances: the range walks code
			// points, not strings.
			d := append([]byte(nil), dst...)
			if n := len(d); n >= 2 {
				v := uint32(d[n-2])<<8 | uint32(d[n-1]) + (code - lo)
				d[n-2], d[n-1] = byte(v>>8), byte(v)
			}
			f.text[code] = utf16BE(d)
		}
	}
}

// beCode reads a code as a big-endian integer, which is how a CMap writes it.
func beCode(b []byte) uint32 {
	var v uint32
	for _, c := range b {
		v = v<<8 | uint32(c)
	}
	return v
}

// utf16BE decodes a CMap destination, which ISO 32000-1 9.10.3 defines as
// UTF-16BE.
func utf16BE(b []byte) string {
	if len(b) == 1 {
		return string(rune(b[0]))
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}
