package piudf

//go:generate go tool stringer -type=genericErr,Filter,ColorSpace-linecomment -output=stringers.go

type genericErr uint8

const (
	_                     genericErr = iota
	ErrMissingHeader                 // missing %PDF- header
	ErrMissingStartXref              // missing startxref
	ErrMalformedStartXref            // malformed startxref
	ErrIncompleteReadAt              // ReadAt did incomplete read
	ErrOOBXref                       // xref offset out of file bounds
	ErrCodecMemLimit                 // Codec memory limit hit
	ErrCodecDepthLimit               // Codec depth limit hit
	ErrInvalidCodecConfig            // Codec invalid config
	errTODO                          // PDF feature not implemented yet
	errValueMismatch                 // value kind/type mismatch
	errNameTooLong                   // name longer than implementation limit
	errObjectNotFound                // object number outside every xref section
	errObjectIDMismatch              // object header does not match its xref entry
	errNotAStream                    // value is not a stream object
	errStreamBadLength               // stream /Length missing or out of bounds
	errObjStmNested                  // object stream within an object stream
	errObjStmBad                     // object stream /N, /First or offset out of bounds
	errEncoderState                  // Encoder method called out of sequence
	errEncoderBase                   // Encoder base document unusable for append
	errUnsupportedFilter             // stream filter not supported
	errBadPredictor                  // bad /Predictor or predictor row
	errBadImage                      // image dictionary missing or bad entry
	ErrUnsupportedImage              // image encoding or color space not supported
	errImageTooLarge                 // image exceeds ImageDecoder.MaxPixels
	// Lexer errors below.
	_errLexErrorsStart              //
	errXrefStreamBad                // xref stream generic error
	errXrefStreamMissingDict        // xref stream missing dict
	errXrefStreamMissingTok         // xref stream missing 'stream' keyword
	ErrIllegalToken                 // illegal token
	errUnexpectedToken              // unexpected token
	ErrPrevLoop                     // /Prev loop found
	errPrevChainTooLong             // /Prev chain too long
	errExpectedXref                 // expected 'xref' keyword
	errBadSubsectionStart           // bad subsection start/entry
	errExpectingSubsectionOrTrailer // unexpected token looking for subsections or trailer
	errBadXrefStreamObjectHeader    // bad xref stream object header
	errBadObjectHeader              // bad 'N G obj' object header
	errUnexpectedEOF                // unexpected end of file
)

func (g genericErr) Error() string { return g.String() }

// IsLexed signals the error was encountered during lexing, so the [piulex.Lexer]
// has state on where the error was encountered in Pos and Error methods.
func (g genericErr) IsLexed() bool { return g > _errLexErrorsStart }

// Filter is a stream filter, one entry of a stream's /Filter (ISO 32000-1
// 7.4). Knowing a filter is not decoding it: [PDF.StreamPayload] names every
// filter it meets so that a caller can pass a payload through untouched — a
// DCTDecode payload is a JPEG file as it stands — while [PDF.OpenPayload]
// decodes only the ones this package implements.
type Filter uint8

const (
	// FilterUnknown is a name the format does not define, or one this package
	// does not recognize.
	FilterUnknown   Filter = iota // Unknown
	FilterASCIIHex                // ASCIIHexDecode
	FilterASCII85                 // ASCII85Decode
	FilterLZW                     // LZWDecode
	FilterFlate                   // FlateDecode
	FilterRunLength               // RunLengthDecode
	FilterCCITTFax                // CCITTFaxDecode
	FilterJBIG2                   // JBIG2Decode
	FilterDCT                     // DCTDecode
	FilterJPX                     // JPXDecode
	FilterCrypt                   // Crypt
)

// maxFilters bounds a stream's filter chain. Real chains are one or two
// filters deep, an ASCII armor over a compressor; four is generous and keeps
// the chain in a fixed array.
const maxFilters = 4

// filterByName maps a /Filter name to its Filter. The abbreviations are the
// ones an inline image may use (ISO 32000-1 Table 94); they mean the same
// filter anywhere they appear.
func filterByName(name []byte) Filter {
	switch b2s(name) {
	case "ASCIIHexDecode", "AHx":
		return FilterASCIIHex
	case "ASCII85Decode", "A85":
		return FilterASCII85
	case "LZWDecode", "LZW":
		return FilterLZW
	case "FlateDecode", "Fl":
		return FilterFlate
	case "RunLengthDecode", "RL":
		return FilterRunLength
	case "CCITTFaxDecode", "CCF":
		return FilterCCITTFax
	case "JBIG2Decode":
		return FilterJBIG2
	case "DCTDecode", "DCT":
		return FilterDCT
	case "JPXDecode":
		return FilterJPX
	case "Crypt":
		return FilterCrypt
	}
	return FilterUnknown
}

// takesPredictor reports whether f's /DecodeParms may carry a /Predictor.
// Only the two general-purpose compressors do (ISO 32000-1 Table 8).
func (f Filter) takesPredictor() bool { return f == FilterFlate || f == FilterLZW }

// ColorSpace is the family of a color space (ISO 32000-1 8.6): the name of a
// device space, or the first element of a color space array. The family fixes
// how many samples a pixel holds, except where the space's own parameters do
// (ICCBased /N, DeviceN's colorant names).
type ColorSpace uint8

const (
	// ColorSpaceUnknown is an absent color space, one named through the
	// page's /Resources, or a family the format does not define.
	ColorSpaceUnknown    ColorSpace = iota // Unknown
	ColorSpaceDeviceGray                   // DeviceGray
	ColorSpaceDeviceRGB                    // DeviceRGB
	ColorSpaceDeviceCMYK                   // DeviceCMYK
	ColorSpaceCalGray                      // CalGray
	ColorSpaceCalRGB                       // CalRGB
	ColorSpaceLab                          // Lab
	ColorSpaceICCBased                     // ICCBased
	ColorSpaceIndexed                      // Indexed
	ColorSpacePattern                      // Pattern
	ColorSpaceSeparation                   // Separation
	ColorSpaceDeviceN                      // DeviceN
)

// colorSpaceByName maps a color space family name to its ColorSpace. The
// abbreviations are an inline image's (ISO 32000-1 Table 93).
func colorSpaceByName(name []byte) ColorSpace {
	switch b2s(name) {
	case "DeviceGray", "G":
		return ColorSpaceDeviceGray
	case "DeviceRGB", "RGB":
		return ColorSpaceDeviceRGB
	case "DeviceCMYK", "CMYK":
		return ColorSpaceDeviceCMYK
	case "CalGray":
		return ColorSpaceCalGray
	case "CalRGB":
		return ColorSpaceCalRGB
	case "Lab":
		return ColorSpaceLab
	case "ICCBased":
		return ColorSpaceICCBased
	case "Indexed", "I":
		return ColorSpaceIndexed
	case "Pattern":
		return ColorSpacePattern
	case "Separation":
		return ColorSpaceSeparation
	case "DeviceN":
		return ColorSpaceDeviceN
	}
	return ColorSpaceUnknown
}

// components returns the samples per pixel the family fixes, or zero when the
// space's parameters decide.
func (cs ColorSpace) components() int {
	switch cs {
	case ColorSpaceDeviceGray, ColorSpaceCalGray, ColorSpaceIndexed, ColorSpaceSeparation:
		return 1
	case ColorSpaceDeviceRGB, ColorSpaceCalRGB, ColorSpaceLab:
		return 3
	case ColorSpaceDeviceCMYK:
		return 4
	}
	return 0
}
