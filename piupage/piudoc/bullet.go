package piudoc

import "github.com/soypat/piudf/piupage"

// BulletStyle is how a list marks and indents its items. It is declared once for
// a whole list, as [ColStyle] is for a row.
type BulletStyle struct {
	Marker string
	// Style is the marker's own, so a list can mark in an accent without
	// tinting the text beside it.
	Style Style
	// Indent is where the content starts, measured from the frame's left edge.
	Indent float64
}

// Bullet is one list item: a marker in the margin and content indented beside it.
// The content keeps the frame's full depth, so a long item breaks across pages
// where a two-column table would overflow.
func (bld *Builder) Bullet(bs BulletStyle, content Drawer) Drawer {
	b := &bullet{indent: bs.Indent, content: content}
	if bs.Marker != "" {
		b.marker = bld.Text(bs.Marker, bs.Style)
	}
	return b
}

// bullet is what [Builder.Bullet] returns.
type bullet struct {
	marker  *Paragraph
	indent  float64
	content Drawer
}

func (b *bullet) Draw(dst []piupage.Canvas, f Frame, yTop float64) (int, float64, error) {
	if b.marker != nil {
		// The marker is one run in the gutter: it never wraps and never breaks.
		mf := f
		mf.Width, mf.Bottom = b.indent, noBreak
		if _, _, err := b.marker.Draw(dst[:1], mf, yTop); err != nil {
			return 0, yTop, err
		}
	}
	if b.content == nil {
		return 0, yTop, nil
	}
	f.X, f.Width = f.X+b.indent, f.Width-b.indent
	return b.content.Draw(dst, f, yTop)
}

func (b *bullet) Unwrap() []Drawer { return []Drawer{b.content} }
