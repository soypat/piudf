package piudf

import (
	"io"

	"github.com/soypat/piudf/piulex"
)

// ImageInfo describes an image XObject (ISO 32000-1 8.9.5): its geometry,
// its sample layout and where its samples are. Nothing of the samples is read.
//
// An image is drawn by name from a page's /Resources /XObject, and every page
// that draws it names the same object: an ImageInfo describes the object, once,
// however many pages paint it.
type ImageInfo struct {
	Width, Height int
	// BitsPerComponent is 1, 2, 4, 8 or 16. A JPXDecode image may leave it
	// out, deferring to its codestream, in which case it is zero.
	BitsPerComponent int
	// ColorSpace is the family of /ColorSpace and Components the samples per
	// pixel it implies. Components is zero when it cannot be told without
	// more than the image dictionary: a space named through the page's
	// resources, or a JPXDecode image deferring to its codestream.
	ColorSpace ColorSpace
	Components int
	// ColorSpaceValue is /ColorSpace as written, a name or an array, for what
	// the family leaves out: an Indexed lookup table, an ICC profile.
	ColorSpaceValue Value
	// ImageMask marks a stencil mask: one bit per pixel saying where to paint
	// the current fill color. It has no color space of its own.
	ImageMask bool
	// SMask is the soft mask image giving per-pixel alpha, zero when absent.
	SMask ObjectID
	// Mask is /Mask: a stencil mask stream reference or a color key array of
	// sample ranges to leave unpainted. Null when absent.
	Mask Value
	// Decode is /Decode, the sample-to-color range mapping; null for the
	// color space's default.
	Decode Value
	// Payload locates the encoded samples. A payload whose only filter is
	// FilterDCT or FilterJPX is a JPEG or JPEG 2000 file as it stands.
	Payload StreamPayload
}

// ImageInfo reads the image XObject dictionary of stream v. The dictionary is
// scanned once; resolving its indirect entries and an ICCBased profile's /N
// cost one object resolve each.
func (pdf *PDF) ImageInfo(r io.ReaderAt, v Value, codec *Codec) (ImageInfo, error) {
	info, err := pdf.imageInfo(r, v, codec)
	if err != nil {
		return ImageInfo{}, pdf.setError(codec, err)
	}
	return info, nil
}

func (pdf *PDF) imageInfo(r io.ReaderAt, v Value, codec *Codec) (info ImageInfo, err error) {
	if v.Tok != tokStream {
		return info, errNotAStream
	}
	null := Value{Tok: piulex.TokNull}
	subtype, width, height, bpc, cs, mask, smask, decode, imask := null, null, null, null, null, null, null, null, null
	err = codec.DictForEach(pdf, r, v, func(key []byte, val Value) bool {
		switch b2s(key) {
		case "Subtype":
			subtype = val
		case "Width":
			width = val
		case "Height":
			height = val
		case "BitsPerComponent":
			bpc = val
		case "ColorSpace":
			cs = val
		case "Mask":
			mask = val
		case "SMask":
			smask = val
		case "Decode":
			decode = val
		case "ImageMask":
			imask = val
		}
		return true
	})
	if err != nil {
		return info, err
	}
	// Everything the scan found may be indirect, and resolving moves the
	// lexer, so it waits until the scan is done. Mask and SMask stay as they
	// are: a stream is always indirect, and the reference is what callers want.
	for _, p := range [...]*Value{&subtype, &width, &height, &bpc, &cs, &decode, &imask} {
		if *p, err = pdf.Deref(r, *p, codec); err != nil {
			return info, err
		}
	}
	if !subtype.IsNull() {
		// /Subtype is required, but a stream drawn as an image and lacking it
		// is still one; only a different subtype is wrong.
		if ok, err := codec.NameIs(pdf, r, subtype, "Image"); err != nil {
			return info, err
		} else if !ok {
			return info, errBadImage
		}
	}
	if info.Payload, err = pdf.streamPayload(r, v, codec); err != nil {
		return info, err
	}
	w, okw := width.Int()
	h, okh := height.Int()
	if !okw || !okh || w <= 0 || h <= 0 || w > 1<<24 || h > 1<<24 {
		return info, errBadImage
	}
	info.Width, info.Height = int(w), int(h)
	info.ImageMask, _ = imask.Bool()
	info.Mask, info.Decode, info.SMask = mask, decode, smask.ObjectID()
	jpx := info.Payload.NumFilters() > 0 && info.Payload.Filter(info.Payload.NumFilters()-1) == FilterJPX

	switch n, ok := bpc.Int(); {
	case info.ImageMask:
		// A stencil mask is one bit, whatever it says (ISO 32000-1 Table 89).
		info.BitsPerComponent, info.Components = 1, 1
		return info, nil
	case !ok && jpx && bpc.IsNull():
		// The codestream says.
	case ok && (n == 1 || n == 2 || n == 4 || n == 8 || n == 16):
		info.BitsPerComponent = int(n)
	default:
		return info, errBadImage
	}

	info.ColorSpaceValue = cs
	info.ColorSpace, info.Components, err = pdf.colorSpace(r, cs, codec)
	if err != nil {
		return info, err
	}
	if info.ColorSpace == ColorSpacePattern {
		return info, errBadImage // A pattern paints areas; it cannot color samples.
	}
	return info, nil
}

// colorSpace returns the family of color space cs and the samples per pixel it
// takes. An unknown family is not an error: a name may refer to the page's
// /Resources /ColorSpace, which an image dictionary cannot see.
func (pdf *PDF) colorSpace(r io.ReaderAt, cs Value, codec *Codec) (fam ColorSpace, n int, err error) {
	switch {
	case cs.IsNull():
		return ColorSpaceUnknown, 0, nil
	case cs.Tok == piulex.TokName:
		lit, err := codec.AppendString(codec.keybuf[:0], pdf, r, cs)
		if err != nil {
			return fam, 0, err
		}
		fam = colorSpaceByName(lit)
		return fam, fam.components(), nil
	case !cs.IsArray():
		return fam, 0, errBadImage
	}
	first, length, err := codec.arrayAt(pdf, r, cs, 0)
	if err != nil {
		return fam, 0, err
	} else if first.Tok != piulex.TokName {
		return fam, 0, errBadImage
	}
	lit, err := codec.AppendString(codec.keybuf[:0], pdf, r, first)
	if err != nil {
		return fam, 0, err
	}
	fam = colorSpaceByName(lit)
	switch fam {
	case ColorSpaceICCBased:
		// [/ICCBased stream]: the profile's /N is the component count.
		n, err = pdf.arrayIntAt(r, cs, 1, "N", codec)
		if err != nil || n != 1 && n != 3 && n != 4 {
			return fam, 0, errBadImage
		}
		return fam, n, nil
	case ColorSpaceDeviceN:
		// [/DeviceN names alternate tint attrs?]: one sample per colorant.
		names, _, err := codec.arrayAt(pdf, r, cs, 1)
		if err != nil {
			return fam, 0, err
		}
		if names, err = pdf.Deref(r, names, codec); err != nil {
			return fam, 0, err
		} else if !names.IsArray() {
			return fam, 0, errBadImage
		}
		_, n, err = codec.arrayAt(pdf, r, names, 0)
		if err != nil || n == 0 {
			return fam, 0, errBadImage
		}
		return fam, n, nil
	case ColorSpaceDeviceGray, ColorSpaceDeviceRGB, ColorSpaceDeviceCMYK:
		// A device family in an array is legal but odd; it takes no
		// parameters.
	default:
		if length < 2 {
			return fam, 0, errBadImage // Every other family takes parameters.
		}
	}
	return fam, fam.components(), nil
}

// arrayIntAt resolves element i of array arrV, which must be a dictionary or
// stream, and returns its integer entry key.
func (pdf *PDF) arrayIntAt(r io.ReaderAt, arrV Value, i int, key string, codec *Codec) (int, error) {
	el, _, err := codec.arrayAt(pdf, r, arrV, i)
	if err != nil {
		return 0, err
	}
	if el, err = pdf.Deref(r, el, codec); err != nil {
		return 0, err
	}
	if !el.IsDict() && el.Tok != tokStream {
		return 0, errBadImage
	}
	v, err := codec.DictGet(pdf, r, el, key)
	if err != nil {
		return 0, err
	}
	if v, err = pdf.Deref(r, v, codec); err != nil {
		return 0, err
	}
	n, ok := v.Int()
	if !ok {
		return 0, errBadImage
	}
	return int(n), nil
}
