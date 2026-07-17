// Command piudf explores the binary layout of PDF files through the lazy
// decoder: no object graph is built, and everything printed carries its
// exact file coordinates (@offset+length).
//
// Usage:
//
//	piudf <command> <file.pdf> [args] [flags]
package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	piudf "github.com/soypat/piudf/internal/piulegacy"
)

// ctx is the shared command context: one decoded document and its reader.
type ctx struct {
	d         *piudf.Decoder
	p         *piudf.PDF
	r         io.ReaderAt
	size      int64
	path      string
	decodeErr error
}

// needDecode gates commands that require a successfully decoded document
// (lex and raw work on arbitrary bytes even when Decode failed).
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
		"info":      {cmdInfo, "", "document summary: version, xref form, revisions, object counts"},
		"trailer":   {cmdTrailer, "", "print the newest trailer dictionary"},
		"obj":       {cmdObj, "<num> [gen] [-depth n]", "resolve and print one object's value tree"},
		"ls":        {cmdLs, "[-k kind] [-max n]", "list every object: kind and file location"},
		"xref":      {cmdXref, "", "cross-reference sections and records, newest revision first"},
		"map":       {cmdMap, "", "byte-range map of the whole file, gaps included"},
		"revisions": {cmdRevisions, "", "incremental-update history, oldest first"},
		"stream":    {cmdStream, "<num> [-n bytes] [-o file]", "stream payload info and raw bytes"},
		"raw":       {cmdRaw, "<num> | <off> <len>", "hexdump an object's span or arbitrary bytes"},
		"lex":       {cmdLex, "<off> [-n tokens]", "token stream from any file offset"},
		"walk":      {cmdWalk, "<path>", "resolve a path, e.g. /Root/Pages/Kids/0"},
		"mem":       {cmdMem, "[-sweep]", "decoder and document memory usage"},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "piudf explores the binary layout of PDF files (lazy decoding).\n\n")
	fmt.Fprintf(os.Stderr, "usage: piudf <command> <file.pdf> [args] [flags]\n\ncommands:\n")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := commands[name]
		fmt.Fprintf(os.Stderr, "  %-10s %-28s %s\n", name, c.args, c.descr)
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
		fmt.Fprintf(os.Stderr, "piudf: unknown command %q\n\n", name)
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
	c := &ctx{d: new(piudf.Decoder), p: new(piudf.PDF), r: f, size: st.Size(), path: path}
	c.decodeErr = c.d.Decode(c.p, c.r, c.size, piudf.DecodeLimits{})
	if err := cmd.run(c, os.Args[3:]); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "piudf:", err)
	os.Exit(1)
}
