package main

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piulex"
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
	fonts, err := pageFonts(c, page)
	if err != nil {
		return err
	}
	dst, err := showText(nil, content, fonts)
	if err != nil {
		return err
	}
	stdout().Write(dst)
	return nil
}

// findPage returns the dictionary of page num, 1-based, by walking the page
// tree in order. Order is the point: /Kids order is the document's page
// order, so the nth leaf is the nth page.
func findPage(c *ctx, num int) (piudf.Value, error) {
	root, err := c.deref(c.pdf.Trailer(), "Root")
	if err != nil {
		return piudf.Value{}, err
	}
	pages, err := c.deref(root, "Pages")
	if err != nil {
		return piudf.Value{}, err
	}
	seen := 0
	page, err := c.walkPages(pages, num, &seen, 0)
	if err != nil {
		return piudf.Value{}, err
	}
	if page.IsNull() {
		return piudf.Value{}, fmt.Errorf("page %d not found: document has %d pages", num, seen)
	}
	return page, nil
}

// walkPages depth-first searches node for the num'th leaf, counting leaves
// into seen. It returns null when the subtree holds fewer.
func (c *ctx) walkPages(node piudf.Value, num int, seen *int, depth int) (piudf.Value, error) {
	if depth > maxPageDepth {
		return piudf.Value{}, fmt.Errorf("page tree deeper than %d: /Kids loop?", maxPageDepth)
	}
	kids, err := c.codec.DictGet(c.pdf, c.r, node, "Kids")
	if err != nil {
		return piudf.Value{}, err
	}
	if kids.IsNull() {
		// No /Kids: a leaf, so this is a page.
		*seen++
		if *seen == num {
			return node, nil
		}
		return piudf.Value{Tok: piulex.TokNull}, nil
	}
	if kids, err = c.pdf.Deref(c.r, kids, c.codec); err != nil {
		return piudf.Value{}, err
	}
	// ArrayForEach hands back one element at a time and the callback cannot
	// recurse into another ArrayForEach: both share the Codec's lexer. So the
	// references are collected first, then walked.
	var refs []piudf.ObjectID
	err = c.codec.ArrayForEach(c.pdf, c.r, kids, func(v piudf.Value) bool {
		if v.Tok == piulex.TokR {
			refs = append(refs, v.ObjectID())
		}
		return true
	})
	if err != nil {
		return piudf.Value{}, err
	}
	for _, ref := range refs {
		kid, err := c.pdf.Resolve(c.r, ref, c.codec)
		if err != nil {
			return piudf.Value{}, err
		}
		found, err := c.walkPages(kid, num, seen, depth+1)
		if err != nil {
			return piudf.Value{}, err
		}
		if !found.IsNull() {
			return found, nil
		}
	}
	return piudf.Value{Tok: piulex.TokNull}, nil
}

// deref reads key from dictionary dict and resolves it if indirect.
func (c *ctx) deref(dict piudf.Value, key string) (piudf.Value, error) {
	v, err := c.codec.DictGet(c.pdf, c.r, dict, key)
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
func pageContent(c *ctx, page piudf.Value) ([]byte, error) {
	contents, err := c.codec.DictGet(c.pdf, c.r, page, "Contents")
	if err != nil {
		return nil, err
	}
	if contents.IsNull() {
		return nil, nil // A page may legally have no content.
	}
	var refs []piudf.ObjectID
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
		err = c.codec.ArrayForEach(c.pdf, c.r, arr, func(v piudf.Value) bool {
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
		rd, err := c.pdf.OpenStream(c.r, v, &strm, c.codec)
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
// A string operand holds codes, not characters, so each is decoded through
// the font the last Tf selected.
func showText(dst []byte, content []byte, fonts map[string]*font) ([]byte, error) {
	var lx piulex.Lexer
	lx.ReuseLiteralBuffer = true
	buf := make([]byte, 4096)
	lx.MaxLiteral = len(buf)
	if err := lx.Reset(bytes.NewReader(content), 0, buf); err != nil {
		return dst, err
	}
	// pending holds the operands seen since the last operator: TJ takes an
	// array of strings, Tf takes the resource name of a font.
	var pending [][]byte
	var lastName string
	var cur *font
	for {
		tok, _, lit := lx.NextToken()
		switch tok {
		case piulex.TokEOF:
			return dst, nil
		case piulex.TokIllegal:
			return dst, lx.Err()
		case piulex.TokName:
			lastName = string(lit)
			continue
		case piulex.TokString, piulex.TokHexString:
			// lit dies at the next token; the operator that consumes it is at
			// least one token away.
			pending = append(pending, append([]byte(nil), lit...))
			continue
		case piulex.TokIdent:
			switch string(lit) {
			case "Tf":
				// Tf's operands are a font resource name and a size. An
				// unknown name leaves cur nil, which decodes as raw bytes.
				cur = fonts[lastName]
			case "Tj", "'", "\"":
				// The operand is the last string: ' and " take other operands
				// before it.
				if n := len(pending); n > 0 {
					dst = cur.decode(dst, pending[n-1])
				}
			case "TJ":
				// TJ's array interleaves strings with kerning numbers, which
				// are not operands of interest. Enough negative kerning means
				// a space, but deciding that needs the font's widths.
				for _, s := range pending {
					dst = cur.decode(dst, s)
				}
			case "ET":
				// One text object per line: the coarsest split that does not
				// require the text matrix.
				dst = append(dst, '\n')
			}
			// Operands belong to the operator that just consumed them. Only
			// an operator ends them: a number between two strings inside a TJ
			// array is not a new statement.
			pending = pending[:0]
		}
	}
}
