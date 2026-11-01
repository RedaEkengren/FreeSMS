package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The code on an invoice (#234): Swish when the invoice was issued with a
// Swish number, otherwise the bankgiro or plusgiro code, for what is still
// owed; and none on a credit note, on an invoice with nothing owed, on a
// cash invoice, or on one issued before references were kept.
func TestTheCodeOnAnInvoiceIsForWhatIsOwed(t *testing.T) {
	doc := workshop.Document{
		Invoice: workshop.Invoice{Number: 1, GrossMinor: 125000, IssuedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)},
		Seller: workshop.Seller{Name: "Verkstaden AB", OrgNumber: "556677-8899", PaymentReference: "Bankgiro 5555-5551",
			PaymentScheme: "bankgiro", PaymentTermsDays: 30},
		PaymentOCR: "136", OwedMinor: 125000,
	}
	var d pageData
	if c := d.PayCode(doc); c == nil || c.Kind != "Bankgiro" || !strings.HasPrefix(string(c.SVG), "<svg") || strings.Contains(string(c.SVG), "http://ex") {
		t.Errorf("a bankgiro invoice: %+v", c)
	}
	swish := doc
	swish.Seller.SwishNumber = "1234567890"
	if c := d.PayCode(swish); c == nil || c.Kind != "Swish" {
		t.Errorf("an invoice with a Swish number: %+v", c)
	}
	none := map[string]workshop.Document{}
	paid := doc
	paid.OwedMinor = 0
	none["nothing owed"] = paid
	old := doc
	old.PaymentOCR = ""
	none["issued before references"] = old
	credit := doc
	credit.CreditOfID = new("x")
	none["a credit note"] = credit
	iban := doc
	iban.Seller.PaymentScheme = "iban"
	none["an IBAN and no Swish"] = iban
	cash := doc
	cash.PaidOnIssue = &workshop.PaidOnIssue{}
	none["a cash invoice"] = cash
	for name, doc := range none {
		if c := d.PayCode(doc); c != nil {
			t.Errorf("%s has a code: %+v", name, c)
		}
	}
}

// On the page: the reference printed plainly and the code drawn inline,
// with nothing fetched from anywhere; a payment found by its reference,
// a mistyped one refused with the sentence why; and once it is paid, no
// code to pay it again.
func TestAnInvoiceIsPaidByItsReference(t *testing.T) {
	ts, pool := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	if _, err := pool.Exec(context.Background(), `UPDATE shops SET org_number = '556677-8899', payment_reference = 'Bankgiro 5555-5551',
		payment_scheme = 'bankgiro', swish_number = '1234567890' WHERE id = $1`, shopA); err != nil {
		t.Fatal(err)
	}
	post(t, desk, ts.URL+"/jobs/"+jobA+"/lines", url.Values{
		"kind": {"labour"}, "description": {"Replace front pads"},
		"quantity": {"2"}, "unit_price": {"500.00"}, "vat_rate": {"25"},
	})
	for _, state := range []string{"estimated", "awaiting_approval", "approved", "in_progress", "ready"} {
		if state == "approved" {
			agreeAtTheCounter(t, ts.URL, jobA, desk, pool)
		}
		post(t, desk, ts.URL+"/jobs/"+jobA+"/state", url.Values{"state": {state}})
	}
	post(t, desk, ts.URL+"/jobs/"+jobA+"/invoice", url.Values{})
	m := regexp.MustCompile(`/invoices/([0-9a-f-]{36})`).FindStringSubmatch(get(t, desk, ts.URL+"/jobs/"+jobA, http.StatusOK))
	if m == nil {
		t.Fatal("no invoice")
	}
	page := get(t, desk, ts.URL+"/invoices/"+m[1], http.StatusOK)
	ref := regexp.MustCompile(`Payment reference \(OCR\) <strong>([0-9]+)</strong>`).FindStringSubmatch(page)
	if ref == nil || !strings.Contains(page, `<figure class="paycode"><svg`) || !strings.Contains(page, "Scan to pay with Swish") {
		t.Fatalf("the invoice shows no reference or no code:\n%s", page)
	}
	wrong := ref[1][:len(ref[1])-1] + string('0'+(ref[1][len(ref[1])-1]-'0'+1)%10)
	if body := post(t, desk, ts.URL+"/payments/by-reference", url.Values{"ocr": {wrong}, "paid_on": {time.Now().Format("2006-01-02")}, "method": {"bank"}}); !strings.Contains(body, "check digit") {
		t.Errorf("a mistyped reference was not refused for its check digit:\n%s", visibleText(body))
	}
	if status := postStatus(t, desk, ts.URL+"/payments/by-reference", url.Values{"ocr": {ref[1]}, "paid_on": {time.Now().Format("2006-01-02")}, "method": {"bank"}}); status != http.StatusSeeOther {
		t.Fatalf("paid by its reference answered %d", status)
	}
	page = get(t, desk, ts.URL+"/invoices/"+m[1], http.StatusOK)
	if strings.Contains(page, `class="paycode"`) || !strings.Contains(page, ref[1]) {
		t.Errorf("a paid invoice still has a code to pay it with, or its payment lost the reference")
	}
}
