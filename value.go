package piudf

import (
	"errors"
	"math"
	"strconv"
)

//go:generate go tool stringer -linecomment -output stringers.go -type=Kind,Token

// ObjectID identifies an indirect PDF object by number and generation.
type ObjectID struct {
	Num uint32 // PDF object number (positive integer).
	Gen uint16 // Generation number (usually 0).
}

// IsZero reports whether id is the zero ObjectID, which is not a valid
// object reference in PDF (object numbers start at 1).
func (id ObjectID) IsZero() bool { return id.Num == 0 && id.Gen == 0 }

// String returns the reference in PDF notation, i.e. "12 0 R".
func (id ObjectID) String() string {
	return strconv.FormatUint(uint64(id.Num), 10) + " " + strconv.FormatUint(uint64(id.Gen), 10) + " R"
}

// Kind discriminates the type of data contained in a Value.
type Kind uint8

const (
	KindInvalid   Kind = iota // invalid
	KindNull                  // null
	KindBool                  // bool
	KindInt                   // int
	KindReal                  // real
	KindString                // string
	KindHexString             // hexstring
	KindName                  // name
	KindArray                 // array
	KindDict                  // dict
	KindStream                // stream
	KindRef                   // ref
)

// Value is a flat tagged union representing any PDF object. It contains no
// pointers: composite values (arrays, dictionaries) reference spans of a
// Decoder-owned arena and string payloads reference file offsets, read on
// demand. A Value is only meaningful together with the Decoder that produced
// it and only until the next call to Resolve on that Decoder.
type Value struct {
	// I holds the integer value (KindInt), bool value 0/1 (KindBool),
	// float bits (KindReal), absolute file offset of the raw payload
	// (KindString, KindHexString), or arena span start (KindArray,
	// KindDict, KindStream).
	I int64
	// N holds the payload byte length (strings) or span element count
	// (arrays, dicts).
	N    uint32
	Ref  ObjectID // Referenced object for KindRef; owning stream dict info for KindStream unused in core.
	Name NameRef  // Interned name for KindName.
	Kind Kind
}

var (
	errKindMismatch = errors.New("piudf: value kind mismatch")
	errNotComposite = errors.New("piudf: value is not array or dict")
)

// Bool returns the boolean contained in v.
func (v Value) Bool() (bool, error) {
	if v.Kind != KindBool {
		return false, errKindMismatch
	}
	return v.I != 0, nil
}

// Int returns the integer contained in v.
func (v Value) Int() (int64, error) {
	if v.Kind != KindInt {
		return 0, errKindMismatch
	}
	return v.I, nil
}

// Float returns the numeric value contained in v. PDF permits integers
// wherever a real number is expected so both kinds convert.
func (v Value) Float() (float64, error) {
	switch v.Kind {
	case KindReal:
		return math.Float64frombits(uint64(v.I)), nil
	case KindInt:
		return float64(v.I), nil
	}
	return 0, errKindMismatch
}

// IsRef reports whether v is an indirect object reference.
func (v Value) IsRef() bool { return v.Kind == KindRef }

// IsNull reports whether v is the PDF null object or an invalid Value.
func (v Value) IsNull() bool { return v.Kind == KindNull || v.Kind == KindInvalid }

// Len returns the number of elements for arrays, the number of key-value
// pairs for dicts and stream dicts, and the payload byte length for strings.
func (v Value) Len() int {
	switch v.Kind {
	case KindArray, KindDict, KindStream, KindString, KindHexString:
		return int(v.N)
	}
	return 0
}

// span returns the arena span of a composite value.
func (v Value) span() (start, n int, err error) {
	switch v.Kind {
	case KindArray, KindDict, KindStream:
		return int(v.I), int(v.N), nil
	}
	return 0, 0, errNotComposite
}
