// Command memcompare decodes one PDF through both decode models and prints
// how much memory each document structure holds, compared to the file size.
//
// Usage:
//
//	go run ./examples/memcompare [file.pdf]
//
// Defaults to the repository's testdata corpus when no file is given.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	piudf "github.com/soypat/piudf/internal/piulegacy"
)

func main() {
	flag.Parse()
	path := flag.Arg(0)
	if path == "" {
		path = "testdata/basis-of-embedded.pdf"
	}
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		log.Fatal(err)
	}

	var d piudf.Decoder
	var p piudf.PDF
	lazytimer := timer()
	if err := d.Decode(&p, ReaderAtPrinter{f}, st.Size(), piudf.DecodeLimits{}); err != nil {
		log.Fatal("lazy Decode: ", err)
	}
	lazyElapsed := lazytimer()
	var pe piudf.PDFEager
	eagerTimer := timer()
	if err := d.DecodeEager(&pe, f, st.Size(), piudf.DecodeLimits{}); err != nil {
		log.Fatal("DecodeEager: ", err)
	}
	eagerElapsed := eagerTimer()
	if dropped := pe.Stats().Dropped; dropped != 0 {
		fmt.Printf("warning: eager decode dropped %d objects\n", dropped)
	}

	fileSize := st.Size()
	lazy := p.SizeOnRAM()
	eager := pe.SizeOnRAM()
	fmt.Printf("file:             %s  (%d bytes)\n", path, fileSize)
	fmt.Printf("PDF (lazy):       %8d bytes  %7.4f%% of file in %s\n", lazy, 100*float64(lazy)/float64(fileSize), lazyElapsed.Round(time.Microsecond))
	fmt.Printf("PDFEager:         %8d bytes  %7.4f%% of file in %s\n", eager, 100*float64(eager)/float64(fileSize), eagerElapsed.Round(time.Microsecond))
	fmt.Printf("eager/lazy ratio: %8.1fx\n", float64(eager)/float64(lazy))
}

func timer() func() time.Duration {
	now := time.Now()
	return func() time.Duration { return time.Since(now) }
}

type ReaderAtPrinter struct {
	r io.ReaderAt
}

func (r ReaderAtPrinter) ReadAt(p []byte, off int64) (n int, err error) {
	n, err = r.r.ReadAt(p, off)
	if n > 0 {
		fmt.Printf("read %d..%d (%d)\n", off, off+int64(n), n)
	}
	return n, err
}
