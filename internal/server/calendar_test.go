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
