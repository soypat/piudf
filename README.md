# piudf

Lazy, memory-constrained PDF decoder for Go, driven entirely by `io.ReaderAt`.

piudf records file offsets instead of contents and parses indirect objects on
demand. It works identically over an in-memory buffer (`bytes.Reader`) or a
file of arbitrary size (`os.File`): memory use is bounded by `Limits` you set
at decode time, independent of document size.

## Design

Two types split the work:

- **`PDF`** — the lazily decoded state of one document. Owns all
  per-document memory (xref sections, name arena, value arena); slice
  capacity bounds allocation, length is usage, element types hold no
  pointers. A PDF references neither the file nor the Decoder: decode,
  close the file, keep the struct, and hand a reader over the identical
  bytes back to lazy methods later.
- **`Decoder`** — the reusable machine (lexer, parse scratch, ~5 KB
  bounded). Decodes into PDF structs without retaining them; its memory
  stays constant no matter how many documents it processes.

The `io.ReaderAt` is never stored. Methods that need the file take it
explicitly — the signature tells you whether an operation touches the file
(`d.Resolve(p, r, id)`) or only already-decoded state (`p.DictGet(v, name)`).

- **`io.ReaderAt` is the only data abstraction.** The decoder never buffers
  the document. Derived readers (raw stream payloads via `io.SectionReader`,
  later: decoded streams, concatenated page content) nest over the same
  interface.
- **The cross-reference table is not materialized.** Classic xref tables are
  already random-access arrays of fixed 20-byte records on disk, so the
  decoder stores only one ~32 B descriptor per xref subsection and reads
  single records with `ReadAt` on lookup. A 10-object file and a 10-million
  object file cost the same memory.
- **Pure lazy resolution.** `Resolve` re-parses the object on every call into
  a preallocated arena that is recycled per call. Values are flat tagged
  unions (no pointers, no interfaces); composites are spans into the arena,
  strings are file-offset spans read on demand, names are interned once into
  a flat byte arena.
- **Soft degradation, never a crash.** With `Limits.Grow` disabled the
  decoder allocates nothing after `Decode`. An operation that would exceed a
  limit fails with `ErrMemoryLimit` and the decoder remains usable —
  scalars still resolve after a huge dictionary was refused.
- **Compressed payloads are never decoded.** `RawStream` exposes the raw
  bytes and the declared `/Filter`; decoding is the caller's decision.
  (A `Filter` interface for the structural minimum — FlateDecode for xref and
  object streams — is the next stage.)

## Usage

```go
f, _ := os.Open("doc.pdf")
st, _ := f.Stat()

var d piudf.Decoder // reusable machine
var p piudf.PDF     // document state, user-owned memory
err := d.Decode(&p, f, st.Size(), piudf.DecodeLimits{
	ValueArena: 512, NameArena: 2048, MaxLiteral: 4096, MaxParseDepth: 16,
	Grow: false, // hard memory bound; use DefaultDecodeLimits() to grow freely
})
if err != nil { /* ... */ }
// p holds no reference to f: you may close f here and reopen the same
// bytes later for the lazy calls below.

catalog, err := d.Resolve(&p, f, p.Root())
pages, err := p.DictGet(catalog, "Pages") // no file needed
pagesDict, err := d.Resolve(&p, f, pages.Ref)
// Values are valid until the next Resolve/Trailer/Decode into that PDF.

sv, _ := d.Resolve(&p, f, contentsID)
r, info, _ := d.RawStream(&p, f, sv) // *io.SectionReader over raw payload + filter name
```

`Decode` parses only the trailer and cross-reference chain; no object
contents are touched. Resolving an object costs one xref record `ReadAt`
plus lexing that object — and zero heap allocations once buffers are warm.

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
