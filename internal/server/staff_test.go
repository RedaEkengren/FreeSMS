package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The whole way, through the pages: the owner adds a technician, who signs in
// with the password handed over, is kept out of the staff page, and changes
// that password on their own.
func TestAnOwnerAddsATechnicianThroughThePage(t *testing.T) {
	ts, _ := testServer(t)
	owner := signIn(t, ts, ownerEmail)

	post := func(c *http.Client, path string, form url.Values) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	status, _ := post(owner, "/staff", url.Values{"name": {"Erik Mekaniker"}, "email": {"erik@shop-a.test"},
		"role": {"technician"}, "password": {techPass}})
	if status != http.StatusSeeOther {
		t.Fatalf("adding a technician answered %d", status)
	}
	// The same email again comes back with what was typed -- not the password.
	status, body := post(owner, "/staff", url.Values{"name": {"Another Erik"}, "email": {"erik@shop-a.test"},
		"role": {"parts"}, "password": {"secret typed once"}})
	if status != http.StatusBadRequest || !strings.Contains(body, `value="Another Erik"`) {
		t.Errorf("a duplicate email answered %d without the form as typed", status)
	}
	if strings.Contains(body, "secret typed once") {
		t.Error("the refused page wrote the password back into itself")
	}

	erik := signIn(t, ts, "erik@shop-a.test")
	resp, err := erik.Get(ts.URL + "/staff")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a technician opening the staff page got %d, want 403", resp.StatusCode)
	}

	if status, body := post(erik, "/account/password", url.Values{"current": {techPass}, "password": {"Eriks eget lösenord"}}); status != http.StatusOK ||
		!strings.Contains(body, "Changed.") {
		t.Errorf("changing one's own password answered %d", status)
	}
	// Still signed in where it was changed.
	resp, _ = erik.Get(ts.URL + "/account")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("signed out by changing the password here: %d", resp.StatusCode)
	}
}
