package internal

// SliceReuse prepares a slice for reuse with capacity at least n.
// After calling SliceReuse, the slice will have:
//   - length = 0
//   - capacity >= n (exactly n if a new allocation was needed)
//
// This function provides specified behavior unlike [slices.Grow] which
// has unspecified capacity growth behavior that differs between Go and TinyGo.
// Use this when the exact capacity matters for subsequent logic.
func SliceReuse[T any](buf *[]T, n int) {
	if cap(*buf) < n {
		*buf = make([]T, 0, n)
	} else {
		*buf = (*buf)[:0]
	}
}

// SliceReclaim extends the slice length by one and returns a pointer to
// the new last element. The returned element is not zeroed, so callers
// can reuse any existing allocations it may hold from prior use.
//
// Beware: as the name implies SliceReclaim reuses previously held memory
// so it is up to caller to ensure the reclaimed data is coherent by zeroing out
// the needed fields and reusing the previously held slices or pointers responsibly.
func SliceReclaim[T any](ptr *[]T) *T {
	b := *ptr
	n := len(b)
	if n == cap(b) {
		var z T
		*ptr = append(b, z)
	} else {
		*ptr = b[:n+1]
	}
	return &(*ptr)[n]
}
