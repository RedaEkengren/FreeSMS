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

// The rental desk adds a unit by its plate and puts it in the fleet; the
// list says what each unit is doing; a technician has no rental pages.
func TestTheRentalDeskAddsAUnitAndPutsItInTheFleet(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	desk := signIn(t, ts, rentalEmail)

	resp, err := desk.Get(ts.URL + "/rental")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	i := strings.Index(body, `name="profile"><option value="`)
	if i < 0 {
		t.Fatalf("no profile to choose on the rental page:\n%s", body)
	}
	profile := body[i+len(`name="profile"><option value="`):][:36]
	resp = postModuleForm(t, desk, ts.URL, "/rental/units", url.Values{"registration": {"KLM 123"},
		"fleet_number": {"V7"}, "category": {"Liten skåpbil"}, "profile": {profile}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("adding the unit: %d %s", resp.StatusCode, readBody(t, resp))
	}
	unit := resp.Header.Get("Location")
	resp = postModuleForm(t, desk, ts.URL, unit+"/fleet", url.Values{"at": {"2026-01-05T08:00"}, "how": {"bought"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("putting it in the fleet: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, _ = desk.Get(ts.URL + "/rental")
	body = readBody(t, resp)
	for _, want := range []string{"V7 · KLM 123", "Liten skåpbil", "Available"} {
		if !strings.Contains(body, want) {
			t.Errorf("the rental page lacks %q", want)
		}
	}
	tech := signIn(t, ts, techEmail)
	if resp, _ := tech.Get(ts.URL + "/rental"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a technician's rental page: %d", resp.StatusCode)
	}
}
