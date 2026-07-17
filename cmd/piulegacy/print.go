package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	piudf "github.com/soypat/piudf/internal/piulegacy"
)

// printer renders Values as an indented tree where every span-backed node
// is annotated with its exact coordinates: @off+len in the file, or
// @objstmN:off+len inside a compressed object stream.
type printer struct {
	c        *ctx
	w        *bufio.Writer
	buf      []byte
	maxDepth int // Composites below this depth print as "..." stubs.
}

func newPrinter(c *ctx, maxDepth int) *printer {
	return &printer{c: c, w: bufio.NewWriter(io.Writer(stdout())), maxDepth: maxDepth}
}

// span formats a Value's coordinate annotation; empty for scalars.
func span(v piudf.Value) string {
	switch v.Kind {
	case piudf.KindString, piudf.KindHexString, piudf.KindName,
		piudf.KindArray, piudf.KindDict, piudf.KindStream:
	default:
		return ""
	}
	if v.Ref.Num != 0 {
		return fmt.Sprintf("@objstm%d:%#x+%d", v.Ref.Num, v.I, v.N)
	}
	return fmt.Sprintf("@%#x+%d", v.I, v.N)
}

// node prints v at the current line position; composites continue on
// indented lines below.
func (pr *printer) node(v piudf.Value, indent, depth int) error {
	c, w := pr.c, pr.w
	switch v.Kind {
	case piudf.KindNull, piudf.KindInvalid:
		io.WriteString(w, "null")
	case piudf.KindBool:
		b, _ := v.Bool()
		fmt.Fprint(w, b)
	case piudf.KindInt:
		n, _ := v.Int()
		fmt.Fprint(w, n)
	case piudf.KindReal:
		f, _ := v.Float()
		fmt.Fprintf(w, "%g", f)
	case piudf.KindRef:
		fmt.Fprint(w, v.Ref)

	case piudf.KindName:
		var err error
		pr.buf, err = c.d.AppendString(pr.buf[:0], c.p, c.r, v)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "/%s", pr.buf)

	case piudf.KindString, piudf.KindHexString:
		var err error
		pr.buf, err = c.d.AppendString(pr.buf[:0], c.p, c.r, v)
		if err != nil {
			return err
		}
		w.WriteByte('(')
		w.Write(escape(nil, pr.buf))
		fmt.Fprintf(w, ") %s", span(v))

	case piudf.KindArray:
		n, err := c.d.ArrayLen(c.p, c.r, v)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "array[%d] %s", n, span(v))
		if depth >= pr.maxDepth {
			io.WriteString(w, " ...")
			return nil
		}
		for i := 0; i < n; i++ {
			elem, err := c.d.ArrayIndex(c.p, c.r, v, i)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "\n%s[%d] ", pad(indent+2), i)
			if err := pr.node(elem, indent+2, depth+1); err != nil {
				return err
			}
		}

	case piudf.KindDict, piudf.KindStream:
		label := "dict"
		if v.Kind == piudf.KindStream {
			label = "stream"
		}
		n, err := c.d.DictLen(c.p, c.r, v)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s[%d] %s", label, n, span(v))
		if v.Kind == piudf.KindStream {
			if _, info, err := c.d.RawStream(c.p, c.r, v); err == nil {
				fmt.Fprintf(w, "\n%spayload @%#x+%d", pad(indent+2), info.Offset, info.Length)
				if info.Filter.Kind == piudf.KindName {
					if fb, err := c.d.AppendString(nil, c.p, c.r, info.Filter); err == nil {
						fmt.Fprintf(w, " filter /%s", fb)
					}
				}
			}
		}
		if depth >= pr.maxDepth {
			io.WriteString(w, " ...")
			return nil
		}
		for i := 0; i < n; i++ {
			key, val, err := c.d.DictIndex(c.p, c.r, v, i)
			if err != nil {
				return err
			}
			pr.buf, err = c.d.AppendString(pr.buf[:0], c.p, c.r, key)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "\n%s/%s ", pad(indent+2), pr.buf)
			if err := pr.node(val, indent+2, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// printValue renders one complete value tree followed by a newline.
func (pr *printer) printValue(v piudf.Value) error {
	err := pr.node(v, 0, 0)
	pr.w.WriteByte('\n')
	if ferr := pr.w.Flush(); err == nil {
		err = ferr
	}
	return err
}

func pad(n int) string {
	const spaces = "                                                                "
	if n > len(spaces) {
		n = len(spaces)
	}
	return spaces[:n]
}

// escape renders string payload bytes for the terminal: printable ASCII
// passes through, PDF delimiters and the rest become escapes.
func escape(dst, s []byte) []byte {
	for _, b := range s {
		switch {
		case b == '\\' || b == '(' || b == ')':
			dst = append(dst, '\\', b)
		case b == '\n':
			dst = append(dst, '\\', 'n')
		case b == '\r':
			dst = append(dst, '\\', 'r')
		case b == '\t':
			dst = append(dst, '\\', 't')
		case b >= 0x20 && b < 0x7f:
			dst = append(dst, b)
		default:
			dst = append(dst, fmt.Sprintf("\\x%02x", b)...)
		}
	}
	return dst
}

// hexdump writes data in xxd format, addressing bytes from baseOff.
func hexdump(w io.Writer, data []byte, baseOff int64) {
	var line strings.Builder
	for i := 0; i < len(data); i += 16 {
		line.Reset()
		fmt.Fprintf(&line, "%08x: ", baseOff+int64(i))
		end := i + 16
		if end > len(data) {
			end = len(data)
		}
		for j := i; j < i+16; j++ {
			if j < end {
				fmt.Fprintf(&line, "%02x", data[j])
			} else {
				line.WriteString("  ")
			}
			if j%2 == 1 {
				line.WriteByte(' ')
			}
		}
		line.WriteString(" |")
		for j := i; j < end; j++ {
			b := data[j]
			if b < 0x20 || b >= 0x7f {
				b = '.'
			}
			line.WriteByte(b)
		}
		line.WriteString("|\n")
		io.WriteString(w, line.String())
	}
}
