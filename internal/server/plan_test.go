package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const shopATech = "aaaaaaaa-0000-0000-0000-000000000002"

// Who is on a job and when, through the pages: put on at the counter, planned
// with a clash that has to be argued with, and a second planner told that
// somebody got there first.
func TestPlanningAJobThroughThePages(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"PLN 001"}, "complaint": {"Pulls left"}})

	_, body := page(t, desk, job)
	for _, want := range []string{"Who and when", "Nobody is on it", "No time planned yet", "Put on the job", "Save the plan"} {
		if !strings.Contains(body, want) {
			t.Errorf("an unplanned job lacks %q", want)
		}
	}
	if status := postStatus(t, desk, job+"/staff", url.Values{"action": {"add"}, "user_id": {shopATech}, "reason": {"knows the car"}}); status != http.StatusSeeOther {
		t.Fatalf("putting a technician on answered %d", status)
	}
	_, body = page(t, desk, job)
	if strings.Contains(body, "Nobody is on it") || !strings.Contains(body, "knows the car") {
		t.Error("the technician is not on the job")
	}

	// The technician sees the plan and has no way to change it.
	_, mine := page(t, tech, job)
	if !strings.Contains(mine, "Who and when") || strings.Contains(mine, "Save the plan") || strings.Contains(mine, "Hand it over") {
		t.Error("the technician's view of the plan")
	}
	for _, path := range []string{"/staff", "/plan", "/plan/carry"} {
		if status := postStatus(t, tech, job+path, url.Values{"action": {"add"}, "user_id": {shopATech}, "version": {"0"}}); status != http.StatusForbidden {
			t.Errorf("a technician posted %s and got %d", path, status)
		}
	}

	// A plan that runs into a shut shop asks for a reason and saves nothing.
	owner := signIn(t, ts, ownerEmail)
	if status := postStatus(t, owner, base+"/calendar/capacity", url.Values{"action": {"shut"}, "date": {"2030-06-21"}, "reason": {"Midsommarafton"}}); status != http.StatusSeeOther {
		t.Fatalf("shutting the day answered %d", status)
	}
	plan := url.Values{"version": {"0"}, "starts": {"2030-06-20T13:00"}, "ends": {"2030-06-21T10:00"}, "hours": {"6,5"}, "on_lift": {"1"}}
	status, clash := postBody(t, desk, job+"/plan", plan)
	if status != http.StatusConflict || !strings.Contains(clash, "Midsommarafton") || !strings.Contains(clash, "Why you are going ahead") {
		t.Fatalf("the clash page answered %d: %s", status, clash)
	}
	if _, body = page(t, desk, job); !strings.Contains(body, "No time planned yet") {
		t.Error("a refused plan was saved")
	}
	plan.Set("override", "collected by the customer")
	if status := postStatus(t, desk, job+"/plan", plan); status != http.StatusSeeOther {
		t.Fatalf("going ahead answered %d", status)
	}
	_, body = page(t, desk, job)
	for _, want := range []string{"Went ahead anyway", "collected by the customer", "Midsommarafton", "6.5 h left"} {
		if !strings.Contains(body, want) {
			t.Errorf("the job lacks %q", want)
		}
	}

	// A second planner still holds version 0.
	status, stale := postBody(t, desk, job+"/plan", url.Values{"version": {"0"}, "starts": {"2030-06-24T08:00"}, "ends": {"2030-06-24T12:00"}})
	if status != http.StatusConflict || !strings.Contains(stale, "Somebody else got there first") {
		t.Errorf("a stale plan answered %d: %s", status, stale)
	}
}

// postBody is a post that is not followed, with what it answered.
func postBody(t *testing.T, c *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.URL.Host)
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
