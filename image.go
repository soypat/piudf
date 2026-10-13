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

// imageKeys are the image dictionary entries ImageInfo reads, as found.
type imageKeys struct {
	subtype, width, height, bpc, cs, mask, smask, decode, imask Value
	// Read for inline images only; a stream's are its own business.
	filter, parms, length Value
}

func newImageKeys() imageKeys {
	null := Value{Tok: piulex.TokNull}
	return imageKeys{null, null, null, null, null, null, null, null, null, null, null, null}
}

// set records entry key. An inline image may spell keys in full or by the
// abbreviations of ISO 32000-1 Table 93; a stream's /F is a file
// specification and its /L nothing, so those are read only when inline.
func (k *imageKeys) set(key []byte, v Value, inline bool) {
	switch b2s(key) {
	case "Subtype":
		k.subtype = v
	case "Width", "W":
		k.width = v
	case "Height", "H":
		k.height = v
	case "BitsPerComponent", "BPC":
		k.bpc = v
	case "ColorSpace", "CS":
		k.cs = v
	case "Mask":
		k.mask = v
	case "SMask":
		k.smask = v
	case "Decode", "D":
		k.decode = v
	case "ImageMask", "IM":
		k.imask = v
	}
	if !inline {
		return
	}
	switch b2s(key) {
	case "Filter", "F":
		k.filter = v
	case "DecodeParms", "DP":
		k.parms = v
	case "Length", "L":
		k.length = v
	}
}

func (pdf *PDF) imageInfo(r io.ReaderAt, v Value, codec *Codec) (info ImageInfo, err error) {
	if v.Tok != tokStream {
		return info, errNotAStream
	}
	keys := newImageKeys()
	err = codec.DictForEach(pdf, r, v, func(key []byte, val Value) bool {
		keys.set(key, val, false)
		return true
	})
	if err != nil {
		return info, err
	}
	// Everything the scan found may be indirect, and resolving moves the
	// lexer, so it waits until the scan is done. Mask and SMask stay as they
	// are: a stream is always indirect, and the reference is what callers want.
	for _, p := range [...]*Value{&keys.subtype, &keys.width, &keys.height, &keys.bpc, &keys.cs, &keys.decode, &keys.imask} {
		if *p, err = pdf.Deref(r, *p, codec); err != nil {
			return info, err
		}
	}
	if !keys.subtype.IsNull() {
		// /Subtype is required, but a stream drawn as an image and lacking it
		// is still one; only a different subtype is wrong.
		if ok, err := codec.NameIs(pdf, r, keys.subtype, "Image"); err != nil {
			return info, err
		} else if !ok {
			return info, errBadImage
		}
	}
	if info.Payload, err = pdf.streamPayload(r, v, codec); err != nil {
		return info, err
	}
	err = pdf.fillImageInfo(r, &info, &keys, codec)
	return info, err
}

// InlineImage reads the inline image whose BI operator is at offset off of
// content stream r (ISO 32000-1 8.9.7) and returns it with the offset just past
// its EI, where the content stream resumes. r is the decoded content stream,
// not the file: the returned Values and Payload are coordinates in it, and
// [PDF.OpenPayload] over r decodes the samples.
//
// An inline image is not an object and is drawn exactly once, where it is.
// SMask and Mask do not apply to one; ColorSpace may name a space in the
// page's /Resources, reported as ColorSpaceUnknown as for an image XObject.
func (pdf *PDF) InlineImage(r io.ReaderAt, off int64, codec *Codec) (info ImageInfo, end int64, err error) {
	info, end, err = pdf.inlineImage(r, off, codec)
	if err != nil {
		return ImageInfo{}, 0, pdf.setError(codec, err)
	}
	return info, end, nil
}

func (pdf *PDF) inlineImage(r io.ReaderAt, off int64, codec *Codec) (info ImageInfo, end int64, err error) {
	if err = codec.lexAt(r, off); err != nil {
		return info, 0, err
	}
	if v, lit, err := codec.nextRaw(); err != nil {
		return info, 0, err
	} else if v.Tok != piulex.TokIdent || !bequal(lit, "BI") {
		return info, 0, errBadImage
	}
	keys := newImageKeys()
	for {
		kv, lit, err := codec.nextRaw()
		if err != nil {
			return info, 0, err
		}
		if kv.Tok == piulex.TokIdent && bequal(lit, "ID") {
			break
		} else if kv.Tok != piulex.TokName || len(lit) > len(codec.keybuf) {
			return info, 0, errBadImage
		}
		// The literal dies at the next token, which the value is.
		key := codec.keybuf[:copy(codec.keybuf[:], lit)]
		v, err := codec.decodeShallow()
		if err != nil {
			return info, 0, err
		}
		keys.set(key, v, true)
	}
	length := int64(-1)
	if n, ok := keys.length.Int(); ok && n >= 0 {
		length = n
	}
	start, n, err := codec.lex.InlineImageData(length)
	if err != nil {
		return info, 0, err
	}
	end = int64(codec.lex.Pos())
	sc, predictorAt, err := codec.readFilters(pdf, r, keys.filter)
	if err != nil {
		return info, 0, err
	}
	if predictorAt >= 0 {
		if err = codec.readPredictor(pdf, r, &sc, keys.parms, predictorAt); err != nil {
			return info, 0, err
		}
	}
	info.Payload = StreamPayload{Offset: int64(start), Length: n, codec: sc}
	keys.mask, keys.smask = Value{Tok: piulex.TokNull}, Value{Tok: piulex.TokNull}
	err = pdf.fillImageInfo(r, &info, &keys, codec)
	return info, end, err
}

// fillImageInfo fills info from keys and info.Payload, which must be set.
func (pdf *PDF) fillImageInfo(r io.ReaderAt, info *ImageInfo, keys *imageKeys, codec *Codec) (err error) {
	w, okw := keys.width.Int()
	h, okh := keys.height.Int()
	if !okw || !okh || w <= 0 || h <= 0 || w > 1<<24 || h > 1<<24 {
		return errBadImage
	}
	info.Width, info.Height = int(w), int(h)
	info.ImageMask, _ = keys.imask.Bool()
	info.Mask, info.Decode, info.SMask = keys.mask, keys.decode, keys.smask.ObjectID()
	jpx := info.Payload.NumFilters() > 0 && info.Payload.Filter(info.Payload.NumFilters()-1) == FilterJPX

	switch n, ok := keys.bpc.Int(); {
	case info.ImageMask:
		// A stencil mask is one bit, whatever it says (ISO 32000-1 Table 89).
		info.BitsPerComponent, info.Components = 1, 1
		return nil
	case !ok && jpx && keys.bpc.IsNull():
		// The codestream says.
	case ok && (n == 1 || n == 2 || n == 4 || n == 8 || n == 16):
		info.BitsPerComponent = int(n)
	default:
		return errBadImage
	}

	info.ColorSpaceValue = keys.cs
	info.ColorSpace, info.Components, err = pdf.colorSpace(r, keys.cs, codec)
	if err != nil {
		return err
	}
	if info.ColorSpace == ColorSpacePattern {
		return errBadImage // A pattern paints areas; it cannot color samples.
	}
	return nil
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

// maxPageTreeDepth bounds a /Parent walk. The page tree is a tree only by
// convention, and a /Parent pointing back down would otherwise spin forever.
const maxPageTreeDepth = 32

// maxFormDepth bounds Form XObject nesting during image discovery. A form may
// draw a form that draws the first, and the depth cap is what ends that.
const maxFormDepth = 8

// Inherited reads key from page tree node dict, walking /Parent while it is
// absent: /Resources, /MediaBox, /CropBox and /Rotate are handed down the tree
// (ISO 32000-1 Table 30). It returns the resolved value, or null when no
// ancestor sets it.
func (pdf *PDF) Inherited(r io.ReaderAt, dict Value, key string, codec *Codec) (Value, error) {
	for range maxPageTreeDepth {
		v, err := codec.DictGet(pdf, r, dict, key)
		if err != nil {
			return v, err
		} else if !v.IsNull() {
			return pdf.Deref(r, v, codec)
		}
		parent, err := codec.DictGet(pdf, r, dict, "Parent")
		if err != nil || parent.IsNull() {
			return Value{Tok: piulex.TokNull}, err
		}
		if dict, err = pdf.Deref(r, parent, codec); err != nil {
			return dict, err
		}
	}
	return Value{}, ErrCodecDepthLimit
}

// ImageRef is an image XObject a page can draw.
type ImageRef struct {
	// ID is the image object. Pages drawing the same image name the same ID,
	// which is what tells a reused image from a repeated one.
	ID ObjectID
	// Form is the Form XObject whose resources name the image, or zero when
	// the page's own resources do. A form is drawn as a unit, so an image
	// inside one is drawn wherever the form is.
	Form ObjectID
}

// AppendPageImages appends to dst the image XObjects page's resources make
// available, each once, in the order the resources name them, and follows
// Form XObjects into theirs. page is a page dictionary; its /Resources may be
// inherited from an ancestor.
//
// Available is not drawn: a resource dictionary may name images the content
// stream never paints, and producers that share one dictionary among all pages
// do. Inline images live in the content stream and are not resources, so they
// are not listed.
//
// dst is the only memory used: forms awaiting a visit are held at its tail
// and removed before return.
func (pdf *PDF) AppendPageImages(dst []ImageRef, r io.ReaderAt, page Value, codec *Codec) ([]ImageRef, error) {
	res, err := pdf.Inherited(r, page, "Resources", codec)
	if err != nil {
		return dst, pdf.setError(codec, err)
	}
	dst, err = pdf.appendResourceImages(dst, len(dst), r, res, ObjectID{}, 0, codec)
	if err != nil {
		return dst, pdf.setError(codec, err)
	}
	return dst, nil
}

// appendResourceImages appends the images of resource dictionary res, named
// by form (zero for a page), to dst, skipping any already in dst[first:].
func (pdf *PDF) appendResourceImages(dst []ImageRef, first int, r io.ReaderAt, res Value, form ObjectID, depth int, codec *Codec) ([]ImageRef, error) {
	if res.IsNull() {
		return dst, nil
	} else if !res.IsDict() {
		return dst, errValueMismatch
	}
	xobj, err := codec.DictGet(pdf, r, res, "XObject")
	if err != nil {
		return dst, err
	}
	if xobj, err = pdf.Deref(r, xobj, codec); err != nil || xobj.IsNull() {
		return dst, err
	} else if !xobj.IsDict() {
		return dst, errValueMismatch
	}
	// The references are collected before any is resolved: one Codec has one
	// lexer, and resolving inside DictForEach would move the scan. They are
	// parked at dst's tail, which is where they are classified too.
	base := len(dst)
	err = codec.DictForEach(pdf, r, xobj, func(_ []byte, v Value) bool {
		if v.Tok == piulex.TokR { // An XObject is a stream, so always indirect.
			dst = append(dst, ImageRef{ID: v.ObjectID(), Form: form})
		}
		return true
	})
	if err != nil {
		return dst[:base], err
	}
	// Partition the tail: images to the front, forms to the back, anything
	// else and repeats dropped.
	end, nimg := len(dst), base
	nform := end
	for i := base; i < nform; {
		ref := dst[i]
		v, err := pdf.Resolve(r, ref.ID, codec)
		if err != nil {
			return dst[:base], err
		}
		kind, err := xobjectKind(pdf, r, v, codec)
		if err != nil {
			return dst[:base], err
		}
		switch {
		case kind == xobjImage && !hasImage(dst[first:nimg], ref.ID):
			dst[nimg] = ref
			nimg++
			i++
		case kind == xobjForm && depth < maxFormDepth:
			nform--
			dst[i], dst[nform] = dst[nform], ref
		default:
			i++ // Dropped: a PostScript XObject, a repeat, too deep a form.
		}
	}
	// Each form appends its images past end, which leaves the parked forms
	// in [nform, end) where they are until every one has been visited.
	for j := nform; j < end; j++ {
		fid := dst[j].ID
		fv, err := pdf.Resolve(r, fid, codec)
		if err != nil {
			return dst[:base], err
		}
		fres, err := codec.DictGet(pdf, r, fv, "Resources")
		if err != nil {
			return dst[:base], err
		}
		// A form without /Resources draws with its page's, which this walk
		// has already listed or is listing.
		if fres, err = pdf.Deref(r, fres, codec); err != nil {
			return dst[:base], err
		}
		if dst, err = pdf.appendResourceImages(dst, first, r, fres, fid, depth+1, codec); err != nil {
			return dst[:base], err
		}
	}
	// Close the gap the classification and the parked forms left.
	n := copy(dst[nimg:], dst[end:])
	return dst[:nimg+n], nil
}

func hasImage(refs []ImageRef, id ObjectID) bool {
	for _, ref := range refs {
		if ref.ID == id {
			return true
		}
	}
	return false
}

type xobjKind uint8

const (
	xobjOther xobjKind = iota
	xobjImage
	xobjForm
)

// xobjectKind reads the /Subtype of XObject stream v.
func xobjectKind(pdf *PDF, r io.ReaderAt, v Value, codec *Codec) (xobjKind, error) {
	if v.Tok != tokStream {
		return xobjOther, nil
	}
	st, err := codec.DictGet(pdf, r, v, "Subtype")
	if err != nil || st.Tok != piulex.TokName {
		return xobjOther, err
	}
	if ok, err := codec.NameIs(pdf, r, st, "Image"); ok || err != nil {
		return xobjImage, err
	}
	if ok, err := codec.NameIs(pdf, r, st, "Form"); ok || err != nil {
		return xobjForm, err
	}
	return xobjOther, nil
}
