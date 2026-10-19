package ubl

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func sample() Invoice {
	return Invoice{
		Reference: "A-14", IssueDate: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC),
		DueDate: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), Currency: "SEK",
		BuyerReference: "Avd 4",
		Seller: Party{Name: "Verkstaden AB", Street: "Verkstadsgatan 1", PostalCode: "123 45", City: "Stockholm", Country: "SE",
			EndpointScheme: "0007", EndpointValue: "5566778899", VATID: "SE556677889901", LegalID: "5566778899", LegalScheme: "0007"},
		Buyer: Party{Name: "Åkeri & Söner <AB>", PostalCode: "111 22", City: "Göteborg", Country: "SE",
			EndpointScheme: "0007", EndpointValue: "5560160680", LegalID: "5560160680", LegalScheme: "0007"},
		Payment: PaymentMeans{Kind: "bankgiro", Account: "1234567"},
		Terms:   "30 days net",
		Lines: []Line{
			{Description: "Byte av bromsklossar", UnitCode: "HUR", QuantityMilli: 2500, UnitMinor: 89500, NetMinor: 223750, VATMinor: 55938, VATCategory: "S", RateBasis: 2500},
			{Description: "Motorolja 5W-30", UnitCode: "LTR", QuantityMilli: 4500, UnitMinor: 12900, NetMinor: 58050, VATMinor: 14513, VATCategory: "S", RateBasis: 2500},
			{Description: "Kaffe", UnitCode: "C62", QuantityMilli: 1000, UnitMinor: 5000, NetMinor: 5000, VATMinor: 300, VATCategory: "S", RateBasis: 600},
		},
	}
}

type amount struct {
	Currency string `xml:"currencyID,attr"`
	Value    string `xml:",chardata"`
}

type parsed struct {
	XMLName       xml.Name
	CustomizeID   string `xml:"CustomizationID"`
	ID            string `xml:"ID"`
	IssueDate     string `xml:"IssueDate"`
	DueDate       string `xml:"DueDate"`
	InvoiceType   string `xml:"InvoiceTypeCode"`
	CreditType    string `xml:"CreditNoteTypeCode"`
	BuyerRef      string `xml:"BuyerReference"`
	CreditOf      string `xml:"BillingReference>InvoiceDocumentReference>ID"`
	Seller        string `xml:"AccountingSupplierParty>Party>PartyLegalEntity>RegistrationName"`
	SellerBranch  string `xml:"PaymentMeans>PayeeFinancialAccount>FinancialInstitutionBranch>ID"`
	SellerAccount string `xml:"PaymentMeans>PayeeFinancialAccount>ID"`
	TaxTotal      amount `xml:"TaxTotal>TaxAmount"`
	Subtotals     []struct {
		Taxable  amount `xml:"TaxableAmount"`
		Tax      amount `xml:"TaxAmount"`
		Category string `xml:"TaxCategory>ID"`
		Percent  string `xml:"TaxCategory>Percent"`
	} `xml:"TaxTotal>TaxSubtotal"`
	Extension amount `xml:"LegalMonetaryTotal>LineExtensionAmount"`
	Exclusive amount `xml:"LegalMonetaryTotal>TaxExclusiveAmount"`
	Inclusive amount `xml:"LegalMonetaryTotal>TaxInclusiveAmount"`
	Payable   amount `xml:"LegalMonetaryTotal>PayableAmount"`
	Lines     []struct {
		ID       string `xml:"ID"`
		Quantity struct {
			Unit  string `xml:"unitCode,attr"`
			Value string `xml:",chardata"`
		} `xml:"InvoicedQuantity"`
		CreditedQuantity struct {
			Unit  string `xml:"unitCode,attr"`
			Value string `xml:",chardata"`
		} `xml:"CreditedQuantity"`
		Net  amount `xml:"LineExtensionAmount"`
		Name string `xml:"Item>Name"`
		Cat  string `xml:"Item>ClassifiedTaxCategory>ID"`
		Rate string `xml:"Item>ClassifiedTaxCategory>Percent"`
		Unit amount `xml:"Price>PriceAmount"`
	} `xml:"InvoiceLine"`
	CreditLines []struct {
		Net amount `xml:"LineExtensionAmount"`
		Qty struct {
			Value string `xml:",chardata"`
		} `xml:"CreditedQuantity"`
	} `xml:"CreditNoteLine"`
}

func read(t *testing.T, b []byte) parsed {
	t.Helper()
	var p parsed
	if err := xml.Unmarshal(b, &p); err != nil {
		t.Fatalf("what was written is not XML: %v\n%s", err, b)
	}
	return p
}

func TestAnInvoiceIsWrittenAsPeppolBIS3(t *testing.T) {
	b, err := sample().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	p := read(t, b)
	if p.XMLName.Local != "Invoice" || p.XMLName.Space != "urn:oasis:names:specification:ubl:schema:xsd:Invoice-2" {
		t.Errorf("root = %+v", p.XMLName)
	}
	if !strings.Contains(p.CustomizeID, "peppol.eu:2017:poacc:billing:3.0") {
		t.Errorf("it does not say which rules it follows: %q", p.CustomizeID)
	}
	if p.ID != "A-14" || p.IssueDate != "2026-10-18" || p.DueDate != "2026-11-17" || p.InvoiceType != "380" || p.BuyerRef != "Avd 4" {
		t.Errorf("header: %+v", p)
	}
	if p.SellerBranch != "SE:BANKGIRO" || p.SellerAccount != "1234567" {
		t.Errorf("payment means: %q %q", p.SellerBranch, p.SellerAccount)
	}
	if len(p.Lines) != 3 || p.Lines[0].Quantity.Unit != "HUR" || p.Lines[0].Quantity.Value != "2.5" || p.Lines[1].Quantity.Unit != "LTR" || p.Lines[0].Unit.Value != "895.00" {
		t.Errorf("lines: %+v", p.Lines)
	}
	// Names and markup characters reach the file as text and not as markup.
	if p.Lines[0].Name != "Byte av bromsklossar" || !strings.Contains(string(b), "Åkeri &amp; Söner &lt;AB&gt;") {
		t.Errorf("a name was not written as text:\n%s", b)
	}
}

// The arithmetic an electronic invoice is checked against by whoever receives it.
func TestTheTotalsOfTheDocumentAddUp(t *testing.T) {
	b, _ := sample().Marshal()
	p := read(t, b)
	// BR-CO-10: the lines add up to the line total.
	// 2237.50 + 580.50 + 50.00
	if p.Extension.Value != "2868.00" || p.Exclusive.Value != "2868.00" {
		t.Errorf("line total = %s / %s, want 2868.00", p.Extension.Value, p.Exclusive.Value)
	}
	// BR-CO-14: the tax total is the sum of the tax per category.
	// 559.38 + 145.13 = 704.51 at 25 per cent (the lines were rounded one by one), and 3.00 at 6.
	if p.TaxTotal.Value != "707.51" || len(p.Subtotals) != 2 {
		t.Fatalf("tax = %s in %d categories", p.TaxTotal.Value, len(p.Subtotals))
	}
	if p.Subtotals[0].Percent != "6" || p.Subtotals[0].Tax.Value != "3.00" || p.Subtotals[1].Percent != "25" || p.Subtotals[1].Taxable.Value != "2818.00" || p.Subtotals[1].Tax.Value != "704.51" {
		t.Errorf("subtotals: %+v", p.Subtotals)
	}
	// BR-CO-15: the total with tax is the total without plus the tax; BR-CO-16: it is what is payable.
	if p.Inclusive.Value != "3575.51" || p.Payable.Value != "3575.51" {
		t.Errorf("with tax %s, payable %s, want 3575.51", p.Inclusive.Value, p.Payable.Value)
	}
	for _, a := range []amount{p.TaxTotal, p.Extension, p.Inclusive, p.Payable} {
		if a.Currency != "SEK" {
			t.Errorf("an amount has currency %q", a.Currency)
		}
	}
}

// A UBL credit note is positive throughout and says so by its type; the paper
// one is negative.
func TestACreditNoteIsPositiveAndSaysWhatItTakesBack(t *testing.T) {
	in := sample()
	in.Credit, in.CreditOf, in.Reference = true, "A-14", "A-15"
	for i := range in.Lines {
		in.Lines[i].QuantityMilli, in.Lines[i].NetMinor, in.Lines[i].VATMinor = -in.Lines[i].QuantityMilli, -in.Lines[i].NetMinor, -in.Lines[i].VATMinor
	}
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	p := read(t, b)
	if p.XMLName.Local != "CreditNote" || p.CreditType != "381" || p.InvoiceType != "" || p.CreditOf != "A-14" {
		t.Errorf("credit note header: %+v", p)
	}
	if p.DueDate != "" {
		t.Errorf("a credit note has no due date, got %q", p.DueDate)
	}
	if p.Payable.Value != "3575.51" || len(p.CreditLines) != 3 || p.CreditLines[0].Net.Value != "2237.50" || p.CreditLines[0].Qty.Value != "2.5" {
		t.Errorf("amounts are not positive: payable %s lines %+v", p.Payable.Value, p.CreditLines)
	}
}

func TestAnExemptCategoryCarriesItsReason(t *testing.T) {
	in := sample()
	in.Lines = []Line{{Description: "Export", UnitCode: "C62", QuantityMilli: 1000, UnitMinor: 10000, NetMinor: 10000, VATCategory: "E", RateBasis: 0, ExemptionReason: "Exempt"}}
	b, _ := in.Marshal()
	if !strings.Contains(string(b), "<cbc:TaxExemptionReason>Exempt</cbc:TaxExemptionReason>") {
		t.Errorf("the reason is missing:\n%s", b)
	}
}

func TestNothingIsWrittenForAnInvoiceWithoutLines(t *testing.T) {
	in := sample()
	in.Lines = nil
	if _, err := in.Marshal(); err == nil {
		t.Error("an invoice with no lines was written")
	}
}

func TestDecimalsAreWrittenWithoutAFloat(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{money(5), "0.05"}, {money(100), "1.00"}, {money(-250), "-2.50"},
		{decimal(2500, 3), "2.5"}, {decimal(1000, 3), "1"}, {decimal(125, 3), "0.125"},
		{percent(2500), "25"}, {percent(1250), "12.5"}, {percent(600), "6"}, {percent(0), "0"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
