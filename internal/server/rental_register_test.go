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
	profiles, _ := workshop.RentalProfiles(ctx, pool, owner)
	unit, err := workshop.AddRentalUnit(ctx, pool, owner, workshop.NewRentalUnit{Registration: "REG 123", FleetNumber: "V9", Category: "Bil", ProfileID: profiles[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.StartFleetMembership(ctx, pool, owner, unit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	plan, _ := workshop.CreateRatePlan(ctx, pool, owner, workshop.NewRatePlan{Name: "Bil", Category: "Bil", VATRateBp: 2500, SeasonRule: "at_start"})
	price := int64(40000)
	workshop.AddRateSeason(ctx, pool, owner, plan, workshop.RateSeason{ValidFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Day: &price})
	renter, _ := workshop.CreateParty(ctx, pool, owner, workshop.NewParty{Kind: "private", Name: "=HYPERLINK(\"x\") Rita Hyrare", Role: workshop.PartyRenter})
	if _, err := workshop.BookRental(ctx, pool, owner, workshop.NewRentalBooking{UnitID: unit, RenterID: renter,
		From: time.Date(2031, 5, 10, 9, 0, 0, 0, time.UTC), To: time.Date(2031, 5, 12, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	client := signIn(t, ts, ownerEmail)
	resp, _ := client.Get(ts.URL + "/rental/settings")
	if body := readBody(t, resp); !strings.Contains(body, "has not been decided") {
		t.Error("the settings do not say the register's retention is missing")
	}
	q := url.Values{"purpose": {"Transportstyrelsens tillsyn"}, "from": {"2031-05-01"}, "to": {"2031-05-31"}}
	resp, _ = client.Get(ts.URL + "/rental/register?" + q.Encode())
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "R-1") || !strings.Contains(body, "Rita Hyrare") {
		t.Errorf("the register page: %d %s", resp.StatusCode, body)
	}
	resp, _ = client.Get(ts.URL + "/rental/register.csv?" + q.Encode())
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "booking,unit,registration") || !strings.Contains(body, `R-1,V9,REG 123,"'=HYPERLINK(""x"") Rita Hyrare"`) {
		t.Errorf("the register's CSV: %d %s", resp.StatusCode, body)
	}
	desk := signIn(t, ts, rentalEmail)
	if resp, _ := desk.Get(ts.URL + "/rental/register?" + q.Encode()); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the rental desk's register: %d", resp.StatusCode)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO shop_module_changes (shop_id, module, enabled) VALUES ('`+shopA+`', 'rental', false)`); err != nil {
		t.Fatal(err)
	}
	off := signIn(t, ts, ownerEmail)
	resp, _ = off.Get(ts.URL + "/rental/register?" + q.Encode())
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "R-1") {
		t.Errorf("the register with Rental off: %d", resp.StatusCode)
	}
}
