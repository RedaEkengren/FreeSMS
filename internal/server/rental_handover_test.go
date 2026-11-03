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

// The rental desk checks a tent out and in from its phone: the booking
// page starts each, the protocol page records each checkpoint and
// finishes it, and at return a checkpoint worse than at check-out is
// shown as new and, ticked, becomes a damage case on the booking page.
func TestTheRentalDeskChecksAUnitOutAndIn(t *testing.T) {
	ts, pool := testServer(t)
	seedRentalDesk(t, func(t *testing.T, stmts []string) { seedShop(t, pool, shopA, stmts) })
	ctx := t.Context()
	owner := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-000000000011", Role: access.Of(access.RoleOwner), Modules: access.Modules}
	desk := access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-0000000000b2", Role: access.Of(access.RoleRentalAgent), Modules: access.Modules}
	tpl, err := workshop.SaveChecklistTemplate(ctx, pool, owner, "", "Tält", []string{"Duk", "Pinnar"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetTemplateSubjects(ctx, pool, owner, tpl, []string{workshop.SubjectCheckOut, workshop.SubjectCheckIn}); err != nil {
		t.Fatal(err)
	}
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
	rules := equipment.Rules
	rules.CheckOutTemplateID, rules.CheckInTemplateID = tpl, tpl
	if _, err := workshop.SaveRentalProfile(ctx, pool, owner, equipment.ID, rules); err != nil {
		t.Fatal(err)
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
	walk := func(action string, status map[string]string, finish url.Values) string {
		t.Helper()
		resp := postModuleForm(t, client, ts.URL, page+"/"+action, url.Values{})
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/rental/handovers/") {
			t.Fatalf("%s: %d %s", action, resp.StatusCode, readBody(t, resp))
		}
		protocol := resp.Header.Get("Location")
		p, err := workshop.ProtocolByID(ctx, pool, desk, strings.TrimPrefix(protocol, "/rental/handovers/"))
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			s := status[it.Label]
			if s == "" {
				s = "pass"
			}
			if resp := postModuleForm(t, client, ts.URL, protocol+"/items/"+it.ID, url.Values{"status": {s}}); resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("recording %s: %d %s", it.Label, resp.StatusCode, readBody(t, resp))
			}
		}
		resp, _ = client.Get(ts.URL + protocol)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="`+protocol+`/finish"`) {
			t.Fatalf("the protocol page: %d %s", resp.StatusCode, body)
		}
		// A generator or a lift has a tank or a battery and no odometer.
		if strings.Contains(body, `name="odometer"`) || !strings.Contains(body, `name="fuel"`) {
			t.Error("a tent's protocol page: an odometer asked for, or no fuel or charge offered")
		}
		if action == "check-in" {
			for _, it := range p.Items {
				if it.Label == "Duk" {
					finish.Set("damage", it.ID)
				}
			}
			if !strings.Contains(body, "new since the check-out") {
				t.Error("the check-in does not show the torn cloth as new")
			}
		}
		if resp := postModuleForm(t, client, ts.URL, protocol+"/finish", finish); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != page {
			t.Fatalf("finishing %s: %d %s", action, resp.StatusCode, readBody(t, resp))
		}
		return protocol
	}
	walk("check-out", nil, url.Values{"renter_present": {"1"}, "renter_accepted": {"1"}})
	resp, _ := client.Get(ts.URL + page)
	if body := readBody(t, resp); !strings.Contains(body, "Checked out") || !strings.Contains(body, `action="`+page+`/check-in"`) {
		t.Errorf("the booking page after check-out:\n%s", body)
	}
	walk("check-in", map[string]string{"Duk": "fail"}, url.Values{"note": {"Nyckeln i lådan"}})
	resp, _ = client.Get(ts.URL + page)
	body := readBody(t, resp)
	for _, want := range []string{"Checked in", "the renter was not there", "Damage on the unit", "Duk"} {
		if !strings.Contains(body, want) {
			t.Errorf("the booking page after check-in lacks %q", want)
		}
	}
	// Returned, a charge is confirmed on the page and listed, its VAT
	// treatment marked not confirmed until the shop says (#195).
	if resp := postModuleForm(t, client, ts.URL, page+"/charges", url.Values{"kind": {"cleaning"}, "description": {"Städning av tältduk"},
		"quantity": {"1"}, "price": {"400"}, "key": {"k1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirming a charge: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp, _ = client.Get(ts.URL + page)
	body = readBody(t, resp)
	for _, want := range []string{"Städning av tältduk", "/charges/", "/inform"} {
		if !strings.Contains(body, want) {
			t.Errorf("the booking page with a charge lacks %q", want)
		}
	}
	if !strings.Contains(body, "VAT treatment not confirmed") && !strings.Contains(body, "Momsbehandlingen inte bekräftad") {
		t.Errorf("the charge's unconfirmed VAT treatment is not said")
	}
	// Told of the charge, the rental is invoiced from the page (#199).
	charges, err := workshop.RentalCharges(ctx, pool, desk, booking)
	if err != nil || len(charges) != 1 {
		t.Fatalf("the charges: %+v, %v", charges, err)
	}
	if resp := postModuleForm(t, client, ts.URL, page+"/invoice", url.Values{}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invoiced before the customer was told of the charge: %d", resp.StatusCode)
	}
	if resp := postModuleForm(t, client, ts.URL, page+"/charges/"+charges[0].ID+"/inform", url.Values{"channel": {"email"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("telling the customer: %d %s", resp.StatusCode, readBody(t, resp))
	}
	if resp := postModuleForm(t, client, ts.URL, page+"/invoice", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("invoicing the rental: %d %s", resp.StatusCode, readBody(t, resp))
	}
	invoices, err := workshop.RentalInvoices(ctx, pool, desk, booking)
	if err != nil || len(invoices) != 1 {
		t.Fatalf("the rental's invoices: %+v, %v", invoices, err)
	}
	resp, _ = client.Get(ts.URL + page)
	if body := readBody(t, resp); !strings.Contains(body, invoices[0].Reference) {
		t.Errorf("the booking page does not list its invoice %s", invoices[0].Reference)
	}
	tech := signIn(t, ts, techEmail)
	if resp, _ := tech.Get(ts.URL + page); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a technician's view of the booking: %d", resp.StatusCode)
	}
}
