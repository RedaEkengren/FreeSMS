package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// agreeAtTheCounter puts the job's lines to the customer and writes down their
// yes for the amount the quote comes to, as the front desk does on the page.
func agreeAtTheCounter(t *testing.T, ts, jobID string, desk *http.Client, pool *pgxpool.Pool) {
	t.Helper()
	if status := postStatus(t, desk, ts+"/jobs/"+jobID+"/quote", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("putting the quote answered %d", status)
	}
	var id string
	var gross int64
	if err := database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, gross_minor FROM quotes WHERE work_order_id = $1 ORDER BY version DESC LIMIT 1`,
			jobID).Scan(&id, &gross)
	}); err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, desk, ts+"/jobs/"+jobID+"/quote/"+id+"/answer", url.Values{
		"decision": {"approved"}, "via": {"counter"}, "agreed": {fmt.Sprintf("%d.%02d", gross/100, gross%100)}}); status != http.StatusSeeOther {
		t.Fatalf("writing down the yes answered %d", status)
	}
}

// From the job page: the customer says no with the car in pieces. The page
// asks what was done, the invoice carries exactly that, and the line the
// customer turned down says so.
func TestDecliningAJobAsksWhatWasDoneAndInvoicesOnlyThat(t *testing.T) {
	ts, pool := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	job := ts.URL + "/jobs/" + jobA

	for _, what := range []string{"Diagnosis", "Replace gearbox"} {
		if status := postStatus(t, desk, job+"/lines", url.Values{
			"kind": {"labour"}, "description": {what}, "quantity": {"1"}, "unit_price": {"1000"}}); status != http.StatusSeeOther {
			t.Fatalf("adding %s answered %d", what, status)
		}
	}
	for _, to := range []string{"estimated", "awaiting_approval", "approved", "in_progress"} {
		if to == "approved" {
			agreeAtTheCounter(t, ts.URL, jobA, desk, pool)
		}
		if status := postStatus(t, desk, job+"/state", url.Values{"state": {to}}); status != http.StatusSeeOther {
			t.Fatalf("moving to %s answered %d", to, status)
		}
	}
	_, body := page(t, desk, job)
	if !strings.Contains(body, "What was done before the customer said no") || !strings.Contains(body, `name="performed" value="1"`) {
		t.Fatal("declining a job in progress does not ask what was done")
	}

	// Not a line, and not a technician's decision.
	if status := postStatus(t, desk, job+"/state", url.Values{"state": {"declined"}, "performed": {"x"}}); status != http.StatusBadRequest {
		t.Errorf("a performed tick that is not a line answered %d", status)
	}
	if status := postStatus(t, tech, job+"/state", url.Values{"state": {"declined"}, "performed": {"1"}}); status != http.StatusForbidden {
		t.Errorf("a technician declining answered %d", status)
	}

	if status := postStatus(t, desk, job+"/state", url.Values{"state": {"declined"}, "performed": {"1"}}); status != http.StatusSeeOther {
		t.Fatalf("declining answered %d", status)
	}
	_, body = page(t, desk, job)
	if !strings.Contains(body, "Replace gearbox") || !strings.Contains(body, `class="tag warranty">declined`) {
		t.Error("the line the customer turned down is not marked declined")
	}
	if status := postStatus(t, desk, job+"/invoice", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("invoicing answered %d", status)
	}
	// The diagnosis alone: the gearbox was only offered.
	_, body = page(t, desk, job)
	m := regexp.MustCompile(`/invoices/([0-9a-f-]{36})`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the job page links to no invoice")
	}
	_, doc := page(t, desk, ts.URL+"/invoices/"+m[1])
	if !strings.Contains(doc, "Diagnosis") || strings.Contains(doc, "Replace gearbox") {
		t.Errorf("the invoice of a declined job is not made of what was performed")
	}
}

// The customer's page shows the quote with every amount, and their answer
// goes to the version they were shown. If the workshop has put a newer one
// forward in the meantime, the answer is not taken for it: they are sent to
// read the new one.
func TestTheCustomerAnswersTheQuoteTheyWereShown(t *testing.T) {
	ts, pool := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	job := ts.URL + "/jobs/" + jobA

	postStatus(t, desk, job+"/lines", url.Values{"kind": {"labour"}, "description": {"Brake pads"}, "quantity": {"1,5"}, "unit_price": {"895"}})
	if status := postStatus(t, desk, job+"/quote", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("putting the quote answered %d", status)
	}
	// The link, as the job page makes it.
	_, body := page(t, desk, job)
	_ = body
	resp := post(t, desk, job+"/link", url.Values{})
	m := regexp.MustCompile(`/k/([A-Za-z0-9_-]{20,})`).FindStringSubmatch(resp)
	if m == nil {
		t.Fatal("the job page gave no link")
	}
	token := m[1]
	customer := &http.Client{}

	_, status := page(t, customer, ts.URL+"/k/"+token)
	for _, want := range []string{"Brake pads", "1.5", "895.00", "1,342.50", "335.63", "1,678.13", "Yes, do it"} {
		if !strings.Contains(status, want) {
			t.Errorf("the customer's page does not show %q:\n%s", want, firstLines(status, 80))
		}
	}

	var quoteID string
	database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM quotes WHERE work_order_id = $1`, jobA).Scan(&quoteID)
	})

	// The workshop adds a line and puts the new version forward.
	postStatus(t, desk, job+"/lines", url.Values{"kind": {"labour"}, "description": {"Alignment"}, "quantity": {"1"}, "unit_price": {"500"}})
	postStatus(t, desk, job+"/quote", url.Values{})

	// The page the customer had open still posts to version 1.
	if status := postStatus(t, customer, ts.URL+"/k/"+token+"/quote/"+quoteID, url.Values{"decision": {"approved"}}); status != http.StatusSeeOther {
		t.Fatalf("the old form answered %d", status)
	}
	var n int
	database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM quote_decisions WHERE work_order_id = $1`, jobA).Scan(&n)
	})
	if n != 0 {
		t.Errorf("an answer to a replaced quote was recorded")
	}
	_, fresh := page(t, customer, ts.URL+"/k/"+token+"?changed=1")
	if !strings.Contains(fresh, "changed the quote while you were reading") || !strings.Contains(fresh, "Alignment") {
		t.Error("the customer is not shown that the quote changed, with the new one")
	}

	// And the page the desk reads says what was put forward and that nobody
	// has answered.
	_, deskPage := page(t, desk, job)
	if !strings.Contains(deskPage, "Version 2") || !strings.Contains(deskPage, "Waiting for the customer") {
		t.Error("the job page does not show the quote waiting for an answer")
	}
}
