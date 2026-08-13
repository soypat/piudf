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

// mm is the unit this invoice is laid out in: every length below is a number
// of millimetres, and the point never appears.
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

	// Styles, mirroring the reportlab ParagraphStyles. Type is sized in points
	// the way type always is, then stated in the document's millimetres.
	base := doc.Style{Font: "Helvetica", Size: 9.5, Leading: 13, Color: DARK}.In(mm)
	small := doc.Style{Font: "Helvetica", Size: 8.5, Leading: 11.5, Color: GREY}.In(mm)
	label := doc.Style{Font: "Helvetica", Size: 8, Leading: 11, Color: GREY, SpaceAfter: 1}.In(mm)
	h1 := doc.Style{Font: "Helvetica-Bold", Size: 20, Leading: 24, Color: DARK}.In(mm)
	smallBold := doc.Style{Font: "Helvetica-Bold", Size: 8.5, Leading: 11.5, Color: GREY}.In(mm)
	tot := doc.Style{Font: "Helvetica", Size: 12, Leading: 15, Color: DARK}.In(mm)
	// pt states the typographic lengths — rule thicknesses, vertical rhythm,
	// cell padding — in the millimetres everything else here is measured in.
	pt := mm.FromPt
	start := time.Now()
	d := doc.New(f, mm, doc.A4.In(mm), doc.Margins{Left: 22, Right: 22, Top: 20, Bottom: 20})
	d.Title = "Commercial Invoice 0001-2026"
	d.Author = "Jane Doe"

	var story []doc.Flowable

	// Header: title + invoice meta.
	var metaStyle doc.TableStyle
	metaStyle.Valign(0, 0, -1, -1, doc.Top).
		Align(1, 0, 1, 0, doc.Right).
		Pad(0, 0, -1, -1, 0, 0, 0, 0)
	story = append(story, &doc.Table{
		ColWidths: []float64{118, 48},
		Style:     metaStyle,
		Rows: [][]doc.Cell{{
			doc.FlowCell(doc.P("COMMERCIAL INVOICE", h1)),
			doc.FlowCell(doc.P(
				`<font size="8pt" color="#555555">INVOICE No.</font><br/>`+
					`<b>0001-2026</b><br/>`+
					`<font size="8pt" color="#555555">DATE</font><br/>`+
					`<b>July 7, 2026</b>`, base)),
		}},
	})
	story = append(story, doc.Spacer{H: pt(6)})
	story = append(story, doc.HRule{Thickness: pt(1.2), Color: DARK})
	story = append(story, doc.Spacer{H: pt(12)})

	// From / To.
	seller := doc.P(
		`<font size="8pt" color="#555555"><b>FROM (SELLER)</b></font><br/>`+
			`<b>Jane Doe</b><br/>`+
			`Independent Software Developer<br/>`+
			`123 Example Avenue (A0000)<br/>`+
			`Sample City, Country<br/>`+
			`Tax ID: 00-00000000-0`, base)
	buyer := doc.P(
		`<font size="8pt" color="#555555"><b>BILL TO (BUYER)</b></font><br/>`+
			`<b>Acme GmbH</b><br/>`+
			"c/o Beispielstraße 2-4<br/>"+
			"Musterstraße 12, 10000 Berlin<br/>"+
			`Germany<br/>`+
			`Amtsgericht Berlin (Charlottenburg), HRB 000000 B`, base)
	var ftStyle doc.TableStyle
	ftStyle.Valign(0, 0, -1, -1, doc.Top).Pad(0, 0, -1, -1, 0, 0, 0, 0)
	story = append(story, &doc.Table{
		ColWidths: []float64{83, 83},
		Style:     ftStyle,
		Rows:      [][]doc.Cell{{doc.FlowCell(seller), doc.FlowCell(buyer)}},
	})
	story = append(story, doc.Spacer{H: pt(16)})

	// Line items.
	desc := doc.P(
		"<b>Software development services — milestone payment</b><br/>"+
			"Integration of networking stack into a client VPN software "+
			"(overlay network). Design, implementation and testing "+
			"of the agreed milestone.<br/>"+
			`<font size="8pt" color="#555555">Service period: June – July 2026. `+
			"Services rendered remotely.</font>", base)
	var itemsStyle doc.TableStyle
	itemsStyle.
		LineBelow(0, 0, -1, 0, pt(0.8), DARK).
		LineBelow(0, 1, -1, 1, pt(0.4), LIGHT).
		Valign(0, 0, -1, -1, doc.Top).
		Align(1, 0, 1, -1, doc.Right).
		Pad(0, 0, -1, -1, 0, 0, pt(2), pt(2)).
		Pad(0, 1, -1, 1, 0, 0, pt(8), pt(10))
	story = append(story, &doc.Table{
		ColWidths: []float64{136, 30},
		Style:     itemsStyle,
		Rows: [][]doc.Cell{
			{doc.FlowCell(doc.P("<b>DESCRIPTION</b>", smallBold)), doc.FlowCell(doc.P("<b>AMOUNT (USD)</b>", smallBold))},
			{doc.FlowCell(desc), doc.FlowCell(doc.P("343.00", base))},
		},
	})
	story = append(story, doc.Spacer{H: pt(4)})

	// Totals.
	var totStyle doc.TableStyle
	totStyle.Align(1, 0, -1, -1, doc.Right).Pad(0, 0, -1, -1, 0, 0, pt(2), pt(2))
	story = append(story, &doc.Table{
		ColWidths: []float64{96, 40, 30},
		Style:     totStyle,
		Rows: [][]doc.Cell{{
			doc.TextCell(""),
			doc.FlowCell(doc.P(`<font size="8pt" color="#555555">TOTAL</font>`, base)),
			doc.FlowCell(doc.P("<b>USD 343.00</b>", tot)),
		}},
	})
	story = append(story, doc.Spacer{H: pt(18)})

	// Payment details.
	story = append(story, doc.P("<b>PAYMENT DETAILS</b>", label))
	story = append(story, doc.Spacer{H: pt(3)})
	payRow := func(k, v string) []doc.Cell {
		return []doc.Cell{doc.FlowCell(doc.P(k, small)), doc.FlowCell(doc.P(v, base))}
	}
	var payStyle doc.TableStyle
	payStyle.Valign(0, 0, -1, -1, doc.Top).Pad(0, 0, -1, -1, 0, pt(6), 0, pt(3))
	story = append(story, &doc.Table{
		ColWidths: []float64{52, 114},
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
	story = append(story, doc.Spacer{H: pt(14)})

	story = append(story, doc.HRule{Thickness: pt(0.5), Color: LIGHT})
	story = append(story, doc.Spacer{H: pt(8)})
	story = append(story, doc.P(
		"Export of services rendered to a foreign customer. "+
			"Not subject to local VAT — exportación de servicios.", small))

	if err := d.Build(story); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", out, "in", time.Since(start).Round(time.Microsecond))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "invoice:", err)
	os.Exit(1)
}
