package piudoc

import (
	"image/color"
	"slices"
	"strings"

	"github.com/soypat/piudf/piupage"
)

// Splittable is a flowable that can be divided across a page boundary. The
// frame asks for a split only when the whole will not fit in what is left of
// the page; a flowable that would rather move than break returns a nil head.
type Splittable interface {
	Flowable
	SplitAt(availWidth, availHeight float64) (head, tail Flowable)
}

// Unwrappable is a grouping flowable that can give up its grouping. The frame
// unwraps a group only as a last resort — when it does not fit even on a page
// of its own — so that "keep these together" never becomes "run off the page".
type Unwrappable interface {
	Flowable
	Unwrap() []Flowable
}

// PageBreak ends the current page. It is inert at the top of a page, so a
// break placed before a section that already starts one does not leave a blank.
type PageBreak struct{}

// Wrap reports no size: the break is handled by the frame, not by layout.
func (PageBreak) Wrap(availWidth float64) (w, h float64) { return availWidth, 0 }

// Draw paints nothing.
func (PageBreak) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {}

// Bookmark names a place in the document for the reader's navigation pane. It
// occupies no space and draws nothing; the frame records where it landed and
// writes the outline tree at the end. Level 0 is a top-level entry, and a
// deeper level nests under the nearest preceding shallower one.
type Bookmark struct {
	Title string
	Level int
}

// Wrap reports no size.
func (Bookmark) Wrap(availWidth float64) (w, h float64) { return availWidth, 0 }

// Draw paints nothing.
func (Bookmark) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {}

// KeepTogether flows its children as one unit, moving the whole group to the
// next page rather than letting a page boundary fall inside it. The canonical
// use is a heading and the paragraph under it: a heading alone at the foot of
// a page is a promise the page does not keep.
type KeepTogether struct {
	Flows []Flowable
}

// Wrap measures the group as the sum of its children.
func (k *KeepTogether) Wrap(availWidth float64) (w, h float64) {
	for _, fl := range k.Flows {
		_, fh := fl.Wrap(availWidth)
		h += fh
	}
	return availWidth, h
}

// Draw stacks the children downward from yTop.
func (k *KeepTogether) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {
	for _, fl := range k.Flows {
		_, h := fl.Wrap(availWidth)
		fl.Draw(c, x, yTop, availWidth)
		yTop -= h
	}
}

// SplitAt breaks the group only inside the child that overflows, and only if
// that child is willing to be broken. Nothing before it moves, so the guarantee
// the group exists for still holds: a heading cannot be left behind by the
// content it introduces, because that content refuses to leave less of itself
// than is worth reading. A child that will not split moves the whole group.
func (k *KeepTogether) SplitAt(availWidth, availHeight float64) (head, tail Flowable) {
	var used float64
	for i, fl := range k.Flows {
		_, h := fl.Wrap(availWidth)
		if used+h <= availHeight {
			used += h
			continue
		}
		sp, ok := fl.(Splittable)
		if !ok {
			return nil, k
		}
		ch, ct := sp.SplitAt(availWidth, availHeight-used)
		if ch == nil {
			return nil, k
		}
		return &KeepTogether{Flows: append(slices.Clone(k.Flows[:i]), ch)},
			&KeepTogether{Flows: append([]Flowable{ct}, k.Flows[i+1:]...)}
	}
	return nil, k
}

// Unwrap surrenders the grouping, letting the children flow and break on their
// own terms. The frame calls it only for a group too tall for any page.
func (k *KeepTogether) Unwrap() []Flowable { return k.Flows }

// Bullet is a list item: a marker set in the left gutter and content indented
// past it, so wrapped lines align under the first line's text rather than
// under the marker.
type Bullet struct {
	// Marker is the bullet glyph or number, drawn in MarkerStyle.
	Marker      string
	MarkerStyle Style
	// Indent is the distance from the item's left edge to its content.
	Indent  float64
	Content Flowable
}

// Wrap measures the content against the width left after the indent.
func (b *Bullet) Wrap(availWidth float64) (w, h float64) {
	_, h = b.Content.Wrap(availWidth - b.Indent)
	return availWidth, h
}

// Draw paints the marker at the item's left edge and the content past Indent.
// Both start at yTop, so their first baselines agree.
func (b *Bullet) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {
	P(b.Marker, b.MarkerStyle).Draw(c, x, yTop, b.Indent)
	b.Content.Draw(c, x+b.Indent, yTop, availWidth-b.Indent)
}

// SplitAt breaks the item's content, keeping the marker with the head: a
// continuation on the next page is the same item, not a new one.
func (b *Bullet) SplitAt(availWidth, availHeight float64) (head, tail Flowable) {
	sp, ok := b.Content.(Splittable)
	if !ok {
		return nil, b
	}
	ch, ct := sp.SplitAt(availWidth-b.Indent, availHeight)
	if ch == nil {
		return nil, b
	}
	return &Bullet{Marker: b.Marker, MarkerStyle: b.MarkerStyle, Indent: b.Indent, Content: ch},
		&Bullet{Indent: b.Indent, Content: ct}
}

// Preformatted is a block of text laid out exactly as written: lines break
// only where the source breaks them and nothing is re-wrapped, which is the
// only honest way to set source code. Markup is not interpreted, so the text
// may contain angle brackets freely.
//
// It does not split, so a block taller than a page will overflow one.
type Preformatted struct {
	Text  string
	Style Style
	// Background fills the block behind the text; nil leaves the page showing.
	Background color.Color
	// Padding insets the text from the filled block's edges.
	Padding float64
	// RuleColor draws a rule down the block's left edge, the conventional
	// marker for a quoted or set-off passage. nil draws none.
	RuleColor color.Color
	RuleWidth float64
	// TabWidth is how many spaces a tab expands to; 0 means 4. No text font
	// has a tab glyph, so an unexpanded tab would draw as .notdef.
	TabWidth int
}

// lines returns the block's text as expanded, trailing-blank-free lines.
func (pre *Preformatted) lines() []string {
	tab := pre.TabWidth
	if tab == 0 {
		tab = 4
	}
	text := strings.ReplaceAll(pre.Text, "\t", strings.Repeat(" ", tab))
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// Wrap reports the block's height: one leading per source line, plus padding
// and the style's surrounding space.
func (pre *Preformatted) Wrap(availWidth float64) (w, h float64) {
	return availWidth, float64(len(pre.lines()))*pre.Style.leading() + 2*pre.Padding +
		pre.Style.SpaceBefore + pre.Style.SpaceAfter
}

// Draw paints the background, the optional edge rule, and the lines.
func (pre *Preformatted) Draw(c *piupage.Canvas, x, yTop, availWidth float64) {
	_, h := pre.Wrap(availWidth)
	top := yTop - pre.Style.SpaceBefore
	boxH := h - pre.Style.SpaceBefore - pre.Style.SpaceAfter
	if pre.Background != nil {
		c.FillRect(x, top-boxH, availWidth, boxH, pre.Background)
	}
	if pre.RuleColor != nil && pre.RuleWidth > 0 {
		c.FillRect(x, top-boxH, pre.RuleWidth, boxH, pre.RuleColor)
	}
	font := pre.Style.Face.face(pre.Style.Bold, pre.Style.Italic)
	if font == nil {
		font = resolveFont(baseFamily(pre.Style.family()), pre.Style.Bold, pre.Style.Italic)
	}
	c.SetFont(font, pre.Style.Size)
	lineH := pre.Style.leading()
	y := top - pre.Padding - 0.8*pre.Style.Size
	for _, line := range pre.lines() {
		c.Text(x+pre.Padding+pre.Style.LeftIndent, y, line, pre.Style.color())
		y -= lineH
	}
}
