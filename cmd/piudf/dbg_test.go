package main

import (
	"os"
	"testing"

	"github.com/soypat/piudf"
)

func openCtx(t *testing.T, path string) *ctx {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	st, _ := f.Stat()
	codec := new(piudf.Codec)
	if err := codec.Configure(piudf.DecoderConfig{
		Buffer: make([]byte, 8192), MaxLazySections: 4096, MaxDepth: 64,
	}); err != nil {
		t.Fatal(err)
	}
	c := &ctx{pdf: new(piudf.PDF), codec: codec, r: f, size: st.Size(), path: path}
	c.decodeErr = c.pdf.Decode(c.r, c.size, c.codec)
	return c
}

func TestShowDbg(t *testing.T) {
	c := openCtx(t, "../../testdata/basis-of-embedded.pdf")
	page, err := findPage(c, 1)
	if err != nil {
		t.Fatal(err)
	}
	fonts, err := pageFonts(c, page)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for n, f := range fonts {
		names = append(names, n)
		_ = f
	}
	t.Logf("fonts: %v", names)
	content, err := pageContent(c, page)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("content %d B, head: %q", len(content), content[:min(120, len(content))])
	buf, err := showText(nil, content, fonts)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("text: %q", string(buf[:min(300, len(buf))]))
}
