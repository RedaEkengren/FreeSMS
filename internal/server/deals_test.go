package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A deal through the pages (#175): opened for a walk-in buyer, a defect
// stated and accepted, how it is taxed chosen, the contract made, signed
// and completed; the car is then sold. The workshop's roles see none of
// it.
func TestASalespersonSellsACarOnThePages(t *testing.T) {
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
	if page := get(t, seller, ts.URL+"/cars/deals", http.StatusOK); !strings.Contains(page, unit.ID) {
		t.Fatalf("the new-deal form does not offer the car:\n%s", visibleText(page))
	}
	resp := postResponse(t, seller, ts.URL+"/cars/deals", url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"},
		"phone": {"0701234567"}, "buyer_kind": {"consumer"}, "price": {"125000"}, "line_desc": {"Vinterhjul"},
		"line_qty": {"1"}, "line_price": {"8000"}, "line_vat": {"25"}, "key": {"deal-page-1"}})
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(where, "/cars/deals/") {
		t.Fatalf("opening the deal answered %d, %s", resp.StatusCode, where)
	}
	page := get(t, seller, ts.URL+where, http.StatusOK)
	if !strings.Contains(page, "Kim Köpare") || !strings.Contains(page, "Vinterhjul") {
		t.Errorf("the deal's page:\n%s", visibleText(page))
	}
	deal, err := workshop.DealByID(ctx, pool, owner, strings.TrimPrefix(where, "/cars/deals/"))
	if err != nil {
		t.Fatal(err)
	}
	kim := deal.Buyers[0].PartyID
	post := func(path string, form url.Values) {
		t.Helper()
		if resp := postResponse(t, seller, ts.URL+where+path, form); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s answered %d", path, resp.StatusCode)
		}
	}
	post("/defects", url.Values{"description": {"Stenskott i vindrutan"}})
	post("/terms", url.Values{"version": {"1"}, "buyer": {kim}, "buyer_kind": {"consumer"}, "price": {"125000"}, "tax": {"ordinary"},
		"line_desc": {"Vinterhjul"}, "line_qty": {"1"}, "line_price": {"8000"}, "line_vat": {"25"}})
	page = get(t, seller, ts.URL+where, http.StatusOK)
	tpl := regexp.MustCompile(`name="template"><option value="([0-9a-f-]{36})"`).FindStringSubmatch(page)
	if tpl == nil {
		t.Fatalf("no template offered:\n%s", visibleText(page))
	}
	post("/contract", url.Values{"template": {tpl[1]}})
	deal, _ = workshop.DealByID(ctx, pool, owner, deal.ID)
	doc, err := workshop.DocumentRecordByID(ctx, pool, owner, deal.ContractID)
	if err != nil {
		t.Fatal(err)
	}
	post("/signed", url.Values{"party": {doc.Parties[0].ID}, "hash": {doc.Hash}, "method": {"in_person_click"}})
	post("/defects/"+deal.Defects[0].ID+"/answer", url.Values{"party": {kim}, "accepted": {"1"}})
	post("/complete", nil)
	if u, _ := workshop.StockUnitByID(ctx, pool, owner, unit.ID); u.Status != workshop.StockSold {
		t.Errorf("the car after the contract on the page: %s", u.Status)
	}
	if page := get(t, signIn(t, ts, techEmail), ts.URL+where, http.StatusForbidden); strings.Contains(page, "Kim Köpare") {
		t.Error("a technician was shown a deal")
	}
}

// A new deal refused comes back with everything that was typed.
func TestARefusedDealKeepsWhatWasTyped(t *testing.T) {
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
	// The margin scheme on a car moved over from the fleet, with no
	// purchase: refused.
	status, page := postBody(t, seller, ts.URL+"/cars/deals", url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"},
		"phone": {"0701234567"}, "buyer_kind": {"consumer"}, "price": {"125000"}, "tax": {"margin_scheme"},
		"warranty": {"Sex månader på motorn"}, "delivery_place": {"Hemma hos Kim"}, "odometer": {"123600"},
		"payment_terms": {"Faktura 10 dagar"}, "note": {"Ring före leverans"}, "line_desc": {"Dragkrok"},
		"line_qty": {"1"}, "line_price": {"6000"}, "line_vat": {"25"}})
	if status != http.StatusBadRequest {
		t.Fatalf("answered %d", status)
	}
	for _, want := range []string{"Sex månader på motorn", "Hemma hos Kim", "123600", "Faktura 10 dagar", "Ring före leverans",
		"Dragkrok", `value="margin_scheme" selected`, "Kim Köpare"} {
		if !strings.Contains(page, want) {
			t.Errorf("the form came back without %q", want)
		}
	}
}
