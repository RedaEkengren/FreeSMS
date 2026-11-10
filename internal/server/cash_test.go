package server

import (
	"context"
	"io"
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

// The counter chooses which of the shop's flows a job paid at the counter
// went through; the owner then sees the flow in use, and on the list of
// what waits for them until its check is recorded.
func TestTheCounterSaysWhichFlowAJobWasPaidThrough(t *testing.T) {
	ts, pool := testServer(t)
	desk, owner := signIn(t, ts, advisorEmail), signIn(t, ts, ownerEmail)
	if status := postStatus(t, owner, ts.URL+"/payment-flows", url.Values{"name": {"Kort i disken"}, "method": {"card"}}); status != http.StatusSeeOther {
		t.Fatalf("noting a flow answered %d", status)
	}
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
	job := get(t, desk, ts.URL+"/jobs/"+jobA, http.StatusOK)
	m := regexp.MustCompile(`<option value="flow:([0-9a-f-]{36})">Kort i disken`).FindStringSubmatch(job)
	if m == nil {
		t.Fatal("the job page does not offer the shop's flow")
	}
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/invoice", url.Values{"paid_now": {"flow:" + m[1]}}); status != http.StatusSeeOther {
		t.Fatalf("invoiced and paid through the flow answered %d", status)
	}
	if _, flows := page(t, owner, ts.URL+"/payment-flows"); !strings.Contains(flows, "1 cash invoice issued through it") {
		t.Error("the flows page does not count the cash invoice through the flow")
	}
	if _, mine := page(t, owner, ts.URL+"/mine"); !strings.Contains(mine, "not checked against the cash register rules: Kort i disken") {
		t.Error("the owner's list does not have the flow in use and not checked")
	}
}

// A cash invoice asked for twice with one Idempotency-Key -- a phone that
// gave up waiting, the offline queue replaying -- is one invoice and one
// payment, and the second answer is the first (#156). The same key for a
// different request is refused, not answered with the first.
func TestARepeatedCashInvoiceRequestIsOneInvoiceAndOnePayment(t *testing.T) {
	ts, pool := testServer(t)
	desk := signIn(t, ts, advisorEmail)
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
	count := func(table string) int {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	invoices, payments := count("invoices"), count("invoice_payments")
	send := func(form url.Values, key string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/"+jobA+"/invoice", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+req.URL.Host)
		req.Header.Set(IdempotencyHeader, key)
		client := *desk
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			t.Logf("answer: %s", visibleText(string(b)))
		}
		return resp.StatusCode
	}
	form := url.Values{"paid_now": {"card"}, "paid_reference": {"Kvitto 1"}}
	if st := send(form, "cash-invoice-key-0001"); st != http.StatusSeeOther {
		t.Fatalf("the first request: %d", st)
	}
	if st := send(form, "cash-invoice-key-0001"); st != http.StatusSeeOther {
		t.Errorf("the repeat: %d, want the first answer again", st)
	}
	if n := count("invoices") - invoices; n != 1 {
		t.Errorf("%d invoices made by a request sent twice", n)
	}
	if n := count("invoice_payments") - payments; n != 1 {
		t.Errorf("%d payments made by a request sent twice", n)
	}
	if st := send(url.Values{"paid_now": {"card"}, "paid_reference": {"Kvitto 2"}}, "cash-invoice-key-0001"); st != http.StatusConflict {
		t.Errorf("the same key for a different request: %d, want %d", st, http.StatusConflict)
	}
}
