package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/soypat/piudf"
)

func stdout() io.Writer { return os.Stdout }

// maxObjNum returns one past the highest object number any cross-reference
// subsection covers.
func maxObjNum(c *ctx) uint32 {
	var max uint32
	for i := 0; i < c.p.NumXrefSections(); i++ {
		s := c.p.XrefSection(i)
		if end := s.First + s.Count; end > max {
			max = end
		}
	}
	return max
}

// objectEnd lexes the object starting at off and returns the offset just
// past its terminating endobj; stream payloads are skipped via /Length.
func objectEnd(c *ctx, id piudf.ObjectID, off int64) (int64, error) {
	var lx piudf.Lexer
	lx.ReuseLiteralBuffer = true
	if err := lx.Reset(c.r, off); err != nil {
		return 0, err
	}
	for {
		tok, pos, _ := lx.NextToken()
		switch tok {
		case piudf.TokStream:
			// Jump the payload; its extent comes from the library.
			v, err := c.d.Resolve(c.p, c.r, id)
			if err != nil {
				return int64(pos), err
			}
			_, info, err := c.d.RawStream(c.p, c.r, v)
			if err != nil {
				return int64(pos), err
			}
			if err := lx.Reset(c.r, info.Offset+info.Length); err != nil {
				return int64(pos), err
			}
		case piudf.TokEndobj:
			return int64(lx.Pos()), nil
		case piudf.TokEOF, piudf.TokIllegal:
			return int64(pos), lx.Err()
		}
	}
}

// location renders where an object lives.
func location(e piudf.XrefEntry) string {
	switch e.Kind {
	case piudf.XrefNormal:
		return fmt.Sprintf("@%#x", e.Offset)
	case piudf.XrefCompressed:
		return fmt.Sprintf("objstm %d[%d]", e.Stream, e.Index)
	}
	return "free"
}

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
	forms := ""
	for i := 0; i < c.p.NumRevisions(); i++ {
		if i > 0 {
			forms += ", "
		}
		if c.p.Revision(i).Classic {
			forms += "classic table"
		} else {
			forms += "xref stream"
		}
	}
	fmt.Fprintf(w, "revisions:  %d (%s), %d xref subsections\n", c.p.NumRevisions(), forms, c.p.NumXrefSections())

	tr, err := c.d.Trailer(c.p, c.r)
	if err != nil {
		return err
	}
	if sz, err := c.d.DictGet(c.p, c.r, tr, "Size"); err == nil {
		if n, err := sz.Int(); err == nil {
			fmt.Fprintf(w, "size:       %d objects declared\n", n)
		}
	}
	fmt.Fprintf(w, "root:       %v\n", c.p.Root())
	if !c.p.Info().IsZero() {
		fmt.Fprintf(w, "info:       %v\n", c.p.Info())
	}
	if enc, err := c.d.DictGet(c.p, c.r, tr, "Encrypt"); err == nil && !enc.IsNull() {
		fmt.Fprintf(w, "encrypted:  yes (/Encrypt present; decryption unsupported)\n")
	}

	// Sweep: count by xref record type and resolved value kind.
	kinds := map[piudf.Kind]int{}
	objstms := map[uint32]bool{}
	var free, normal, compressed, failed int
	max := maxObjNum(c)
	for num := uint32(1); num < max; num++ {
		e, err := c.p.Lookup(c.r, num)
		if err != nil {
			continue
		}
		switch e.Kind {
		case piudf.XrefFree:
			free++
			continue
		case piudf.XrefNormal:
			normal++
		case piudf.XrefCompressed:
			compressed++
			objstms[e.Stream] = true
		}
		v, err := c.d.Resolve(c.p, c.r, e.ID)
		if err != nil {
			failed++
			continue
		}
		kinds[v.Kind]++
	}
	fmt.Fprintf(w, "objects:    %d plain, %d in %d object streams, %d free", normal, compressed, len(objstms), free)
	if failed > 0 {
		fmt.Fprintf(w, ", %d unreadable", failed)
	}
	fmt.Fprintln(w)
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k.String())
	}
	sort.Strings(names)
	fmt.Fprintf(w, "kinds:     ")
	for _, name := range names {
		for k, n := range kinds {
			if k.String() == name {
				fmt.Fprintf(w, " %s:%d", name, n)
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "index RAM:  %d bytes (lazy PDF, after sweep)\n", c.p.SizeOnRAM())
	return nil
}

func cmdTrailer(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	tr, err := c.d.Trailer(c.p, c.r)
	if err != nil {
		return err
	}
	return newPrinter(c, 8).printValue(tr)
}

func cmdObj(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	fs := flag.NewFlagSet("obj", flag.ExitOnError)
	depth := fs.Int("depth", 8, "max composite nesting to expand")
	num, gen, rest, err := parseObjArg(args)
	if err != nil {
		return err
	}
	fs.Parse(rest)
	id := piudf.ObjectID{Num: num, Gen: gen}
	e, err := c.p.Lookup(c.r, num)
	if err != nil {
		return err
	}
	fmt.Printf("%v  %s\n", id, location(e))
	v, err := c.d.Resolve(c.p, c.r, id)
	if err != nil {
		return err
	}
	return newPrinter(c, *depth).printValue(v)
}

// parseObjArg reads "<num> [gen]" from the front of args.
func parseObjArg(args []string) (num uint32, gen uint16, rest []string, err error) {
	if len(args) == 0 {
		return 0, 0, nil, errors.New("missing object number")
	}
	n, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("bad object number %q", args[0])
	}
	rest = args[1:]
	if len(rest) > 0 {
		if g, err := strconv.ParseUint(rest[0], 10, 16); err == nil {
			return uint32(n), uint16(g), rest[1:], nil
		}
	}
	return uint32(n), 0, rest, nil
}

func cmdLs(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	kindFilter := fs.String("k", "", "only objects of this kind (dict, stream, array, ...)")
	max := fs.Int("max", 0, "stop after n objects (0: all)")
	fs.Parse(args)
	w := stdout()
	fmt.Fprintf(w, "%7s %3s  %-9s  %-22s %s\n", "NUM", "GEN", "KIND", "LOCATION", "SPAN")
	printed := 0
	maxNum := maxObjNum(c)
	for num := uint32(1); num < maxNum; num++ {
		e, err := c.p.Lookup(c.r, num)
		if err != nil || e.Kind == piudf.XrefFree {
			continue
		}
		v, err := c.d.Resolve(c.p, c.r, e.ID)
		kind := "error"
		spanStr := ""
		if err == nil {
			kind = v.Kind.String()
			spanStr = span(v)
		}
		if *kindFilter != "" && kind != *kindFilter {
			continue
		}
		fmt.Fprintf(w, "%7d %3d  %-9s  %-22s %s\n", e.ID.Num, e.ID.Gen, kind, location(e), spanStr)
		printed++
		if *max > 0 && printed >= *max {
			break
		}
	}
	return nil
}

func cmdXref(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	w := stdout()
	// owner[i] answers "which subsection wins for this object number":
	// the first section containing it, in newest-first order.
	owner := func(num uint32) int {
		for i := 0; i < c.p.NumXrefSections(); i++ {
			s := c.p.XrefSection(i)
			if num >= s.First && num < s.First+s.Count {
				return i
			}
		}
		return -1
	}
	for ri := 0; ri < c.p.NumRevisions(); ri++ {
		rev := c.p.Revision(ri)
		form := "xref stream"
		if rev.Classic {
			form = "classic table"
		}
		fmt.Fprintf(w, "revision %d (%s) xref @%#x trailer @%#x\n", ri, form, rev.XrefOff, rev.TrailerOff)
		for si := rev.FirstSection; si < rev.EndSection; si++ {
			s := c.p.XrefSection(si)
			if s.Classic {
				fmt.Fprintf(w, "  subsection [%d, %d) records @%#x\n", s.First, s.First+s.Count, s.FileOff)
			} else {
				fmt.Fprintf(w, "  subsection [%d, %d) decoded stream rows\n", s.First, s.First+s.Count)
			}
			for num := s.First; num < s.First+s.Count; num++ {
				if own := owner(num); own != si {
					fmt.Fprintf(w, "    %7d shadowed by newer subsection %d\n", num, own)
					continue
				}
				e, err := c.p.Lookup(c.r, num)
				if err != nil {
					fmt.Fprintf(w, "    %7d error: %v\n", num, err)
					continue
				}
				switch e.Kind {
				case piudf.XrefNormal:
					fmt.Fprintf(w, "    %7d gen %-5d normal     @%#x\n", num, e.ID.Gen, e.Offset)
				case piudf.XrefCompressed:
					fmt.Fprintf(w, "    %7d gen %-5d compressed objstm %d[%d]\n", num, e.ID.Gen, e.Stream, e.Index)
				default:
					fmt.Fprintf(w, "    %7d gen %-5d free\n", num, e.ID.Gen)
				}
			}
		}
	}
	return nil
}

func cmdMap(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	type frange struct {
		start, end int64
		label      string
	}
	var ranges []frange
	add := func(start, end int64, label string) {
		if end > start {
			ranges = append(ranges, frange{start, end, label})
		}
	}

	// Header line.
	var head [64]byte
	n, _ := c.r.ReadAt(head[:], 0)
	if i := strings.IndexByte(string(head[:n]), '\n'); i >= 0 {
		add(0, int64(i+1), "%PDF header")
	}
	// Cross-reference stream objects double as revisions: label them.
	xrefStreamAt := map[int64]int{}
	for ri := 0; ri < c.p.NumRevisions(); ri++ {
		rev := c.p.Revision(ri)
		if rev.Classic {
			add(rev.XrefOff, rev.TrailerOff, fmt.Sprintf("xref table (revision %d)", ri))
			if tr, err := c.d.ParseValueAt(c.r, rev.TrailerOff); err == nil && tr.Kind == piudf.KindDict {
				add(tr.I, tr.I+int64(tr.N), fmt.Sprintf("trailer (revision %d)", ri))
			}
		} else {
			xrefStreamAt[rev.XrefOff] = ri
		}
	}
	// Every plain object.
	maxNum := maxObjNum(c)
	for num := uint32(1); num < maxNum; num++ {
		e, err := c.p.Lookup(c.r, num)
		if err != nil || e.Kind != piudf.XrefNormal {
			continue
		}
		end, err := objectEnd(c, e.ID, e.Offset)
		if err != nil && end <= e.Offset {
			continue
		}
		label := fmt.Sprintf("obj %d %d", e.ID.Num, e.ID.Gen)
		if ri, ok := xrefStreamAt[e.Offset]; ok {
			label += fmt.Sprintf(" (cross-reference stream, revision %d)", ri)
		}
		add(e.Offset, end, label)
	}

	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	w := stdout()
	pos := int64(0)
	for _, fr := range ranges {
		if fr.start > pos {
			fmt.Fprintf(w, "%#10x..%#-10x %8d  -- gap --\n", pos, fr.start, fr.start-pos)
		} else if fr.start < pos {
			fmt.Fprintf(w, "%42s (overlaps previous by %d)\n", "", pos-fr.start)
		}
		fmt.Fprintf(w, "%#10x..%#-10x %8d  %s\n", fr.start, fr.end, fr.end-fr.start, fr.label)
		if fr.end > pos {
			pos = fr.end
		}
	}
	if pos < c.size {
		fmt.Fprintf(w, "%#10x..%#-10x %8d  file tail (startxref, %%%%EOF)\n", pos, c.size, c.size-pos)
	}
	return nil
}

func cmdRevisions(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	w := stdout()
	// Oldest first: the story of the file as it was written.
	for i := c.p.NumRevisions() - 1; i >= 0; i-- {
		rev := c.p.Revision(i)
		age := c.p.NumRevisions() - 1 - i
		form := "cross-reference stream"
		if rev.Classic {
			form = "classic xref table"
		}
		fmt.Fprintf(w, "revision %d: %s @%#x\n", age, form, rev.XrefOff)
		var nobjs uint32
		for si := rev.FirstSection; si < rev.EndSection; si++ {
			s := c.p.XrefSection(si)
			fmt.Fprintf(w, "  defines objects [%d, %d)\n", s.First, s.First+s.Count)
			nobjs += s.Count
		}
		tr, err := c.d.ParseValueAt(c.r, rev.TrailerOff)
		if err == nil && tr.Kind == piudf.KindDict {
			fmt.Fprintf(w, "  trailer %s:", span(tr))
			for _, key := range [...]string{"Size", "Root", "Info", "Prev"} {
				if v, err := c.d.DictGet(c.p, c.r, tr, key); err == nil && !v.IsNull() {
					switch v.Kind {
					case piudf.KindInt:
						n, _ := v.Int()
						fmt.Fprintf(w, " /%s %d", key, n)
					case piudf.KindRef:
						fmt.Fprintf(w, " /%s %v", key, v.Ref)
					}
				}
			}
			fmt.Fprintln(w)
		}
	}
	return nil
}

func cmdStream(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	nBytes := fs.Int("n", 256, "hexdump at most n payload bytes")
	out := fs.String("o", "", "write full raw payload to file instead")
	num, gen, rest, err := parseObjArg(args)
	if err != nil {
		return err
	}
	fs.Parse(rest)
	v, err := c.d.Resolve(c.p, c.r, piudf.ObjectID{Num: num, Gen: gen})
	if err != nil {
		return err
	}
	sr, info, err := c.d.RawStream(c.p, c.r, v)
	if err != nil {
		return err
	}
	fmt.Printf("stream %d %d: dict %s, payload @%#x+%d", num, gen, span(v), info.Offset, info.Length)
	if info.Filter.Kind == piudf.KindName {
		if fb, err := c.d.AppendString(nil, c.p, c.r, info.Filter); err == nil {
			fmt.Printf(" filter /%s", fb)
		}
	}
	fmt.Println()
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := io.Copy(f, sr)
		fmt.Printf("wrote %d raw bytes to %s\n", n, *out)
		return err
	}
	buf := make([]byte, min64(int64(*nBytes), info.Length))
	if _, err := io.ReadFull(sr, buf); err != nil {
		return err
	}
	hexdump(stdout(), buf, info.Offset)
	if int64(len(buf)) < info.Length {
		fmt.Printf("... %d more bytes (use -n or -o)\n", info.Length-int64(len(buf)))
	}
	return nil
}

func cmdRaw(c *ctx, args []string) error {
	var off, length int64
	switch len(args) {
	case 1: // Object span.
		if err := c.needDecode(); err != nil {
			return err
		}
		num, _, _, err := parseObjArg(args)
		if err != nil {
			return err
		}
		e, err := c.p.Lookup(c.r, num)
		if err != nil {
			return err
		}
		if e.Kind != piudf.XrefNormal {
			return fmt.Errorf("object %d is %s; raw spans exist only for plain objects", num, e.Kind)
		}
		end, err := objectEnd(c, e.ID, e.Offset)
		if err != nil {
			return err
		}
		off, length = e.Offset, end-e.Offset
	case 2: // Arbitrary span.
		var err error
		if off, err = strconv.ParseInt(args[0], 0, 64); err != nil {
			return fmt.Errorf("bad offset %q", args[0])
		}
		if length, err = strconv.ParseInt(args[1], 0, 64); err != nil {
			return fmt.Errorf("bad length %q", args[1])
		}
	default:
		return errors.New("usage: raw <num> | raw <off> <len>")
	}
	if length > 1<<20 {
		return fmt.Errorf("span of %d bytes; refusing to dump more than 1 MB", length)
	}
	if off < 0 || off+length > c.size {
		return fmt.Errorf("span [%#x, +%d) outside file of %d bytes", off, length, c.size)
	}
	buf := make([]byte, length)
	if _, err := c.r.ReadAt(buf, off); err != nil {
		return err
	}
	hexdump(stdout(), buf, off)
	return nil
}

func cmdLex(c *ctx, args []string) error {
	fs := flag.NewFlagSet("lex", flag.ExitOnError)
	nTok := fs.Int("n", 40, "max tokens to print")
	if len(args) == 0 {
		return errors.New("missing offset")
	}
	off, err := strconv.ParseInt(args[0], 0, 64)
	if err != nil {
		return fmt.Errorf("bad offset %q", args[0])
	}
	fs.Parse(args[1:])
	var lx piudf.Lexer
	lx.ReuseLiteralBuffer = true
	if err := lx.Reset(c.r, off); err != nil {
		return err
	}
	w := stdout()
	for i := 0; i < *nTok; i++ {
		tok, pos, lit := lx.NextToken()
		fmt.Fprintf(w, "%#8x  %-12v %q\n", int64(pos), tok, lit)
		if tok == piudf.TokEOF || tok == piudf.TokIllegal {
			if err := lx.Err(); err != nil {
				fmt.Fprintf(w, "lexer stopped: %v\n", err)
			}
			break
		}
	}
	return nil
}

func cmdWalk(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("missing path, e.g. /Root/Pages/Kids/0")
	}
	v, err := c.d.Trailer(c.p, c.r)
	if err != nil {
		return err
	}
	w := stdout()
	fmt.Fprintf(w, "trailer %s\n", span(v))
	for _, seg := range strings.Split(strings.Trim(args[0], "/"), "/") {
		if seg == "" {
			continue
		}
		// A reference between hops resolves implicitly.
		if v.Kind == piudf.KindRef {
			id := v.Ref
			if v, err = c.d.Resolve(c.p, c.r, id); err != nil {
				return err
			}
			e, _ := c.p.Lookup(c.r, id.Num)
			fmt.Fprintf(w, "  = %v %s %s %s\n", id, v.Kind, location(e), span(v))
		}
		switch {
		case v.Kind == piudf.KindArray:
			i, err := strconv.Atoi(seg)
			if err != nil {
				return fmt.Errorf("array reached; %q is not an index", seg)
			}
			if v, err = c.d.ArrayIndex(c.p, c.r, v, i); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s\n", strings.TrimRight(fmt.Sprintf("/%s -> %s %s", seg, v.Kind, span(v)), " "))
		case v.Kind == piudf.KindDict || v.Kind == piudf.KindStream:
			if v, err = c.d.DictGet(c.p, c.r, v, seg); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s\n", strings.TrimRight(fmt.Sprintf("/%s -> %s %s", seg, v.Kind, span(v)), " "))
		default:
			return fmt.Errorf("cannot descend into %s with %q", v.Kind, seg)
		}
	}
	if v.Kind == piudf.KindRef {
		id := v.Ref
		if v, err = c.d.Resolve(c.p, c.r, id); err != nil {
			return err
		}
		e, _ := c.p.Lookup(c.r, id.Num)
		fmt.Fprintf(w, "  = %v %s %s %s\n", id, v.Kind, location(e), span(v))
	}
	return newPrinter(c, 3).printValue(v)
}

func cmdMem(c *ctx, args []string) error {
	if err := c.needDecode(); err != nil {
		return err
	}
	fs := flag.NewFlagSet("mem", flag.ExitOnError)
	sweep := fs.Bool("sweep", false, "resolve every object first (fills xref + object stream caches)")
	fs.Parse(args)
	w := stdout()
	fmt.Fprintf(w, "after Decode:  PDF %d bytes on RAM (file: %d bytes, %.4f%%)\n",
		c.p.SizeOnRAM(), c.size, 100*float64(c.p.SizeOnRAM())/float64(c.size))
	if *sweep {
		maxNum := maxObjNum(c)
		for num := uint32(1); num < maxNum; num++ {
			if e, err := c.p.Lookup(c.r, num); err == nil && e.Kind != piudf.XrefFree {
				c.d.Resolve(c.p, c.r, e.ID)
			}
		}
		fmt.Fprintf(w, "after sweep:   PDF %d bytes on RAM (object-stream cache warm)\n", c.p.SizeOnRAM())
	}
	st := c.p.Stats()
	fmt.Fprintf(w, "stats:         %d dropped\n", st.Dropped)
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
