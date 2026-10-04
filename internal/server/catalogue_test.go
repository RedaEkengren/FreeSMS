package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The parts desk adds a part through the page and finds it on the stock
// list; a refused one comes back with what was typed; a technician is not
// shown the form.
func TestAPartIsAddedThroughThePage(t *testing.T) {
	ts, _ := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	tech := signIn(t, ts, techEmail)

	do := func(c *http.Client, method, path string, form url.Values) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(b)
	}

	if status, _, _ := do(tech, http.MethodGet, "/stock/parts/new", nil); status != http.StatusForbidden {
		t.Errorf("a technician opening the new-part form got %d, want 403", status)
	}
	if status, _, _ := do(desk, http.MethodGet, "/stock/parts/new", nil); status != http.StatusOK {
		t.Fatalf("the new-part form answered %d", status)
	}

	part := url.Values{"number": {"OF-1"}, "name": {"Oil filter"}, "unit": {"each"},
		"cost": {"45"}, "price": {"129"}, "minimum": {"2"}, "codes": {"7310000000011\nMANN-W712"}}
	status, where, _ := do(desk, http.MethodPost, "/stock/parts", part)
	if status != http.StatusSeeOther || !strings.HasPrefix(where, "/stock/parts/") {
		t.Fatalf("adding a part answered %d, %q", status, where)
	}
	if _, _, body := do(desk, http.MethodGet, "/stock", nil); !strings.Contains(body, "Oil filter") {
		t.Error("the new part is not on the stock page")
	}

	// The same number again: refused, naming the part, with the form as typed.
	part.Set("name", "A second oil filter")
	status, _, body := do(desk, http.MethodPost, "/stock/parts", part)
	if status != http.StatusBadRequest {
		t.Fatalf("a duplicate number answered %d, want 400", status)
	}
	if !strings.Contains(body, "OF-1 is already OF-1 Oil filter") || !strings.Contains(body, `value="A second oil filter"`) ||
		!strings.Contains(body, "MANN-W712") {
		t.Error("the refusal did not say which part has the number, or lost what was typed")
	}

	// A price that does not parse comes back the same way, not as an error page.
	part.Set("number", "OF-2")
	part.Set("price", "a lot")
	if status, _, body := do(desk, http.MethodPost, "/stock/parts", part); status != http.StatusBadRequest ||
		!strings.Contains(body, `value="OF-2"`) {
		t.Errorf("an unparseable price answered %d without the form", status)
	}
}
