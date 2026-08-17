package piupage

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/soypat/piudf"
)

var (
	errImageDims      = errors.New("piupage: image has no extent")
	errImageBits      = errors.New("piupage: bits per component is not 1, 2, 4, 8 or 16")
	errImageNoSamples = errors.New("piupage: image carries no samples")
	errImageSpace     = errors.New("piupage: image has no colour space")

	errJPEGSOI         = errors.New("piupage: not a JPEG: no start of image")
	errJPEGTruncated   = errors.New("piupage: JPEG ends inside a segment")
	errJPEGNoFrame     = errors.New("piupage: JPEG carries no frame header")
	errJPEGProgressive = errors.New("piupage: JPEG is progressive and DCTDecode reads baseline; re-save it as baseline")
	errJPEGComponents  = errors.New("piupage: JPEG frame is not 1, 3 or 4 components")
	errJPEGNoEOI       = errors.New("piupage: JPEG never ends: no end of image marker, so the data is truncated")

	errPNGSignature  = errors.New("piupage: not a PNG: bad signature")
	errPNGTruncated  = errors.New("piupage: PNG ends inside a chunk")
	errPNGNoHeader   = errors.New("piupage: PNG does not open with IHDR")
	errPNGNoData     = errors.New("piupage: PNG carries no IDAT")
	errPNGMethod     = errors.New("piupage: PNG uses a compression or filter method PDF cannot read")
	errPNGInterlaced = errors.New("piupage: PNG is interlaced and PDF has no Adam7; decode it first")
	errPNGAlpha      = errors.New("piupage: PNG carries alpha, which belongs in a separate /SMask image")
	errPNGPalette    = errors.New("piupage: PNG is paletted and needs an /Indexed colour space")
	errPNGDepth      = errors.New("piupage: PNG bit depth is not one its colour type allows")
)

// ColorSpace is how an image's samples are to be read. It is written as the
// PDF name it is called after.
type ColorSpace uint8

const (
	DeviceGray ColorSpace = iota + 1
	DeviceRGB
	DeviceCMYK
)

// Comps is how many components one sample carries.
func (cs ColorSpace) Comps() int {
	switch cs {
	case DeviceGray:
		return 1
	case DeviceRGB:
		return 3
	case DeviceCMYK:
		return 4
	}
	return 0
}

func (cs ColorSpace) name() string {
	switch cs {
	case DeviceGray:
		return "DeviceGray"
	case DeviceRGB:
		return "DeviceRGB"
	case DeviceCMYK:
		return "DeviceCMYK"
	}
	return ""
}

// Image is a raster and what a PDF image dictionary must say to read it.
//
// Data is the stream body exactly as it will be written: this type compresses
// nothing and converts nothing. That is not a shortcut but the point of it --
// PDF's two image filters are the two formats a caller already has. A JPEG's
// bytes are a /DCTDecode stream, and a PNG's IDAT is a /FlateDecode stream
// under PNG's own row predictors, so [JPEG] and [PNG] hand the file's own
// bytes straight to the document and no pixel is ever decoded.
type Image struct {
	// W and H are the raster's size in samples.
	W, H int
	// Bits is the bits per component, 8 for anything a camera or a screen
	// produced.
	Bits int
	// Space is how a sample's components are read.
	Space ColorSpace
	// Filter is the PDF filter Data is encoded under: "DCTDecode" for JPEG,
	// "FlateDecode" for PNG, and "" for raw samples.
	Filter string
	// Predictor is the /DecodeParms predictor Data was encoded with, 15 for
	// PNG and 0 for everything else.
	Predictor int
	// Decode is an optional /Decode array, which inverts or remaps components
	// as they are read. Adobe's CMYK JPEGs need one; nothing else here does.
	Decode []float64
	// Data is the stream body, untouched.
	Data []byte
}

// Valid reports what is wrong with im, or nil.
func (im Image) Valid() error {
	switch {
	case im.W <= 0 || im.H <= 0:
		return errImageDims
	case im.Space.Comps() == 0:
		return errImageSpace
	case im.Bits != 1 && im.Bits != 2 && im.Bits != 4 && im.Bits != 8 && im.Bits != 16:
		return errImageBits
	case len(im.Data) == 0:
		return errImageNoSamples
	}
	return nil
}

// SameAs reports whether im and b are one image, and so one resource that a
// document writes once however many pages draw it. An Image holds slices and
// cannot be compared with ==; what settles it is that the samples are the same
// samples, which is the identity a caller reusing one Image has.
func (im Image) SameAs(b Image) bool {
	return im.W == b.W && im.H == b.H && im.Bits == b.Bits && im.Space == b.Space &&
		im.Filter == b.Filter && im.Predictor == b.Predictor &&
		len(im.Data) == len(b.Data) &&
		(len(im.Data) == 0 || &im.Data[0] == &b.Data[0])
}

// WriteImage emits im as an image XObject and returns its id. A page reaches
// it by naming it in its /Resources /XObject and invoking it with Do.
func WriteImage(enc *piudf.Encoder, im Image) (piudf.ObjectID, error) {
	if err := im.Valid(); err != nil {
		enc.Fail(err)
		return piudf.ObjectID{}, err
	}
	id := enc.NewID()
	enc.BeginObject(id)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("XObject")
	enc.Name("Subtype")
	enc.Name("Image")
	enc.Name("Width")
	enc.Int(int64(im.W))
	enc.Name("Height")
	enc.Int(int64(im.H))
	enc.Name("ColorSpace")
	enc.Name(im.Space.name())
	enc.Name("BitsPerComponent")
	enc.Int(int64(im.Bits))
	if im.Filter != "" {
		enc.Name("Filter")
		enc.Name(im.Filter)
	}
	if im.Predictor > 1 {
		// The predictor is PNG's own: the filter byte that opens each row is
		// read by the decoder rather than by us, which is what lets an IDAT
		// through untouched.
		enc.Name("DecodeParms")
		enc.DictOpen()
		enc.Name("Predictor")
		enc.Int(int64(im.Predictor))
		enc.Name("Colors")
		enc.Int(int64(im.Space.Comps()))
		enc.Name("BitsPerComponent")
		enc.Int(int64(im.Bits))
		enc.Name("Columns")
		enc.Int(int64(im.W))
		enc.DictClose()
	}
	if len(im.Decode) > 0 {
		enc.Name("Decode")
		enc.ArrayOpen()
		for _, v := range im.Decode {
			enc.Real(v)
		}
		enc.ArrayClose()
	}
	// The payload is in hand, so /Length is a direct integer and the stream
	// needs no second object to carry a length discovered late.
	enc.Name("Length")
	enc.Int(int64(len(im.Data)))
	enc.DictClose()
	enc.BeginStreamPayload()
	if _, err := enc.PayloadWriter().Write(im.Data); err != nil {
		return id, err
	}
	enc.EndStreamPayload()
	enc.EndObject()
	return id, enc.Err()
}

// JPEG describes b as a /DCTDecode image without decoding it. The returned
// Image's Data aliases b up to its end-of-image marker, and no pixel of it is
// ever read.
//
// Only the frame header is parsed, for the size and component count a PDF
// image dictionary has to state and JPEG keeps to itself. That header sits
// near the front of a file, so finding it proves nothing about the entropy
// data behind it; the end-of-image marker is what says the whole picture is
// here, and a stream handed on without it would reach a reader as a torn image
// rather than as an error.
func JPEG(b []byte) (Image, error) {
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return Image{}, errJPEGSOI
	}
	// Trailing bytes past the marker are common enough -- a camera's own
	// appendix, a second thumbnail -- and are no part of the image.
	eoi := bytes.LastIndex(b, []byte{0xff, 0xd9})
	if eoi < 0 {
		return Image{}, errJPEGNoEOI
	}
	b = b[:eoi+2]
	for i := 2; i+1 < len(b); {
		if b[i] != 0xff {
			// Fill bytes and entropy-coded data between segments; the next
			// marker is at the next 0xff that is not one.
			i++
			continue
		}
		marker := b[i+1]
		i += 2
		switch {
		case marker == 0xff:
			// A run of fill bytes: back up onto the one just consumed.
			i--
			continue
		case marker == 0x01 || (marker >= 0xd0 && marker <= 0xd9):
			// Markers that stand alone and carry no segment.
			continue
		}
		if i+2 > len(b) {
			return Image{}, errJPEGTruncated
		}
		size := int(binary.BigEndian.Uint16(b[i:]))
		if size < 2 || i+size > len(b) {
			return Image{}, errJPEGTruncated
		}
		seg := b[i+2 : i+size]

		switch marker {
		case 0xc2, 0xc6, 0xca, 0xce:
			return Image{}, errJPEGProgressive
		case 0xc0, 0xc1, 0xc3, 0xc5, 0xc7, 0xc9, 0xcb, 0xcd, 0xcf:
			// A frame header: precision, height, width, component count. The
			// markers left out of this list are 0xc4 DHT, 0xc8 JPG and 0xcc
			// DAC, which are not frames at all.
			if len(seg) < 6 {
				return Image{}, errJPEGTruncated
			}
			im := Image{
				Bits:   int(seg[0]),
				H:      int(binary.BigEndian.Uint16(seg[1:])),
				W:      int(binary.BigEndian.Uint16(seg[3:])),
				Filter: "DCTDecode",
				Data:   b,
			}
			switch seg[5] {
			case 1:
				im.Space = DeviceGray
			case 3:
				im.Space = DeviceRGB
			case 4:
				im.Space = DeviceCMYK
				if adobeJPEG(b) {
					// Adobe writes CMYK inverted and says so nowhere but by
					// being Adobe. Without this the image comes out a negative.
					im.Decode = []float64{1, 0, 1, 0, 1, 0, 1, 0}
				}
			default:
				return Image{}, errJPEGComponents
			}
			return im, im.Valid()
		}
		i += size
	}
	return Image{}, errJPEGNoFrame
}

// adobeJPEG reports whether b carries an APP14 Adobe segment.
func adobeJPEG(b []byte) bool {
	for i := 2; i+4 < len(b); {
		if b[i] != 0xff {
			i++
			continue
		}
		marker := b[i+1]
		i += 2
		if marker == 0xff {
			i--
			continue
		}
		if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd9) {
			continue
		}
		if i+2 > len(b) {
			return false
		}
		size := int(binary.BigEndian.Uint16(b[i:]))
		if size < 2 || i+size > len(b) {
			return false
		}
		if marker == 0xee && size >= 7 && string(b[i+2:i+7]) == "Adobe" {
			return true
		}
		i += size
	}
	return false
}

const pngMagic = "\x89PNG\r\n\x1a\n"

// PNG describes b as a /FlateDecode image under PNG's own row predictors,
// without decoding it: PDF's Predictor 15 is precisely PNG's filtering, so the
// IDAT payload is already a PDF image stream.
//
// The returned Image's Data aliases b's single IDAT where there is only one,
// and is a fresh slice of their contents where the encoder split them.
//
// What cannot be passed through says so: a paletted, interlaced or
// alpha-carrying PNG has to be decoded and re-encoded by the caller, since
// none of the three is anything PDF's filters can read as it stands.
func PNG(b []byte) (Image, error) {
	if len(b) < len(pngMagic)+25 || string(b[:len(pngMagic)]) != pngMagic {
		return Image{}, errPNGSignature
	}
	var im Image
	var idat []byte
	var parts [][]byte
	seen := false
	for i := len(pngMagic); i+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[i:]))
		kind := string(b[i+4 : i+8])
		// Length, type, payload and a four byte CRC that is the file's own
		// business rather than ours.
		if size < 0 || i+12+size > len(b) {
			return Image{}, errPNGTruncated
		}
		body := b[i+8 : i+8+size]
		i += 12 + size

		switch kind {
		case "IHDR":
			if seen || size < 13 {
				return Image{}, errPNGNoHeader
			}
			seen = true
			depth, colorType := int(body[8]), body[9]
			if body[10] != 0 || body[11] != 0 {
				return Image{}, errPNGMethod
			}
			if body[12] != 0 {
				return Image{}, errPNGInterlaced
			}
			switch colorType {
			case 0:
				if depth != 1 && depth != 2 && depth != 4 && depth != 8 && depth != 16 {
					return Image{}, errPNGDepth
				}
				im.Space = DeviceGray
			case 2:
				if depth != 8 && depth != 16 {
					return Image{}, errPNGDepth
				}
				im.Space = DeviceRGB
			case 3:
				return Image{}, errPNGPalette
			case 4, 6:
				return Image{}, errPNGAlpha
			default:
				return Image{}, errPNGNoHeader
			}
			im.W = int(binary.BigEndian.Uint32(body[0:]))
			im.H = int(binary.BigEndian.Uint32(body[4:]))
			im.Bits = depth
			im.Filter = "FlateDecode"
			im.Predictor = 15
		case "IDAT":
			if !seen {
				return Image{}, errPNGNoHeader
			}
			switch {
			case idat == nil && parts == nil:
				idat = body
			case parts == nil:
				// A second one: the payload has to be joined after all.
				parts = [][]byte{idat, body}
			default:
				parts = append(parts, body)
			}
		case "IEND":
			i = len(b)
		}
	}
	switch {
	case !seen:
		return Image{}, errPNGNoHeader
	case parts != nil:
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		joined := make([]byte, 0, n)
		for _, p := range parts {
			joined = append(joined, p...)
		}
		im.Data = joined
	case idat != nil:
		im.Data = idat
	default:
		return Image{}, errPNGNoData
	}
	return im, im.Valid()
}
