package piudf

import "errors"

// NameRef is a handle to a name interned in the Decoder's name arena.
// The zero NameRef is invalid.
type NameRef uint32

// nameArena interns PDF names in a single flat byte buffer. Each unique name
// is stored once; a NameRef packs the entry index. Deduplication uses an
// open-addressing hash table of entry indices so lookups allocate nothing.
type nameArena struct {
	buf     []byte      // all name bytes, concatenated.
	entries []nameEntry // entry 0 reserved (zero NameRef invalid).
	index   []uint32    // open addressing buckets: entry index+0, 0 means empty.
	limit   int         // max bytes in buf; <=0 means unlimited.
	grow    bool        // allow exceeding limit by growing.
}

type nameEntry struct {
	off uint32
	len uint32
}

var errNameArenaFull = errors.New("piudf: name arena full")

func (na *nameArena) reset(limitBytes int, grow bool) {
	na.limit = limitBytes
	na.grow = grow
	if cap(na.buf) == 0 && limitBytes > 0 {
		na.buf = make([]byte, 0, limitBytes)
	}
	na.buf = na.buf[:0]
	// Average PDF name ~8 bytes; size entry table and buckets accordingly.
	nEntries := limitBytes/8 + 2
	if cap(na.entries) < nEntries {
		na.entries = make([]nameEntry, 1, nEntries)
	} else {
		na.entries = na.entries[:1]
	}
	nBuckets := 1
	for nBuckets < 2*nEntries {
		nBuckets <<= 1
	}
	if cap(na.index) < nBuckets {
		na.index = make([]uint32, nBuckets)
	} else {
		na.index = na.index[:nBuckets]
		clear(na.index)
	}
}

// fnv1a hashes b without allocating.
func fnv1a(b []byte) uint32 {
	const offset32, prime32 = 2166136261, 16777619
	h := uint32(offset32)
	for _, c := range b {
		h = (h ^ uint32(c)) * prime32
	}
	return h
}

// intern returns the NameRef for name, adding it to the arena if new.
// Returns errNameArenaFull wrapped in ErrMemoryLimit context when the arena
// cannot hold the name and growth is disabled.
func (na *nameArena) intern(name []byte) (NameRef, error) {
	mask := uint32(len(na.index) - 1)
	h := fnv1a(name) & mask
	for {
		ei := na.index[h]
		if ei == 0 {
			break // Empty bucket: name not present.
		}
		e := na.entries[ei]
		if string(na.buf[e.off:e.off+e.len]) == string(name) {
			return NameRef(ei), nil
		}
		h = (h + 1) & mask
	}
	// Insert new entry.
	if !na.grow {
		if na.limit > 0 && len(na.buf)+len(name) > na.limit {
			return 0, errNameArenaFull
		}
		if len(na.entries) >= cap(na.entries) || 2*len(na.entries) >= len(na.index) {
			return 0, errNameArenaFull
		}
	} else if 2*(len(na.entries)+1) >= len(na.index) {
		na.rehash()
		mask = uint32(len(na.index) - 1)
		h = fnv1a(name) & mask
		for na.index[h] != 0 {
			h = (h + 1) & mask
		}
	}
	off := len(na.buf)
	na.buf = append(na.buf, name...)
	na.entries = append(na.entries, nameEntry{off: uint32(off), len: uint32(len(name))})
	ei := uint32(len(na.entries) - 1)
	na.index[h] = ei
	return NameRef(ei), nil
}

func (na *nameArena) rehash() {
	nBuckets := 2 * len(na.index)
	if cap(na.index) >= nBuckets {
		na.index = na.index[:nBuckets]
		clear(na.index)
	} else {
		na.index = make([]uint32, nBuckets)
	}
	mask := uint32(nBuckets - 1)
	for ei := 1; ei < len(na.entries); ei++ {
		e := na.entries[ei]
		h := fnv1a(na.buf[e.off:e.off+e.len]) & mask
		for na.index[h] != 0 {
			h = (h + 1) & mask
		}
		na.index[h] = uint32(ei)
	}
}

// bytes returns the interned bytes for ref, or nil if ref is invalid.
func (na *nameArena) bytes(ref NameRef) []byte {
	if ref == 0 || int(ref) >= len(na.entries) {
		return nil
	}
	e := na.entries[ref]
	return na.buf[e.off : e.off+e.len]
}

// is reports whether ref refers to name s, without allocating.
func (na *nameArena) is(ref NameRef, s string) bool {
	return string(na.bytes(ref)) == s && ref != 0
}

// lookup returns the NameRef for s if already interned, else 0.
func (na *nameArena) lookup(s string) NameRef {
	if len(na.index) == 0 {
		return 0
	}
	mask := uint32(len(na.index) - 1)
	h := fnv1aString(s) & mask
	for {
		ei := na.index[h]
		if ei == 0 {
			return 0
		}
		e := na.entries[ei]
		if string(na.buf[e.off:e.off+e.len]) == s {
			return NameRef(ei)
		}
		h = (h + 1) & mask
	}
}

func fnv1aString(s string) uint32 {
	const offset32, prime32 = 2166136261, 16777619
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * prime32
	}
	return h
}
