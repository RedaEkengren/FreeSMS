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

// The repair summary through the pages: the counter hands it over, the
// customer reads it on their link, the technician never reaches the hand-over
// and can suggest something worth doing later.
func TestTheRepairSummaryThroughThePages(t *testing.T) {
	ts, pool := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"SUM 001"}, "complaint": {"Squeal"}})
	jobID := job[strings.LastIndex(job, "/")+1:]

	if status := postStatus(t, desk, job+"/lines", url.Values{"kind": {"labour"}, "description": {"Replace brake pads"}, "quantity": {"1"}, "unit_price": {"800"}}); status != http.StatusSeeOther {
		t.Fatalf("adding the line answered %d", status)
	}
	if status := postStatus(t, tech, job+"/recommendations", url.Values{"body": {"Replace rear discs"}, "when": {"at the next service"}}); status != http.StatusSeeOther {
		t.Fatalf("a technician's suggestion answered %d", status)
	}
	if status, _ := page(t, tech, job+"/summary"); status != http.StatusForbidden {
		t.Errorf("a technician opened the hand-over page: %d", status)
	}
	if status := postStatus(t, tech, job+"/summary", url.Values{"delivered_to": {"x"}}); status != http.StatusForbidden {
		t.Errorf("a technician handed a car over: %d", status)
	}
	// Nothing has been done on a job that is only taken in.
	if status := postStatus(t, desk, job+"/summary", url.Values{"delivered_to": {"Greta"}}); status != http.StatusBadRequest {
		t.Errorf("handing over a job nothing was done on answered %d", status)
	}

	// The work is done: the line agreed and performed, the car in work.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE work_order_lines SET approved_at = now(), performed_at = now() WHERE work_order_id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE work_orders SET state = 'in_progress' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	_, draft := page(t, desk, job+"/summary")
	if !strings.Contains(draft, "Replace brake pads") || !strings.Contains(draft, "Replace rear discs") || !strings.Contains(draft, "Hand it over") {
		t.Fatalf("the hand-over page does not show the work and the suggestion: %s", draft)
	}
	status, _ := postBody(t, desk, job+"/summary", url.Values{"delivered_to": {"Greta's brother Karl"}, "instructions": {"Brake in the pads gently."}, "odometer": {"152 300"}})
	if status != http.StatusSeeOther && status != http.StatusOK {
		t.Fatalf("handing it over answered %d", status)
	}
	_, doc := page(t, desk, job+"/summary/1")
	for _, want := range []string{"Repair summary", "Karl", "Replace brake pads", "Brake in the pads gently.", "not a statement that the car is safe", "152"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the issued summary does not say %q: %s", want, doc)
		}
	}
	if strings.Contains(doc, "800") {
		t.Error("the summary carries a price")
	}

	// The customer's link shows it.
	_, linked := postBody(t, desk, job+"/link", url.Values{})
	m := regexp.MustCompile(`/k/([A-Za-z0-9_-]{20,})`).FindStringSubmatch(linked)
	if m == nil {
		t.Fatalf("no customer link on the page: %s", linked)
	}
	anon := &http.Client{}
	get := func(path string) (int, string) {
		resp, err := anon.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, own := get("/k/" + m[1]); status != http.StatusOK || !strings.Contains(own, "Read the repair summary") {
		t.Errorf("the customer's page answered %d without the summary link", status)
	}
	status, theirs := get("/k/" + m[1] + "/summary")
	if status != http.StatusOK || !strings.Contains(theirs, "Replace brake pads") || !strings.Contains(theirs, "Brake in the pads gently.") {
		t.Errorf("the customer's summary answered %d: %s", status, theirs)
	}
	if strings.Contains(theirs, "Put the record right") || strings.Contains(theirs, "/jobs/") {
		t.Error("the customer's copy carries the staff's controls or links")
	}
	if status, _ := get("/k/not-a-token/summary"); status != http.StatusNotFound {
		t.Errorf("a wrong link's summary answered %d, want 404", status)
	}
}
