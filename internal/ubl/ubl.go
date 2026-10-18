// Package ubl writes an invoice as a Peppol BIS Billing 3.0 document: UBL 2.1,
// the EN 16931 core invoice, which is the open format Swedish and European
// public buyers require and most invoicing services accept.
//
// It knows nothing about workshops. It takes a finished invoice as plain
// values and writes the XML, the way package sie does for the accountant. The
// figures are the ones frozen on the document and are never recomputed here,
// because an e-invoice that disagrees by an öre with the paper one it came
// from is a second document under the same number.
package ubl

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The identifiers the document declares itself to be, so that a receiving
// service knows which rules to check it against.
const (
	customizationID = "urn:cen.eu:en16931:2017#compliant#urn:fdc:peppol.eu:2017:poacc:billing:3.0"
	profileID       = "urn:fdc:peppol.eu:2017:poacc:billing:01:1.0"
)

// Party is the seller or the buyer.
type Party struct {
	Name       string
	Street     string
	PostalCode string
	City       string
	Country    string // ISO 3166 alpha-2

	// Where the party receives electronic invoices: a scheme from the
	// Peppol list (0007 is a Swedish company number) and the value in it.
	EndpointScheme, EndpointValue string

	VATID string // with the country prefix, such as SE556677889901
	// The company number and the scheme it is registered under.
	LegalID, LegalScheme string
}

// PaymentMeans is how the seller is paid, as one of three kinds.
type PaymentMeans struct {
	Kind    string // "bankgiro", "plusgiro" or "iban"
	Account string // the number, without a label or spaces
}

// Line is one line of the invoice, in the sign the paper document has it: a
// credit note's lines are negative, and are written positive here, because a
// UBL credit note is positive throughout and says by its type that it takes
// back.
type Line struct {
	Description   string
	UnitCode      string
	QuantityMilli int64
	UnitMinor     int64
	NetMinor      int64
	VATMinor      int64
	VATCategory   string
	RateBasis     int
	// Why no VAT is charged, for the categories that need it. Empty for S.
	ExemptionReason string
}

// Invoice is what is written.
type Invoice struct {
	// Credit is set on a credit note, which is a different UBL document.
	Credit bool
	// Reference is the number it is known by on paper: A-1001.
	Reference string
	// CreditOf is the invoice a credit note takes back.
	CreditOf       string
	IssueDate      time.Time
	DueDate        time.Time
	Currency       string
	BuyerReference string
	Seller, Buyer  Party
	Payment        PaymentMeans
	// Terms is the payment terms in words, such as "30 days net".
	Terms string
	Lines []Line
}

type vatKey struct {
	category string
	rate     int
}

type subtotal struct {
	taxable, tax int64
	reason       string
}

// Totals are the figures of the document, worked out from its lines.
type Totals struct {
	Net, Tax, Gross int64
}

// Sum adds the frozen line figures. The tax is the sum of what each line was
// frozen with, which is how the paper invoice reached its own total.
func (in Invoice) Sum() Totals {
	var t Totals
	for _, l := range in.Lines {
		t.Net += abs(l.NetMinor)
		t.Tax += abs(l.VATMinor)
	}
	t.Gross = t.Net + t.Tax
	return t
}

// Marshal writes the document.
func (in Invoice) Marshal() ([]byte, error) {
	if len(in.Lines) == 0 {
		return nil, fmt.Errorf("ubl: an invoice with no lines")
	}
	var w writer
	root, typeCode, lineTag, qtyTag := "Invoice", "InvoiceTypeCode", "InvoiceLine", "InvoicedQuantity"
	code := "380"
	if in.Credit {
		root, typeCode, lineTag, qtyTag, code = "CreditNote", "CreditNoteTypeCode", "CreditNoteLine", "CreditedQuantity", "381"
	}
	w.raw(xml.Header)
	w.open(root, `xmlns="urn:oasis:names:specification:ubl:schema:xsd:`+root+`-2"`+
		` xmlns:cac="urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2"`+
		` xmlns:cbc="urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2"`)

	w.text("cbc:CustomizationID", customizationID)
	w.text("cbc:ProfileID", profileID)
	w.text("cbc:ID", in.Reference)
	w.text("cbc:IssueDate", in.IssueDate.Format("2006-01-02"))
	if !in.Credit {
		w.text("cbc:DueDate", in.DueDate.Format("2006-01-02"))
	}
	w.text("cbc:"+typeCode, code)
	w.text("cbc:DocumentCurrencyCode", in.Currency)
	w.text("cbc:BuyerReference", in.BuyerReference)

	if in.Credit && in.CreditOf != "" {
		w.open("cac:BillingReference")
		w.open("cac:InvoiceDocumentReference")
		w.text("cbc:ID", in.CreditOf)
		w.close("cac:InvoiceDocumentReference")
		w.close("cac:BillingReference")
	}

	w.open("cac:AccountingSupplierParty")
	w.party(in.Seller)
	w.close("cac:AccountingSupplierParty")
	w.open("cac:AccountingCustomerParty")
	w.party(in.Buyer)
	w.close("cac:AccountingCustomerParty")

	w.open("cac:PaymentMeans")
	w.text("cbc:PaymentMeansCode", "30")
	w.text("cbc:PaymentID", in.Reference)
	w.open("cac:PayeeFinancialAccount")
	w.text("cbc:ID", in.Payment.Account)
	switch in.Payment.Kind {
	case "bankgiro":
		w.open("cac:FinancialInstitutionBranch")
		w.text("cbc:ID", "SE:BANKGIRO")
		w.close("cac:FinancialInstitutionBranch")
	case "plusgiro":
		w.open("cac:FinancialInstitutionBranch")
		w.text("cbc:ID", "SE:PLUSGIRO")
		w.close("cac:FinancialInstitutionBranch")
	}
	w.close("cac:PayeeFinancialAccount")
	w.close("cac:PaymentMeans")

	if in.Terms != "" && !in.Credit {
		w.open("cac:PaymentTerms")
		w.text("cbc:Note", in.Terms)
		w.close("cac:PaymentTerms")
	}

	groups := map[vatKey]*subtotal{}
	for _, l := range in.Lines {
		k := vatKey{l.VATCategory, l.RateBasis}
		g := groups[k]
		if g == nil {
			g = &subtotal{}
			groups[k] = g
		}
		g.taxable += abs(l.NetMinor)
		g.tax += abs(l.VATMinor)
		if g.reason == "" {
			g.reason = l.ExemptionReason
		}
	}
	keys := make([]vatKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].category != keys[j].category {
			return keys[i].category < keys[j].category
		}
		return keys[i].rate < keys[j].rate
	})

	tot := in.Sum()
	w.open("cac:TaxTotal")
	w.amount("cbc:TaxAmount", in.Currency, tot.Tax)
	for _, k := range keys {
		g := groups[k]
		w.open("cac:TaxSubtotal")
		w.amount("cbc:TaxableAmount", in.Currency, g.taxable)
		w.amount("cbc:TaxAmount", in.Currency, g.tax)
		w.open("cac:TaxCategory")
		w.text("cbc:ID", k.category)
		w.text("cbc:Percent", percent(k.rate))
		if g.reason != "" {
			w.text("cbc:TaxExemptionReason", g.reason)
		}
		w.open("cac:TaxScheme")
		w.text("cbc:ID", "VAT")
		w.close("cac:TaxScheme")
		w.close("cac:TaxCategory")
		w.close("cac:TaxSubtotal")
	}
	w.close("cac:TaxTotal")

	w.open("cac:LegalMonetaryTotal")
	w.amount("cbc:LineExtensionAmount", in.Currency, tot.Net)
	w.amount("cbc:TaxExclusiveAmount", in.Currency, tot.Net)
	w.amount("cbc:TaxInclusiveAmount", in.Currency, tot.Gross)
	w.amount("cbc:PayableAmount", in.Currency, tot.Gross)
	w.close("cac:LegalMonetaryTotal")

	for i, l := range in.Lines {
		w.open("cac:" + lineTag)
		w.text("cbc:ID", fmt.Sprint(i+1))
		w.quantity("cbc:"+qtyTag, l.UnitCode, abs(l.QuantityMilli))
		w.amount("cbc:LineExtensionAmount", in.Currency, abs(l.NetMinor))
		w.open("cac:Item")
		w.text("cbc:Name", l.Description)
		w.open("cac:ClassifiedTaxCategory")
		w.text("cbc:ID", l.VATCategory)
		w.text("cbc:Percent", percent(l.RateBasis))
		w.open("cac:TaxScheme")
		w.text("cbc:ID", "VAT")
		w.close("cac:TaxScheme")
		w.close("cac:ClassifiedTaxCategory")
		w.close("cac:Item")
		w.open("cac:Price")
		w.amount("cbc:PriceAmount", in.Currency, abs(l.UnitMinor))
		w.close("cac:Price")
		w.close("cac:" + lineTag)
	}

	w.close(root)
	return w.buf.Bytes(), w.err
}

type writer struct {
	buf   bytes.Buffer
	depth int
	err   error
}

func (w *writer) raw(s string) { w.buf.WriteString(s) }

func (w *writer) indent() { w.buf.WriteString(strings.Repeat("  ", w.depth)) }

func (w *writer) open(name string, attrs ...string) {
	w.indent()
	w.buf.WriteString("<" + name)
	for _, a := range attrs {
		w.buf.WriteString(" " + a)
	}
	w.buf.WriteString(">\n")
	w.depth++
}

func (w *writer) close(name string) {
	w.depth--
	w.indent()
	w.buf.WriteString("</" + name + ">\n")
}

func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// text writes an element, and nothing at all for a value that is empty: an
// empty element is a statement that the value is blank, not that it is unknown.
func (w *writer) text(name, value string, attrs ...string) {
	if value == "" {
		return
	}
	w.indent()
	w.buf.WriteString("<" + name)
	for _, a := range attrs {
		w.buf.WriteString(" " + a)
	}
	w.buf.WriteString(">" + escape(value) + "</" + name + ">\n")
}

func (w *writer) amount(name, currency string, minor int64) {
	w.text(name, money(minor), `currencyID="`+escape(currency)+`"`)
}

func (w *writer) quantity(name, unit string, milli int64) {
	w.text(name, decimal(milli, 3), `unitCode="`+escape(unit)+`"`)
}

func (w *writer) party(p Party) {
	w.open("cac:Party")
	if p.EndpointValue != "" {
		w.text("cbc:EndpointID", p.EndpointValue, `schemeID="`+escape(p.EndpointScheme)+`"`)
	}
	w.open("cac:PartyName")
	w.text("cbc:Name", p.Name)
	w.close("cac:PartyName")
	w.open("cac:PostalAddress")
	w.text("cbc:StreetName", p.Street)
	w.text("cbc:CityName", p.City)
	w.text("cbc:PostalZone", p.PostalCode)
	w.open("cac:Country")
	w.text("cbc:IdentificationCode", p.Country)
	w.close("cac:Country")
	w.close("cac:PostalAddress")
	if p.VATID != "" {
		w.open("cac:PartyTaxScheme")
		w.text("cbc:CompanyID", p.VATID)
		w.open("cac:TaxScheme")
		w.text("cbc:ID", "VAT")
		w.close("cac:TaxScheme")
		w.close("cac:PartyTaxScheme")
	}
	w.open("cac:PartyLegalEntity")
	w.text("cbc:RegistrationName", p.Name)
	if p.LegalID != "" {
		w.text("cbc:CompanyID", p.LegalID, `schemeID="`+escape(p.LegalScheme)+`"`)
	}
	w.close("cac:PartyLegalEntity")
	w.close("cac:Party")
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// money writes minor units as a decimal with two places, never through a float.
func money(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// decimal writes n thousandths (or hundredths) as a decimal, without trailing
// zeros: 2500 with three places is 2.5.
func decimal(n int64, places int) string {
	div := int64(1)
	for i := 0; i < places; i++ {
		div *= 10
	}
	whole, frac := n/div, n%div
	if frac == 0 {
		return fmt.Sprint(whole)
	}
	s := strings.TrimRight(fmt.Sprintf("%0*d", places, frac), "0")
	return fmt.Sprintf("%d.%s", whole, s)
}

// percent writes a rate in basis points as a percentage: 2500 is 25.
func percent(basis int) string { return decimal(int64(basis), 2) }
