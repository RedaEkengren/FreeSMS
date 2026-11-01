package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The one search (#159) says nothing of a record the role may not see: a
// technician's answer to a name reads the same whether a
// customer of that name exists or not, while the counter finds them.
func TestASearchSaysNothingOfWhatTheRoleMayNotSee(t *testing.T) {
	ts, pool := testServer(t)
	tech, desk := signIn(t, ts, techEmail), signIn(t, ts, advisorEmail)
	q := ts.URL + "/search?" + url.Values{"q": {"Hemligsson"}}.Encode()
	before := visibleText(get(t, tech, q, http.StatusOK))
	seedShop(t, pool, shopA, []string{
		`INSERT INTO people (id, shop_id, display_name) VALUES ('aaaaaaaa-0000-0000-0000-0000000001ff', '` + shopA + `', 'Hemligsson')`,
		`INSERT INTO customers (id, shop_id, kind, person_id) VALUES ('aaaaaaaa-0000-0000-0000-0000000002ff', '` + shopA + `', 'private',
		 'aaaaaaaa-0000-0000-0000-0000000001ff')`,
	})
	if after := visibleText(get(t, tech, q, http.StatusOK)); after != before {
		t.Error("a technician's search for a name answers differently once a customer of that name exists")
	}
	// The counter finds the one customer, and goes straight to them.
	req, _ := http.NewRequest(http.MethodGet, q, nil)
	resp, err := desk.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasSuffix(resp.Header.Get("Location"), "/customers/aaaaaaaa-0000-0000-0000-0000000002ff") {
		t.Errorf("the counter's search for the name: %d to %q, want the customer", resp.StatusCode, resp.Header.Get("Location"))
	}
}
