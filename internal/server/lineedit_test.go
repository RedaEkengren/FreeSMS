package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Correcting a line through the pages: see what it comes to first, save with
// a second press, be told when somebody else got there first, and have the
// technician kept out of pricing.
func TestCorrectingAJobLineThroughThePages(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"COR 001"}, "complaint": {"Squeal"}})

	if status := postStatus(t, desk, job+"/lines", url.Values{
		"kind": {"labour"}, "description": {"Replace brake pads"}, "quantity": {"2"}, "unit_price": {"800"}}); status != http.StatusSeeOther {
		t.Fatalf("adding the line answered %d", status)
	}
	_, body := page(t, desk, job)
	if !strings.Contains(body, "/lines/1/edit") {
		t.Fatal("the desk has no way to correct the line")
	}
	if _, mine := page(t, tech, job); strings.Contains(mine, "/lines/1/edit") {
		t.Error("the technician is offered a correction")
	}
	if status, _ := page(t, tech, job+"/lines/1/edit"); status != http.StatusForbidden {
		t.Errorf("a technician opened the correction page: %d", status)
	}
	if status, form := page(t, desk, job+"/lines/1/edit"); status != http.StatusOK || !strings.Contains(form, "Replace brake pads") || !strings.Contains(form, "Versions") {
		t.Fatalf("the correction page answered %d", status)
	}

	// Step one shows what it comes to and writes nothing.
	edit := url.Values{"revision": {"1"}, "description": {"Replace front brake pads"}, "quantity": {"1,5"},
		"unit_price": {"800"}, "vat": {"25"}, "cost_bearer": {"customer"}}
	status, preview := postBody(t, desk, job+"/lines/1/edit", edit)
	if status != http.StatusOK || !strings.Contains(preview, "Check the correction") || !strings.Contains(preview, "Save the correction") {
		t.Fatalf("the preview answered %d: %s", status, preview)
	}
	if _, body = page(t, desk, job); strings.Contains(body, "Replace front brake pads") {
		t.Error("the preview saved the correction")
	}

	// Step two saves.
	edit.Set("confirm", "1")
	if status := postStatus(t, desk, job+"/lines/1/edit", edit); status != http.StatusSeeOther {
		t.Fatalf("saving answered %d", status)
	}
	if _, body = page(t, desk, job); !strings.Contains(body, "Replace front brake pads") {
		t.Error("the correction is not on the job")
	}

	// A form still carrying version 1 is told so, and saves nothing.
	edit.Set("quantity", "9")
	status, stale := postBody(t, desk, job+"/lines/1/edit", edit)
	if status != http.StatusConflict || !strings.Contains(stale, "Somebody else got there first") {
		t.Errorf("a stale correction answered %d: %s", status, stale)
	}

	// A technician cannot post one either.
	if status := postStatus(t, tech, job+"/lines/1/edit", edit); status != http.StatusForbidden {
		t.Errorf("a technician corrected a line: %d", status)
	}
	if status := postStatus(t, tech, job+"/lines/1/withdraw", url.Values{"revision": {"2"}, "reason": {"x"}, "confirm": {"1"}}); status != http.StatusForbidden {
		t.Errorf("a technician withdrew a line: %d", status)
	}

	// Withdrawing needs a reason, shows first, and leaves the line struck out.
	if status, _ := postBody(t, desk, job+"/lines/1/withdraw", url.Values{"revision": {"2"}, "reason": {""}}); status != http.StatusBadRequest {
		t.Errorf("withdrawing with no reason answered %d", status)
	}
	w := url.Values{"revision": {"2"}, "reason": {"Customer brought their own pads"}}
	if status, p := postBody(t, desk, job+"/lines/1/withdraw", w); status != http.StatusOK || !strings.Contains(p, "Withdraw the line") {
		t.Fatalf("the withdrawal preview answered %d", status)
	}
	w.Set("confirm", "1")
	if status := postStatus(t, desk, job+"/lines/1/withdraw", w); status != http.StatusSeeOther {
		t.Fatalf("withdrawing answered %d", status)
	}
	_, body = page(t, desk, job)
	if !strings.Contains(body, "Customer brought their own pads") || strings.Contains(body, "/lines/1/edit") {
		t.Error("a withdrawn line should show its reason and offer no more corrections")
	}
}
