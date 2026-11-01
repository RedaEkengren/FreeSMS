package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// At the counter, a job is invoiced and paid in one step, and the document
// says it was paid and how; the payment list says a recorded payment is no
// cash-register receipt. The owner keeps the ways money is taken in person
// and the year's figure; the front desk is not shown them.
func TestAJobIsInvoicedAndPaidAtTheCounter(t *testing.T) {
	ts, pool := testServer(t)
	desk, owner := signIn(t, ts, advisorEmail), signIn(t, ts, ownerEmail)
	post(t, desk, ts.URL+"/jobs/"+jobA+"/lines", url.Values{
		"kind": {"labour"}, "description": {"Replace front pads"},
		"quantity": {"2.5"}, "unit_price": {"895.00"}, "vat_rate": {"25"},
	})
	for _, state := range []string{"estimated", "awaiting_approval", "approved", "in_progress", "ready"} {
		if state == "approved" {
			agreeAtTheCounter(t, ts.URL, jobA, desk, pool)
		}
		post(t, desk, ts.URL+"/jobs/"+jobA+"/state", url.Values{"state": {state}})
	}
	if job := get(t, desk, ts.URL+"/jobs/"+jobA, http.StatusOK); !strings.Contains(job, "Paid now, at the counter") || !strings.Contains(job, "not a cash-register receipt") {
		t.Error("the job page offers no cash invoice, or does not say a payment is no receipt")
	}
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/invoice", url.Values{"paid_now": {"bank"}}); status != http.StatusBadRequest {
		t.Errorf("a cash invoice paid by bank answered %d", status)
	}
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/invoice", url.Values{"paid_now": {"card"}, "paid_reference": {"Kvitto 4711"}}); status != http.StatusSeeOther {
		t.Fatalf("invoiced and paid by card answered %d", status)
	}
	job := get(t, desk, ts.URL+"/jobs/"+jobA, http.StatusOK)
	m := regexp.MustCompile(`/invoices/([0-9a-f-]{36})`).FindStringSubmatch(job)
	if m == nil {
		t.Fatal("the job page links to no invoice")
	}
	doc := get(t, desk, ts.URL+"/invoices/"+m[1], http.StatusOK)
	for _, want := range []string{"<strong>Paid</strong>", "Kvitto 4711", "These are notes that money arrived, not cash-register receipts", "settled"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the cash invoice lacks %q", want)
		}
	}
	if strings.Contains(doc, "Pay to ") {
		t.Error("a cash invoice asks to be paid")
	}
	// Its payment stands with it: no reverse button, a credit note instead.
	if strings.Contains(doc, "/reverse") || !strings.Contains(doc, "a mistake is corrected with a credit note") {
		t.Error("a cash invoice's payment offers to be reversed, or does not say it is corrected with a credit note")
	}

	if status, _ := page(t, desk, ts.URL+"/payment-flows"); status != http.StatusForbidden {
		t.Errorf("the front desk opened the payment flows: %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/payment-flows", url.Values{"name": {"Kort i disken"}, "method": {"card"}}); status != http.StatusSeeOther {
		t.Fatalf("noting a flow answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/payment-flows/threshold", url.Values{"threshold": {"236800"}, "year_starts": {"1"}}); status != http.StatusSeeOther {
		t.Fatalf("setting the threshold answered %d", status)
	}
	_, flows := page(t, owner, ts.URL+"/payment-flows")
	for _, want := range []string{"Kort i disken", "not checked", "236", "Taken in cash, by card or by Swish since", "not a cash register"} {
		if !strings.Contains(flows, want) {
			t.Errorf("the payment flows page lacks %q", want)
		}
	}
}
