package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// From the job page: the customer says no with the car in pieces. The page
// asks what was done, the invoice carries exactly that, and the line the
// customer turned down says so.
func TestDecliningAJobAsksWhatWasDoneAndInvoicesOnlyThat(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	job := ts.URL + "/jobs/" + jobA

	for _, what := range []string{"Diagnosis", "Replace gearbox"} {
		if status := postStatus(t, desk, job+"/lines", url.Values{
			"kind": {"labour"}, "description": {what}, "quantity": {"1"}, "unit_price": {"1000"}}); status != http.StatusSeeOther {
			t.Fatalf("adding %s answered %d", what, status)
		}
	}
	for _, to := range []string{"estimated", "awaiting_approval", "approved", "in_progress"} {
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
