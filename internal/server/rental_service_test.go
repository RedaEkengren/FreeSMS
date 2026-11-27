package server

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// The rental desk plans a unit's service and records it from the unit's page, and
// the page of the fleet's services says what falls due and sets time aside; the
// technician reaches none of it.
func TestTheRentalDeskPlansAndRecordsAServiceFromTheUnitsPage(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	desk, tech := signIn(t, ts, rentalEmail), signIn(t, ts, techEmail)
	resp, _ := desk.Get(ts.URL + "/rental")
	body := readBody(t, resp)
	i := strings.Index(body, `name="profile"><option value="`)
	profile := body[i+len(`name="profile"><option value="`):][:36]
	resp = postModuleForm(t, desk, ts.URL, "/rental/units", url.Values{"registration": {"SVC 777"},
		"fleet_number": {"V9"}, "category": {"Liten skåpbil"}, "profile": {profile}})
	unit := resp.Header.Get("Location")
	postModuleForm(t, desk, ts.URL, unit+"/fleet", url.Values{"at": {"2026-01-05T08:00"}, "how": {"bought"}})

	if status := postStatus(t, tech, ts.URL+unit+"/service-plan", url.Values{"kind": {"service"}, "months": {"12"}, "hours": {"4"}}); status != 403 {
		t.Errorf("a technician making a plan answered %d", status)
	}
	if status := getStatus(t, tech, ts.URL+"/rental/service"); status != 403 {
		t.Errorf("a technician reading the fleet's services answered %d", status)
	}
	if status := postStatus(t, desk, ts.URL+unit+"/service-plan", url.Values{"kind": {"service"}, "months": {"12"}, "hours": {"4"}}); status != 303 {
		t.Fatalf("making a plan answered %d", status)
	}
	done := time.Now().AddDate(-1, 0, 7).Format("2006-01-02")
	if status := postStatus(t, desk, ts.URL+unit+"/service-done", url.Values{"kind": {"service"}, "done_on": {done}}); status != 303 {
		t.Fatalf("recording it done answered %d", status)
	}
	resp, _ = desk.Get(ts.URL + unit)
	if page := visibleText(readBody(t, resp)); !strings.Contains(page, "Last done") {
		t.Errorf("the unit's page after recording:\n%s", page)
	}
	resp, _ = desk.Get(ts.URL + "/rental/service")
	if page := visibleText(readBody(t, resp)); !strings.Contains(page, "SVC 777") || !strings.Contains(page, "No time set aside") {
		t.Errorf("the fleet's services:\n%s", page)
	}
	if status := postStatus(t, desk, ts.URL+"/rental/service/plan", url.Values{}); status != 200 {
		t.Fatalf("setting time aside answered %d", status)
	}
	resp, _ = desk.Get(ts.URL + "/rental/service")
	if page := visibleText(readBody(t, resp)); !strings.Contains(page, "Time is set aside") {
		t.Errorf("after setting time aside:\n%s", page)
	}
}
