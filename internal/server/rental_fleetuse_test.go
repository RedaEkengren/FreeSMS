package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// From the unit's page the rental desk records that the car was reported
// into rental use and that it is insured for it (#202); the page says
// which it lacks until then.
func TestTheRentalDeskRecordsAUnitsReportAndInsurance(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	desk := signIn(t, ts, rentalEmail)
	resp, _ := desk.Get(ts.URL + "/rental")
	body := readBody(t, resp)
	i := strings.Index(body, `name="profile"><option value="`)
	profile := body[i+len(`name="profile"><option value="`):][:36]
	resp = postModuleForm(t, desk, ts.URL, "/rental/units", url.Values{"registration": {"KLM 123"},
		"fleet_number": {"V7"}, "category": {"Liten skåpbil"}, "profile": {profile}})
	unit := resp.Header.Get("Location")
	postModuleForm(t, desk, ts.URL, unit+"/fleet", url.Values{"at": {"2026-01-05T08:00"}, "how": {"bought"}})
	resp, _ = desk.Get(ts.URL + unit)
	body = readBody(t, resp)
	if !strings.Contains(body, "not reported into rental use") {
		t.Error("the unit's page does not say it is not reported")
	}
	j := strings.Index(body, `action="/rental/memberships/`)
	if j < 0 {
		t.Fatal("no membership to record against")
	}
	m := body[j+len(`action="`):][:len("/rental/memberships/")+36]
	back := url.Values{"unit": {strings.TrimPrefix(unit, "/rental/units/")}}
	form := url.Values{"direction": {"in"}, "on": {"2026-01-06"}, "reference": {"Anmäld via e-tjänsten"}}
	for k, v := range back {
		form[k] = v
	}
	if resp := postModuleForm(t, desk, ts.URL, m+"/report", form); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != unit {
		t.Fatalf("recording the report: %d %s", resp.StatusCode, readBody(t, resp))
	}
	form = url.Values{"insurer": {"Bolaget"}, "policy_ref": {"P-77"}, "valid_from": {"2026-01-05"}, "covers": {"yes"}}
	for k, v := range back {
		form[k] = v
	}
	if resp := postModuleForm(t, desk, ts.URL, m+"/insurance", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recording the insurance: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, _ = desk.Get(ts.URL + unit)
	body = readBody(t, resp)
	for _, want := range []string{"Anmäld via e-tjänsten", "P-77", "covers rental"} {
		if !strings.Contains(body, want) {
			t.Errorf("the unit's page lacks %q", want)
		}
	}
	if strings.Contains(body, "not reported into rental use") {
		t.Error("the unit's page still says it is not reported")
	}
}
