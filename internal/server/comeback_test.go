package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// A car that comes back, through the pages: the counter opens the return and
// takes a position with a reason; the technician sees what was reported on the
// job and nothing about money, and cannot reach the case.
func TestACarThatComesBackThroughThePages(t *testing.T) {
	ts, pool := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"RET 001"}, "complaint": {"Brakes"}})
	jobID := job[strings.LastIndex(job, "/")+1:]

	// Not finished: nothing to come back from.
	if status := postStatus(t, desk, job+"/comeback", url.Values{"action": {"open"}, "symptoms": {"Squeal"}, "returned_on": {"2026-10-12"}}); status != http.StatusBadRequest {
		t.Errorf("a return on an unfinished job answered %d", status)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE work_orders SET state = 'ready' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	if status, _ := page(t, tech, job+"/comeback"); status != http.StatusForbidden {
		t.Errorf("a technician opened the case page: %d", status)
	}
	if status := postStatus(t, tech, job+"/comeback", url.Values{"action": {"open"}, "symptoms": {"x"}, "returned_on": {"2026-10-12"}}); status != http.StatusForbidden {
		t.Errorf("a technician opened a return: %d", status)
	}
	if status := postStatus(t, desk, job+"/comeback", url.Values{"action": {"open"}, "symptoms": {"Squeal"}, "returned_on": {"nonsense"}}); status != http.StatusBadRequest {
		t.Errorf("a bad date answered %d", status)
	}

	if status := postStatus(t, desk, job+"/comeback", url.Values{
		"action": {"open"}, "symptoms": {"Squeals again at low speed"}, "returned_on": {"2026-10-12"}, "odometer": {"81 500"}}); status != http.StatusSeeOther {
		t.Fatalf("opening the return answered %d", status)
	}
	var follow string
	if err := pool.QueryRow(context.Background(), `SELECT follow_up_work_order_id::text FROM comebacks`).Scan(&follow); err != nil {
		t.Fatal(err)
	}
	status, body := page(t, desk, base+"/jobs/"+follow+"/comeback")
	if status != http.StatusOK {
		t.Fatalf("the case page answered %d", status)
	}
	for _, want := range []string{"Squeals again at low speed", "12 Oct 2026", "81", "Looking into it decides nothing"} {
		if !strings.Contains(body, want) {
			t.Errorf("the case page does not say %q", want)
		}
	}

	// The technician's job page says what was reported and nothing more.
	_, tj := page(t, tech, base+"/jobs/"+follow)
	if !strings.Contains(tj, "Squeals again at low speed") || !strings.Contains(tj, "Return visit") {
		t.Errorf("the technician's job does not say it is a return visit")
	}
	if strings.Contains(tj, "/comeback") {
		t.Error("the technician is offered the case page")
	}
	// The original says it was come back to.
	_, dj := page(t, desk, job)
	if !strings.Contains(dj, "come back to") {
		t.Error("the original job does not say it was come back to")
	}

	// A position needs its reason.
	cs := regexp.MustCompile(`name="case" value="([0-9a-f-]{36})"`).FindStringSubmatch(body)
	if cs == nil {
		t.Fatal("no case on the page")
	}
	if status := postStatus(t, desk, base+"/jobs/"+follow+"/comeback", url.Values{"action": {"classify"}, "case": {cs[1]}, "kind": {"warranty"}}); status != http.StatusBadRequest {
		t.Errorf("warranty without a reason answered %d", status)
	}
	if status, _ := postBody(t, desk, base+"/jobs/"+follow+"/comeback", url.Values{"action": {"classify"}, "case": {cs[1]}, "kind": {"warranty"}, "note": {"Pad delaminated"}}); status != http.StatusSeeOther {
		t.Errorf("warranty with a reason answered %d", status)
	}
	_, page2 := page(t, desk, base+"/jobs/"+follow+"/comeback")
	for _, want := range []string{"Pad delaminated", "File a claim"} {
		if !strings.Contains(page2, want) {
			t.Errorf("after warranty the page does not say %q", want)
		}
	}
	if status := postStatus(t, desk, base+"/jobs/"+follow+"/comeback", url.Values{"action": {"claim"}, "case": {cs[1]}, "supplier": {"Bromsgrossen"}, "amount": {"900"}}); status != http.StatusSeeOther {
		t.Errorf("filing a claim answered %d", status)
	}
}
