package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A draft is the server's to drop only once the submission it holds has been
// saved, and the browser learns that from a receipt rather than from having
// pressed the button.
func TestADraftOutlivesARefusedSubmission(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts, advisorEmail)

	do := func(method, path, contentType, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Origin", ts.URL)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	draft := func() string {
		t.Helper()
		resp := do(http.MethodGet, "/drafts?form=intake", "", "")
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(b))
	}
	receipt := func(resp *http.Response) string {
		for _, c := range resp.Cookies() {
			if c.Name == savedCookie && c.MaxAge > 0 {
				return c.Value
			}
		}
		return ""
	}

	resp := do(http.MethodPost, "/drafts", "application/json",
		`{"form":"intake","fields":{"registration":"ABC123","complaint":"Rattles at 80"}}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("saving the draft answered %d", resp.StatusCode)
	}

	// Refused: the odometer is not a number. The draft stays, and there is
	// no receipt to tell the browser otherwise.
	refused := url.Values{
		"registration": {"ABC123"}, "complaint": {"Rattles at 80"},
		"odometer_km": {"lots"}, "draft_form": {"intake"},
	}
	resp = do(http.MethodPost, "/jobs/new", "application/x-www-form-urlencoded", refused.Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad odometer answered %d, want 400", resp.StatusCode)
	}
	if got := receipt(resp); got != "" {
		t.Errorf("a refused submission handed out a receipt for %q", got)
	}
	if !strings.Contains(draft(), "Rattles at 80") {
		t.Fatalf("the draft is gone after a refusal: %s", draft())
	}

	// Saved: now the server drops its copy and says so.
	refused.Set("odometer_km", "18 500")
	resp = do(http.MethodPost, "/jobs/new", "application/x-www-form-urlencoded", refused.Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a good intake answered %d, want 303", resp.StatusCode)
	}
	if got := receipt(resp); got != "intake" {
		t.Errorf("receipt = %q, want %q", got, "intake")
	}
	if got := draft(); strings.Contains(got, "Rattles") {
		t.Errorf("the server kept a draft that was saved: %s", got)
	}
}

// A receipt not yet read is added to, not replaced, so the offline queue
// sending two held submissions back to back loses neither.
func TestReceiptsAccumulateUntilRead(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts, advisorEmail)
	u, _ := url.Parse(ts.URL)
	client.Jar.SetCookies(u, []*http.Cookie{{Name: savedCookie, Value: "shop-details", Path: "/"}})

	form := url.Values{"registration": {"XYZ987"}, "complaint": {"Brakes"},
		"draft_form": {"odd.name"}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var got string
	for _, c := range resp.Cookies() {
		if c.Name == savedCookie {
			got = c.Value
		}
	}
	// The dot in the name is escaped, because the dot is the separator.
	if want := "shop-details.odd%2Ename"; got != want {
		t.Errorf("receipt = %q, want %q", got, want)
	}
}
