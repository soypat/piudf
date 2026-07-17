package ppdf

import (
	"math"
	"strconv"

	"github.com/soypat/piudf/piulex"
)

// ObjectID identifies an indirect PDF object by number and generation.
type ObjectID struct {
	Num uint32 // PDF object number (positive integer).
	Gen uint16 // Generation number (usually 0).
}

// IsZero reports whether id is the zero ObjectID, which is not a valid
// object reference in PDF (object numbers start at 1).
func (id ObjectID) IsZero() bool { return id.Num == 0 && id.Gen == 0 }

// AppendString appends the reference in PDF notation, i.e. "12 0 R".
func (id ObjectID) AppendString(dst []byte) []byte {
	dst = strconv.AppendUint(dst, uint64(id.Num), 10)
	dst = append(dst, ' ')
	dst = strconv.AppendUint(dst, uint64(id.Gen), 10)
	dst = append(dst, ' ', 'R')
	return dst
}

type Value struct {
	// I value semantics depends on Tok.
	//  - Real/Integer: I is float64 bits or integer value.
	//  - Name/String/Stream/HexString/DictOpen/ArrayOpen: I is file position offset
	//  - Null/True/False: I is zero. Truthiness in Tok==TokTrue
	//  - Ref: I stores Gen (16bit). N stores Num
	I int64
	// N value semantics depends on Tok. If not specified below is don't-care.
	//  - Name/String/Stream/HexString: Stores span/length of data.
	//  - Ref: Stores Num.
	N   uint32
	Stm uint32 // TODO: can this be collapsed into N with Tok information? The less state there is the easier it is to keep "safe"
	Tok piulex.Token
}

func (v Value) ObjectID() ObjectID {
	if v.Tok != piulex.TokR {
		return ObjectID{}
	}
	return ObjectID{Num: v.N, Gen: uint16(v.I)}
}

// isSpan reports whether v addresses a raw file span.
func (v Value) isSpan() bool {
	switch v.Tok {
	case piulex.TokHexString, piulex.TokString, piulex.TokName, tokArray, tokDict, tokStream:
		return true
	}
	return false
}

// tagObjStm marks span Values parsed inside object stream stm so accessors
// know their coordinates address its decompressed data, not the file.
// Scalars and references pass through untouched; stm 0 (file space) is the
// identity.
func tagObjStm(v Value, stm uint32) Value {
	if stm != 0 && v.isSpan() {
		v.Stm = stm
	}
	return v
}

// Bool returns the boolean contained in v.
func (v Value) Bool() (b bool, ok bool) {
	b = v.Tok == piulex.TokTrue
	return b, b || v.Tok == piulex.TokFalse
}

// Int returns the integer contained in v.
func (v Value) Int() (_ int64, ok bool) {
	return v.I, v.Tok == piulex.TokInt
}

// IsArray returns true if the value references an array with entries.
func (v Value) IsArray() bool {
	return v.Tok == tokArray
}

// IsDict returns true if the value references a dictionary with entries that can be searched by [PDF.GetDict]
func (v Value) IsDict() bool {
	return v.Tok == tokDict
}

// IsNull reports whether v is the PDF null object or an invalid Value.
func (v Value) IsNull() bool { return v.Tok == piulex.TokNull || v.Tok <= piulex.TokEOF }

// IsTruthy returns true if either are true:
//   - value is true token
//   - v.I is nonzero and non-null.
func (v Value) IsTruthy() bool {
	return v.Tok == piulex.TokTrue || (v.Tok != piulex.TokFalse && v.I != 0 && !v.IsNull())
}

// Float returns the numeric value contained in v. PDF permits integers
// wherever a real number is expected so both kinds convert.
func (v Value) Float() (f float64, ok bool) {
	switch v.Tok {
	case piulex.TokReal:
		f = math.Float64frombits(uint64(v.I))
	case piulex.TokInt:
		f = float64(v.I)
	default:
		return math.NaN(), false
	}
	return f, true
}
