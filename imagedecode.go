package piudf

import (
	"image"
	"image/color"
	"image/jpeg"
	"io"

	"github.com/soypat/piudf/piulex"
)

// defaultMaxPixels bounds an image when ImageDecoder.MaxPixels is zero: 64M
// pixels, a 256 MB RGBA image.
const defaultMaxPixels = 1 << 26

// ImageDecoder turns the images a PDF holds into Go images. It is the one place
// in piudf where pixels are materialized: [PDF.ImageInfo] locates an image and
// says how its samples are laid out, and an ImageDecoder reads them.
//
// Memory is split by size. The caller owns the decoded image, which is as large
// as the file says, and lends it back to Decode; the ImageDecoder owns a few
// kilobytes of fixed scratch (a read buffer, sample tables, the palette) and
// its inflate window, allocated on the first compressed image. With both warm,
// decoding allocates nothing.
//
// The zero ImageDecoder is ready to use.
type ImageDecoder struct {
	// MaxPixels bounds Width*Height of an image and of its masks. Zero means
	// 64M. Image dimensions come from the file, and a hostile file names
	// whatever it likes.
	MaxPixels int

	strm  Stream
	ready bool
	sr    sampleReader
	l     imageLayout
}

// Decode returns the image info describes, with its soft mask (/SMask) or
// stencil or color key mask (/Mask) applied as alpha. pdf, r and codec are
// those info was read with; for an inline image, r is the content stream.
//
// The result type follows the image: *image.Gray or *image.Gray16 for one
// component, *image.RGBA or *image.RGBA64 for three, *image.CMYK for four,
// *image.Alpha for a stencil mask (opaque where it paints), and *image.NRGBA
// for any image a mask applies to. An Indexed image takes the type of its base
// space, its palette applied.
//
// dst lends its pixels: when it has the result's type, the result is dst,
// resized, and its Pix is reused when it has the capacity. A nil dst, or one
// of another type, gets a new image. Decoding into a dst that held the same
// image before allocates nothing, except for a JPEG (DCTDecode) image or mask,
// which image/jpeg decodes into a new image.
//
// Sample data cut short decodes as far as it goes, the rest as zero samples,
// which is what viewers show. ICC profiles are not applied: an ICCBased image
// is read as the device space of its component count. Lab, Separation and
// DeviceN spaces, which need their parameters or tint transforms, fail with
// [ErrUnsupportedImage], as do filters other than FlateDecode and DCTDecode.
// The payload of such an image is still located by its ImageInfo: a JPXDecode
// payload, for one, is a JPEG 2000 file as it stands.
func (d *ImageDecoder) Decode(dst image.Image, pdf *PDF, r io.ReaderAt, info ImageInfo, codec *Codec) (image.Image, error) {
	if err := d.checkSize(&info); err != nil {
		return nil, err
	}
	var mask ObjectID
	switch {
	case info.ImageMask:
		// A stencil is a mask itself; masks of its own are ignored.
	case !info.SMask.IsZero():
		mask = info.SMask
	case info.Mask.Tok == piulex.TokR:
		mask = info.Mask.ObjectID()
	}
	keyed := info.Mask.IsArray() && !info.ImageMask
	if mask.IsZero() && !keyed {
		return d.decodeColor(dst, pdf, r, &info, codec)
	}

	w, h := info.Width, info.Height
	var jimg image.Image
	if isDCT(&info) {
		var err error
		if jimg, err = d.decodeJPEG(pdf, r, &info, codec); err != nil {
			return nil, err
		}
		w, h = jimg.Bounds().Dx(), jimg.Bounds().Dy()
	}
	out := reuse[image.NRGBA](dst)
	out.Pix, out.Stride, out.Rect = resize(out.Pix, 4*w*h), 4*w, image.Rect(0, 0, w, h)
	// The mask goes first, into the alpha channel, so that a mask row stretched
	// over several image rows can be copied from the row above.
	masked := !mask.IsZero()
	if masked {
		if err := d.decodeMask(out, pdf, r, mask, codec); err != nil {
			return nil, err
		}
	}
	if jimg != nil {
		// A color key on a JPEG would key out its decoded samples, which are
		// not the ones the key was written against; it is ignored.
		b := jimg.Bounds()
		for y := range h {
			for x := range w {
				c := color.NRGBAModel.Convert(jimg.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
				p := out.Pix[y*out.Stride+4*x:]
				p[0], p[1], p[2] = c.R, c.G, c.B
				if !masked {
					p[3] = c.A
				}
			}
		}
		return out, nil
	}
	if err := d.open(pdf, r, &info, codec); err != nil {
		return nil, err
	}
	if keyed {
		n, err := readFloats(d.l.key[:], pdf, r, info.Mask, codec)
		if err != nil {
			return nil, err
		} else if n == 2*d.l.comps {
			d.l.nkey = n
		} // Else malformed: nothing keyed out.
	}
	d.fill(out, masked, w, h)
	if err := d.sr.result(); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeColor decodes an image no mask applies to.
func (d *ImageDecoder) decodeColor(dst image.Image, pdf *PDF, r io.ReaderAt, info *ImageInfo, codec *Codec) (image.Image, error) {
	if isDCT(info) {
		return d.decodeJPEG(pdf, r, info, codec)
	}
	if err := d.open(pdf, r, info, codec); err != nil {
		return nil, err
	}
	w, h, l := info.Width, info.Height, &d.l
	rect := image.Rect(0, 0, w, h)
	var out image.Image
	switch {
	case l.kind == kindStencil:
		m := reuse[image.Alpha](dst)
		m.Pix, m.Stride, m.Rect = resize(m.Pix, w*h), w, rect
		out = m
	case l.bpc == 16 && l.kind == kindGray:
		m := reuse[image.Gray16](dst)
		m.Pix, m.Stride, m.Rect = resize(m.Pix, 2*w*h), 2*w, rect
		out = m
	case l.bpc == 16 && l.kind == kindRGB:
		m := reuse[image.RGBA64](dst)
		m.Pix, m.Stride, m.Rect = resize(m.Pix, 8*w*h), 8*w, rect
		out = m
	case l.n == 1:
		m := reuse[image.Gray](dst)
		m.Pix, m.Stride, m.Rect = resize(m.Pix, w*h), w, rect
		out = m
	case l.n == 3:
		m := reuse[image.RGBA](dst)
		m.Pix, m.Stride, m.Rect = resize(m.Pix, 4*w*h), 4*w, rect
		out = m
	default: // CMYK, 16-bit included.
		m := reuse[image.CMYK](dst)
		m.Pix, m.Stride, m.Rect = resize(m.Pix, 4*w*h), 4*w, rect
		out = m
	}
	d.fill(out, false, w, h)
	if err := d.sr.result(); err != nil {
		return nil, err
	}
	return out, nil
}

// fill reads w*h pixels from the open payload into out. masked tells that an
// *image.NRGBA's alpha holds a mask already; color keyed pixels are made
// transparent either way.
func (d *ImageDecoder) fill(out image.Image, masked bool, w, h int) {
	l, s := &d.l, &d.sr
	var raw [4]int
	var px [4]uint8
	for y := range h {
		for x := range w {
			l.read(s, &raw)
			switch m := out.(type) {
			case *image.Alpha:
				l.to8(&raw, &px)
				m.Pix[y*m.Stride+x] = px[0]
			case *image.Gray:
				l.to8(&raw, &px)
				m.Pix[y*m.Stride+x] = px[0]
			case *image.Gray16:
				v := l.to16(raw[0], 0)
				p := m.Pix[y*m.Stride+2*x:]
				p[0], p[1] = byte(v>>8), byte(v)
			case *image.RGBA64:
				p := m.Pix[y*m.Stride+8*x:]
				for c := range 3 {
					v := l.to16(raw[c], c)
					p[2*c], p[2*c+1] = byte(v>>8), byte(v)
				}
				p[6], p[7] = 0xff, 0xff
			case *image.RGBA:
				l.to8(&raw, &px)
				p := m.Pix[y*m.Stride+4*x:]
				p[0], p[1], p[2], p[3] = px[0], px[1], px[2], 0xff
			case *image.CMYK:
				l.to8(&raw, &px)
				copy(m.Pix[y*m.Stride+4*x:][:4], px[:])
			case *image.NRGBA:
				l.to8(&raw, &px)
				p := m.Pix[y*m.Stride+4*x:]
				p[0], p[1], p[2] = l.rgb(&px)
				if !masked {
					p[3] = 0xff
				}
				if l.keyed(&raw) {
					p[3] = 0
				}
			}
		}
		s.endRow()
	}
}

// decodeMask decodes mask image object id into out's alpha channel, scaled to
// out's size by nearest sample. A mask's own masks, which the format forbids,
// are ignored. Mask rows are read in order and once: a row stretched over
// several of out's is copied from the one above, and a row no pixel lands in is
// skipped.
func (d *ImageDecoder) decodeMask(out *image.NRGBA, pdf *PDF, r io.ReaderAt, id ObjectID, codec *Codec) error {
	v, err := pdf.Resolve(r, id, codec)
	if err != nil {
		return err
	}
	info, err := pdf.ImageInfo(r, v, codec)
	if err != nil {
		return err
	}
	if err := d.checkSize(&info); err != nil {
		return err
	}
	w, h := out.Rect.Dx(), out.Rect.Dy()
	mw, mh := info.Width, info.Height
	if isDCT(&info) {
		m, err := d.decodeJPEG(pdf, r, &info, codec)
		if err != nil {
			return err
		}
		mb := m.Bounds()
		for y := range h {
			for x := range w {
				a := alphaOf(m.At(mb.Min.X+x*mb.Dx()/w, mb.Min.Y+y*mb.Dy()/h))
				out.Pix[y*out.Stride+4*x+3] = a
			}
		}
		return nil
	}
	if err := d.open(pdf, r, &info, codec); err != nil {
		return err
	}
	l, s := &d.l, &d.sr
	rowlen := (mw*l.comps*l.bpc + 7) / 8
	var raw [4]int
	var px [4]uint8
	my := -1 // The mask row last read.
	for y := range h {
		row := out.Pix[y*out.Stride:]
		want := y * mh / h
		if want == my {
			above := out.Pix[(y-1)*out.Stride:]
			for x := range w {
				row[4*x+3] = above[4*x+3]
			}
			continue
		}
		s.skip(rowlen * (want - my - 1))
		mx := -1 // The mask column last read.
		var a uint8
		for x := range w {
			for t := x * mw / w; mx < t; mx++ {
				l.read(s, &raw)
				l.to8(&raw, &px)
				a = l.gray(&px)
			}
			row[4*x+3] = a
		}
		for ; mx+1 < mw; mx++ {
			l.read(s, &raw)
		}
		s.endRow()
		my = want
	}
	return s.result()
}

// decodeJPEG decodes a DCTDecode payload, which is a JPEG file as it stands.
// The JPEG carries its own color model; a /Decode inverting every component,
// which is how PDF producers mark Adobe-inverted CMYK, is applied on top.
func (d *ImageDecoder) decodeJPEG(pdf *PDF, r io.ReaderAt, info *ImageInfo, codec *Codec) (image.Image, error) {
	sp := info.Payload
	img, err := jpeg.Decode(io.NewSectionReader(r, sp.Offset, sp.Length))
	if err != nil {
		return nil, err
	}
	var dec [8]float64
	n, err := readFloats(dec[:], pdf, r, info.Decode, codec)
	if err != nil || !allInverted(dec[:n]) {
		return img, err
	}
	switch m := img.(type) {
	case *image.CMYK:
		invert(m.Pix)
	case *image.Gray:
		invert(m.Pix)
	default:
		// YCbCr: inverting it means converting it, and a JPEG producer that
		// meant inverted RGB is one nobody has met.
	}
	return img, nil
}

// open readies d.l for info's samples and points d.sr at its payload.
func (d *ImageDecoder) open(pdf *PDF, r io.ReaderAt, info *ImageInfo, codec *Codec) error {
	sp := info.Payload
	if sp.NumFilters() > 1 || sp.NumFilters() == 1 && sp.Filter(0) != FilterFlate {
		return ErrUnsupportedImage
	}
	if err := d.setup(pdf, r, info, codec); err != nil {
		return err
	}
	// The palette, read by setup, is done with the stream.
	rd, err := pdf.OpenPayload(r, sp, d.stream())
	if err != nil {
		return err
	}
	d.sr.reset(rd, d.l.bpc)
	return nil
}

func (d *ImageDecoder) checkSize(info *ImageInfo) error {
	maxpx := d.MaxPixels
	if maxpx <= 0 {
		maxpx = defaultMaxPixels
	}
	if int64(info.Width)*int64(info.Height) > int64(maxpx) {
		return errImageTooLarge
	}
	return nil
}

func isDCT(info *ImageInfo) bool {
	return info.Payload.NumFilters() == 1 && info.Payload.Filter(0) == FilterDCT
}

// reuse returns dst as a *T, or a new T when it is not one.
func reuse[T any](dst image.Image) *T {
	if m, _ := any(dst).(*T); m != nil {
		return m
	}
	return new(T)
}

// resize returns pix with length n, reusing its storage when it has room.
func resize(pix []byte, n int) []byte {
	if cap(pix) < n {
		return make([]byte, n)
	}
	return pix[:n]
}

func invert(pix []byte) {
	for i := range pix {
		pix[i] = 255 - pix[i]
	}
}

// allInverted reports whether decode is [1 0 1 0 ...].
func allInverted(decode []float64) bool {
	if len(decode) == 0 || len(decode)%2 != 0 {
		return false
	}
	for i := 0; i < len(decode); i += 2 {
		if decode[i] != 1 || decode[i+1] != 0 {
			return false
		}
	}
	return true
}

// imageLayout is how one image's raw samples map to colors. Its tables are
// fixed-size, sized for the at most four components and 8-bit indices this
// decoder handles.
type imageLayout struct {
	kind  imageKind
	bpc   int
	comps int // Samples per pixel.
	n     int // Color components per pixel: comps, or an Indexed base's.
	// lut maps a sample of component c, as stored, to its 8-bit value with
	// /Decode applied, or for Indexed to a palette index: lut[c<<bpc|v].
	// Unused for 16-bit samples.
	lut [4 << 8]uint8
	// decode is /Decode, ndecode long, zero for the default; read for 16-bit
	// samples.
	decode  [8]float64
	ndecode int
	// key is a /Mask color key array, nkey long, zero for none.
	key  [8]float64
	nkey int
	// pal is an Indexed image's palette, n bytes per entry.
	pal  [256 * 4]uint8
	npal int
}

type imageKind uint8

const (
	kindGray imageKind = iota
	kindRGB
	kindCMYK
	kindIndexed
	kindStencil
)

// setup fills d.l for info.
func (d *ImageDecoder) setup(pdf *PDF, r io.ReaderAt, info *ImageInfo, codec *Codec) (err error) {
	l := &d.l
	l.bpc, l.comps = info.BitsPerComponent, info.Components
	l.ndecode, l.nkey = 0, 0
	switch {
	case info.ImageMask:
		l.kind, l.comps = kindStencil, 1
	case info.ColorSpace == ColorSpaceIndexed:
		l.kind, l.comps = kindIndexed, 1
		if err = d.readPalette(pdf, r, info.ColorSpaceValue, codec); err != nil {
			return err
		}
	case info.ColorSpace == ColorSpaceLab || info.ColorSpace == ColorSpaceSeparation ||
		info.ColorSpace == ColorSpaceDeviceN || l.bpc == 0:
		return ErrUnsupportedImage
	case l.comps == 1:
		l.kind = kindGray
	case l.comps == 3:
		l.kind = kindRGB
	case l.comps == 4:
		l.kind = kindCMYK
	default:
		return ErrUnsupportedImage // Including a space named by the page's resources.
	}
	if l.kind != kindIndexed {
		l.n = l.comps
	}
	n, err := readFloats(l.decode[:], pdf, r, info.Decode, codec)
	if err != nil {
		return err
	} else if n == 2*l.comps {
		l.ndecode = n
	} // Else malformed: the default stands, as viewers do.
	if l.kind == kindStencil {
		// A stencil paints where its sample is 0, or 1 under /Decode [1 0].
		paint := uint8(0)
		if allInverted(l.decode[:l.ndecode]) {
			paint = 1
		}
		l.lut[0], l.lut[1] = 255*(1-paint), 255*paint
		return nil
	}
	if l.bpc == 16 {
		return nil
	}
	maxv := 1<<l.bpc - 1
	for c := range l.comps {
		dmin, dmax := 0.0, 1.0
		if l.kind == kindIndexed {
			dmax = float64(maxv)
		}
		if l.ndecode != 0 {
			dmin, dmax = l.decode[2*c], l.decode[2*c+1]
		}
		for v := 0; v <= maxv; v++ {
			x := dmin + float64(v)*(dmax-dmin)/float64(maxv)
			if l.kind == kindIndexed {
				// An index, rounded and clamped to the palette.
				l.lut[c<<l.bpc|v] = uint8(min(max(x+0.5, 0), float64(l.npal-1)))
			} else {
				l.lut[c<<l.bpc|v] = uint8(min(max(x, 0), 1)*255 + 0.5)
			}
		}
	}
	return nil
}

// read reads one pixel's samples into raw.
func (l *imageLayout) read(s *sampleReader, raw *[4]int) {
	for c := range l.comps {
		raw[c] = s.sample()
	}
}

// to8 converts raw samples to 8-bit color components, l.n of them.
func (l *imageLayout) to8(raw *[4]int, px *[4]uint8) {
	switch {
	case l.kind == kindIndexed:
		i := int(l.lut[raw[0]]) * l.n
		copy(px[:l.n], l.pal[i:])
	case l.bpc == 16:
		for c := range l.comps {
			px[c] = uint8(l.to16(raw[c], c) >> 8)
		}
	default:
		for c := range l.comps {
			px[c] = l.lut[c<<l.bpc|raw[c]]
		}
	}
}

// to16 converts 16-bit sample v of component c to a 16-bit color value.
func (l *imageLayout) to16(v, c int) uint16 {
	if l.ndecode == 0 {
		return uint16(v)
	}
	dmin, dmax := l.decode[2*c], l.decode[2*c+1]
	x := dmin + float64(v)*(dmax-dmin)/65535
	return uint16(min(max(x, 0), 1)*65535 + 0.5)
}

// rgb converts the color to8 made to RGB.
func (l *imageLayout) rgb(px *[4]uint8) (r, g, b uint8) {
	switch l.n {
	case 1:
		return px[0], px[0], px[0]
	case 3:
		return px[0], px[1], px[2]
	}
	return color.CMYKToRGB(px[0], px[1], px[2], px[3])
}

// gray converts the color to8 made to a gray level, the way color.GrayModel
// does.
func (l *imageLayout) gray(px *[4]uint8) uint8 {
	if l.n == 1 {
		return px[0]
	}
	r, g, b := l.rgb(px)
	return uint8((19595*uint32(r) + 38470*uint32(g) + 7471*uint32(b) + 1<<15) >> 16)
}

// keyed reports whether raw lies within the color key, every component in its
// range.
func (l *imageLayout) keyed(raw *[4]int) bool {
	if l.nkey == 0 {
		return false
	}
	for c := range l.comps {
		v := float64(raw[c])
		if v < l.key[2*c] || v > l.key[2*c+1] {
			return false
		}
	}
	return true
}

// sampleReader reads an image's samples in order through a fixed buffer, so
// decoding holds the same few kilobytes whatever the image's size. Past the
// end of the data every sample reads as zero.
type sampleReader struct {
	rd   io.Reader
	err  error
	bpc  int
	i, n int  // buf[i:n] is unread.
	bit  int  // Bits of cur read, for samples narrower than a byte.
	cur  byte // The byte sub-byte samples are being read from.
	buf  [4096]byte
}

func (s *sampleReader) reset(rd io.Reader, bpc int) {
	s.rd, s.err, s.bpc = rd, nil, bpc
	s.i, s.n, s.bit = 0, 0, 0
}

// fill refills buf once it is spent, reporting whether bytes remain.
func (s *sampleReader) fill() bool {
	if s.i < s.n {
		return true
	} else if s.err != nil {
		return false
	}
	s.i = 0
	s.n, s.err = io.ReadAtLeast(s.rd, s.buf[:], 1)
	return s.n > 0
}

func (s *sampleReader) byte() byte {
	if !s.fill() {
		return 0
	}
	s.i++
	return s.buf[s.i-1]
}

// sample reads the next sample. bpc is 1, 2, 4, 8 or 16, so a sample never
// straddles bytes.
func (s *sampleReader) sample() int {
	switch s.bpc {
	case 8:
		return int(s.byte())
	case 16:
		hi := s.byte()
		return int(hi)<<8 | int(s.byte())
	}
	if s.bit == 0 {
		s.cur = s.byte()
	}
	s.bit += s.bpc
	v := int(s.cur>>(8-s.bit)) & (1<<s.bpc - 1)
	s.bit &= 7
	return v
}

// endRow skips to the next byte: rows start on a byte boundary.
func (s *sampleReader) endRow() { s.bit = 0 }

// skip skips n bytes. It is called at a row boundary.
func (s *sampleReader) skip(n int) {
	for n > 0 && s.fill() {
		k := min(n, s.n-s.i)
		s.i += k
		n -= k
	}
}

// result returns the error that ended the samples, or nil when the data simply
// ran out: data cut short decodes as zeros.
func (s *sampleReader) result() error {
	if s.err == io.EOF || s.err == io.ErrUnexpectedEOF {
		return nil
	}
	return s.err
}

// alphaOf reads a mask pixel as alpha: an *image.Alpha's alpha, or the gray
// level of anything else, which is what a soft mask is.
func alphaOf(c color.Color) uint8 {
	if a, ok := c.(color.Alpha); ok {
		return a.A
	}
	return color.GrayModel.Convert(c).(color.Gray).Y
}

// stream returns the ImageDecoder's payload stream, configuring it on first use.
func (d *ImageDecoder) stream() *Stream {
	if !d.ready {
		d.strm.ConfigureDefault()
		d.ready = true
	}
	return &d.strm
}

// readPalette reads an Indexed color space [/Indexed base hival lookup] into
// d.l's palette.
func (d *ImageDecoder) readPalette(pdf *PDF, r io.ReaderAt, cs Value, codec *Codec) error {
	l := &d.l
	cs, err := pdf.Deref(r, cs, codec)
	if err != nil {
		return err
	} else if !cs.IsArray() {
		return ErrUnsupportedImage
	}
	var els [4]Value
	n := 0
	err = codec.ArrayForEach(pdf, r, cs, func(v Value) bool {
		if n < len(els) {
			els[n] = v
		}
		n++
		return true
	})
	if err != nil {
		return err
	} else if n != 4 {
		return ErrUnsupportedImage
	}
	base, err := pdf.Deref(r, els[1], codec)
	if err != nil {
		return err
	}
	_, comps, err := pdf.colorSpace(r, base, codec)
	if err != nil {
		return err
	} else if comps != 1 && comps != 3 && comps != 4 {
		return ErrUnsupportedImage // A base this decoder cannot convert.
	}
	hival, err := pdf.Deref(r, els[2], codec)
	if err != nil {
		return err
	}
	hv, ok := hival.Int()
	if !ok || hv < 0 || hv > 255 {
		return ErrUnsupportedImage
	}
	size := int(hv+1) * comps
	switch lookup := els[3]; lookup.Tok {
	case piulex.TokString, piulex.TokHexString:
		// Appended in place: only a table longer than any palette's allocates.
		table, err := codec.AppendString(l.pal[:0], pdf, r, lookup)
		if err != nil {
			return err
		}
		n = copy(l.pal[:size], table)
	case piulex.TokR:
		v, err := pdf.Resolve(r, lookup.ObjectID(), codec)
		if err != nil {
			return err
		}
		// The image's own payload is opened after the palette is read, so
		// the two can share the stream.
		rd, err := pdf.OpenStream(r, v, d.stream(), codec)
		if err != nil {
			return err
		}
		n, err = io.ReadFull(rd, l.pal[:size])
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
	default:
		return ErrUnsupportedImage
	}
	// A short table leaves the high indices zero, as viewers do.
	clear(l.pal[n:size])
	l.n, l.npal = comps, int(hv+1)
	return nil
}

// readFloats reads numeric array v into dst and returns its length; zero when
// v is null, not a numeric array, or longer than dst.
func readFloats(dst []float64, pdf *PDF, r io.ReaderAt, v Value, codec *Codec) (int, error) {
	v, err := pdf.Deref(r, v, codec)
	if err != nil || !v.IsArray() {
		return 0, err
	}
	n, ok := 0, true
	err = codec.ArrayForEach(pdf, r, v, func(el Value) bool {
		f, isNum := el.Float()
		if !isNum || n == len(dst) {
			ok = false
			return false
		}
		dst[n] = f
		n++
		return true
	})
	if !ok {
		return 0, err
	}
	return n, err
}
