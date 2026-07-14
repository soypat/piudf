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

// Value represents any PDF object as either an inline scalar or a raw file
// span. It is a set of file coordinates: producing a Value reads no object
// data beyond its extent, and a Value never goes stale — it stays usable
// for as long as a reader over the identical file bytes is available.
// Composite and text Values are accessed by re-lexing their span on demand
// via the Decoder methods (DictGet, ArrayIndex, AppendString, ...).
type Value struct {
	// I holds the scalar payload for KindInt (value), KindBool (0/1) and
	// KindReal (float bits). For span kinds (KindString, KindHexString,
	// KindName, KindArray, KindDict, KindStream) it is the absolute file
	// offset of the first token byte, opening delimiter included:
	// '(', '<', '/', '[' or '<<'. KindStream spans the stream dictionary.
	I int64
	// N is the raw byte length of the full token span, closing delimiter
	// included. Zero for scalar kinds.
	N uint32
	// Ref is the referenced object for KindRef. For span kinds parsed
	// inside a compressed object stream (PDF 1.5+), Ref.Num instead
	// identifies that object stream: I and N then address its
	// decompressed data, not the file. Zero otherwise.
	Ref  ObjectID
	Kind Kind
}

var errKindMismatch = errors.New("piudf: value kind mismatch")

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

// isSpan reports whether v addresses a raw file span.
func (v Value) isSpan() bool {
	switch v.Kind {
	case KindString, KindHexString, KindName, KindArray, KindDict, KindStream:
		return true
	}
	return false
}
