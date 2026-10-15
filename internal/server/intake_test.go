package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// intake sends the form and returns the job it made.
func intake(t *testing.T, ts string, c *http.Client, form url.Values) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts+"/jobs/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts)
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("taking the car in answered %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// A walk-in is asked the same things as a booked car, and every answer can be
// "later": the car is taken in and the job says what is not known.
func TestAWalkInIsAskedWhoPaysWhoDoesTheWorkAndWhenItIsPromised(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)

	_, form := page(t, desk, ts.URL+"/jobs/new")
	for _, want := range []string{"Who pays and who is rung", `name="new_name"`, `name="technician_id"`,
		`name="promised_at"`, "Decide later", "/jobs/new/customers"} {
		if !strings.Contains(form, want) {
			t.Errorf("the walk-in form lacks %q", want)
		}
	}

	// Nothing but the complaint.
	bare := intake(t, ts.URL, desk, url.Values{"registration": {"ZZZ 001"}, "complaint": {"Rattles"}})
	_, body := page(t, desk, ts.URL+bare)
	for _, want := range []string{"Next:", "Price the work", "the front desk", "no customer", "nobody on it", "no promised time"} {
		if !strings.Contains(body, want) {
			t.Errorf("the job of a bare walk-in lacks %q", want)
		}
	}

	// Everything given: the customer made from what was typed, and a promise.
	full := intake(t, ts.URL, desk, url.Values{"registration": {"ZZZ 002"}, "complaint": {"Squeaks"},
		"customer": {"new"}, "new_name": {"Walter Walkin"}, "new_phone": {"070-111 22 33"},
		"technician_id": {"aaaaaaaa-0000-0000-0000-000000000002"}, "promised_at": {"2026-12-01T15:00"}})
	_, body = page(t, desk, ts.URL+full)
	if !strings.Contains(body, "Walter Walkin") || !strings.Contains(body, "Promised") {
		t.Error("what the desk confirmed is not on the job")
	}
	if strings.Contains(body, "Not known yet") {
		t.Errorf("a job with everything given still says something is unknown:\n%s", firstLines(body, 60))
	}

	// A name typed under "a new customer" is not dropped for want of a tick.
	unticked := intake(t, ts.URL, desk, url.Values{"registration": {"ZZZ 004"}, "complaint": {"Smells"}, "new_name": {"Ulla Otickad"}})
	if _, body := page(t, desk, ts.URL+unticked); !strings.Contains(body, "Ulla Otickad") {
		t.Error("a name typed for a new customer was dropped")
	}

	// A promise that is not a time is sent back, and the form keeps the rest.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/new", strings.NewReader(url.Values{
		"registration": {"ZZZ 003"}, "complaint": {"Pulls left"}, "promised_at": {"tomorrow"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := desk.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a promise that is not a time answered %d", resp.StatusCode)
	}

	// The technician is told the step and that things are unknown, but never
	// who the customer is.
	_, techView := page(t, tech, ts.URL+full)
	if !strings.Contains(techView, "Price the work") || strings.Contains(techView, "Walter Walkin") || strings.Contains(techView, "070-111 22 33") {
		t.Error("the technician's view lacks the step or carries the customer")
	}
	_, techBare := page(t, tech, ts.URL+bare)
	if !strings.Contains(techBare, "no customer") {
		t.Error("the technician is not told that no customer is recorded")
	}

	// The search for a customer on file is the desk's.
	if resp, err := tech.Get(ts.URL + "/jobs/new/customers?q=Anna"); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a technician searching customers at intake: %v %v", resp, err)
	}
}

// A job with lines that were only offered, and nothing done, can be cancelled;
// the button is there exactly when SetState would allow it.
func TestAJobWithOnlyOfferedLinesCanBeCancelledFromThePage(t *testing.T) {
	ts, _ := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	job := ts.URL + "/jobs/" + jobA
	postStatus(t, desk, job+"/lines", url.Values{"kind": {"labour"}, "description": {"Clutch"}, "quantity": {"1"}, "unit_price": {"9000"}})
	_, body := page(t, desk, job)
	if !strings.Contains(body, `value="cancelled"`) {
		t.Error("a job with lines and nothing done cannot be cancelled from its page")
	}
	if status := postStatus(t, desk, job+"/state", url.Values{"state": {"cancelled"}}); status != http.StatusSeeOther {
		t.Errorf("cancelling it answered %d", status)
	}
}
