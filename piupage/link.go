package piupage

import "github.com/soypat/piudf"

// Link is an undrawn rectangular area of a page that resolves to a URI when clicked.
type Link struct {
	X, Y, W, H float64
	URI        string
}

// Rect returns the annotation's lower-left and upper-right corners. A Link
// stores an origin and a size; PDF's /Rect is written as opposite corners.
func (l Link) Rect() (x0, y0, x1, y1 float64) {
	return l.X, l.Y, l.X + l.W, l.Y + l.H
}

// WriteLink emits ln as a /Link annotation object and returns its id.
func WriteLink(enc *piudf.Encoder, ln Link) (piudf.ObjectID, error) {
	x0, y0, x1, y1 := ln.Rect()
	id := enc.NewID()
	enc.BeginObject(id)
	enc.DictOpen()
	enc.Name("Type")
	enc.Name("Annot")
	enc.Name("Subtype")
	enc.Name("Link")
	enc.Name("Rect")
	enc.ArrayOpen()
	enc.Real(x0)
	enc.Real(y0)
	enc.Real(x1)
	enc.Real(y1)
	enc.ArrayClose()
	// A zero border suppresses the legacy visible box viewers would otherwise
	// draw around the rectangle.
	enc.Name("Border")
	enc.ArrayOpen()
	enc.Int(0)
	enc.Int(0)
	enc.Int(0)
	enc.ArrayClose()
	enc.Name("A")
	enc.DictOpen()
	enc.Name("S")
	enc.Name("URI")
	// A URI is bytes, not a text string: it must not go through the UTF-16BE
	// treatment a /Title would get.
	enc.Name("URI")
	enc.String(ln.URI)
	enc.DictClose()
	enc.DictClose()
	enc.EndObject()
	return id, enc.Err()
}
