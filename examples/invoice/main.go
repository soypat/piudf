// Command invoice reproduces local/make_invoice.py — a one-page commercial
// invoice — using the piudf flowable layer (piupage + piupage/piudoc) instead
// of reportlab. It writes the PDF to the path given as the first argument, or
// invoice.pdf by default.
package main

import (
	"fmt"
	"os"
	"time"

	canvas "github.com/soypat/piudf/piupage"
	doc "github.com/soypat/piudf/piupage/piudoc"
)

const mm = canvas.MM

// Palette, mirroring the Python DARK/GREY/LIGHT hex colors.
var (
	DARK  = canvas.HexColor("#1a1a1a")
	GREY  = canvas.HexColor("#555555")
	LIGHT = canvas.HexColor("#e8e8e8")
)

func main() {
	out := "invoice.pdf"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	f, err := os.Create(out)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	// Styles, mirroring the reportlab ParagraphStyles.
	base := doc.Style{Font: "Helvetica", Size: 9.5, Leading: 13, Color: DARK}
	small := doc.Style{Font: "Helvetica", Size: 8.5, Leading: 11.5, Color: GREY}
	label := doc.Style{Font: "Helvetica", Size: 8, Leading: 11, Color: GREY, SpaceAfter: 1}
	h1 := doc.Style{Font: "Helvetica-Bold", Size: 20, Leading: 24, Color: DARK}
	smallBold := doc.Style{Font: "Helvetica-Bold", Size: 8.5, Leading: 11.5, Color: GREY}
	tot := doc.Style{Font: "Helvetica", Size: 12, Leading: 15, Color: DARK}
	start := time.Now()
	d := &doc.Doc{
		Size:    doc.SizeA4(),
		Margins: doc.Margins{Left: 22 * mm, Right: 22 * mm, Top: 20 * mm, Bottom: 20 * mm},
		Title:   "Commercial Invoice 0001-2026",
		Author:  "Jane Doe",
	}

	var story []doc.Drawer

	// Header: title + invoice meta.
	var metaStyle doc.TableStyle
	metaStyle.Valign(0, 0, -1, -1, doc.Top).
		Align(1, 0, 1, 0, doc.Right).
		Pad(0, 0, -1, -1, 0, 0, 0, 0)
	story = append(story, &doc.Table{
		ColWidths: []float64{118 * mm, 48 * mm},
		Style:     metaStyle,
		Rows: [][]doc.Cell{{
			doc.Cell{Drawer: doc.P("COMMERCIAL INVOICE", h1)},
			doc.Cell{Drawer: doc.P(
				`<font size="8" color="#555555">INVOICE No.</font><br/>`+
					`<b>0001-2026</b><br/>`+
					`<font size="8" color="#555555">DATE</font><br/>`+
					`<b>July 7, 2026</b>`, base)},
		}},
	})
	story = append(story, doc.Spacer{H: 6})
	story = append(story, doc.HRule{Thickness: 1.2, Color: DARK})
	story = append(story, doc.Spacer{H: 12})

	// From / To.
	seller := doc.P(
		`<font size="8" color="#555555"><b>FROM (SELLER)</b></font><br/>`+
			`<b>Jane Doe</b><br/>`+
			`Independent Software Developer<br/>`+
			`123 Example Avenue (A0000)<br/>`+
			`Sample City, Country<br/>`+
			`Tax ID: 00-00000000-0`, base)
	buyer := doc.P(
		`<font size="8" color="#555555"><b>BILL TO (BUYER)</b></font><br/>`+
			`<b>Acme GmbH</b><br/>`+
			"c/o Beispielstraße 2-4<br/>"+
			"Musterstraße 12, 10000 Berlin<br/>"+
			`Germany<br/>`+
			`Amtsgericht Berlin (Charlottenburg), HRB 000000 B`, base)
	var ftStyle doc.TableStyle
	ftStyle.Valign(0, 0, -1, -1, doc.Top).Pad(0, 0, -1, -1, 0, 0, 0, 0)
	story = append(story, &doc.Table{
		ColWidths: []float64{83 * mm, 83 * mm},
		Style:     ftStyle,
		Rows:      [][]doc.Cell{{doc.Cell{Drawer: seller}, doc.Cell{Drawer: buyer}}},
	})
	story = append(story, doc.Spacer{H: 16})

	// Line items.
	desc := doc.P(
		"<b>Software development services — milestone payment</b><br/>"+
			"Integration of networking stack into a client VPN software "+
			"(overlay network). Design, implementation and testing "+
			"of the agreed milestone.<br/>"+
			`<font size="8" color="#555555">Service period: June – July 2026. `+
			"Services rendered remotely.</font>", base)
	var itemsStyle doc.TableStyle
	itemsStyle.
		LineBelow(0, 0, -1, 0, 0.8, DARK).
		LineBelow(0, 1, -1, 1, 0.4, LIGHT).
		Valign(0, 0, -1, -1, doc.Top).
		Align(1, 0, 1, -1, doc.Right).
		Pad(0, 0, -1, -1, 0, 0, 2, 2).
		Pad(0, 1, -1, 1, 0, 0, 8, 10)
	story = append(story, &doc.Table{
		ColWidths: []float64{136 * mm, 30 * mm},
		Style:     itemsStyle,
		Rows: [][]doc.Cell{
			{doc.Cell{Drawer: doc.P("<b>DESCRIPTION</b>", smallBold)}, doc.Cell{Drawer: doc.P("<b>AMOUNT (USD)</b>", smallBold)}},
			{doc.Cell{Drawer: desc}, doc.Cell{Drawer: doc.P("343.00", base)}},
		},
	})
	story = append(story, doc.Spacer{H: 4})

	// Totals.
	var totStyle doc.TableStyle
	totStyle.Align(1, 0, -1, -1, doc.Right).Pad(0, 0, -1, -1, 0, 0, 2, 2)
	story = append(story, &doc.Table{
		ColWidths: []float64{96 * mm, 40 * mm, 30 * mm},
		Style:     totStyle,
		Rows: [][]doc.Cell{{
			doc.TextCell(""),
			doc.Cell{Drawer: doc.P(`<font size="8" color="#555555">TOTAL</font>`, base)},
			doc.Cell{Drawer: doc.P("<b>USD 343.00</b>", tot)},
		}},
	})
	story = append(story, doc.Spacer{H: 18})

	// Payment details.
	story = append(story, doc.P("<b>PAYMENT DETAILS</b>", label))
	story = append(story, doc.Spacer{H: 3})
	payRow := func(k, v string) []doc.Cell {
		return []doc.Cell{{Drawer: doc.P(k, small)}, {Drawer: doc.P(v, base)}}
	}
	var payStyle doc.TableStyle
	payStyle.Valign(0, 0, -1, -1, doc.Top).Pad(0, 0, -1, -1, 0, 6, 0, 3)
	story = append(story, &doc.Table{
		ColWidths: []float64{52 * mm, 114 * mm},
		Style:     payStyle,
		Rows: [][]doc.Cell{
			payRow("Payment method", "Wire transfer (USD)"),
			payRow("Payment date", "July 9, 2026"),
			payRow("Transaction ID", "0000000000"),
			payRow("Transfer ID", "0000000000000000"),
			payRow("Beneficiary account", "XX00 0000 0000 0000 0000 00"),
			payRow("Note", "USD 15.00 transfer fee deducted at withdrawal; "+
				"net amount transferred: USD 328.00."),
		},
	})
	story = append(story, doc.Spacer{H: 14})

	story = append(story, doc.HRule{Thickness: 0.5, Color: LIGHT})
	story = append(story, doc.Spacer{H: 8})
	// The footnote's <a href> becomes a /Link annotation on whichever page it lands on.
	footnote := small
	footnote.Link = doc.LinkStyle{Color: canvas.HexColor("#0645AD"), Underline: true}
	story = append(story, doc.P(
		"Export of services rendered to a foreign customer. "+
			"Not subject to local VAT — exportación de servicios. "+
			`Terms at <a href="https://pkg.go.dev/github.com/soypat/piudf">pkg.go.dev/github.com/soypat/piudf</a>.`,
		footnote))

	// The document owns no memory of its own: the pages it may use, their
	// content buffers and the encoder's are all supplied here.
	const maxPages = 4
	pages := make([]canvas.Canvas, maxPages)
	if err := d.Build(f, pages, story, make([]byte, 4096), make([]byte, maxPages*4096)); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", out, "in", time.Since(start).Round(time.Microsecond))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "invoice:", err)
	os.Exit(1)
}
