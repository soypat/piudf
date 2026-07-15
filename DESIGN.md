# piudf

Lazy, memory-constrained PDF decoder for Go, driven entirely by `io.ReaderAt`.

piudf records file offsets instead of contents and parses indirect objects on
demand. It works identically over an in-memory buffer (`bytes.Reader`) or a
file of arbitrary size (`os.File`): memory use is independent of document
size. Both classic cross-reference tables and PDF 1.5+ cross-reference
streams with compressed object streams are supported.

## Two decode models

| | `PDF` (lazy) | `PDFEager` |
|---|---|---|
| `Decode` cost | O(1) in document size (~4 µs: header, xref, trailer)¹ | O(document structure): every object deep-parsed once |
| Memory | O(#xref subsections), ~independent of object count¹ | ~32 B per dict pair / array element + name pool |
| Navigation | re-lexes the value's span per access (~µs) | table lookups, no lexer, no reader (~ns) |
| Bulk data | `{offset, size}` spans, read on demand | same |
| Value semantics | file coordinates: valid for any reader over the same bytes | handles into this PDFEager's tables |

¹ For PDF 1.5+ files using cross-reference *streams*, compressed data has
no random access, so the lazy index necessarily keeps the decoded xref
records and a cache holding one decompressed object stream at a time, both
capped by `DecodeLimits.MaxDecompress`. The 7.9 MB `rp2350-datasheet.pdf`
corpus (15 888 objects, 90% inside object streams) opens lazily in ~540 µs
holding 131 KB; a classic-table file of any size still opens in ~4 µs
holding ~160 B.

Rule of thumb: touching a small fraction of a potentially huge document →
lazy `PDF`. Walking most of a normal document, repeatedly → `DecodeEager`,
which converts the whole structure to machine representation up front so
that navigation afterwards is lexer-free and reader-free (measured on the
benchmark corpus: full-document object sweep 140 µs lazy vs 0.2 µs eager;
decode 3.8 µs lazy vs 314 µs eager).

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
  descriptors and trailer metadata. For classic-table files memory is
  O(number of xref subsections) — ~32 B each, typically fewer than ten —
  regardless of object count; PDF 1.5 files add the decoded xref records
  and the object-stream cache (see below). A PDF references neither the
  file nor the Decoder.
- **`Decoder`** — the reusable machine (lexer + token pushback, ~5 KB
  bounded). Operates on PDF structs without retaining them; its memory
  stays constant no matter how many documents it processes.

The `io.ReaderAt` is never stored. Every operation that touches the file
takes it explicitly: decode, close the file, keep the PDF struct and any
Values, then reopen the same bytes later and keep reading.

- **The cross-reference table is not materialized when the file permits.**
  Classic xref tables are already random-access arrays of fixed 20-byte
  records on disk, so the decoder stores only one descriptor per xref
  subsection and reads single records with `ReadAt` on lookup. PDF 1.5
  cross-reference streams are FlateDecode-compressed, which forbids random
  access; their decoded records (a few bytes per object) are kept in the
  PDF, capped by `MaxDecompress`.
- **Objects inside object streams (PDF 1.5+) resolve transparently.** The
  containing stream is inflated into a one-entry cache on first touch;
  Values parsed there are tagged with their stream (`Value.Ref`) and the
  accessors read them from the cache, re-inflating only when a different
  stream is touched. Sequential access to one stream's objects costs one
  decompression (~12 µs per cached resolve vs ~230 µs cache-miss on the
  datasheet corpus).
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
pages, _ := d.DictGet(&p, f, catalog, "Pages") // re-lexes the dict span
pagesDict, _ := d.Resolve(&p, f, pages.Ref)
kids, _ := d.DictGet(&p, f, pagesDict, "Kids")
first, _ := d.ArrayIndex(&p, f, kids, 0)
// Values are plain file coordinates: store them, they never go stale.

sv, _ := d.Resolve(&p, f, first.Ref)
r, info, _ := d.RawStream(&p, f, sv) // *io.SectionReader over raw payload
_ = d.NameIs(&p, f, info.Filter, "FlateDecode")
```

`Decode` parses only the trailer and cross-reference chain; no object
contents are touched. Resolving an object costs one xref record `ReadAt`
plus a shallow lex of that object — zero heap allocations once buffers are
warm, including `DictGet` and `NameIs`.

### Eager usage

```go
var d piudf.Decoder
var pe piudf.PDFEager
err := d.DecodeEager(&pe, f, st.Size(), piudf.DecodeLimits{})
// Structure is machine representation now: no reader, no lexing.
catalog, _ := pe.Resolve(pe.Root())
pages, _ := pe.DictGet(catalog, "Pages")     // entry-pool scan
pagesDict, _ := pe.ResolveRef(pages, 1)      // O(1) table lookup
kids, _ := pe.DictGet(pagesDict, "Kids")
n, _ := pe.ArrayLen(kids)                    // O(1)
// r only returns for bulk data:
sv, _ := pe.Resolve(someStream)
sr, info, _ := pe.RawStream(f, sv)
title, _ := pe.AppendString(nil, f, titleVal)
```

Objects that exceed `MaxEntries` or fail to parse are dropped individually
(`Stats().Dropped`, `ErrObjectNotFound` on access) while the rest of the
document decodes; only structural failures (header, xref chain, trailer)
fail `DecodeEager` itself.

## CLI: binary-level PDF explorer

`cmd/piudf` explores a file's layout through the lazy decoder — nothing is
materialized, and everything printed carries exact file coordinates
(`@offset+len`, or `@objstmN:offset+len` inside a compressed object
stream). In the spirit of `mutool show`, `qpdf --show-*`, `pdf-parser.py`
and `readelf`.

```sh
go install github.com/soypat/piudf/cmd/piudf@latest
piudf <command> <file.pdf> [args] [flags]
```

| command | shows |
|---|---|
| `info` | version, xref form, revisions, object counts by kind |
| `trailer`, `obj <n>` | annotated value trees |
| `ls` | every object: kind + location (file offset or objstm) |
| `xref` | subsections and records, shadowing marked |
| `map` | byte-range map of the whole file, gaps included |
| `revisions` | incremental-update history, oldest first |
| `stream <n>`, `raw`, `lex <off>` | payload info/hexdumps, token stream |
| `walk /Root/Pages/Kids/0` | path navigation with per-hop coordinates |
| `mem` | index memory footprint |

```
$ piudf walk doc.pdf /Root/Pages/Kids/0
trailer @0x77a26c+187
/Root -> ref
  = 2 0 R dict @0x12 @0x1a+178
/Pages -> ref
  = 1 0 R dict objstm 15815[0] @objstm15815:0x76b+11072
/Kids -> array @objstm15815:0x77d+11041
/0 -> ref
  = 3 0 R dict objstm 15815[2] @objstm15815:0x3431+270
dict[9] @objstm15815:0x3431+270
  /Type /Page
  ...
```

The introspection API behind it (`PDF.Lookup`, `XrefSection`, `Revision`,
`Decoder.ParseValueAt`, `DictLen`/`DictIndex`) is exported in `explore.go`
for building similar tools.

## Status / roadmap

Supported now: classic cross-reference tables and PDF 1.5+ cross-reference
streams (FlateDecode, PNG predictors), objects inside compressed object
streams in both decode models, incremental updates (`/Prev` chains, newest
revision shadows, classic and stream sections mixing freely), all object
kinds, stream objects with raw payload access, exact-offset syntax errors
(`Pos.ToLineCol` for diagnostics).

Not yet (returns `ErrUnsupported`): payload filters/decompression beyond
the structural internals (RawStream stays raw by design), TIFF predictor 2,
encryption, hybrid-reference files' `/XRefStm` fallback, page-tree
navigation (`NumPages`/`Page(i)`), linearized fast path.

## Development

```sh
go generate ./...   # stringer for Kind/Token
go test -race ./...
```

The lexer follows a byte-oriented lookahead design with a perfect-hash
keyword table; see `tokens.go` for the hash search notes.
