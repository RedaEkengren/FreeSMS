package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func pageStatus(t *testing.T, ts *httptest.Server, client *http.Client, path string) int {
	t.Helper()
	resp, err := client.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Input that a person could send and the database cannot take is answered with
// a refusal the person can read, not with "Something went wrong": the second is
// what an outage looks like, and an alert on it would ring for a typo.

func TestALinkToAnIdThatIsNotAnIdIsNotFound(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts, advisorEmail)
	for _, path := range []string{"/jobs/abc", "/jobs/comeback", "/invoices/abc", "/jobs/abc/customer", "/inspections/x"} {
		if got := pageStatus(t, ts, client, path); got != 404 {
			t.Errorf("GET %s = %d, want 404", path, got)
		}
	}
	if got := postForm(t, ts, client, "/jobs/abc/clock-in", url.Values{}); got != 404 {
		t.Errorf("POST /jobs/abc/clock-in = %d, want 404", got)
	}
}

func TestANulCharacterIsRefusedWhereverItIsTyped(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts, advisorEmail)
	// The refusal says so in its own words, which a database error turned into
	// a generic one would not: it is the request that is refused, before any
	// handler has begun to write.
	form := url.Values{"registration": {"A\x00B"}, "complaint": {"x"}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(body), "null character") {
		t.Errorf("a NUL in a form field = %d, want 400 saying so", resp.StatusCode)
	}
	if text := pageText(t, ts, client, "/search?q=a%00b"); !strings.Contains(text, "null character") {
		t.Error("a NUL in the query was not refused as one")
	}
}

func TestAValueTheDatabaseWouldRefuseIsTheirsToCorrect(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts, advisorEmail)
	for _, year := range []string{"3000", "1800", "-1", "abc"} {
		form := url.Values{"registration": {"YEAR01"}, "complaint": {"x"}, "model_year": {year}}
		if got := postForm(t, ts, client, "/jobs/new", form); got != 400 {
			t.Errorf("model year %q = %d, want 400", year, got)
		}
	}
	for _, km := range []string{"-5", "99999999999", "abc"} {
		form := url.Values{"registration": {"KM0001"}, "complaint": {"x"}, "odometer_km": {km}}
		if got := postForm(t, ts, client, "/jobs/new", form); got != 400 {
			t.Errorf("odometer %q = %d, want 400", km, got)
		}
	}
	// A time of a thousand hours is a check the table makes, not one the handler did: still a 400.
	lab := url.Values{"operation": {"Byte"}, "hours": {"1000"}}
	if got := postForm(t, ts, client, "/labour", lab); got != 400 {
		t.Errorf("a thousand hours = %d, want 400", got)
	}
}
