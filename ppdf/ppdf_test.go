package ppdf

import (
	"io"
	"os"
	"testing"
)

// countingReaderAt records the reads a Decode issues. Read count is the
// primary regression metric of the window rewrite: correctness alone would
// not notice the buffer being discarded on every jump.
type countingReaderAt struct {
	r     io.ReaderAt
	reads int
	bytes int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	n, err := c.r.ReadAt(p, off)
	c.bytes += int64(n)
	return n, err
}

func openCounted(t *testing.T, name string) (*countingReaderAt, int64) {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return &countingReaderAt{r: f}, fi.Size()
}

func newCodec(buf []byte) *Codec {
	c := &Codec{MaxLazySections: 64, MaxDepth: 32}
	c.SetBuffer(buf)
	return c
}

// TestDecodeClassicReadCount pins the point of the offset-addressed window:
// sto.pdf is incrementally updated, so its xref chain walks many revisions
// whose trailers sit within a few hundred bytes of each other. Before the
// window each jump refilled, costing ~26 reads.
func TestDecodeClassicReadCount(t *testing.T) {
	c, size := openCounted(t, "../testdata/sto.pdf")
	var pdf PDF
	if err := pdf.Decode(c, size, newCodec(make([]byte, 4096))); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(pdf.sections) == 0 {
		t.Error("no xref sections decoded")
	}
	if len(pdf.revs) < 2 {
		t.Errorf("decoded %d revisions, want >1: /Prev chain did not walk", len(pdf.revs))
	}
	t.Logf("%d reads, %d bytes, %d sections, %d revisions",
		c.reads, c.bytes, len(pdf.sections), len(pdf.revs))
	const maxReads = 12
	if c.reads > maxReads {
		t.Errorf("Decode took %d reads, want <=%d", c.reads, maxReads)
	}
}
