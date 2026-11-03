package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Purchases (#161).

// buy records vehicleA bought from the private seller for 85 000 kr, at
// 123 456 km, with no evidence yet.
func buy(t *testing.T, pool *pgxpool.Pool, reason string) workshop.Purchase {
	t.Helper()
	km := int64(123456)
	p, err := workshop.RecordPurchase(context.Background(), pool, buyerScope(), workshop.NewPurchase{
		VehicleID: vehicleA, Reason: reason, SellerPartyID: theSeller(t, pool),
		PurchasedOn: time.Now(), OdometerKm: &km, PriceMinor: 8_500_000, SellerType: workshop.SellerPrivate})
	if err != nil {
		t.Fatalf("RecordPurchase: %v", err)
	}
	return p
}

// boughtCar is a car of its own, with a chassis number, bought from the
// private seller for 200 000 kr: for a test that needs a purchase whatever
// else is in stock.
func boughtCar(t *testing.T, pool *pgxpool.Pool) workshop.Purchase {
	t.Helper()
	return boughtCarFrom(t, pool, theSeller(t, pool))
}

// boughtCarFrom is boughtCar from seller.
func boughtCarFrom(t *testing.T, pool *pgxpool.Pool, seller string) workshop.Purchase {
	t.Helper()
	ctx := context.Background()
	var vehicle string
	if err := pool.QueryRow(ctx, `INSERT INTO vehicles (shop_id, make, model, vin)
		VALUES ($1, 'Saab', '9-5', 'YS3' || upper(substr(md5(random()::text), 1, 14))) RETURNING id`, shopID).Scan(&vehicle); err != nil {
		t.Fatal(err)
	}
	p, err := workshop.RecordPurchase(ctx, pool, buyerScope(), workshop.NewPurchase{VehicleID: vehicle, Reason: workshop.StockFromPurchase,
		SellerPartyID: seller, PurchasedOn: time.Now(), PriceMinor: 20_000_000, SellerType: workshop.SellerPrivate})
	if err != nil {
		t.Fatalf("RecordPurchase: %v", err)
	}
	return p
}

// contractFor is a purchase agreement about the purchase, made by the buyer.
func contractFor(t *testing.T, pool *pgxpool.Pool, purchaseID string) string {
	t.Helper()
	tpl := purchaseTemplate(t, pool)
	id, err := workshop.ProduceDocument(context.Background(), pool, buyerScope(), workshop.NewDocumentRecord{
		Kind: workshop.DocPurchaseAgreement, SubjectKind: "purchase", SubjectID: purchaseID,
		TemplateVersionID: tpl.VersionID, Title: "Köpeavtal ABC 12D",
		Parties: []workshop.DocumentPartyRole{{PartyID: theSeller(t, pool), Role: "seller"}},
		Data:    map[string]string{"seller": "Sven Säljare", "registration": "ABC 12D", "price": "85 000 kr"}})
	if err != nil {
		t.Fatalf("ProduceDocument: %v", err)
	}
	return id
}

// The rule, in its one place: who may be sold under the margin scheme, on
// what.
func TestEligibilityFollowsFromTheSellerType(t *testing.T) {
	for _, c := range []struct {
		seller, reason string
		evidence       int
		want           bool
		note           string
	}{
		{workshop.SellerPrivate, workshop.StockFromPurchase, 1, true, ""},
		{workshop.SellerCompanyNoDeduction, workshop.StockFromTradeIn, 2, true, ""},
		{workshop.SellerSwedishDealerMargin, workshop.StockFromPurchase, 1, true, ""},
		{workshop.SellerPrivate, workshop.StockFromPurchase, 0, false, workshop.EligibilityNoEvidence},
		{workshop.SellerPrivate, workshop.StockFromRepurchase, 1, false, workshop.EligibilityRepurchased},
		{workshop.SellerEUDealerMargin, workshop.StockFromPurchase, 1, false, workshop.EligibilityEUDealer},
		{workshop.SellerCompanyChargedVAT, workshop.StockFromPurchase, 1, false, ""},
		{workshop.SellerEUReverseCharge, workshop.StockFromPurchase, 1, false, ""},
		{workshop.SellerNonEUImport, workshop.StockFromPurchase, 1, false, ""},
		{workshop.SellerNewMeansOfTransport, workshop.StockFromPurchase, 1, false, ""},
		{"", workshop.StockFromPurchase, 1, false, ""},
	} {
		if got, note := workshop.EligibleFor(c.seller, c.reason, c.evidence); got != c.want || note != c.note {
			t.Errorf("%s, %s, %d evidence: %v %q, want %v %q", c.seller, c.reason, c.evidence, got, note, c.want, c.note)
		}
	}
}

// A car bought is recorded once, with the cycle it opens, the odometer
// then and the seller as a seller; with no evidence it is not eligible, and
// the evidence added is a correction that supersedes the first assessment,
// with why, on the purchase's trail.
func TestAPurchaseFreezesItsEvidenceForTheMarginScheme(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	if p.TaxPurchasePriceMinor != 8_500_000 || p.OdometerKm == nil || *p.OdometerKm != 123456 || p.VehicleID != vehicleA {
		t.Errorf("the purchase: %+v", p)
	}
	if e := p.Eligibility; e.Eligible || e.SellerType != workshop.SellerPrivate || e.Note != workshop.EligibilityNoEvidence || e.Reason != "" {
		t.Errorf("assessed with no evidence: %+v", e)
	}
	unit, err := workshop.StockUnitByID(ctx, pool, buyerScope(), p.StockUnitID)
	if err != nil || unit.Reason != workshop.StockFromPurchase || unit.Status != workshop.StockInPreparation {
		t.Errorf("the unit it opened: %+v, %v", unit, err)
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT source FROM odometer_readings WHERE vehicle_id = $1 AND km = 123456`, vehicleA).Scan(&source); err != nil || source != "purchase" {
		t.Errorf("the odometer reading: %q, %v", source, err)
	}
	seller, err := workshop.PartyByID(ctx, pool, buyerScope(), p.SellerPartyID)
	if open, _ := rolesOf(seller, workshop.PartySeller); err != nil || open != 1 {
		t.Errorf("the seller as a seller: %d open, %v", open, err)
	}

	doc := contractFor(t, pool, p.ID)
	if _, err := workshop.CorrectVATEligibility(ctx, pool, buyerScope(), p.ID, workshop.SellerPrivate, []string{doc}, " "); !errors.Is(err, workshop.ErrEligibilityNoReason) {
		t.Errorf("corrected with no reason: %v", err)
	}
	if _, err := workshop.CorrectVATEligibility(ctx, pool, buyerScope(), p.ID, workshop.SellerPrivate,
		[]string{"99999999-9999-9999-9999-999999999999"}, "Avtal"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("evidence that is no document: %v", err)
	}
	e, err := workshop.CorrectVATEligibility(ctx, pool, buyerScope(), p.ID, workshop.SellerPrivate, []string{doc}, "Köpeavtalet upprättat")
	if err != nil || !e.Eligible || e.Note != "" || len(e.EvidenceDocumentIDs) != 1 || e.EvidenceDocumentIDs[0] != doc || e.Reason != "Köpeavtalet upprättat" {
		t.Fatalf("with the contract as evidence: %+v, %v", e, err)
	}
	// The seller turned out to be a business that charged VAT.
	e, err = workshop.CorrectVATEligibility(ctx, pool, buyerScope(), p.ID, workshop.SellerCompanyChargedVAT, []string{doc}, "Säljaren var momsregistrerad")
	if err != nil || e.Eligible {
		t.Errorf("a seller that charged VAT: %+v, %v", e, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vat_eligibility WHERE purchase_id = $1`, p.ID).Scan(&rows); err != nil || rows != 3 {
		t.Errorf("%d assessments kept, want all 3 (%v)", rows, err)
	}
	trail, err := workshop.AuditTrail(ctx, pool, buyerScope(), "purchase", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, ev := range trail {
		events = append(events, ev.Event+":"+ev.From+">"+ev.To)
	}
	if len(events) != 3 || events[0] != "purchase.recorded:>" || events[1] != "vat_eligibility.corrected:not_eligible>eligible" ||
		events[2] != "vat_eligibility.corrected:eligible>not_eligible" {
		t.Errorf("the purchase's trail: %v", events)
	}
}

// What was paid in euros is kept as paid, with the rate and where it came
// from, and turned into kronor for the tax purchase price.
func TestAPurchaseInAnotherCurrencyKeepsTheRate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	n := workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase, SellerPartyID: theSeller(t, pool),
		PurchasedOn: time.Now(), SellerType: workshop.SellerEUDealerMargin,
		Currency: "eur", OriginalMinor: 1_000_000, RateMicro: 11_234_567, RateSource: "Riksbanken 2026-10-30"}
	bad := n
	bad.RateSource = ""
	if _, err := workshop.RecordPurchase(ctx, pool, buyerScope(), bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("euros with no source for the rate: %v", err)
	}
	bad = n
	bad.PriceMinor = 1
	if _, err := workshop.RecordPurchase(ctx, pool, buyerScope(), bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("euros with kronor as well: %v", err)
	}
	p, err := workshop.RecordPurchase(ctx, pool, buyerScope(), n)
	if err != nil {
		t.Fatal(err)
	}
	// 10 000,00 EUR at 11,234567 kr is 112 345,67 kr.
	if p.TaxPurchasePriceMinor != 11_234_567 || p.Currency != "EUR" || p.OriginalMinor != 1_000_000 || p.RateMicro != 11_234_567 ||
		p.RateSource != "Riksbanken 2026-10-30" {
		t.Errorf("bought in euros: %+v", p)
	}
	if p.Eligibility.Eligible || p.Eligibility.Note != workshop.EligibilityEUDealer {
		t.Errorf("an EU dealer's margin-scheme sale before the accountant has answered: %+v", p.Eligibility)
	}
}

// The purchase price is the buyer's and the owner's: a salesperson asking
// directly is refused and handed nothing, and reads the eligibility, which
// carries no price. Nobody else records a purchase; with car sales off it
// stays readable and none is recorded.
func TestOnlyThoseWhoMaySeeWhatACarCostReadItsPurchase(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	got, err := workshop.PurchaseOfUnit(ctx, pool, sales(), p.StockUnitID)
	if !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson read the purchase: %v", err)
	}
	if handed := testsupport.Handed(got, "TaxPurchasePriceMinor", "OriginalMinor", "SellerPartyID", "ID"); len(handed) != 0 {
		t.Errorf("a salesperson was handed %v", handed)
	}
	if fields := testsupport.FieldsLike(workshop.VATEligibility{}, `(?i)price|minor|cost|amount|rate`); len(fields) != 0 {
		t.Errorf("the eligibility a salesperson reads has %v", fields)
	}
	if e, err := workshop.VATEligibilityOfUnit(ctx, pool, sales(), p.StockUnitID); err != nil || e.ID != p.Eligibility.ID {
		t.Errorf("a salesperson reading the eligibility: %+v, %v", e, err)
	}
	if _, err := workshop.AuditTrail(ctx, pool, sales(), "purchase", p.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson read the purchase's trail: %v", err)
	}
	for name, s := range map[string]access.Scope{"a salesperson": sales(), "the front desk": everything(advisor()), "the rental desk": rentalDesk()} {
		if _, err := workshop.RecordPurchase(ctx, pool, s, workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase,
			SellerPartyID: theSeller(t, pool), PurchasedOn: time.Now(), SellerType: workshop.SellerPrivate}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s recorded a purchase: %v", name, err)
		}
		if _, err := workshop.CorrectVATEligibility(ctx, pool, s, p.ID, workshop.SellerPrivate, nil, "x"); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s corrected an assessment: %v", name, err)
		}
	}
	if _, err := workshop.VATEligibilityOfUnit(ctx, pool, everything(technician()), p.StockUnitID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the eligibility: %v", err)
	}

	off := buyerScope()
	off.Modules = []access.Module{access.Workshop}
	if got, err := workshop.PurchaseOfUnit(ctx, pool, off, p.StockUnitID); err != nil || got.TaxPurchasePriceMinor != 8_500_000 {
		t.Errorf("the purchase with car sales off: %+v, %v", got, err)
	}
	if _, err := workshop.CorrectVATEligibility(ctx, pool, off, p.ID, workshop.SellerPrivate, nil, "x"); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("corrected with car sales off: %v", err)
	}
	if _, err := workshop.RecordPurchase(ctx, pool, off, workshop.NewPurchase{}); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("recorded with car sales off: %v", err)
	}
}

// A purchase and its assessments are facts: not changed or deleted by the
// application, and an assessment not by anybody. Once the car's sale is
// invoiced, its eligibility can no longer be corrected.
func TestAPurchaseAndItsEligibilityStayAsTheyWere(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	for _, stmt := range []string{
		`UPDATE vehicle_purchases SET tax_purchase_price_minor = 1`,
		`DELETE FROM vehicle_purchases`,
		`UPDATE vat_eligibility SET eligible = true`,
		`DELETE FROM vat_eligibility`,
	} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE vat_eligibility SET note = NULL`); err == nil {
		t.Error("an assessment was changed by the table's owner")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vat_eligibility (shop_id, purchase_id, seller_type, eligible) VALUES ($1, $2, 'company_charged_vat', true)`,
		shopID, p.ID); err == nil {
		t.Error("a seller that charged VAT was written as eligible")
	}

	// The car sold and invoiced, as the sale (M8) will write it.
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var sale string
		if err := tx.QueryRow(ctx, `UPDATE stock_units SET vehicle_sale_id = gen_random_uuid() WHERE id = $1 RETURNING vehicle_sale_id`,
			p.StockUnitID).Scan(&sale); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO invoices (shop_id, series, number, source, vehicle_sale_id, customer_id, customer_name, net_minor, vat_minor, gross_minor)
			VALUES ($1, 'S', 1, 'vehicle_sale', $2, $3, 'A Customer', 1000, 250, 1250)`, shopID, sale, fixtureCustomer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CorrectVATEligibility(ctx, pool, buyerScope(), p.ID, workshop.SellerCompanyChargedVAT, nil, "För sent"); !errors.Is(err, workshop.ErrInvalid) ||
		err.Error() != "workshop: invalid: the car's sale has been invoiced, so its VAT eligibility can no longer be corrected" {
		t.Errorf("corrected after the sale was invoiced: %v", err)
	}
}

// Replayed from a phone, a purchase finds the one it made; the same key for
// another purchase is refused. A car bought back starts a new cycle, assessed
// again rather than inheriting the first.
func TestAPurchaseReplayedFindsItselfAndABuyBackIsAssessedAgain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	n := workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase, SellerPartyID: theSeller(t, pool),
		PurchasedOn: time.Now(), PriceMinor: 5_000_000, SellerType: workshop.SellerPrivate, Key: "köp-1"}
	a, err := workshop.RecordPurchase(ctx, pool, buyerScope(), n)
	if err != nil {
		t.Fatal(err)
	}
	b, err := workshop.RecordPurchase(ctx, pool, buyerScope(), n)
	if err != nil || b.ID != a.ID {
		t.Errorf("replayed: %+v, %v; want %s", b, err, a.ID)
	}
	n.PriceMinor = 1
	if _, err := workshop.RecordPurchase(ctx, pool, buyerScope(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the key used for another price: %v", err)
	}
	doc := contractFor(t, pool, a.ID)
	if _, err := workshop.CorrectVATEligibility(ctx, pool, buyerScope(), a.ID, workshop.SellerPrivate, []string{doc}, "Avtal"); err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, a.StockUnitID, workshop.StockForSale, workshop.StockReserved, workshop.StockSold, workshop.StockDelivered)
	back := buy(t, pool, workshop.StockFromRepurchase)
	if back.StockUnitID == a.StockUnitID || back.Eligibility.Eligible || back.Eligibility.Note != workshop.EligibilityRepurchased {
		t.Errorf("bought back: %+v", back)
	}
	if units, _ := workshop.StockUnitsOfVehicle(ctx, pool, sales(), vehicleA); len(units) != 2 {
		t.Errorf("%d cycles, want 2", len(units))
	}
}

// A seller typed with the purchase is made in the purchase's transaction:
// recorded, the car is bought from them; refused -- the car is already in
// stock -- nobody is left behind with no purchase to be kept for. Replayed
// with the same key, the seller is not made twice.
func TestASellerTypedWithThePurchaseIsMadeOnlyWithIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	people := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM people WHERE display_name = 'Sam Säljare'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase, PurchasedOn: time.Now(),
		PriceMinor: 8_500_000, SellerType: workshop.SellerPrivate, Key: "buy-1",
		NewSeller: &workshop.NewParty{Kind: "private", Name: "Sam Säljare", Phone: "070-111 22 33"}}
	p, err := workshop.RecordPurchase(ctx, pool, buyerScope(), n)
	if err != nil {
		t.Fatal(err)
	}
	if p.SellerPartyID == "" || people() != 1 {
		t.Errorf("bought from a seller typed with it: %+v, %d people", p, people())
	}
	again, err := workshop.RecordPurchase(ctx, pool, buyerScope(), n)
	if err != nil || again.ID != p.ID || people() != 1 {
		t.Errorf("the same request again: %+v, %v, %d people", again, err, people())
	}
	n.Key = "buy-2"
	if _, err := workshop.RecordPurchase(ctx, pool, buyerScope(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("the car bought again while in stock: %v", err)
	}
	if people() != 1 {
		t.Errorf("a refused purchase left its seller behind: %d people", people())
	}
}
