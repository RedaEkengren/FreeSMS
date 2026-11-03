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

// The owner sets a plan's prices including VAT; the rental desk reads them
// as entered and may not add one.
func TestTheRentalPricesPage(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	owner := signIn(t, ts, ownerEmail)
	desk := signIn(t, ts, rentalEmail)
	resp := postModuleForm(t, owner, ts.URL, "/rental/plans", url.Values{"name": {"Sommar"}, "category": {"Släpvagn"},
		"vat_percent": {"25"}, "season_rule": {"split"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("adding a plan: %d %s", resp.StatusCode, readBody(t, resp))
	}
	var plan string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM rate_plans`).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	resp = postModuleForm(t, owner, ts.URL, "/rental/plans/"+plan+"/seasons", url.Values{"vat_bp": {"2500"},
		"valid_from": {"2026-05-01"}, "day": {"499"}, "grace_minutes": {"30"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("adding prices: %d %s", resp.StatusCode, readBody(t, resp))
	}
	var net int64
	if err := pool.QueryRow(t.Context(), `SELECT day_minor FROM rate_plan_seasons`).Scan(&net); err != nil || net != 39920 {
		t.Errorf("499 kr including VAT stored as %d net, %v; want 39920", net, err)
	}
	resp, _ = desk.Get(ts.URL + "/rental/prices")
	body := readBody(t, resp)
	for _, want := range []string{"Sommar", "Släpvagn", "499.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("the prices page lacks %q", want)
		}
	}
	resp = postModuleForm(t, desk, ts.URL, "/rental/plans", url.Values{"name": {"x"}, "category": {"x"}, "vat_percent": {"25"}, "season_rule": {"split"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("the rental desk adding a plan: %d", resp.StatusCode)
	}
}

// The rental desk books a unit in the fleet for a new renter from its
// page, sees the price including VAT and the renter on the booking, and
// cancels it; the same form sent twice is one booking.
func TestTheRentalDeskBooksAUnit(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	desk := signIn(t, ts, rentalEmail)
	owner := signIn(t, ts, ownerEmail)

	resp, _ := desk.Get(ts.URL + "/rental")
	body := readBody(t, resp)
	i := strings.Index(body, `name="profile"><option value="`)
	profile := body[i+len(`name="profile"><option value="`):][:36]
	resp = postModuleForm(t, desk, ts.URL, "/rental/units", url.Values{"registration": {"KLM 123"},
		"fleet_number": {"V7"}, "category": {"Liten skåpbil"}, "profile": {profile}})
	unit := resp.Header.Get("Location")
	postModuleForm(t, desk, ts.URL, unit+"/fleet", url.Values{"at": {"2026-01-05T08:00"}, "how": {"bought"}})
	postModuleForm(t, owner, ts.URL, "/rental/plans", url.Values{"name": {"Standard"}, "category": {"Liten skåpbil"},
		"vat_percent": {"25"}, "season_rule": {"at_start"}})
	var plan string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM rate_plans`).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	postModuleForm(t, owner, ts.URL, "/rental/plans/"+plan+"/seasons", url.Values{"vat_bp": {"2500"}, "valid_from": {"2026-01-01"}, "day": {"500"}})

	form := url.Values{"unit": {strings.TrimPrefix(unit, "/rental/units/")}, "key": {"form-1"},
		"from": {"2031-05-10T09:00"}, "to": {"2031-05-12T09:00"}, "renter_name": {"Rita Hyrare"}, "renter_phone": {"070-1234567"}}
	resp = postModuleForm(t, desk, ts.URL, "/rental/bookings", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("booking: %d %s", resp.StatusCode, readBody(t, resp))
	}
	booking := resp.Header.Get("Location")
	resp, _ = desk.Get(ts.URL + booking)
	body = readBody(t, resp)
	for _, want := range []string{"R-1", "V7 · KLM 123", "Rita Hyrare", "1,000.00", "Confirmed"} {
		if !strings.Contains(body, want) {
			t.Errorf("the booking page lacks %q", want)
		}
	}
	// The same form again, the renter picked this time: one booking.
	var renter string
	if err := pool.QueryRow(t.Context(), `SELECT renter_party_id FROM rental_bookings`).Scan(&renter); err != nil {
		t.Fatal(err)
	}
	form.Set("renter", renter)
	if resp := postModuleForm(t, desk, ts.URL, "/rental/bookings", form); resp.Header.Get("Location") != booking {
		t.Errorf("the form sent again went to %q", resp.Header.Get("Location"))
	}
	resp = postModuleForm(t, desk, ts.URL, booking+"/cancel", url.Values{"reason": {"Kunden ringde"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancelling: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, _ = desk.Get(ts.URL + booking)
	if body := readBody(t, resp); !strings.Contains(body, "Kunden ringde") || !strings.Contains(body, "Cancelled") {
		t.Error("the cancelled booking does not say so, or why")
	}
	tech := signIn(t, ts, techEmail)
	if resp, _ := tech.Get(ts.URL + booking); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a technician's view of a booking: %d", resp.StatusCode)
	}
}

// From the booking page the rental desk names the renter as driver, sees
// the licence check missing, records it, makes the agreement and records
// that the renter accepted it.
func TestTheBookingPageTakesTheDriverAndTheAgreement(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	desk := signIn(t, ts, rentalEmail)
	owner := signIn(t, ts, ownerEmail)
	resp, _ := desk.Get(ts.URL + "/rental")
	body := readBody(t, resp)
	i := strings.Index(body, `name="profile"><option value="`)
	profile := body[i+len(`name="profile"><option value="`):][:36]
	resp = postModuleForm(t, desk, ts.URL, "/rental/units", url.Values{"registration": {"KLM 123"},
		"fleet_number": {"V7"}, "category": {"Liten skåpbil"}, "profile": {profile}})
	unit := resp.Header.Get("Location")
	postModuleForm(t, desk, ts.URL, unit+"/fleet", url.Values{"at": {"2026-01-05T08:00"}, "how": {"bought"}})
	postModuleForm(t, owner, ts.URL, "/rental/plans", url.Values{"name": {"Standard"}, "category": {"Liten skåpbil"},
		"vat_percent": {"25"}, "season_rule": {"at_start"}})
	var plan string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM rate_plans`).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	postModuleForm(t, owner, ts.URL, "/rental/plans/"+plan+"/seasons", url.Values{"vat_bp": {"2500"}, "valid_from": {"2026-01-01"}, "day": {"500"}})
	resp = postModuleForm(t, desk, ts.URL, "/rental/bookings", url.Values{"unit": {strings.TrimPrefix(unit, "/rental/units/")},
		"from": {"2031-05-10T09:00"}, "to": {"2031-05-12T09:00"}, "renter_name": {"Rita Hyrare"}})
	booking := resp.Header.Get("Location")
	var renter string
	if err := pool.QueryRow(t.Context(), `SELECT renter_party_id FROM rental_bookings`).Scan(&renter); err != nil {
		t.Fatal(err)
	}
	if resp := postModuleForm(t, desk, ts.URL, booking+"/drivers", url.Values{"party": {renter}, "main": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("adding the driver: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, _ = desk.Get(ts.URL + booking)
	body = readBody(t, resp)
	if !strings.Contains(body, "Rita Hyrare has not shown a valid licence for this rental.") {
		t.Error("the page does not say the licence check is missing")
	}
	var driver string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM rental_drivers`).Scan(&driver); err != nil {
		t.Fatal(err)
	}
	resp = postModuleForm(t, desk, ts.URL, "/rental/drivers/"+driver+"/check", url.Values{"booking": {strings.TrimPrefix(booking, "/rental/bookings/")},
		"shown": {"1"}, "age_ok": {"1"}, "categories": {"B"}, "valid_until": {"2035-01-01"}, "method": {"card_inspected"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != booking {
		t.Fatalf("checking the licence: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, _ = desk.Get(ts.URL + booking)
	body = readBody(t, resp)
	if strings.Contains(body, "has not shown a valid licence") || !strings.Contains(body, "licence checked") {
		t.Error("the page still asks for the licence check")
	}
	i = strings.Index(body, `name="template"><option value="`)
	if i < 0 {
		t.Fatal("no template to make the agreement from")
	}
	template := body[i+len(`name="template"><option value="`):][:36]
	resp = postModuleForm(t, desk, ts.URL, booking+"/agreement", url.Values{"template": {template}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/contracts/") {
		t.Fatalf("making the agreement: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, _ = desk.Get(ts.URL + booking)
	body = readBody(t, resp)
	j := strings.Index(body, `action="/rental/agreements/`)
	if j < 0 {
		t.Fatal("no way to record the acceptance")
	}
	accept := body[j+len(`action="`):]
	accept = accept[:strings.Index(accept, `"`)]
	form := url.Values{"booking": {strings.TrimPrefix(booking, "/rental/bookings/")}, "method": {"in_person_click"}}
	for _, name := range []string{"party", "hash"} {
		k := strings.Index(body, `name="`+name+`" value="`)
		v := body[k+len(`name="`+name+`" value="`):]
		form.Set(name, v[:strings.Index(v, `"`)])
	}
	if resp := postModuleForm(t, desk, ts.URL, accept, form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recording the acceptance: %d %s", resp.StatusCode, readBody(t, resp))
	}
	var signed int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM document_signatures`).Scan(&signed); err != nil || signed != 1 {
		t.Errorf("signatures: %d, %v", signed, err)
	}
}
