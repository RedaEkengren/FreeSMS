package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The whole way, over HTTP: a shop with no checklist says so on the job, the
// owner makes one, and the job then offers it.
func TestAnOwnerMakesAChecklistAndTheJobOffersIt(t *testing.T) {
	ts, _ := testServer(t)
	owner := signIn(t, ts, ownerEmail)
	tech := signIn(t, ts, techEmail)

	get := func(c *http.Client, path string) (int, string) {
		t.Helper()
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(c *http.Client, path string, form url.Values) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if _, body := get(tech, "/jobs/"+jobA); !strings.Contains(body, "There is no inspection checklist yet.") {
		t.Error("a job in a shop with no checklist does not say so")
	}

	// Refused: a line twice. Sent back with what was typed.
	typed := "Front brakes\nTyres\ntyres"
	status, body := post(owner, "/checklists", url.Values{"name": {"Spring service"}, "checkpoints": {typed}})
	if status != http.StatusBadRequest {
		t.Fatalf("a duplicate line answered %d, want 400", status)
	}
	if !strings.Contains(body, "Front brakes\nTyres\ntyres") || !strings.Contains(body, `value="Spring service"`) {
		t.Error("the refused checklist was not sent back with what was typed")
	}

	status, _ = post(owner, "/checklists", url.Values{"name": {"Spring service"}, "checkpoints": {"Front brakes\nTyres"}})
	if status != http.StatusSeeOther {
		t.Fatalf("making a checklist answered %d, want 303", status)
	}
	if _, body := get(owner, "/checklists"); !strings.Contains(body, "Spring service") {
		t.Error("the new checklist is not listed")
	}
	if _, body := get(tech, "/jobs/"+jobA); !strings.Contains(body, ">Spring service</option>") {
		t.Error("the job does not offer the new checklist")
	}

	// Somebody who does not run the shop uses checklists and does not edit them.
	if status, _ := get(tech, "/checklists"); status != http.StatusForbidden {
		t.Errorf("a technician opening the checklists got %d, want 403", status)
	}
	if status, _ := post(tech, "/checklists", url.Values{"name": {"Mine"}, "checkpoints": {"A"}}); status != http.StatusForbidden {
		t.Errorf("a technician making a checklist got %d, want 403", status)
	}
}
