package main

import (
	"fmt"
	"strconv"

	"github.com/soypat/piudf/ppdf"
	"github.com/soypat/piudf/ppdf/piulex"
)

// maxSpanPrint bounds what a span renders as; a content stream dictionary is
// small but an array of 15888 kids is not.
const maxSpanPrint = 96

// readSpan returns the file bytes a span Value addresses. Values are
// coordinates, so this is the only place bytes are copied, and the copy is
// the caller's, not the library's.
func readSpan(c *ctx, v ppdf.Value) ([]byte, error) {
	if v.Stm != 0 {
		return nil, fmt.Errorf("value lives in object stream %d: unsupported", v.Stm)
	}
	n := int64(v.N)
	if n <= 0 || v.I < 0 || v.I+n > c.size {
		return nil, fmt.Errorf("span @%#x+%d outside a %d byte file", v.I, v.N, c.size)
	}
	b := make([]byte, min(n, maxSpanPrint))
	_, err := c.r.ReadAt(b, v.I)
	if err != nil {
		return nil, err
	}
	if n > maxSpanPrint {
		b = append(b, "..."...)
	}
	return b, nil
}

// vstr renders v as PDF source. Scalars carry their whole value in the Value
// itself; a span carries only coordinates, so its text is read back from the
// file and its coordinates are printed alongside.
func vstr(c *ctx, v ppdf.Value) string {
	switch v.Tok {
	case piulex.TokR:
		return string(v.ObjectID().AppendString(nil))
	case piulex.TokInt:
		n, _ := v.Int()
		return strconv.FormatInt(n, 10)
	case piulex.TokReal:
		f, _ := v.Float()
		return strconv.FormatFloat(f, 'g', -1, 64)
	case piulex.TokTrue:
		return "true"
	case piulex.TokFalse:
		return "false"
	case piulex.TokNull:
		return "null"
	}
	b, err := readSpan(c, v)
	if err != nil {
		return fmt.Sprintf("<%v: %v>", v.Tok, err)
	}
	// A span starts at its delimiter, so the bytes already read as PDF source:
	// escapes, name #-codes and all. This tool reports what is on disk, not
	// what a text extractor would make of it.
	switch v.Tok {
	case piulex.TokName:
		return string(b)
	}
	return fmt.Sprintf("%s @%#x+%d", b, v.I, v.N)
}
