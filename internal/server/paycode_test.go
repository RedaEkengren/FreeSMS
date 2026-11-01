package server

import (
	"context"
	"fmt"
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
	if body := post(t, desk, ts.URL+"/payments/by-reference", url.Values{"ocr": {wrong}, "amount": {"500"}, "paid_on": {time.Now().Format("2006-01-02")}, "method": {"bank"}}); !strings.Contains(body, "check digit") {
		t.Errorf("a mistyped reference was not refused for its check digit:\n%s", visibleText(body))
	}
	// A bank payment is what arrived: no amount is not "all of it".
	if status := postStatus(t, desk, ts.URL+"/payments/by-reference", url.Values{"ocr": {ref[1]}, "paid_on": {time.Now().Format("2006-01-02")}, "method": {"bank"}}); status != http.StatusBadRequest {
		t.Errorf("paid by its reference with no amount answered %d", status)
	}
	if page := get(t, desk, ts.URL+"/invoices/"+m[1], http.StatusOK); !strings.Contains(page, `class="paycode"`) {
		t.Fatal("a payment with no amount settled the invoice")
	}
	if status := postStatus(t, desk, ts.URL+"/payments/by-reference", url.Values{"ocr": {ref[1]}, "amount": {"500"}, "paid_on": {time.Now().Format("2006-01-02")}, "method": {"bank"}}); status != http.StatusSeeOther {
		t.Fatalf("paid in part by its reference answered %d", status)
	}
	page = get(t, desk, ts.URL+"/invoices/"+m[1], http.StatusOK)
	if !strings.Contains(page, `class="paycode"`) || !strings.Contains(page, ref[1]) {
		t.Errorf("an invoice paid in part has no code for the rest, or its payment lost the reference")
	}
	var owed int64
	if err := pool.QueryRow(context.Background(), `SELECT gross_minor - (SELECT sum(amount_minor + rounding_minor) FROM invoice_payments WHERE invoice_id = $1)
		FROM invoices WHERE id = $1`, m[1]).Scan(&owed); err != nil || owed <= 0 {
		t.Fatalf("owed after paying in part: %d, %v", owed, err)
	}
	rest := fmt.Sprintf("%d.%02d", owed/100, owed%100)
	if status := postStatus(t, desk, ts.URL+"/payments/by-reference", url.Values{"ocr": {ref[1]}, "amount": {rest}, "paid_on": {time.Now().Format("2006-01-02")}, "method": {"bank"}}); status != http.StatusSeeOther {
		t.Fatalf("paid the rest by its reference answered %d", status)
	}
	if page := get(t, desk, ts.URL+"/invoices/"+m[1], http.StatusOK); strings.Contains(page, `class="paycode"`) {
		t.Errorf("a paid invoice still has a code to pay it with")
	}
}

// Payment details that do not read as the giro they say they are -- the
// number lost among the digits of a telephone number, or no number at
// all -- are said on the shop's page, rather than found as an invoice
// with no code. A telephone number after a whole giro is not read as part
// of it.
func TestThePaymentDetailsAreReadAsTheShopPageSays(t *testing.T) {
	ts, pool := testServer(t)
	owner := signIn(t, ts, ownerEmail)
	const warning = "do not read as a bankgiro or plusgiro number"
	for details, unread := range map[string]bool{
		"Bankgiro 5050-1055":           false,
		"Bankgiro 5050-1055 08 123 45": false,
		"Bankgiro 12345678901":         true,
		"Ring oss":                     true,
	} {
		if _, err := pool.Exec(context.Background(), `UPDATE shops SET payment_reference = $2, payment_scheme = 'bankgiro' WHERE id = $1`, shopA, details); err != nil {
			t.Fatal(err)
		}
		if page := visibleText(get(t, owner, ts.URL+"/shop", http.StatusOK)); strings.Contains(page, warning) != unread {
			t.Errorf("%q: warned %v, want %v", details, !unread, unread)
		}
	}
}
