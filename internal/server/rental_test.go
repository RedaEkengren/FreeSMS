package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
)

const rentalEmail = "rental@shop-a.test"

// seedRentalDesk makes shop A run Rental beside the workshop, with a person
// at the rental desk.
func seedRentalDesk(t *testing.T, s func(t *testing.T, stmts []string)) {
	t.Helper()
	hash, err := auth.HashPassword(techPass)
	if err != nil {
		t.Fatal(err)
	}
	s(t, []string{
		`INSERT INTO shop_module_changes (shop_id, module, enabled) VALUES
		 ('` + shopA + `','workshop',true), ('` + shopA + `','rental',true)`,
		`INSERT INTO people (id, shop_id, display_name, email) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000b1','` + shopA + `','A Rental Agent','` + rentalEmail + `')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000b2','` + shopA + `',
		  'aaaaaaaa-0000-0000-0000-0000000000b1','rental_agent','` + hash + `')`,
	})
}

// The rental settings page: the owner records the permit and changes a
// profile, the rental desk reads both and changes neither, and a shop that
// does not run Rental has no such page.
func TestTheRentalSettingsPage(t *testing.T) {
	ts, pool := testServer(t)
	owner := signIn(t, ts, ownerEmail)
	if resp, err := owner.Get(ts.URL + "/rental/settings"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the settings in a shop that does not rent: %v, %v", resp, err)
	}
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	owner = signIn(t, ts, ownerEmail)
	desk := signIn(t, ts, rentalEmail)

	resp := postModuleForm(t, owner, ts.URL, "/rental/permits", url.Values{
		"reference": {"TSV 2026-42"}, "responsible": {"Olle Ägare"}, "decided_on": {"2026-01-05"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recording the permit: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp = postModuleForm(t, desk, ts.URL, "/rental/permits", url.Values{
		"reference": {"TSV 2026-43"}, "responsible": {"Someone"}, "decided_on": {"2026-01-05"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("the rental desk recording a permit: %d", resp.StatusCode)
	}

	resp, err := desk.Get(ts.URL + "/rental/settings")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	for _, want := range []string{"TSV 2026-42", "Olle Ägare", "Trailer", "not confirmed"} {
		if !strings.Contains(body, want) {
			t.Errorf("the rental desk's settings page lacks %q", want)
		}
	}
	if strings.Contains(body, "TSV 2026-43") || strings.Contains(body, "Save as a new version") {
		t.Error("the rental desk's page offers to change what only the owner may")
	}
}
