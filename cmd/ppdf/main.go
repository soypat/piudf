// Command ppdf explores the binary layout of PDF files through the lazy
// decoder in [github.com/soypat/piudf/ppdf]: no object graph is built, and
// everything printed carries its exact file coordinates (@offset+length).
//
// Usage:
//
//	ppdf <command> <file.pdf> [args] [flags]
package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/soypat/piudf/ppdf"
)

// ctx is the shared command context: one decoded document and its reader.
// The reader is held here and passed to every ppdf call; the library never
// stores it.
type ctx struct {
	pdf       *ppdf.PDF
	codec     *ppdf.Codec
	r         io.ReaderAt
	size      int64
	path      string
	decodeErr error
}

// needDecode gates commands that require a decoded document. lex works on
// arbitrary bytes, so it runs even when Decode failed.
func (c *ctx) needDecode() error {
	if c.decodeErr != nil {
		return fmt.Errorf("decoding %s: %w", c.path, c.decodeErr)
	}
	return nil
}

type command struct {
	run   func(*ctx, []string) error
	args  string
	descr string
}

var commands map[string]command

func init() {
	// Filled in init to avoid an initialization cycle through usage().
	commands = map[string]command{
		"info":      {cmdInfo, "", "document summary: version, xref form, revisions"},
		"trailer":   {cmdTrailer, "", "print the newest trailer dictionary"},
		"revisions": {cmdRevisions, "", "incremental-update history, newest first"},
		"lex":       {cmdLex, "<off> [-n tokens]", "token stream from any file offset"},
		"pagetext":  {cmdPageText, "<page>", "text of one page, in content-stream order"},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "ppdf explores the binary layout of PDF files (lazy decoding).\n\n")
	fmt.Fprintf(os.Stderr, "usage: ppdf <command> <file.pdf> [args] [flags]\n\ncommands:\n")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := commands[name]
		fmt.Fprintf(os.Stderr, "  %-10s %-22s %s\n", name, c.args, c.descr)
	}
}

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}
	name, path := os.Args[1], os.Args[2]
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "ppdf: unknown command %q\n\n", name)
		usage()
		os.Exit(2)
	}
	f, err := os.Open(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fatal(err)
	}
	cfg := ppdf.DecoderConfig{
		Buffer:          make([]byte, 8192),
		MaxLazySections: 4096,
		MaxDepth:        64,
	}
	codec := new(ppdf.Codec)
	if err := codec.Configure(cfg); err != nil {
		fatal(err)
	}
	c := &ctx{pdf: new(ppdf.PDF), codec: codec, r: f, size: st.Size(), path: path}
	c.decodeErr = c.pdf.Decode(c.r, c.size, c.codec)
	// Now that the document is indexed it can say what caching its
	// cross-reference rows costs, which is knowable no earlier. This tool
	// reads one document and exits, so it buys them: a page deep in a file
	// resolves objects in no particular order, and every backward step without
	// a cache is another decode of the table.
	if c.decodeErr == nil {
		if n := c.pdf.XrefCacheSize(); n > 0 {
			cfg.XrefCache = make([]byte, n)
			if err := codec.Configure(cfg); err != nil {
				fatal(err)
			}
		}
	}
	if err := cmd.run(c, os.Args[3:]); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ppdf:", err)
	os.Exit(1)
}
