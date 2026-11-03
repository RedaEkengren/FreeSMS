package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Trade-ins (#165).

func tradeIn(credit, loan int64) workshop.TradeIn {
	km := int64(210_000)
	return workshop.TradeIn{Registration: "TRD 123", Make: "Saab", Model: "9-5", OdometerKm: &km, CreditMinor: credit, LoanMinor: loan,
		Lender: "Banken AB", SellerType: workshop.SellerPrivate}
}

// contractWith contracts a margin-scheme deal on unit for buyer with
// trade-ins; consumerTerms come to 135 000 kr in all.
func contractWith(t *testing.T, pool *pgxpool.Pool, unit, buyer string, ts ...workshop.TradeIn) workshop.Deal {
	t.Helper()
	ctx := context.Background()
	terms := consumerTerms(buyer)
	terms.TaxTreatment = workshop.TaxMarginScheme
	terms.TradeIns = ts
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	done, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

// clearChecks has the owner let a received trade-in's checks through, and
// credits it: what passing them would do.
func clearChecks(t *testing.T, pool *pgxpool.Pool, saleID, key string) {
	t.Helper()
	ctx := context.Background()
	var unit string
	if err := pool.QueryRow(ctx, `SELECT p.stock_unit_id FROM vehicle_sale_trade_in_receipts r JOIN vehicle_purchases p ON p.id = r.purchase_id
		WHERE r.sale_id = $1`, saleID).Scan(&unit); err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideStockChecks(ctx, pool, everything(owner()), unit, "Kontrollerat per telefon"); err != nil {
		t.Fatal(err)
	}
	if credited, err := workshop.CreditTradeIn(ctx, pool, buyerScope(), saleID, key); err != nil || !credited {
		t.Fatalf("credited once its checks were let through: %v, %v", credited, err)
	}
}

func outstanding(t *testing.T, pool *pgxpool.Pool, invoice string) int64 {
	t.Helper()
	var owed int64
	if err := pool.QueryRow(context.Background(), `
		SELECT i.gross_minor - coalesce((SELECT sum(p.amount_minor + p.rounding_minor) FROM invoice_payments p WHERE p.invoice_id = i.id), 0)
		FROM invoices i WHERE i.id = $1`, invoice).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	return owed
}

// A trade-in settles what is owed and never reduces the price: the car
// sold is taxed on its full price, the contract says what is to pay and
// why, and the invoice is issued at the full price with the trade-in's
// equity -- its credit less the loan settled with the lender -- paid on it
// once the trade-in is received. Received again, nothing more happens.
func TestATradeInSettlesWhatIsOwedAndNeverTheSalePrice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)
	plain := contractWith(t, pool, eligibleCar(t, pool), kim)
	without, err := workshop.SaleTaxOfDeal(ctx, pool, buyerScope(), plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), plain.ID, "Ny affär med inbyte"); err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, plain.StockUnitID, workshop.StockForSale)

	d := contractWith(t, pool, plain.StockUnitID, kim, tradeIn(4_000_000, 1_000_000))
	with, err := workshop.SaleTaxOfDeal(ctx, pool, buyerScope(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if with.MarginMinor != without.MarginMinor || with.VATMinor != without.VATMinor {
		t.Errorf("the trade-in changed the car's tax: %+v, without it %+v", with, without)
	}
	doc, err := workshop.DocumentRecordByID(ctx, pool, sales(), d.ContractID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Body, "TRD 123") || !strings.Contains(doc.Body, "Banken AB") {
		t.Errorf("the contract does not state the trade-in:\n%s", doc.Body)
	}

	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inv.GrossMinor != 13_500_000+48_000 && inv.GrossMinor < 13_500_000 {
		t.Errorf("the invoice was reduced by the trade-in: %d", inv.GrossMinor)
	}
	before := outstanding(t, pool, inv.ID)
	if before != inv.GrossMinor {
		t.Fatalf("owed before the trade-in came: %d of %d", before, inv.GrossMinor)
	}
	credited, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now())
	if err != nil || credited || outstanding(t, pool, inv.ID) != before {
		t.Fatalf("received with its checks not done: credited %v, %v; owed %d", credited, err, outstanding(t, pool, inv.ID))
	}
	clearChecks(t, pool, d.ID, "TRD123")
	if after := outstanding(t, pool, inv.ID); after != before-3_000_000 {
		t.Errorf("owed after the trade-in: %d, want %d less its equity of 30 000 kr", after, before)
	}
	again, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD 123", time.Now())
	if err != nil || !again || outstanding(t, pool, inv.ID) != before-3_000_000 {
		t.Errorf("received again: %v, %v, owed %d", again, err, outstanding(t, pool, inv.ID))
	}
	after, err := workshop.DealByID(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	ti := after.Terms.TradeIns[0]
	p, err := workshop.PurchaseOfUnit(ctx, pool, buyerScope(), func() string {
		var unit string
		if err := pool.QueryRow(ctx, `SELECT stock_unit_id FROM vehicle_purchases WHERE id = $1`, ti.PurchaseID).Scan(&unit); err != nil {
			t.Fatal(err)
		}
		return unit
	}())
	if err != nil || p.TaxPurchasePriceMinor != 4_000_000 || p.SellerPartyID != kim || !ti.Credited {
		t.Errorf("the trade-in's purchase: %+v, %v, credited %v", p, err, ti.Credited)
	}
}

// A trade-in received before the invoice is held for the buyer and paid on
// the invoice as it is issued; a loan larger than the credit is refused
// until the accountant has answered how it is charged.
func TestATradeInReceivedFirstIsPaidOnTheInvoiceAsItIsIssued(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)
	unit := eligibleCar(t, pool)
	terms := consumerTerms(kim)
	terms.TradeIns = []workshop.TradeIn{tradeIn(1_000_000, 2_000_000)}
	if _, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: terms}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("negative equity accepted: %v", err)
	}
	d := contractWith(t, pool, unit, kim, tradeIn(2_500_000, 0))
	if _, err := workshop.ReceiveTradeIn(ctx, pool, sales(), d.ID, "TRD123", time.Now()); err == nil {
		t.Error("a salesperson received a trade-in, which is the buyer's to appraise")
	}
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now()); err != nil {
		t.Fatalf("received: %v", err)
	}
	clearChecks(t, pool, d.ID, "TRD123")
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if owed := outstanding(t, pool, inv.ID); owed != inv.GrossMinor-2_500_000 {
		t.Errorf("owed on the invoice: %d, want %d less the trade-in", owed, inv.GrossMinor)
	}
}
