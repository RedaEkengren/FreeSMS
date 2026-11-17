package workshop_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The edges of a trade-in (#165): two on one deal, one worth more than the
// car sold, a deal called off before and after the car is handed over, and a
// seller who charges VAT.

// tradeInPurchases reads the purchases made for a deal's trade-ins, by key.
func tradeInPurchases(t *testing.T, pool *pgxpool.Pool, saleID string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT key, purchase_id FROM vehicle_sale_trade_in_receipts WHERE sale_id = $1`, saleID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, p string
		if err := rows.Scan(&k, &p); err != nil {
			t.Fatal(err)
		}
		out[k] = p
	}
	return out
}

// heldForSale is what the shop holds for the buyer of a sale, over its
// deposits.
func heldForSale(t *testing.T, pool *pgxpool.Pool, saleID string) int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT id FROM deposits WHERE subject_kind = 'sale' AND subject_id = $1`, saleID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	var held int64
	for _, id := range ids {
		b, err := workshop.DepositBalanceFor(ctx, pool, everything(owner()), id)
		if err != nil {
			t.Fatal(err)
		}
		held += b.Held()
	}
	return held
}

// Two trade-ins on one deal are two purchases, two stock units, each
// credited once, and the invoice owes what is left after both.
func TestTwoTradeInsOnOneDealAreTwoPurchases(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	first := tradeIn(3_000_000, 0)
	second := tradeIn(2_000_000, 500_000)
	second.Registration = "TRD 456"
	d := contractWith(t, pool, eligibleCar(t, pool), driver(t, pool), first, second)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"TRD123", "TRD456"} {
		if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, k, time.Now()); err != nil {
			t.Fatalf("receive %s: %v", k, err)
		}
		clearChecks(t, pool, d.ID, k)
	}
	purchases := tradeInPurchases(t, pool, d.ID)
	if len(purchases) != 2 || purchases["TRD123"] == purchases["TRD456"] {
		t.Fatalf("the purchases: %v; want two, each its own", purchases)
	}
	if want := inv.GrossMinor - 3_000_000 - 1_500_000; outstanding(t, pool, inv.ID) != want {
		t.Errorf("owed after both: %d, want %d", outstanding(t, pool, inv.ID), want)
	}
	// Each is its own car in stock, bought at its own credit.
	var units, prices int64
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT stock_unit_id), sum(tax_purchase_price_minor) FROM vehicle_purchases WHERE id = ANY($1)`,
		[]string{purchases["TRD123"], purchases["TRD456"]}).Scan(&units, &prices); err != nil || units != 2 || prices != 5_000_000 {
		t.Errorf("the units and their prices: %d, %d (%v); want 2 units at 50 000 kr in all", units, prices, err)
	}
	// Received again, nothing more is made or credited.
	for _, k := range []string{"TRD123", "TRD456"} {
		if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, k, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if len(tradeInPurchases(t, pool, d.ID)) != 2 || outstanding(t, pool, inv.ID) != inv.GrossMinor-4_500_000 {
		t.Errorf("a second receipt changed something: %d owed", outstanding(t, pool, inv.ID))
	}
}

// A trade-in worth more than the car sold clears the invoice and leaves the
// difference held for the buyer, to be paid back: the invoice is never owed
// a negative amount.
func TestATradeInWorthMoreThanTheCarLeavesTheDifferenceHeld(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := contractWith(t, pool, eligibleCar(t, pool), driver(t, pool), tradeIn(20_000_000, 0))
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now()); err != nil {
		t.Fatal(err)
	}
	clearChecks(t, pool, d.ID, "TRD123")
	if owed := outstanding(t, pool, inv.ID); owed != 0 {
		t.Errorf("owed after a trade-in worth more than the car: %d", owed)
	}
	if want := 20_000_000 - inv.GrossMinor; heldForSale(t, pool, d.ID) != want || want <= 0 {
		t.Errorf("held for the buyer: %d, want the difference %d", heldForSale(t, pool, d.ID), want)
	}
}

// A deal called off before the car is handed over has no purchase to cancel
// and takes none afterwards; one called off after it keeps the purchase, a
// completed one, and what is held for the buyer.
func TestADealCalledOffBeforeAndAfterTheTradeInIsHandedOver(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)

	before := contractWith(t, pool, eligibleCar(t, pool), kim, tradeIn(2_000_000, 0))
	if err := workshop.CancelDeal(ctx, pool, sales(), before.ID, "Kunden ångrade sig"); err != nil {
		t.Fatal(err)
	}
	if got := tradeInPurchases(t, pool, before.ID); len(got) != 0 {
		t.Errorf("a trade-in not handed over has purchases: %v", got)
	}
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), before.ID, "TRD123", time.Now()); err == nil {
		t.Error("a trade-in was received for a deal that is off")
	}

	moveStock(t, pool, before.StockUnitID, workshop.StockForSale)
	after := contractWith(t, pool, before.StockUnitID, kim, tradeIn(2_000_000, 0))
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), after.ID, "TRD123", time.Now()); err != nil {
		t.Fatal(err)
	}
	clearChecks(t, pool, after.ID, "TRD123")
	if err := workshop.CancelDeal(ctx, pool, sales(), after.ID, "Kunden ångrade sig"); err != nil {
		t.Fatal(err)
	}
	purchases := tradeInPurchases(t, pool, after.ID)
	if len(purchases) != 1 {
		t.Fatalf("the purchases of a trade-in that was handed over: %v", purchases)
	}
	var closed bool
	if err := pool.QueryRow(ctx, `SELECT u.closed_at IS NOT NULL FROM vehicle_purchases p JOIN stock_units u ON u.id = p.stock_unit_id WHERE p.id = $1`,
		purchases["TRD123"]).Scan(&closed); err != nil || closed {
		t.Errorf("the trade-in's purchase after the deal was called off: closed %v, %v", closed, err)
	}
	if held := heldForSale(t, pool, after.ID); held != 2_000_000 {
		t.Errorf("held for the buyer after the deal was called off: %d, want the trade-in's 20 000 kr", held)
	}
}

// A trade-in from a business that charges VAT is bought, but not under the
// margin scheme.
func TestATradeInFromABusinessChargingVATIsNotEligible(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	ti := tradeIn(2_000_000, 0)
	ti.SellerType = workshop.SellerCompanyChargedVAT
	d := contractWith(t, pool, eligibleCar(t, pool), driver(t, pool), ti)
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now()); err != nil {
		t.Fatal(err)
	}
	var unit string
	if err := pool.QueryRow(ctx, `SELECT p.stock_unit_id FROM vehicle_sale_trade_in_receipts r JOIN vehicle_purchases p ON p.id = r.purchase_id
		WHERE r.sale_id = $1`, d.ID).Scan(&unit); err != nil {
		t.Fatal(err)
	}
	e, err := workshop.VATEligibilityOfUnit(ctx, pool, buyerScope(), unit)
	// Not for want of evidence -- that has a reason of its own -- but because
	// of who sold it: a seller that charged VAT leaves no margin to tax.
	if err != nil || e.Eligible || e.SellerType != workshop.SellerCompanyChargedVAT || e.Note != "" {
		t.Errorf("the trade-in's eligibility: %+v, %v; want not eligible as a company charging VAT, with no other reason", e, err)
	}
}

// The buyer's car is looked at when it is handed over and is worth less than
// the contract says (#165). The dealer revises the terms with the lower
// credit; that is a new version the buyer must sign, and until it is, the
// car is not received at the new figure -- never a silent change. What is
// held for the buyer is what the signed terms say, and the first contract
// stays as it was.
func TestAReValuedTradeInIsANewContractTheBuyerSignsBeforeAnythingIsCredited(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)
	d := contractWith(t, pool, eligibleCar(t, pool), kim, tradeIn(2_000_000, 0))
	firstContract := d.ContractedDocumentID

	terms := d.Terms
	terms.TradeIns = []workshop.TradeIn{tradeIn(1_500_000, 0)}
	revised, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, d.Version, terms)
	if err != nil {
		t.Fatal(err)
	}
	if revised.Completed() {
		t.Fatal("revised terms stand as a completed contract without the buyer's signature")
	}
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now()); err == nil {
		t.Error("the car was received at a figure the buyer has not signed")
	}
	if held := heldForSale(t, pool, d.ID); held != 0 {
		t.Errorf("held before the new contract is signed: %d", held)
	}

	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", revised.ContractID)
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	again, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ContractedDocumentID == firstContract || again.Terms.TradeIns[0].CreditMinor != 1_500_000 {
		t.Errorf("the new contract: %s (first %s), credit %d", again.ContractedDocumentID, firstContract, again.Terms.TradeIns[0].CreditMinor)
	}
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now()); err != nil {
		t.Fatal(err)
	}
	clearChecks(t, pool, d.ID, "TRD123")
	if held := heldForSale(t, pool, d.ID); held != 1_500_000 {
		t.Errorf("held for the buyer: %d, want the 15 000 kr the buyer signed for", held)
	}
	var price int64
	if err := pool.QueryRow(ctx, `SELECT p.tax_purchase_price_minor FROM vehicle_sale_trade_in_receipts r JOIN vehicle_purchases p ON p.id = r.purchase_id WHERE r.sale_id = $1`, d.ID).Scan(&price); err != nil || price != 1_500_000 {
		t.Errorf("the trade-in's purchase price: %d, %v", price, err)
	}
}

// Once the car is received and credited, its value is settled; a revision of
// the terms that would change what was credited is what the shop cannot
// quietly do.
func TestATradeInAlreadyCreditedCannotBeRevaluedByRevisingTheTerms(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)
	d := contractWith(t, pool, eligibleCar(t, pool), kim, tradeIn(2_000_000, 0))
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now()); err != nil {
		t.Fatal(err)
	}
	clearChecks(t, pool, d.ID, "TRD123")
	terms := d.Terms
	terms.TradeIns = []workshop.TradeIn{tradeIn(1_500_000, 0)}
	_, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, d.Version, terms)
	if err == nil {
		t.Errorf("the terms were revised under a trade-in already credited at another figure; held %d", heldForSale(t, pool, d.ID))
	}
}
