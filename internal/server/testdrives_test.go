package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A walk-in test drive on a phone (#173): booked now for a driver who is
// only a name and a number, the licence checked, let go unsigned by the
// owner with a note, and taken back with the odometer.
func TestAWalkInTestDriveOnThePage(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	unit, err := workshop.TakeIntoStock(ctx, pool, owner, vehicleA, workshop.StockFromDefleet, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, owner, unit.ID, workshop.StockForSale, ""); err != nil {
		t.Fatal(err)
	}
	seller := signIn(t, ts, salesEmail)
	if form := get(t, seller, ts.URL+"/cars/test-drives/new", http.StatusOK); !strings.Contains(form, unit.ID) {
		t.Fatalf("the form does not offer the car:\n%s", visibleText(form))
	}
	// A wall clock ahead of now in any zone the shop could be in.
	now := time.Now().UTC().Add(3 * time.Hour).Format("2006-01-02T15:04")
	resp := postResponse(t, seller, ts.URL+"/cars/test-drives", url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"},
		"phone": {"0701234567"}, "from": {now}, "minutes": {"45"}, "area": {"Inom tullarna"}, "key": {"td-page-1"}})
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(where, "/cars/test-drives/") {
		t.Fatalf("booking answered %d, %s", resp.StatusCode, where)
	}
	if page := get(t, seller, ts.URL+where, http.StatusOK); !strings.Contains(page, "Kim Köpare") || !strings.Contains(page, "Inom tullarna") {
		t.Errorf("the drive's page:\n%s", visibleText(page))
	}
	for _, step := range []struct {
		as   *http.Client
		path string
		form url.Values
	}{
		{seller, "/licence", url.Values{"id_checked": {"1"}, "method": {"card_inspected"}, "categories": {"B"}, "age_ok": {"1"}}},
		{signIn(t, ts, ownerEmail), "/start", url.Values{"km": {"12000"}, "unsigned": {"Skrivaren är trasig"}}},
		{seller, "/return", url.Values{"km": {"12031"}}},
	} {
		if resp := postResponse(t, step.as, ts.URL+where+step.path, step.form); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s answered %d", step.path, resp.StatusCode)
		}
	}
	if page := get(t, seller, ts.URL+where, http.StatusOK); !strings.Contains(page, "12031") || !strings.Contains(page, "Skrivaren är trasig") {
		t.Errorf("the drive after it came back:\n%s", visibleText(page))
	}
	if page := get(t, signIn(t, ts, techEmail), ts.URL+where, http.StatusForbidden); strings.Contains(page, "Kim Köpare") {
		t.Error("a technician was shown a test drive")
	}
}

// The test-drive agreement is made from the shipped template when none is
// chosen, made in that write; reading the page makes nothing.
func TestATestDriveAgreementFromTheShippedTemplate(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	unit, err := workshop.TakeIntoStock(ctx, pool, owner, vehicleA, workshop.StockFromDefleet, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, owner, unit.ID, workshop.StockForSale, ""); err != nil {
		t.Fatal(err)
	}
	seller := signIn(t, ts, salesEmail)
	when := time.Now().UTC().Add(3 * time.Hour).Format("2006-01-02T15:04")
	resp := postResponse(t, seller, ts.URL+"/cars/test-drives", url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"},
		"phone": {"0701234567"}, "from": {when}, "minutes": {"30"}, "key": {"td-tpl"}})
	where := resp.Header.Get("Location")
	if page := get(t, seller, ts.URL+where, http.StatusOK); !strings.Contains(page, "From the shipped agreement") {
		t.Fatalf("the shipped agreement is not offered:\n%s", visibleText(page))
	}
	if resp := postResponse(t, seller, ts.URL+where+"/agreement", url.Values{"template": {""}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("making the agreement answered %d", resp.StatusCode)
	}
	if d, _ := workshop.TestDriveByID(ctx, pool, owner, strings.TrimPrefix(where, "/cars/test-drives/")); d.AgreementID == "" {
		t.Error("no agreement was made")
	}
}
