package main

import (
	"bytes"
	"fmt"
	"io"
	"strconv"

	"github.com/soypat/piudf/ppdf"
	"github.com/soypat/piudf/ppdf/piulex"
)

// maxPageDepth bounds the page tree walk. The tree is a tree only by
// convention: a /Kids pointing at an ancestor would otherwise spin forever.
const maxPageDepth = 32

func cmdPageText(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	if len(args) < 1 {
		return fmt.Errorf("pagetext: want <page>")
	}
	pageNum, err := strconv.Atoi(args[0])
	if err != nil || pageNum < 1 {
		return fmt.Errorf("pagetext: bad page number %q", args[0])
	}
	page, err := findPage(c, pageNum)
	if err != nil {
		return err
	}
	content, err := pageContent(c, page)
	if err != nil {
		return err
	}
	return showText(c, content, stdout())
}

// findPage returns the dictionary of page num, 1-based, by walking the page
// tree in order. Order is the point: /Kids order is the document's page
// order, so the nth leaf is the nth page.
func findPage(c *ctx, num int) (ppdf.Value, error) {
	root, err := c.deref(c.pdf.Trailer(), "Root")
	if err != nil {
		return ppdf.Value{}, err
	}
	pages, err := c.deref(root, "Pages")
	if err != nil {
		return ppdf.Value{}, err
	}
	seen := 0
	page, err := c.walkPages(pages, num, &seen, 0)
	if err != nil {
		return ppdf.Value{}, err
	}
	if page.IsNull() {
		return ppdf.Value{}, fmt.Errorf("page %d not found: document has %d pages", num, seen)
	}
	return page, nil
}

// walkPages depth-first searches node for the num'th leaf, counting leaves
// into seen. It returns null when the subtree holds fewer.
func (c *ctx) walkPages(node ppdf.Value, num int, seen *int, depth int) (ppdf.Value, error) {
	if depth > maxPageDepth {
		return ppdf.Value{}, fmt.Errorf("page tree deeper than %d: /Kids loop?", maxPageDepth)
	}
	kids, err := c.codec.DictGet(c.r, node, "Kids")
	if err != nil {
		return ppdf.Value{}, err
	}
	if kids.IsNull() {
		// No /Kids: a leaf, so this is a page.
		*seen++
		if *seen == num {
			return node, nil
		}
		return ppdf.Value{Tok: piulex.TokNull}, nil
	}
	if kids, err = c.pdf.Deref(c.r, kids, c.codec); err != nil {
		return ppdf.Value{}, err
	}
	// ArrayForEach hands back one element at a time and the callback cannot
	// recurse into another ArrayForEach: both share the Codec's lexer. So the
	// references are collected first, then walked.
	var refs []ppdf.ObjectID
	err = c.codec.ArrayForEach(c.r, kids, func(v ppdf.Value) bool {
		if v.Tok == piulex.TokR {
			refs = append(refs, v.ObjectID())
		}
		return true
	})
	if err != nil {
		return ppdf.Value{}, err
	}
	for _, ref := range refs {
		kid, err := c.pdf.Resolve(c.r, ref, c.codec)
		if err != nil {
			return ppdf.Value{}, err
		}
		found, err := c.walkPages(kid, num, seen, depth+1)
		if err != nil {
			return ppdf.Value{}, err
		}
		if !found.IsNull() {
			return found, nil
		}
	}
	return ppdf.Value{Tok: piulex.TokNull}, nil
}

// deref reads key from dictionary dict and resolves it if indirect.
func (c *ctx) deref(dict ppdf.Value, key string) (ppdf.Value, error) {
	v, err := c.codec.DictGet(c.r, dict, key)
	if err != nil {
		return v, fmt.Errorf("/%s: %w", key, err)
	}
	if v.IsNull() {
		return v, fmt.Errorf("/%s: absent", key)
	}
	return c.pdf.Deref(c.r, v, c.codec)
}

// pageContent returns the page's decoded content stream. /Contents is one
// stream or an array of them, and an array is one stream cut at arbitrary
// points — a text operator may straddle the seam — so they concatenate.
func pageContent(c *ctx, page ppdf.Value) ([]byte, error) {
	contents, err := c.codec.DictGet(c.r, page, "Contents")
	if err != nil {
		return nil, err
	}
	if contents.IsNull() {
		return nil, nil // A page may legally have no content.
	}
	var refs []ppdf.ObjectID
	if contents.Tok == piulex.TokR {
		refs = append(refs, contents.ObjectID())
	} else {
		arr, err := c.pdf.Deref(c.r, contents, c.codec)
		if err != nil {
			return nil, err
		}
		if !arr.IsArray() {
			return nil, fmt.Errorf("/Contents is %v, want a stream or array", arr.Tok)
		}
		err = c.codec.ArrayForEach(c.r, arr, func(v ppdf.Value) bool {
			if v.Tok == piulex.TokR {
				refs = append(refs, v.ObjectID())
			}
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	for _, ref := range refs {
		v, err := c.pdf.Resolve(c.r, ref, c.codec)
		if err != nil {
			return nil, fmt.Errorf("resolving content %v: %w", ref, err)
		}
		rd, err := c.pdf.OpenStream(c.r, v, c.codec)
		if err != nil {
			return nil, fmt.Errorf("opening content %v: %w", ref, err)
		}
		if _, err = buf.ReadFrom(rd); err != nil {
			return nil, fmt.Errorf("reading content %v: %w", ref, err)
		}
		buf.WriteByte('\n') // The seam between streams is a token boundary.
	}
	return buf.Bytes(), nil
}

// showText writes the page's text in the order the content stream shows it.
// That order is the drawing order, which is the easiest to get and is usually
// but not always the reading order: nothing stops a generator from painting
// the last line first. Getting reading order means sorting by the text matrix,
// which means tracking it.
//
// Bytes are written as they appear in the string operands. Mapping them to
// runes needs the font's /Encoding and /ToUnicode, so anything but simple
// ASCII-ish encodings comes out wrong here.
func showText(c *ctx, content []byte, w io.Writer) error {
	var lx piulex.Lexer
	lx.ReuseLiteralBuffer = true
	buf := make([]byte, 4096)
	lx.MaxLiteral = len(buf)
	if err := lx.Reset(bytes.NewReader(content), 0, buf); err != nil {
		return err
	}
	// pending holds the string operands seen since the last operator: TJ
	// takes an array of them, every other show operator takes one.
	var pending [][]byte
	for {
		tok, _, lit := lx.NextToken()
		switch tok {
		case piulex.TokEOF:
			return nil
		case piulex.TokIllegal:
			return lx.Err()
		case piulex.TokString, piulex.TokHexString:
			// lit dies at the next token; the operator that consumes it is at
			// least one token away.
			pending = append(pending, append([]byte(nil), lit...))
			continue
		case piulex.TokIdent:
			switch string(lit) {
			case "Tj", "'", "\"":
				// The operand is the last string: ' and " take other operands
				// before it.
				if n := len(pending); n > 0 {
					w.Write(pending[n-1])
				}
			case "TJ":
				// The array's numbers are kerning, which the lexer already
				// left out of pending. Enough of them means a space, but
				// deciding that needs the font's widths.
				for _, s := range pending {
					w.Write(s)
				}
			case "ET":
				// One text object per line: the coarsest split that does not
				// require the text matrix.
				fmt.Fprintln(w)
			}
		}
		pending = pending[:0]
	}
}
