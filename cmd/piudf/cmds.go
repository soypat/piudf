package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piulex"
)

func stdout() io.Writer { return os.Stdout }

func cmdInfo(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	w := stdout()
	var header [16]byte
	n, _ := c.r.ReadAt(header[:], 0)
	version := strings.TrimRight(strings.SplitN(string(header[:n]), "\n", 2)[0], "\r\n")
	fmt.Fprintf(w, "file:       %s (%d bytes)\n", c.path, c.size)
	fmt.Fprintf(w, "header:     %s\n", version)

	forms := make([]string, 0, c.pdf.NumRevisions())
	for i := range c.pdf.NumRevisions() {
		forms = append(forms, revForm(c.pdf.Revision(i)))
	}
	fmt.Fprintf(w, "revisions:  %d (%s), %d xref subsections\n",
		c.pdf.NumRevisions(), strings.Join(forms, ", "), c.pdf.NumXrefSections())
	ram := c.pdf.SizeOnRAM()
	fmt.Fprintf(w, "held:       %d B of PDF state (%.5f%% of file)\n",
		ram, 100*float64(ram)/float64(c.size))

	tr := c.pdf.Trailer()
	for _, key := range []string{"Size", "Root", "Info", "Encrypt"} {
		v, err := c.codec.DictGet(c.pdf, c.r, tr, key)
		if err != nil {
			return fmt.Errorf("trailer /%s: %w", key, err)
		}
		if v.IsNull() {
			continue
		}
		fmt.Fprintf(w, "%-11s %s\n", strings.ToLower(key)+":", vstr(c, v))
	}
	return nil
}

// revForm names the cross-reference form a revision uses.
func revForm(rev piudf.Revision) string {
	if rev.Classic {
		return "classic table"
	}
	return "xref stream"
}

func cmdTrailer(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	w := stdout()
	tr := c.pdf.Trailer()
	fmt.Fprintf(w, "trailer @%#x\n", tr.I)
	// DictForEach hands back a key and a shallow Value per entry in one scan;
	// the callback must not hold key, which points into the Codec's arena.
	return c.codec.DictForEach(c.pdf, c.r, tr, func(key []byte, v piudf.Value) bool {
		fmt.Fprintf(w, "  /%-12s %s\n", key, vstr(c, v))
		return true
	})
}

func cmdRevisions(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	w := stdout()
	fmt.Fprintf(w, "%-4s %-14s %-12s %-10s %s\n", "rev", "form", "xref", "trailer", "sections")
	for i := range c.pdf.NumRevisions() {
		rev := c.pdf.Revision(i)
		// A revision owns the sections from its own first up to the next
		// revision's; the oldest owns the rest.
		end := c.pdf.NumXrefSections()
		if i+1 < c.pdf.NumRevisions() {
			end = c.pdf.Revision(i + 1).FirstSection
		}
		fmt.Fprintf(w, "%-4d %-14s %#-12x %#-10x %d..%d\n",
			i, revForm(rev), rev.XrefOffset, rev.Trailer().I, rev.FirstSection, end)
	}
	return nil
}

func cmdLex(c *ctx, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("lex: want <off>")
	}
	off, err := strconv.ParseInt(args[0], 0, 64)
	if err != nil {
		return fmt.Errorf("lex: bad offset %q: %w", args[0], err)
	}
	fs := flag.NewFlagSet("lex", flag.ExitOnError)
	max := fs.Int("n", 32, "maximum tokens to print")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	w := stdout()
	var lx piulex.Lexer
	lx.ReuseLiteralBuffer = true
	buf := make([]byte, 4096)
	lx.MaxLiteral = len(buf)
	if err := lx.Reset(c.r, off, buf); err != nil {
		return err
	}
	for range *max {
		tok, pos, lit := lx.NextToken()
		fmt.Fprintf(w, "%#-10x %-12v %s\n", int64(pos), tok, lit)
		if tok == piulex.TokEOF {
			return nil
		} else if tok == piulex.TokIllegal {
			return lx.Err()
		}
	}
	return nil
}
