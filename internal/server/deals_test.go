package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RedaEkengren/FreeSMS/internal/database"
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
	if page := get(t, seller, ts.URL+where, http.StatusOK); !strings.Contains(page, "From the shipped contract") {
		t.Fatalf("the shipped contract is not offered:\n%s", visibleText(page))
	}
	post("/contract", url.Values{"template": {""}})
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

// Reading a deal writes nothing: the shipped contract template is made when
// a contract is first made from it, not when a page is looked at.
func TestReadingADealWritesNothing(t *testing.T) {
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
	resp := postResponse(t, seller, ts.URL+"/cars/deals", url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"},
		"phone": {"0701234567"}, "buyer_kind": {"consumer"}, "price": {"125000"}, "key": {"read-1"}})
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(where, "/cars/deals/") {
		t.Fatalf("opening the deal answered %d, %q", resp.StatusCode, where)
	}
	get(t, seller, ts.URL+where, http.StatusOK)
	var n int
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM document_templates WHERE kind = 'sales_contract'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("reading the deal made %d templates (%v)", n, err)
	}
	if resp := postResponse(t, seller, ts.URL+where+"/contract", url.Values{"template": {""}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("making the contract from the shipped template answered %d", resp.StatusCode)
	}
	if d, _ := workshop.DealByID(ctx, pool, owner, strings.TrimPrefix(where, "/cars/deals/")); d.ContractID == "" {
		t.Error("no contract was made from the shipped template")
	}
}

// A form refused for one bad field -- a new deal or a revision -- comes
// back with every field as it was typed, the bad one too.
func TestARefusedTermsFormKeepsEveryTypedField(t *testing.T) {
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
	typed := url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"}, "phone": {"0701234567"}, "buyer_kind": {"business"},
		"price": {"125000"}, "tax": {"ordinary"}, "warranty": {"Sex månader"}, "delivery_on": {"2026-12-24"},
		"delivery_place": {"Hemma"}, "odometer": {"12x"}, "payment_terms": {"Faktura"}, "note": {"Ring först"},
		"line_desc": {"Dragkrok"}, "line_qty": {"2"}, "line_price": {"6000"}, "line_vat": {"12"}}
	want := []string{"Sex månader", `value="2026-12-24"`, "Hemma", `value="12x"`, "Faktura", "Ring först", "Dragkrok",
		`value="2"`, `value="6000"`, `value="12"`, `value="business" selected`, `value="ordinary" selected`}
	status, page := postBody(t, seller, ts.URL+"/cars/deals", typed)
	if status != http.StatusBadRequest {
		t.Fatalf("a new deal with a bad odometer answered %d", status)
	}
	for _, w := range want {
		if !strings.Contains(page, w) {
			t.Errorf("the new deal came back without %s", w)
		}
	}

	typed.Set("odometer", "123600")
	resp := postResponse(t, seller, ts.URL+"/cars/deals", typed)
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the deal answered %d", resp.StatusCode)
	}
	d, err := workshop.DealByID(ctx, pool, owner, strings.TrimPrefix(where, "/cars/deals/"))
	if err != nil {
		t.Fatal(err)
	}
	typed.Set("odometer", "12x")
	typed.Set("version", "1")
	typed.Set("buyer", d.Buyers[0].PartyID)
	typed.Set("warranty", "Ett år")
	status, page = postBody(t, seller, ts.URL+where+"/terms", typed)
	if status != http.StatusBadRequest {
		t.Fatalf("a revision with a bad odometer answered %d", status)
	}
	for _, w := range append(want[1:], "Ett år") {
		if !strings.Contains(page, w) {
			t.Errorf("the revision came back without %s", w)
		}
	}
}

// A margin-scheme sale invoiced from the deal's page: the invoice the
// owner opens says "Vinstmarginalbeskattning för begagnade varor", shows
// the car at its price with no VAT for it, and shows neither the margin
// nor what the car was bought for.
func TestAMarginSchemeInvoiceOnThePage(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	// Bought for 85 000 kr from a private seller, with the agreement as
	// evidence: eligible for the margin scheme.
	sellerParty, err := workshop.CreateParty(ctx, pool, owner, workshop.NewParty{Kind: "private", Name: "Sälj Säljare",
		Role: workshop.PartySeller})
	if err != nil {
		t.Fatal(err)
	}
	p, err := workshop.RecordPurchase(ctx, pool, owner, workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase,
		SellerPartyID: sellerParty, PurchasedOn: time.Now(), PriceMinor: 8_500_000, SellerType: workshop.SellerPrivate})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := workshop.SaveDocumentTemplate(ctx, pool, owner, "", workshop.DocPurchaseAgreement, "Köpeavtal", "{seller} säljer.")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := workshop.ProduceDocument(ctx, pool, owner, workshop.NewDocumentRecord{Kind: workshop.DocPurchaseAgreement,
		SubjectKind: "purchase", SubjectID: p.ID, TemplateVersionID: tpl.VersionID, Title: "Köpeavtal",
		Parties: []workshop.DocumentPartyRole{{PartyID: sellerParty, Role: "seller"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CorrectVATEligibility(ctx, pool, owner, p.ID, workshop.SellerPrivate, []string{evidence}, "Avtalet"); err != nil {
		t.Fatal(err)
	}
	unit := workshop.StockUnit{ID: p.StockUnitID}
	if err := workshop.MoveStockUnit(ctx, pool, owner, unit.ID, workshop.StockForSale, ""); err != nil {
		t.Fatal(err)
	}
	seller := signIn(t, ts, salesEmail)
	resp := postResponse(t, seller, ts.URL+"/cars/deals", url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"},
		"phone": {"0701234567"}, "buyer_kind": {"consumer"}, "price": {"125000"}, "tax": {"margin_scheme"}})
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("opening the deal answered %d", resp.StatusCode)
	}
	post := func(path string, form url.Values) {
		t.Helper()
		if resp := postResponse(t, seller, ts.URL+where+path, form); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s answered %d", path, resp.StatusCode)
		}
	}
	post("/contract", url.Values{"template": {""}})
	deal, _ := workshop.DealByID(ctx, pool, owner, strings.TrimPrefix(where, "/cars/deals/"))
	doc, _ := workshop.DocumentRecordByID(ctx, pool, owner, deal.ContractID)
	post("/signed", url.Values{"party": {doc.Parties[0].ID}, "hash": {doc.Hash}, "method": {"in_person_click"}})
	post("/complete", nil)
	post("/invoice", nil)
	deal, _ = workshop.DealByID(ctx, pool, owner, deal.ID)
	if deal.InvoiceID == "" {
		t.Fatal("the deal was not invoiced")
	}
	page := get(t, signIn(t, ts, ownerEmail), ts.URL+"/invoices/"+deal.InvoiceID, http.StatusOK)
	text := visibleText(page)
	for _, want := range []string{"Vinstmarginalbeskattning för begagnade varor", "125"} {
		if !strings.Contains(text, want) {
			t.Errorf("the invoice does not say %q:\n%s", want, text)
		}
	}
	for _, never := range []string{"40 000", "40000", "85 000", "85000", "8 000,00", "8000.00"} {
		if strings.Contains(text, never) {
			t.Errorf("the invoice shows %q, the margin or what the car cost:\n%s", never, text)
		}
	}
}

// A revision made over one somebody else saved meanwhile is refused, and
// the form comes back with the terms as they now stand -- so sending it
// again cannot overwrite theirs unseen -- and what was typed shown apart.
func TestARevisionOverAnotherShowsTheCurrentTermsAndWhatWasTyped(t *testing.T) {
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
	form := url.Values{"unit": {unit.ID}, "name": {"Kim Köpare"}, "phone": {"0701234567"}, "buyer_kind": {"consumer"}, "price": {"125000"}}
	resp := postResponse(t, seller, ts.URL+"/cars/deals", form)
	where := resp.Header.Get("Location")
	d, err := workshop.DealByID(ctx, pool, owner, strings.TrimPrefix(where, "/cars/deals/"))
	if err != nil {
		t.Fatal(err)
	}
	theirs := d.Terms
	theirs.Warranty = "Deras garanti"
	if _, err := workshop.ReviseDeal(ctx, pool, owner, d.ID, 1, theirs); err != nil {
		t.Fatal(err)
	}
	mine := url.Values{"version": {"1"}, "buyer": {d.Buyers[0].PartyID}, "buyer_kind": {"business"}, "price": {"125000"},
		"tax": {"ordinary"}, "warranty": {"Min garanti"}}
	status, page := postBody(t, seller, ts.URL+where+"/terms", mine)
	if status != http.StatusBadRequest {
		t.Fatalf("a revision over another answered %d", status)
	}
	for _, want := range []string{`name="version" value="2"`, `value="Deras garanti"`, "Min garanti", "How the sale is taxed: Ordinary VAT",
		"The buyer is: A business"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not have %s", want)
		}
	}
	if strings.Contains(page, `value="Min garanti"`) {
		t.Error("what was typed is back in the form, to overwrite theirs when sent again")
	}
}
