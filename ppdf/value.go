package ppdf

import (
	"strconv"

	"github.com/soypat/piudf/ppdf/piulex"
)

// ObjectID identifies an indirect PDF object by number and generation.
type ObjectID struct {
	Num uint32 // PDF object number (positive integer).
	Gen uint16 // Generation number (usually 0).
}

// IsZero reports whether id is the zero ObjectID, which is not a valid
// object reference in PDF (object numbers start at 1).
func (id ObjectID) IsZero() bool { return id.Num == 0 && id.Gen == 0 }

// String returns the reference in PDF notation, i.e. "12 0 R".
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
	Tok piulex.Token
}

func (v Value) ObjectID() ObjectID {
	if v.Tok != piulex.TokR {
		panic("invalid use of ObjectID")
	}
	return ObjectID{Num: v.N, Gen: uint16(v.I)}
}
