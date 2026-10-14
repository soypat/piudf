package piudoc

import (
	"io"

	"github.com/soypat/piudf/piupage"
)

// Image draws a picture at the story cursor, the reportlab Image flowable
// analogue. The picture is written once for the document however many pages
// or places draw it.
//
// Once means once per Img value: images are told apart by ==, which for the
// pointers every image/... type is means by identity, not by content. Image
// flowables and OnPage drawings sharing one Img embed it once; equal pixels
// behind two pointers, or two *piupage.JPEG reset to the same file, are
// embedded once each. Build a picture once and reuse it.
type Image struct {
	Img piupage.Image
	// Width and Height are the drawn size in points. One left zero follows
	// from the other by the image's aspect ratio; both zero draw the image at
	// one point per pixel.
	Width, Height float64
	// Align places the image within the frame's width.
	Align Align
}

// Draw paints the image below yTop, moving to the next page if it does not fit
// on this one.
func (im *Image) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	if im.Img == nil {
		return 0, yTop, nil
	}
	w, h := im.size()
	y := yTop
	if y-h < f.Bottom && y < f.Top {
		adv++
		if adv >= len(dst) {
			return adv, y, io.ErrShortBuffer
		}
		y = f.Top
	}
	x := f.X
	switch im.Align {
	case Center:
		x += (f.Width - w) / 2
	case Right:
		x += f.Width - w
	}
	dst[adv].Image(x, y-h, w, h, im.Img)
	return adv, y - h, nil
}

// size resolves the drawn size from Width, Height and the image's pixels.
func (im *Image) size() (w, h float64) {
	b := im.Img.Bounds()
	pw, ph := float64(b.Dx()), float64(b.Dy())
	w, h = im.Width, im.Height
	switch {
	case pw <= 0 || ph <= 0:
		return w, h
	case w <= 0 && h <= 0:
		return pw, ph
	case w <= 0:
		return h * pw / ph, h
	case h <= 0:
		return w, w * ph / pw
	}
	return w, h
}
