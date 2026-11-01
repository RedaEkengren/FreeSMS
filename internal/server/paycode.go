package server

import (
	"html/template"

	"github.com/RedaEkengren/FreeSMS/internal/barcode"
	"github.com/RedaEkengren/FreeSMS/internal/payref"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// PayCode is the code a customer scans to pay an invoice (#234): Swish when
// the shop had a Swish number when it was issued, otherwise a bankgiro or
// plusgiro code, from the fields frozen on the invoice and the amount still
// owed. Drawn here as inline SVG: opening an invoice asks nobody else for
// anything. Nothing on a credit note, an invoice with nothing owed, or one
// issued before references were kept.
type PayCode struct {
	// "Swish" or "Bankgiro"/"Plusgiro", as a catalogue key.
	Kind string
	SVG  template.HTML
}

// PayCode is the document's payment code, or nil for none.
func (d pageData) PayCode(doc workshop.Document) *PayCode {
	if doc.CreditOfID != nil || doc.OwedMinor <= 0 || doc.PaymentOCR == "" || doc.PaidOnIssue != nil {
		return nil
	}
	var payload, kind string
	eci := 0
	if n := doc.Seller.SwishNumber; n != "" {
		p, err := payref.Swish(n, doc.OwedMinor, doc.PaymentOCR)
		if err != nil {
			return nil
		}
		payload, kind = p, "Swish"
	} else {
		giro := payref.GiroOf(doc.Seller.PaymentScheme, doc.Seller.PaymentReference)
		p, err := payref.UsingQR(doc.Seller.Name, doc.Seller.OrgNumber, doc.PaymentOCR,
			d.Local(doc.IssuedAt), d.Due(doc), doc.OwedMinor, giro)
		if err != nil {
			return nil
		}
		payload, kind, eci = p, map[string]string{"BG": "Bankgiro", "PG": "Plusgiro"}[giro.Type], payref.UsingQRECI
	}
	q, err := barcode.EncodeQR([]byte(payload), eci)
	if err != nil {
		return nil
	}
	// The SVG is built from digits and escaped text only.
	return &PayCode{Kind: kind, SVG: template.HTML(q.SVG(d.T("Scan to pay")))} //nolint:gosec
}
