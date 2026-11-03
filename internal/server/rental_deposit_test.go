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

// The rental desk asks a deposit on the booking page, records a hold on
// the card that runs out before the return and is told so, releases it,
// and records cash received instead (#196).
func TestTheBookingPageKeepsTheDeposit(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	ctx := t.Context()
	owner := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-000000000011", Role: access.Of(access.RoleOwner), Modules: access.Modules}
	desk := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-0000000000b2", Role: access.Of(access.RoleRentalAgent), Modules: access.Modules}
	profiles, err := workshop.RentalProfiles(ctx, pool, owner)
	if err != nil {
		t.Fatal(err)
	}
	var equipment workshop.RentalProfile
	for _, p := range profiles {
		if p.Base == workshop.ProfileEquipment {
			equipment = p
		}
	}
	unit, err := workshop.AddRentalUnit(ctx, pool, desk, workshop.NewRentalUnit{Name: "Tält", FleetNumber: "T1", Category: "Tält", ProfileID: equipment.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.StartFleetMembership(ctx, pool, desk, unit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	plan, _ := workshop.CreateRatePlan(ctx, pool, owner, workshop.NewRatePlan{Name: "Tält", Category: "Tält", VATRateBp: 2500, SeasonRule: "at_start"})
	day := int64(10000)
	if _, err := workshop.AddRateSeason(ctx, pool, owner, plan, workshop.RateSeason{ValidFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Day: &day}); err != nil {
		t.Fatal(err)
	}
	renter, _ := workshop.CreateParty(ctx, pool, desk, workshop.NewParty{Kind: "private", Name: "Tina", Role: workshop.PartyRenter})
	booking, err := workshop.BookRental(ctx, pool, desk, workshop.NewRentalBooking{UnitID: unit, RenterID: renter,
		From: time.Date(2031, 5, 10, 9, 0, 0, 0, time.UTC), To: time.Date(2031, 5, 11, 9, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	client := signIn(t, ts, rentalEmail)
	page := "/rental/bookings/" + booking
	post := func(what string, form url.Values) {
		t.Helper()
		if resp := postModuleForm(t, client, ts.URL, page+"/deposit/"+what, form); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", what, resp.StatusCode, readBody(t, resp))
		}
	}
	read := func() string {
		t.Helper()
		resp, err := client.Get(ts.URL + page)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the booking page: %d %s", resp.StatusCode, body)
		}
		return body
	}
	post("require", url.Values{"amount": {"3000"}, "method": {"card_hold"}})
	post("hold", url.Values{"amount": {"3000"}, "reference": {"T-9"}, "valid_until": {"2031-05-10T20:00"}})
	body := read()
	tooSoon := []string{"The hold on the card runs out before the rental is due back.", "Reservationen på kortet upphör innan"}
	switch {
	case !strings.Contains(body, "T-9") || !strings.Contains(body, `action="`+page+`/deposit/release"`):
		t.Errorf("the hold is not on the page")
	case !strings.Contains(body, tooSoon[0]) && !strings.Contains(body, tooSoon[1]):
		t.Errorf("a hold ending before the return is not said to")
	}
	post("release", url.Values{"reason": {"Kortet räcker inte hela hyran"}})
	post("receive", url.Values{"amount": {"3000"}, "method": {"cash"}, "key": {"k1"}})
	post("receive", url.Values{"amount": {"3000"}, "method": {"cash"}, "key": {"k1"}})
	d, err := workshop.RentalDepositOf(ctx, pool, desk, booking)
	switch {
	case err != nil:
		t.Fatal(err)
	case d.Hold != nil || d.Balance.Received != 300_000 || d.Short() != 0:
		t.Errorf("after the release and the cash sent twice: hold %+v, %+v, short %d", d.Hold, d.Balance, d.Short())
	}
	if body := read(); !strings.Contains(body, `action="`+page+`/deposit/refund"`) {
		t.Errorf("the money received cannot be given back from the page")
	}
}
