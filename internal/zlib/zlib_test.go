package zlib

import (
	"bytes"
	stdzlib "compress/zlib"
	"errors"
	"io"
	"math/rand"
	"testing"
)

// newReader returns a Reader configured with generous buffers for tests.
func newReader(t testing.TB) *Reader {
	t.Helper()
	var z Reader
	if err := z.Configure(Config{
		Window: make([]byte, MaxWindow),
		Links:  make([]uint32, MaxLinks),
		Input:  make([]byte, 4096),
	}); err != nil {
		t.Fatal(err)
	}
	return &z
}

// zlibCompress returns data compressed at the given level.
func zlibCompress(t testing.TB, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := stdzlib.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// skewed returns compressible data with a non-uniform byte distribution, which
// forces the encoder into dynamic-Huffman blocks with a spread of code lengths
// (some longer than 9 bits), the case that needs overflow tables. Random data
// would not: being incompressible, it is emitted as stored blocks.
func skewed(n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	for i := range b {
		x := rng.Float64()
		b[i] = byte(int(x * x * x * 256)) // biased toward small values
		if rng.Intn(6) == 0 {
			b[i] = byte(rng.Intn(256)) // entropy tail -> long codes
		}
	}
	return b
}

// inflateAll decodes the whole stream at src into a fresh buffer.
func inflateAll(z *Reader, src io.Reader) ([]byte, error) {
	if err := z.Reset(src); err != nil {
		return nil, err
	}
	return io.ReadAll(z)
}

func TestRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"empty":        {},
		"one byte":     {0x42},
		"stored small": bytes.Repeat([]byte{0xAB}, 3), // tiny, may store
		"text":         []byte("the quick brown fox jumps over the lazy dog\n"),
	}
	// A run-length friendly input (back references).
	cases["repeated"] = bytes.Repeat([]byte("abcdefgh"), 4096)
	// High-entropy input forces dynamic Huffman with long codes.
	rng := rand.New(rand.NewSource(1))
	rnd := make([]byte, 200000)
	rng.Read(rnd)
	cases["random"] = rnd
	// Mixed: structure plus noise, many blocks.
	mixed := make([]byte, 0, 300000)
	for k := 0; k < 3000; k++ {
		mixed = append(mixed, []byte("header line ")...)
		chunk := make([]byte, 64)
		rng.Read(chunk)
		mixed = append(mixed, chunk...)
	}
	cases["mixed"] = mixed

	z := newReader(t)
	for name, data := range cases {
		for _, level := range []int{stdzlib.NoCompression, stdzlib.BestSpeed, stdzlib.DefaultCompression, stdzlib.BestCompression} {
			comp := zlibCompress(t, data, level)
			got, err := inflateAll(z, bytes.NewReader(comp))
			if err != nil {
				t.Fatalf("%s level %d: %v", name, level, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("%s level %d: %d bytes decoded, want %d", name, level, len(got), len(data))
			}
		}
	}
}

// TestReuseAcrossStreams checks that one configured Reader decodes many streams
// correctly — the reuse the package is built for.
func TestReuseAcrossStreams(t *testing.T) {
	z := newReader(t)
	rng := rand.New(rand.NewSource(2))
	for iter := 0; iter < 50; iter++ {
		data := make([]byte, rng.Intn(50000))
		rng.Read(data)
		comp := zlibCompress(t, data, stdzlib.DefaultCompression)
		got, err := inflateAll(z, bytes.NewReader(comp))
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("iter %d: mismatch", iter)
		}
	}
}

// TestRewindAllocs is the regression this package exists for: re-inflating a
// dynamic-Huffman stream must not allocate once the Reader is warm.
func TestRewindAllocs(t *testing.T) {
	data := skewed(120000, 3) // large enough for several dynamic blocks
	comp := zlibCompress(t, data, stdzlib.BestCompression)
	src := bytes.NewReader(comp)
	z := newReader(t)
	sink := make([]byte, 8192)

	read := func() {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if err := z.Reset(src); err != nil {
			t.Fatal(err)
		}
		for {
			_, err := z.Read(sink)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	read() // warm up

	allocs := testing.AllocsPerRun(20, read)
	if allocs > 0 {
		t.Errorf("re-inflate allocates %.1f times per run, want 0", allocs)
	}
}

func TestShortBuffers(t *testing.T) {
	var z Reader
	if err := z.Configure(Config{Window: make([]byte, MaxWindow-1), Input: make([]byte, MinInput)}); err != ErrShortWindow {
		t.Errorf("short window: got %v, want ErrShortWindow", err)
	}
	if err := z.Configure(Config{Window: make([]byte, MaxWindow), Input: make([]byte, MinInput-1)}); err != ErrShortInput {
		t.Errorf("short input: got %v, want ErrShortInput", err)
	}
}

// TestShortLinks feeds a stream needing overflow tables to a Reader with no link
// buffer and expects the explicit error, not an allocation or a wrong result.
func TestShortLinks(t *testing.T) {
	data := skewed(100000, 4) // dynamic Huffman with long codes -> needs links
	comp := zlibCompress(t, data, stdzlib.BestCompression)

	var z Reader
	if err := z.Configure(Config{
		Window: make([]byte, MaxWindow),
		Links:  nil, // deliberately none
		Input:  make([]byte, 4096),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := inflateAll(&z, bytes.NewReader(comp))
	if !errors.Is(err, ErrShortLinks) {
		t.Fatalf("got %v, want ErrShortLinks", err)
	}
}

func TestNotConfigured(t *testing.T) {
	var z Reader
	if err := z.Reset(bytes.NewReader([]byte{0x78, 0x9c})); err != ErrNotConfigured {
		t.Fatalf("got %v, want ErrNotConfigured", err)
	}
}

func TestBadHeader(t *testing.T) {
	z := newReader(t)
	// CM != 8.
	if err := z.Reset(bytes.NewReader([]byte{0x79, 0x9c})); err != ErrHeader {
		t.Errorf("bad CM: got %v, want ErrHeader", err)
	}
	// FCHECK fails (not a multiple of 31).
	if err := z.Reset(bytes.NewReader([]byte{0x78, 0x9d})); err != ErrHeader {
		t.Errorf("bad FCHECK: got %v, want ErrHeader", err)
	}
}

func TestPresetDict(t *testing.T) {
	z := newReader(t)
	// 0x78 0xBB: CM=8, and a value with FDICT set that is a multiple of 31.
	// 0x78BB = 30907 = 31*997, FDICT bit (0x20 in FLG) set.
	if err := z.Reset(bytes.NewReader([]byte{0x78, 0xBB})); err != ErrPresetDict {
		t.Fatalf("got %v, want ErrPresetDict", err)
	}
}

func TestSkipChecksum(t *testing.T) {
	data := []byte("checksum tolerance test payload, of some length to matter")
	comp := zlibCompress(t, data, stdzlib.DefaultCompression)
	// Corrupt the last adler byte.
	comp[len(comp)-1] ^= 0xFF

	// Default: checksum rejected.
	z := newReader(t)
	if _, err := inflateAll(z, bytes.NewReader(comp)); err != ErrChecksum {
		t.Fatalf("got %v, want ErrChecksum", err)
	}
	// SkipChecksum: decodes anyway.
	var zs Reader
	if err := zs.Configure(Config{
		Window: make([]byte, MaxWindow), Links: make([]uint32, MaxLinks),
		Input: make([]byte, 4096), SkipChecksum: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := inflateAll(&zs, bytes.NewReader(comp))
	if err != nil {
		t.Fatalf("skip checksum: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("skip checksum: wrong data")
	}
}

// TestOracleMalformed feeds random bytes to both the stdlib zlib reader and ours
// and requires them to agree: either both decode to the same bytes or both fail.
func TestOracleMalformed(t *testing.T) {
	z := newReader(t)
	rng := rand.New(rand.NewSource(5))
	for iter := 0; iter < 2000; iter++ {
		n := rng.Intn(64)
		raw := make([]byte, n)
		rng.Read(raw)
		if n >= 2 { // give many a valid-looking header
			raw[0] = 0x78
			raw[1] = 0x9c
		}
		compareWithStdlib(t, z, raw)
	}
}

func compareWithStdlib(t *testing.T, z *Reader, comp []byte) {
	t.Helper()
	stdOut, stdErr := stdlibInflate(comp)
	ourOut, ourErr := inflateAll(z, bytes.NewReader(comp))
	if (stdErr == nil) != (ourErr == nil) {
		t.Fatalf("disagreement on error: std=%v ours=%v input=%x", stdErr, ourErr, comp)
	}
	if stdErr == nil && !bytes.Equal(stdOut, ourOut) {
		t.Fatalf("output mismatch: std=%x ours=%x input=%x", stdOut, ourOut, comp)
	}
}

func stdlibInflate(comp []byte) ([]byte, error) {
	r, err := stdzlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("hello world"))
	f.Add(bytes.Repeat([]byte("ab"), 5000))
	z := newReader(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		comp := zlibCompress(t, data, stdzlib.DefaultCompression)
		got, err := inflateAll(z, bytes.NewReader(comp))
		if err != nil {
			t.Fatalf("inflate: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("round-trip mismatch: %d vs %d bytes", len(got), len(data))
		}
	})
}

func FuzzOracle(f *testing.F) {
	f.Add([]byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})
	z := newReader(f)
	f.Fuzz(func(t *testing.T, comp []byte) {
		compareWithStdlib(t, z, comp)
	})
}
