package piudoc

import (
	"errors"
	"io"
	"slices"
	"unsafe"

	"github.com/soypat/piudf"
	"github.com/soypat/piudf/piupage"
)

var (
	errGroupCycle = errors.New("piudoc: group placed within itself")
	errGroupDepth = errors.New("piudoc: groups nested too deeply")
)

// maxGroupDepth bounds how deeply groups nest within groups. Every group is
// written once whatever the depth; the bound is on the recursion that finds
// them.
const maxGroupDepth = 32

// DrawGroup lays dr out on g, a canvas to be placed with
// [piupage.Canvas.Place] or [Group], in a frame width wide whose top edge is
// g's origin: the drawing hangs below it, so placing g at (x, y) puts its
// top-left corner there. It returns the height dr took, which is what a
// [Group] advances the story by. A group is one page, so dr never breaks.
func DrawGroup(g *piupage.Canvas, dr Drawer, width float64) (height float64, err error) {
	f := Frame{X: 0, Width: width, Top: 0, Bottom: noBreak}
	// Draw takes pages as a slice; g is the one page, viewed in place rather
	// than copied, since a Canvas holds its drawing by value.
	_, yEnd, err := dr.Draw(unsafe.Slice(g, 1), f, 0)
	if err != nil {
		return 0, err
	}
	return -yEnd, nil
}

// Group places a canvas at the story cursor: its origin at the cursor, which
// is its top-left corner when it was drawn with [DrawGroup]. The canvas is
// written once for the document however many pages or places draw it, by
// pointer as for [Image].
type Group struct {
	Canvas *piupage.Canvas
	// Width and Height are the room the group takes in the story, the width
	// it was laid out to and the height [DrawGroup] returned.
	Width, Height float64
	// Align places the group within the frame's width.
	Align Align
}

// Draw places the group below yTop, moving to the next page if it does not
// fit on this one.
func (gr *Group) Draw(dst []piupage.Canvas, f Frame, yTop float64) (adv int, yEnd float64, err error) {
	if gr.Canvas == nil {
		return 0, yTop, nil
	}
	y := yTop
	if y-gr.Height < f.Bottom && y < f.Top {
		adv++
		if adv >= len(dst) {
			return adv, y, io.ErrShortBuffer
		}
		y = f.Top
	}
	x := f.X
	switch gr.Align {
	case Center:
		x += (f.Width - gr.Width) / 2
	case Right:
		x += f.Width - gr.Width
	}
	dst[adv].Place(x, y, gr.Canvas)
	return adv, y - gr.Height, nil
}

// collectGroups registers the groups cv places, and the groups they place,
// reserving each a Form XObject id so that one may name another whichever is
// written first. A group found among the groups that place it is a cycle,
// which a reader would follow forever.
func (d *Doc) collectGroups(cv *piupage.Canvas) error {
	for _, g := range cv.Groups() {
		if slices.Contains(d.groupPath, g) {
			return errGroupCycle
		} else if d.groupidx(g) >= 0 {
			continue
		} else if len(d.groupPath) >= maxGroupDepth {
			return errGroupDepth
		}
		d.groups = append(d.groups, g)
		d.groupids = append(d.groupids, d.enc.NewID())
		d.groupPath = append(d.groupPath, g)
		err := d.collectGroups(g)
		d.groupPath = d.groupPath[:len(d.groupPath)-1]
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Doc) groupidx(g *piupage.Canvas) int {
	for i := range d.groups {
		if d.groups[i] == g {
			return i
		}
	}
	return -1
}

// writeGroup writes g as Form XObject id (ISO 32000-1 8.10): its drawing as
// the content stream, its resources by the names it drew them with, and its
// extent as the /BBox, outside of which a reader clips.
func (d *Doc) writeGroup(g *piupage.Canvas, id piudf.ObjectID) error {
	if err := g.Err(); err != nil {
		return err
	}
	x0, y0, x1, y1, _ := g.Extent()
	body := g.Bytes()
	d.enc.BeginObject(id)
	d.enc.DictOpen()
	d.enc.Name("Type")
	d.enc.Name("XObject")
	d.enc.Name("Subtype")
	d.enc.Name("Form")
	d.enc.Name("BBox")
	d.enc.ArrayOpen()
	d.enc.Real(x0)
	d.enc.Real(y0)
	d.enc.Real(x1)
	d.enc.Real(y1)
	d.enc.ArrayClose()
	d.writeResources(g)
	d.enc.Name("Length")
	d.enc.Int(int64(len(body)))
	d.enc.DictClose()
	d.enc.BeginStreamPayload()
	if _, err := d.enc.PayloadWriter().Write(body); err != nil {
		return err
	}
	d.enc.EndStreamPayload()
	d.enc.EndObject()
	return d.enc.Err()
}
