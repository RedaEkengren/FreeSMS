package server

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Booked at the counter, seen on the week, taken in when the car comes --
// and the booking then points at the job it became.
func TestABookingIsTakenInFromThePlanner(t *testing.T) {
	ts, _ := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	post := func(path string, form url.Values) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		client := *desk
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}
	get := func(path string) string {
		t.Helper()
		resp, err := desk.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s answered %d", path, resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	status, where := post("/calendar", url.Values{"date": {"2026-10-15"}, "time": {"09:00"}, "minutes": {"90"},
		"registration": {"BKG 123"}, "what": {"Service och bromsar"}, "customer_name": {"Bo Kund"}})
	if status != http.StatusSeeOther || !strings.HasPrefix(where, "/bookings/") {
		t.Fatalf("booking answered %d, %q", status, where)
	}
	if body := get("/calendar?week=2026-10-15"); !strings.Contains(body, "BKG 123") || !strings.Contains(body, "cs-4 cd-3") {
		t.Error("the week does not show the booking at 09:00 for an hour and a half")
	}
	if body := get("/calendar?q=bkg123"); !strings.Contains(body, where) {
		t.Error("searching the registration does not find the booking")
	}

	// The car comes: the intake is filled in from the booking.
	id := strings.TrimPrefix(where, "/bookings/")
	intake := get("/jobs/new?booking=" + id)
	if !strings.Contains(intake, `value="BKG 123"`) || !strings.Contains(intake, `name="booking_id" value="`+id+`"`) {
		t.Fatal("taking it in does not carry the booking's car and id")
	}
	status, job := post("/jobs/new", url.Values{"registration": {"BKG 123"}, "complaint": {"Service och bromsar"}, "booking_id": {id}})
	if status != http.StatusSeeOther || !regexp.MustCompile(`^/jobs/[0-9a-f-]{36}$`).MatchString(job) {
		t.Fatalf("taking it in answered %d, %q", status, job)
	}
	if body := get(where); !strings.Contains(body, `href="`+job+`"`) {
		t.Error("the booking does not point at the job it became")
	}
}

// A shut day and the lifts are the owner's to set; the front desk sees them
// on the week it books into, and a technician cannot set either.
func TestCapacityIsSetAndShownOnThePlanner(t *testing.T) {
	ts, _ := testServer(t)
	post := func(c *http.Client, form url.Values) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/calendar/capacity", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		client := *c
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	owner, desk, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	shut := url.Values{"action": {"shut"}, "date": {"2026-10-16"}, "reason": {"Inventering"}}
	if status := post(tech, shut); status != http.StatusForbidden {
		t.Errorf("a technician shutting the shop answered %d", status)
	}
	if status := post(desk, shut); status != http.StatusForbidden {
		t.Errorf("the front desk shutting the shop answered %d", status)
	}
	if status := post(owner, shut); status != http.StatusSeeOther {
		t.Fatalf("the owner shutting a day answered %d", status)
	}
	if status := post(owner, url.Values{"action": {"lifts"}, "lifts": {"two"}}); status != http.StatusBadRequest {
		t.Errorf("lifts as a word answered %d", status)
	}
	if status := post(owner, url.Values{"action": {"lifts"}, "lifts": {"2"}}); status != http.StatusSeeOther {
		t.Errorf("setting the lifts answered %d", status)
	}

	resp, err := desk.Get(ts.URL + "/calendar?week=2026-10-16")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Inventering") {
		t.Error("the front desk's week does not say the shop is shut")
	}
	if strings.Contains(string(body), `value="shut"`) {
		t.Error("the front desk is offered a form only the owner may use")
	}
}
