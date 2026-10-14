package piudoc

import (
	"bytes"
	"image"
	"testing"
)

// TestImageOncePerDocument draws one image on two pages and checks it is
// written once, and named in each page's resources.
func TestImageOncePerDocument(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 4, 2))
	d := &Doc{Size: PageSize{W: 200, H: 200}}
	// 150pt tall images on a 200pt page: the second cannot fit beside the first.
	story := []Drawer{
		&Image{Img: img, Height: 150},
		&Image{Img: img, Height: 150, Align: Right},
	}
	out := build(t, d, story, 2)
	if n := bytes.Count(out, []byte("/Subtype/Image")); n != 1 {
		t.Errorf("image written %d times, want once", n)
	}
	if n := bytes.Count(out, []byte("/XObject<<")); n != 2 {
		t.Errorf("%d pages name an XObject, want 2", n)
	}
	if n := bytes.Count(out, []byte("/Im1 Do")); n != 2 {
		t.Errorf("image drawn %d times, want 2", n)
	}
}

func TestImageSize(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 40, 20))
	for _, tc := range []struct {
		w, h, wantW, wantH float64
	}{
		{0, 0, 40, 20},
		{80, 0, 80, 40},
		{0, 10, 20, 10},
		{5, 5, 5, 5},
	} {
		im := Image{Img: img, Width: tc.w, Height: tc.h}
		if w, h := im.size(); w != tc.wantW || h != tc.wantH {
			t.Errorf("Width %v Height %v: size %vx%v, want %vx%v", tc.w, tc.h, w, h, tc.wantW, tc.wantH)
		}
	}
}
