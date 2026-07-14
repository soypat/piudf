# piudf

Lazy, memory-constrained PDF decoder for Go, driven entirely by `io.ReaderAt`.

piudf records file offsets instead of contents and parses indirect objects on
demand. It works identically over an in-memory buffer (`bytes.Reader`) or a
file of arbitrary size (`os.File`): memory use is independent of document
size.

## Design

**Values are file coordinates.** A `Value` is either an inline scalar
(int, real, bool, null, reference) or the `{offset, length}` of its raw text
in the file (strings, names, dictionaries, arrays, streams). Composite and
text access re-lexes the span on demand — there is no object graph, no arena
and no cache. A Value never goes stale: it stays usable for as long as a
reader over the identical file bytes exists, across any number of other
operations or documents.

Two types split the work:

- **`PDF`** — the lazy index of one document: cross-reference section
  descriptors and trailer metadata, nothing else. Memory is O(number of
  xref subsections) — ~32 B each, typically fewer than ten — regardless of
  object count. A PDF references neither the file nor the Decoder.
- **`Decoder`** — the reusable machine (lexer + token pushback, ~5 KB
  bounded). Operates on PDF structs without retaining them; its memory
  stays constant no matter how many documents it processes.

The `io.ReaderAt` is never stored. Every operation that touches the file
takes it explicitly: decode, close the file, keep the PDF struct and any
Values, then reopen the same bytes later and keep reading.

- **The cross-reference table is not materialized.** Classic xref tables are
  already random-access arrays of fixed 20-byte records on disk, so the
  decoder stores only one descriptor per xref subsection and reads single
  records with `ReadAt` on lookup.
- **Resolution is shallow.** `Resolve` parses scalars inline and returns
  composites as raw spans; `DictGet`/`ArrayIndex` scan the raw text of one
  span per call (~µs). Cost lives at access time, not decode time.
- **Soft degradation, never a crash.** Operations that would exceed
  `DecodeLimits` fail with `ErrMemoryLimit` while everything else keeps
  working. With `Grow` false nothing allocates after `Decode`.
- **Compressed payloads are never decoded.** `RawStream` exposes the raw
  bytes and the declared `/Filter`; decoding is the caller's decision.
  (A `Filter` interface for the structural minimum — FlateDecode for xref
  and object streams — is the next stage.)

## Usage

```go
f, _ := os.Open("doc.pdf")
st, _ := f.Stat()

var d piudf.Decoder // reusable machine
var p piudf.PDF     // per-document lazy index
err := d.Decode(&p, f, st.Size(), piudf.DecodeLimits{
	MaxLiteral: 4096, MaxParseDepth: 16, MaxXrefSections: 16,
	Grow: false, // hard bound; use DefaultDecodeLimits() to grow freely
})
if err != nil { /* ... */ }
// Neither p nor any Value references f: close and reopen the same bytes
// whenever you like.

catalog, _ := d.Resolve(&p, f, p.Root())
pages, _ := d.DictGet(f, catalog, "Pages")   // re-lexes the dict span
pagesDict, _ := d.Resolve(&p, f, pages.Ref)
kids, _ := d.DictGet(f, pagesDict, "Kids")
first, _ := d.ArrayIndex(f, kids, 0)
// Values are plain file coordinates: store them, they never go stale.

sv, _ := d.Resolve(&p, f, first.Ref)
r, info, _ := d.RawStream(&p, f, sv) // *io.SectionReader over raw payload
_ = d.NameIs(f, info.Filter, "FlateDecode")
```

`Decode` parses only the trailer and cross-reference chain; no object
contents are touched. Resolving an object costs one xref record `ReadAt`
plus a shallow lex of that object — zero heap allocations once buffers are
warm, including `DictGet` and `NameIs`.

## Status / roadmap

Supported now: classic cross-reference tables, incremental updates
(`/Prev` chains, newest revision shadows), all object kinds, stream objects
with raw payload access, exact-offset syntax errors (`Pos.ToLineCol` for
diagnostics).

Not yet (returns `ErrUnsupported`): cross-reference streams and object
streams (PDF 1.5+), filters/decompression, encryption, page-tree navigation
(`NumPages`/`Page(i)`), linearized fast path.

## Development

```sh
go generate ./...   # stringer for Kind/Token
go test -race ./...
```

The lexer follows a byte-oriented lookahead design with a perfect-hash
keyword table; see `tokens.go` for the hash search notes.
