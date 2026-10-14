package piupage

import (
	"image"
	"image/color"
	"io"

	"github.com/soypat/piudf"
)

// Image is a picture a Canvas draws. Any image.Image is one, its pixels
// encoded when it is written; a *JPEG is one whose file is embedded as it
// stands. A canvas tells images apart by ==, so an Image must be comparable:
// pass pointers, which every image/... type is.
type Image interface {
	Bounds() image.Rectangle
}

// JPEG is a JPEG file drawn without decoding: its bytes become a DCTDecode
// stream, which is a JPEG file as it stands. The zero JPEG holds no file; see
// [JPEG.Reset].
type JPEG struct {
	data        []byte
	w, h, comps int
	// adobe is set by an APP14 Adobe segment, whose CMYK samples are stored
	// inverted.
	adobe bool
}

// Reset points j at the JPEG file data, reading its frame header for the size
// and components. data is kept, not copied, and must not change while j is in
// use. Only what a PDF DCTDecode stream holds is accepted: 8-bit samples, and
// one, three or four components.
func (j *JPEG) Reset(data []byte) error {
	*j = JPEG{}
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return errJPEG
	}
	for i := 2; ; {
		if i+2 > len(data) || data[i] != 0xff {
			return errJPEG
		}
		marker := data[i+1]
		i += 2
		switch {
		case marker == 0xff:
			i-- // A fill byte: the next 0xff may start the marker.
			continue
		case marker == 0x01 || marker >= 0xd0 && marker <= 0xd8:
			continue // TEM, RSTn and SOI stand alone.
		case marker == 0xd9 || marker == 0xda:
			return errJPEG // EOI or SOS before any frame header.
		}
		if i+2 > len(data) {
			return errJPEG
		}
		n := int(data[i])<<8 | int(data[i+1])
		if n < 2 || i+n > len(data) {
			return errJPEG
		}
		seg := data[i+2 : i+n]
		i += n
		switch {
		case marker == 0xee && len(seg) >= 5 && string(seg[:5]) == "Adobe":
			j.adobe = true
		case marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc:
			// SOFn: precision, height, width, components. DHT, JPG and DAC
			// share the range and are not frame headers.
			if len(seg) < 6 || seg[0] != 8 {
				return errJPEG
			}
			j.h = int(seg[1])<<8 | int(seg[2])
			j.w = int(seg[3])<<8 | int(seg[4])
			j.comps = int(seg[5])
			if j.w == 0 || j.h == 0 || j.comps != 1 && j.comps != 3 && j.comps != 4 {
				*j = JPEG{} // A height left to a DNL segment included.
				return errJPEG
			}
			j.data = data
			return nil
		}
	}
}

// Bounds returns the image's size, anchored at the origin.
func (j *JPEG) Bounds() image.Rectangle { return image.Rect(0, 0, j.w, j.h) }

// Data returns the JPEG file j was reset to.
func (j *JPEG) Data() []byte { return j.data }

// ImageEncoder writes images as image XObjects (ISO 32000-1 8.9.5). Unlike a
// font or a link, which [WriteFont] and [WriteLink] write from nothing, an
// image is written through a compressor, and the ImageEncoder is what owns it,
// along with a fixed staging buffer. Once warm it writes every image/... type
// and *JPEG without allocating; any other image.Image is read through At,
// which allocates.
//
// The zero ImageEncoder is ready to use.
type ImageEncoder struct {
	// Flate compresses the samples; its Level is the compression level.
	Flate piudf.FlateWriter

	w   io.Writer // The compressor the staged samples go to.
	err error
	n   int // buf[:n] is staged.
	buf [4096]byte
	// pal is a *image.Paletted's palette: RGB entries for the /Indexed lookup
	// table, and their alpha for its soft mask.
	pal  [256 * 3]byte
	palA [256]uint8
}

// Write writes img and returns its image XObject's id. An image with
// transparency gets a soft mask, written first as its own image XObject and
// referenced by /SMask.
func (ie *ImageEncoder) Write(enc *piudf.Encoder, img Image) (piudf.ObjectID, error) {
	switch m := img.(type) {
	case *JPEG:
		return ie.writeJPEG(enc, m)
	case image.Image:
		b := m.Bounds()
		if b.Dx() <= 0 || b.Dy() <= 0 {
			return piudf.ObjectID{}, errImageEmpty
		}
		var smask piudf.ObjectID
		if !opaque(m) {
			smask = enc.NewID()
			if err := ie.writeSamples(enc, smask, m, true, piudf.ObjectID{}); err != nil {
				return piudf.ObjectID{}, err
			}
		}
		id := enc.NewID()
		return id, ie.writeSamples(enc, id, m, false, smask)
	}
	return piudf.ObjectID{}, errImageType
}

func (ie *ImageEncoder) writeJPEG(enc *piudf.Encoder, j *JPEG) (piudf.ObjectID, error) {
	if j.data == nil {
		return piudf.ObjectID{}, errJPEG
	}
	id := enc.NewID()
	enc.BeginObject(id)
	enc.DictOpen()
	imageDictHead(enc, j.w, j.h)
	enc.Name("ColorSpace")
	enc.Name(deviceSpace(j.comps))
	enc.Name("BitsPerComponent")
	enc.Int(8)
	if j.adobe && j.comps == 4 {
		// The convention PDF producers follow for Adobe's inverted CMYK.
		enc.Name("Decode")
		enc.ArrayOpen()
		for range 4 {
			enc.Int(1)
			enc.Int(0)
		}
		enc.ArrayClose()
	}
	enc.Name("Filter")
	enc.Name("DCTDecode")
	enc.Name("Length")
	enc.Int(int64(len(j.data)))
	enc.DictClose()
	enc.BeginStreamPayload()
	enc.PayloadWriter().Write(j.data)
	enc.EndStreamPayload()
	enc.EndObject()
	return id, enc.Err()
}

// writeSamples writes m's samples as image XObject id, FlateDecode compressed:
// its colors, or with alpha set its alpha as the DeviceGray soft mask smask
// names.
func (ie *ImageEncoder) writeSamples(enc *piudf.Encoder, id piudf.ObjectID, m image.Image, alpha bool, smask piudf.ObjectID) error {
	b := m.Bounds()
	lenID := enc.NewID()
	enc.BeginObject(id)
	enc.DictOpen()
	imageDictHead(enc, b.Dx(), b.Dy())
	enc.Name("ColorSpace")
	bpc := 8
	switch m := m.(type) {
	case *image.Gray16, *image.RGBA64, *image.NRGBA64:
		bpc = 16
		switch {
		case alpha:
			enc.Name("DeviceGray")
		case isGray16(m):
			enc.Name("DeviceGray")
		default:
			enc.Name("DeviceRGB")
		}
	case *image.Paletted:
		// The soft mask, written first, reads the palette's alpha.
		n := ie.palette(m.Palette)
		if alpha {
			enc.Name("DeviceGray")
			break
		}
		enc.ArrayOpen()
		enc.Name("Indexed")
		enc.Name("DeviceRGB")
		enc.Int(int64(n - 1))
		enc.HexString(ie.pal[:3*n])
		enc.ArrayClose()
	case *image.Gray:
		enc.Name("DeviceGray")
	case *image.CMYK:
		if alpha {
			enc.Name("DeviceGray")
		} else {
			enc.Name("DeviceCMYK")
		}
	default:
		if alpha {
			enc.Name("DeviceGray")
		} else {
			enc.Name("DeviceRGB")
		}
	}
	enc.Name("BitsPerComponent")
	enc.Int(int64(bpc))
	if !smask.IsZero() {
		enc.Name("SMask")
		enc.Ref(smask.Num, smask.Gen)
	}
	enc.Name("Filter")
	enc.Name("FlateDecode")
	enc.Name("Length")
	enc.Ref(lenID.Num, lenID.Gen)
	enc.DictClose()
	enc.BeginStreamPayload()
	w, err := ie.Flate.Reset(enc.PayloadWriter())
	if err != nil {
		enc.Fail(err)
		return err
	}
	ie.w, ie.n, ie.err = w, 0, nil
	ie.samples(m, alpha)
	ie.flush()
	if err := ie.Flate.Close(); err != nil && ie.err == nil {
		ie.err = err
	}
	n := enc.EndStreamPayload()
	enc.EndObject()
	enc.WriteLength(lenID, n)
	if ie.err != nil {
		enc.Fail(ie.err)
		return ie.err
	}
	return enc.Err()
}

// samples stages m's samples, row by row: its colors, or with alpha set its
// alpha.
func (ie *ImageEncoder) samples(m image.Image, alpha bool) {
	b := m.Bounds()
	w := b.Dx()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		switch m := m.(type) {
		case *image.Gray:
			i := m.PixOffset(b.Min.X, y)
			ie.put(m.Pix[i : i+w])
		case *image.Gray16:
			i := m.PixOffset(b.Min.X, y)
			ie.put(m.Pix[i : i+2*w]) // Big-endian, as a PDF wants them.
		case *image.CMYK:
			i := m.PixOffset(b.Min.X, y)
			ie.put(m.Pix[i : i+4*w])
		case *image.Paletted:
			i := m.PixOffset(b.Min.X, y)
			row := m.Pix[i : i+w]
			if !alpha {
				ie.put(row)
				break
			}
			for _, c := range row {
				p := ie.room(1)
				p[0] = ie.palA[c]
			}
		case *image.NRGBA:
			i := m.PixOffset(b.Min.X, y)
			for x := range w {
				px := m.Pix[i+4*x : i+4*x+4]
				if alpha {
					ie.room(1)[0] = px[3]
				} else {
					p := ie.room(3)
					p[0], p[1], p[2] = px[0], px[1], px[2]
				}
			}
		case *image.RGBA:
			i := m.PixOffset(b.Min.X, y)
			for x := range w {
				px := m.Pix[i+4*x : i+4*x+4]
				if alpha {
					ie.room(1)[0] = px[3]
				} else {
					p := ie.room(3)
					p[0], p[1], p[2] = unpremul8(px[0], px[3]), unpremul8(px[1], px[3]), unpremul8(px[2], px[3])
				}
			}
		case *image.NRGBA64:
			i := m.PixOffset(b.Min.X, y)
			for x := range w {
				px := m.Pix[i+8*x : i+8*x+8]
				if alpha {
					p := ie.room(2)
					p[0], p[1] = px[6], px[7]
				} else {
					copy(ie.room(6), px[:6])
				}
			}
		case *image.RGBA64:
			i := m.PixOffset(b.Min.X, y)
			for x := range w {
				px := m.Pix[i+8*x : i+8*x+8]
				a := uint32(px[6])<<8 | uint32(px[7])
				if alpha {
					p := ie.room(2)
					p[0], p[1] = px[6], px[7]
					continue
				}
				p := ie.room(6)
				for c := range 3 {
					v := unpremul16(uint32(px[2*c])<<8|uint32(px[2*c+1]), a)
					p[2*c], p[2*c+1] = byte(v>>8), byte(v)
				}
			}
		case *image.YCbCr:
			for x := b.Min.X; x < b.Max.X; x++ {
				yi, ci := m.YOffset(x, y), m.COffset(x, y)
				p := ie.room(3)
				p[0], p[1], p[2] = color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
			}
		default:
			for x := b.Min.X; x < b.Max.X; x++ {
				c := color.NRGBAModel.Convert(m.At(x, y)).(color.NRGBA)
				if alpha {
					ie.room(1)[0] = c.A
				} else {
					p := ie.room(3)
					p[0], p[1], p[2] = c.R, c.G, c.B
				}
			}
		}
	}
}

// palette fills ie.pal and ie.palA from pal and returns how many entries it
// holds, at least one: an empty palette is written as one black entry.
func (ie *ImageEncoder) palette(pal color.Palette) int {
	n := min(len(pal), 256)
	for i, c := range pal[:n] {
		r, g, b, a := c.RGBA()
		ie.pal[3*i] = uint8(unpremul16(r, a) >> 8)
		ie.pal[3*i+1] = uint8(unpremul16(g, a) >> 8)
		ie.pal[3*i+2] = uint8(unpremul16(b, a) >> 8)
		ie.palA[i] = uint8(a >> 8)
	}
	if n == 0 {
		ie.pal[0], ie.pal[1], ie.pal[2], ie.palA[0] = 0, 0, 0, 0xff
		n = 1
	}
	// An index past the palette is clamped to its last entry by readers; its
	// alpha follows suit.
	for i := n; i < len(ie.palA); i++ {
		ie.palA[i] = ie.palA[n-1]
	}
	return n
}

// room returns the next n bytes of the staging buffer to fill, flushing it
// first if they do not fit.
func (ie *ImageEncoder) room(n int) []byte {
	if ie.n+n > len(ie.buf) {
		ie.flush()
	}
	p := ie.buf[ie.n : ie.n+n]
	ie.n += n
	return p
}

// put stages b, flushing as the buffer fills.
func (ie *ImageEncoder) put(b []byte) {
	for len(b) > 0 {
		if ie.n == len(ie.buf) {
			ie.flush()
		}
		c := copy(ie.buf[ie.n:], b)
		ie.n += c
		b = b[c:]
	}
}

// flush hands the staged bytes to the compressor. After an error the bytes are
// dropped: the error is reported once the payload ends.
func (ie *ImageEncoder) flush() {
	if ie.n > 0 && ie.err == nil {
		_, ie.err = ie.w.Write(ie.buf[:ie.n])
	}
	ie.n = 0
}

// imageDictHead writes the entries every image XObject dictionary starts with.
func imageDictHead(enc *piudf.Encoder, w, h int) {
	enc.Name("Type")
	enc.Name("XObject")
	enc.Name("Subtype")
	enc.Name("Image")
	enc.Name("Width")
	enc.Int(int64(w))
	enc.Name("Height")
	enc.Int(int64(h))
}

func deviceSpace(comps int) string {
	switch comps {
	case 1:
		return "DeviceGray"
	case 3:
		return "DeviceRGB"
	}
	return "DeviceCMYK"
}

func isGray16(m image.Image) bool {
	_, ok := m.(*image.Gray16)
	return ok
}

// opaque reports whether every pixel of m is fully opaque, which is when it
// needs no soft mask.
func opaque(m image.Image) bool {
	if o, ok := m.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	b := m.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := m.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

// unpremul8 undoes alpha premultiplication of an 8-bit component.
func unpremul8(v, a uint8) uint8 {
	if a == 0 {
		return 0
	}
	return uint8(min((uint32(v)*0xff+uint32(a)/2)/uint32(a), 0xff))
}

// unpremul16 undoes alpha premultiplication of a 16-bit component.
func unpremul16(v, a uint32) uint32 {
	if a == 0 {
		return 0
	}
	return min((v*0xffff+a/2)/a, 0xffff)
}
