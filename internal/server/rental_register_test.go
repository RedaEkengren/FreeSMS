package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The owner reads the rental register, as a page and as a CSV, saying
// why; the rental desk does not; and with Rental switched off the
// register is still there. The settings say its retention is missing
// until the owner decides it (#200). A name a spreadsheet would run as a
// formula is written so that it does not (review r6, #4).
func TestTheOwnerReadsTheRentalRegister(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	ctx := t.Context()
	owner := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-000000000011", Role: access.Of(access.RoleOwner), Modules: access.Modules}
	// A tent out on a rental: the register holds what was out (#200).
	tpl, err := workshop.SaveChecklistTemplate(ctx, pool, owner, "", "Tält", []string{"Duk"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetTemplateSubjects(ctx, pool, owner, tpl, []string{workshop.SubjectCheckOut, workshop.SubjectCheckIn}); err != nil {
		t.Fatal(err)
	}
	profiles, _ := workshop.RentalProfiles(ctx, pool, owner)
	var equipment workshop.RentalProfile
	for _, p := range profiles {
		if p.Base == workshop.ProfileEquipment {
			equipment = p
		}
	}
	rules := equipment.Rules
	rules.CheckOutTemplateID, rules.CheckInTemplateID = tpl, tpl
	if _, err := workshop.SaveRentalProfile(ctx, pool, owner, equipment.ID, rules); err != nil {
		t.Fatal(err)
	}
	unit, err := workshop.AddRentalUnit(ctx, pool, owner, workshop.NewRentalUnit{Name: "Tält", FleetNumber: "T9", Category: "Tält", ProfileID: equipment.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.StartFleetMembership(ctx, pool, owner, unit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	plan, _ := workshop.CreateRatePlan(ctx, pool, owner, workshop.NewRatePlan{Name: "Tält", Category: "Tält", VATRateBp: 2500, SeasonRule: "at_start"})
	price := int64(10000)
	workshop.AddRateSeason(ctx, pool, owner, plan, workshop.RateSeason{ValidFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Day: &price})
	renter, _ := workshop.CreateParty(ctx, pool, owner, workshop.NewParty{Kind: "private", Name: "=HYPERLINK(\"x\") Rita Hyrare", Role: workshop.PartyRenter})
	booking, err := workshop.BookRental(ctx, pool, owner, workshop.NewRentalBooking{UnitID: unit, RenterID: renter,
		From: time.Date(2031, 5, 10, 9, 0, 0, 0, time.UTC), To: time.Date(2031, 5, 12, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := workshop.StartHandover(ctx, pool, owner, booking, workshop.HandoverOut)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.FinishHandover(ctx, pool, owner, out, workshop.HandoverReading{RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	client := signIn(t, ts, ownerEmail)
	resp, _ := client.Get(ts.URL + "/rental/settings")
	if body := readBody(t, resp); !strings.Contains(body, "has not been decided") {
		t.Error("the settings do not say the register's retention is missing")
	}
	q := url.Values{"purpose": {"Transportstyrelsens tillsyn"}, "from": {time.Now().AddDate(0, 0, -1).Format("2006-01-02")}, "to": {"2031-05-31"}}
	// A read records why, so it is sent, not linked: a GET with the
	// purpose in its address reads nothing (review r6, #10).
	resp, _ = client.Get(ts.URL + "/rental/register?" + q.Encode())
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || strings.Contains(body, "R-1") {
		t.Errorf("the register read by a link: %d", resp.StatusCode)
	}
	resp = postModuleForm(t, client, ts.URL, "/rental/register", q)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "R-1") || !strings.Contains(body, "Rita Hyrare") ||
		!strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Errorf("the register page: %d %q %s", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
	}
	resp = postModuleForm(t, client, ts.URL, "/rental/register.csv", q)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "booking,unit,registration") || !strings.Contains(body, `R-1,T9 · Tält,,"'=HYPERLINK(""x"") Rita Hyrare"`) {
		t.Errorf("the register's CSV: %d %s", resp.StatusCode, body)
	}
	desk := signIn(t, ts, rentalEmail)
	if resp := postModuleForm(t, desk, ts.URL, "/rental/register", q); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the rental desk's register: %d", resp.StatusCode)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO shop_module_changes (shop_id, module, enabled) VALUES ('`+shopA+`', 'rental', false)`); err != nil {
		t.Fatal(err)
	}
	off := signIn(t, ts, ownerEmail)
	resp = postModuleForm(t, off, ts.URL, "/rental/register", q)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "R-1") {
		t.Errorf("the register with Rental off: %d", resp.StatusCode)
	}
}
