# piudf

Memory constrained PDF toolbox. Processing a RP2350 datasheet of ~1300 pages incurs 260kB RAM usage in 70 microseconds.


```
go test -bench=. .
goos: linux
goarch: amd64
pkg: github.com/soypat/piudf
cpu: 12th Gen Intel(R) Core(TM) i5-12400F
BenchmarkDecodeInit-12                    876114              1367 ns/op              0 B/op           0 allocs/op
BenchmarkLexObjects-12                     14582             82361 ns/op        187.45 MB/s           2990 tokens/op           0 B/op          0 allocs/op
BenchmarkResolveObjects-12                 10000            108066 ns/op        142.87 MB/s            0 B/op          0 allocs/op
BenchmarkDecodeInitXrefStream-12          133195              9232 ns/op               0 B/op          0 allocs/op
BenchmarkResolveCompressedHit-12            8545            128588 ns/op               0 B/op          0 allocs/op
BenchmarkResolveCompressedMiss-12           1052           1144718 ns/op               0 B/op          0 allocs/op
BenchmarkResolveCatalog-12                670806              1793 ns/op               0 B/op          0 allocs/op
PASS
ok      github.com/soypat/piudf 8.314s
```