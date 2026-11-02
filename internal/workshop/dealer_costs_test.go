package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Costs per stock unit (#164).

func cost(kind string, net int64) workshop.NewStockCost {
	return workshop.NewStockCost{Kind: kind, NetMinor: net, VATRateBasis: 2500, IncurredOn: time.Now()}
}

// Costs are the car's and never its tax purchase price: the margin
// scheme's VAT on a sale is what it was before any cost was entered. A
// supplier's credit note is a cost below nothing, a mistake is reversed,
// and a cost after the sale changes the margin and not the VAT.
func TestCostsAreKeptApartFromThePurchasePrice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	vmb := func() int64 {
		got, err := workshop.PurchaseOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
		if err != nil {
			t.Fatal(err)
		}
		return money.MarginSchemeVAT(12_500_000, got.TaxPurchasePriceMinor, 2500)
	}
	before := vmb()

	transport, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, cost(workshop.CostTransport, 500_000))
	if err != nil {
		t.Fatal(err)
	}
	if transport.InputVATMinor != 125_000 || transport.Source != workshop.CostFromManual || transport.PostSale {
		t.Errorf("a transport cost: %+v", transport)
	}
	recon := cost(workshop.CostReconditioning, 800_000)
	recon.FromInvoice, recon.ExternalRef = true, "F-1001"
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, recon); err != nil {
		t.Fatal(err)
	}
	credit := cost(workshop.CostReconditioning, -100_000)
	credit.FromInvoice, credit.ExternalRef = true, "K-17"
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, credit); err != nil {
		t.Fatalf("a supplier's credit note: %v", err)
	}
	if _, err := workshop.ReverseStockCost(ctx, pool, buyerScope(), transport.ID, ""); !errors.Is(err, workshop.ErrCostNoReason) {
		t.Errorf("reversed with no reason: %v", err)
	}
	back, err := workshop.ReverseStockCost(ctx, pool, buyerScope(), transport.ID, "Fel bil")
	if err != nil || back.NetMinor != -500_000 || back.ReversesID != transport.ID {
		t.Fatalf("the reversal: %+v, %v", back, err)
	}
	if _, err := workshop.ReverseStockCost(ctx, pool, buyerScope(), transport.ID, "Igen"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("reversed twice: %v", err)
	}
	// In euros, at the rate of the day.
	euros := workshop.NewStockCost{Kind: workshop.CostTransport, VATRateBasis: 0, IncurredOn: time.Now(),
		Currency: "EUR", OriginalMinor: 30_000, RateMicro: 11_500_000, RateSource: "Riksbanken"}
	e, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, euros)
	if err != nil || e.NetMinor != 345_000 || e.Currency != "EUR" || e.InputVATMinor != 0 {
		t.Errorf("a cost in euros: %+v, %v", e, err)
	}
	costs, err := workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
	if err != nil || len(costs.Entries) != 5 || costs.BeforeSale != 800_000-100_000+345_000 || costs.AfterSale != 0 {
		t.Errorf("the unit's costs: %+v, %v", costs, err)
	}
	if after := vmb(); after != before {
		t.Errorf("the margin scheme's VAT moved from %d to %d with the costs", before, after)
	}

	// Sold, the next cost is after the sale.
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale, workshop.StockReserved, workshop.StockSold)
	late, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, cost(workshop.CostWorkshop, 200_000))
	if err != nil || !late.PostSale {
		t.Errorf("a cost after the sale: %+v, %v", late, err)
	}
	if costs, _ := workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID); costs.AfterSale != 200_000 || costs.TotalMinor() != 1_245_000 {
		t.Errorf("after the sale: %+v", costs)
	}
	if after := vmb(); after != before {
		t.Errorf("the margin scheme's VAT moved with a cost after the sale: %d, %d", before, after)
	}
}

// A workshop job on a stock car posts what it cost the business, once:
// parts at what they cost off the shelf, hours at the internal rate. A job
// on a car that is not in stock posts nothing.
func TestAJobOnAStockCarPostsItsCostOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	customerJob := newJob(t, pool)
	if _, err := workshop.PostJobCost(ctx, pool, buyerScope(), customerJob); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a customer's job posted: %v", err)
	}
	move(t, pool, customerJob, workshop.StateCancelled)

	p := buy(t, pool, workshop.StockFromPurchase)
	if err := workshop.SetInternalLabourRate(ctx, pool, buyerScope(), 40_000); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer set the internal rate: %v", err)
	}
	if err := workshop.SetInternalLabourRate(ctx, pool, everything(owner()), 40_000); err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{Kind: "labour", Description: "Byt kamrem",
		QuantityMilli: 2500, UnitPriceMinor: 95_000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH part AS (INSERT INTO parts (shop_id, number, name, cost_minor, price_minor) VALUES ($1, 'KR-1', 'Kamremssats', 150000, 250000) RETURNING id)
		INSERT INTO stock_movements (shop_id, part_id, kind, quantity, unit_cost_minor, work_order_id)
		SELECT $1, part.id, 'consumed', -2, 150000, $2 FROM part`, shopID, job); err != nil {
		t.Fatal(err)
	}
	// Open, the job may yet take more parts and hours: its cost is not
	// posted until it is done.
	if _, err := workshop.PostJobCost(ctx, pool, buyerScope(), job); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an open job's cost was posted: %v", err)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved, workshop.StateInProgress)
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 1, true); err != nil {
		t.Fatal(err)
	}
	// Ready is not done: a ready job may go back on the lift for more.
	move(t, pool, job, workshop.StateReady)
	if _, err := workshop.PostJobCost(ctx, pool, buyerScope(), job); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a ready job's cost was posted: %v", err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	posted, err := workshop.PostJobCost(ctx, pool, buyerScope(), job)
	// 2,5 h at 400 kr and two kits at 1 500 kr: 4 000 kr.
	if err != nil || posted.NetMinor != 400_000 || posted.Source != workshop.CostFromWorkOrder || posted.Kind != workshop.CostWorkshop ||
		posted.StockUnitID != p.StockUnitID || posted.WorkOrderID != job {
		t.Fatalf("the job's posting: %+v, %v", posted, err)
	}
	again, err := workshop.PostJobCost(ctx, pool, buyerScope(), job)
	if err != nil || again.ID != posted.ID {
		t.Errorf("posted twice: %+v, %v", again, err)
	}
	if _, err := workshop.ReverseStockCost(ctx, pool, buyerScope(), posted.ID, "Fel timmar"); err != nil {
		t.Fatal(err)
	}
	if fresh, err := workshop.PostJobCost(ctx, pool, buyerScope(), job); err != nil || fresh.ID == posted.ID {
		t.Errorf("posted again after the reversal: %+v, %v", fresh, err)
	}
	if _, err := workshop.PostJobCost(ctx, pool, sales(), job); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson posted a job's cost: %v", err)
	}
}

// A supplier's invoice for work sent out from a job on a stock car is the
// car's cost, as it is noted; on a customer's car it is nothing of the
// dealer's.
func TestOutsourcedWorkOnAStockCarPostsTheSuppliersCost(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	customer, customerLine := turboJob(t, pool)
	out := sendOut(t, pool, customer, customerLine, time.Now().AddDate(0, 0, 3))
	record(t, pool, out, workshop.OutsourcedEventInput{Kind: "invoice", AmountMinor: pointerTo(int64(420_000)), InvoiceRef: "TV-1"})
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM stock_cost_entries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a customer's job posted %d costs (%v)", n, err)
	}
	move(t, pool, customer, workshop.StateDeclined)

	p := buy(t, pool, workshop.StockFromPurchase)
	job, line := turboJob(t, pool)
	stock := sendOut(t, pool, job, line, time.Now().AddDate(0, 0, 3))
	record(t, pool, stock, workshop.OutsourcedEventInput{Kind: "invoice", AmountMinor: pointerTo(int64(450_000)), InvoiceRef: "TV-2291"})
	costs, err := workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
	if err != nil || len(costs.Entries) != 1 {
		t.Fatalf("the stock car's costs: %+v, %v", costs, err)
	}
	if c := costs.Entries[0]; c.NetMinor != 450_000 || c.InputVATMinor != 112_500 || c.Source != workshop.CostFromOutsourced ||
		c.ExternalRef != "TV-2291" || c.WorkOrderID != job {
		t.Errorf("the supplier's cost: %+v", c)
	}

	// The supplier's invoice noted again, corrected: it replaces the first,
	// which is reversed, not counted twice.
	record(t, pool, stock, workshop.OutsourcedEventInput{Kind: "invoice", AmountMinor: pointerTo(int64(420_000)), InvoiceRef: "TV-2291b"})
	costs, _ = workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
	if len(costs.Entries) != 3 || costs.BeforeSale != 420_000 || costs.InputVATMinor != 105_000 {
		t.Errorf("after the corrected invoice: %d entries, %d net, %d VAT; want the first reversed and the second standing",
			len(costs.Entries), costs.BeforeSale, costs.InputVATMinor)
	}

	// Re-issued again with no amount yet: the latest stands, so the cost
	// before it is reversed now, not once the amount is known.
	record(t, pool, stock, workshop.OutsourcedEventInput{Kind: "invoice", InvoiceRef: "TV-2291c"})
	costs, _ = workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
	if len(costs.Entries) != 4 || costs.TotalMinor() != 0 || costs.InputVATMinor != 0 {
		t.Errorf("after an invoice with no amount: %d entries, %d net, %d VAT; want the standing cost reversed",
			len(costs.Entries), costs.TotalMinor(), costs.InputVATMinor)
	}
}

// Costs are for whoever may see what a car cost: a salesperson asking
// directly is refused and handed nothing. With car sales off they stay
// readable and none is added. Nothing changes or deletes an entry.
func TestCostsByRoleAndModule(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, cost(workshop.CostTransport, 1000)); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]access.Scope{"a salesperson": sales(), "the front desk": everything(advisor()), "a technician": everything(technician())} {
		got, err := workshop.CostsOfUnit(ctx, pool, s, p.StockUnitID)
		if !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the costs: %v", name, err)
		}
		if handed := testsupport.Handed(got, "Entries", "BeforeSale", "AfterSale"); len(handed) != 0 {
			t.Errorf("%s was handed %v", name, handed)
		}
		if _, err := workshop.RecordStockCost(ctx, pool, s, p.StockUnitID, cost(workshop.CostTransport, 1)); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s recorded a cost: %v", name, err)
		}
		if _, err := workshop.InternalLabourRate(ctx, pool, s); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the internal rate: %v", name, err)
		}
	}
	off := buyerScope()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.RecordStockCost(ctx, pool, off, p.StockUnitID, cost(workshop.CostTransport, 1)); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("recorded with car sales off: %v", err)
	}
	if costs, err := workshop.CostsOfUnit(ctx, pool, off, p.StockUnitID); err != nil || len(costs.Entries) != 1 {
		t.Errorf("read with car sales off: %+v, %v", costs, err)
	}
	bad := cost(workshop.CostTransport, 1000)
	bad.InputVATMinor = refTo(int64(2000))
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("more VAT than the cost: %v", err)
	}
	for _, stmt := range []string{`UPDATE stock_cost_entries SET net_minor = 1`, `DELETE FROM stock_cost_entries`,
		`DELETE FROM stock_cost_settings`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}

func refTo(n int64) *int64 { return &n }

// A supplier's cost reversed by hand while its corrected invoice is being
// noted is reversed once: the hand waits for the invoice, finds the cost
// already taken back, and is told so rather than failing.
func TestACostReversedByHandWhileItsInvoiceIsCorrectedIsReversedOnce(t *testing.T) {
	// The entry's id as the page sent it, and in capitals: both are the
	// same cost, and so the same lock.
	for name, spell := range map[string]func(string) string{"as stored": func(s string) string { return s },
		"in capitals": strings.ToUpper} {
		t.Run(name, func(t *testing.T) { reversedOnceWhileCorrected(t, spell) })
	}
}

func reversedOnceWhileCorrected(t *testing.T, spell func(string) string) {
	pool := setup(t)
	ctx := context.Background()
	buy(t, pool, workshop.StockFromPurchase)
	job, line := turboJob(t, pool)
	out := sendOut(t, pool, job, line, time.Now().AddDate(0, 0, 3))
	record(t, pool, out, workshop.OutsourcedEventInput{Kind: "invoice", AmountMinor: pointerTo(int64(450_000)), InvoiceRef: "TV-1"})
	var entry string
	if err := pool.QueryRow(ctx, `SELECT id FROM stock_cost_entries WHERE source = 'outsourced'`).Scan(&entry); err != nil {
		t.Fatal(err)
	}
	// The corrected invoice noted, its transaction still open: the trigger
	// has reversed the cost and not yet committed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO outsourced_events (shop_id, outsourced_id, kind, amount_minor, invoice_ref, by_user)
		VALUES ($1, $2, 'invoice', 420000, 'TV-1b', $3)`, shopID, out, userID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := workshop.ReverseStockCost(ctx, pool, buyerScope(), spell(entry), "Fel bil")
		done <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reversal by hand never waited for the invoice")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the reversal by hand: %v; want it told the cost is already reversed", err)
	}
	var reversals int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM stock_cost_entries WHERE reverses_id = $1`, entry).Scan(&reversals); err != nil || reversals != 1 {
		t.Errorf("reversed %d times (%v)", reversals, err)
	}
}

// After the sale is one rule for every cost, and it is about what the cost
// is for, not when it was posted: work on a job opened before the sale, or
// a cost dated before the day of the sale, is before it however late its
// invoice comes; a corrected invoice stays on its side; and a sale that
// falls through was never a sale.
func TestAfterTheSaleIsWhatTheCostIsFor(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	before, beforeLine := turboJob(t, pool)
	early := sendOut(t, pool, before, beforeLine, time.Now().AddDate(0, 0, 3))
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale, workshop.StockReserved, workshop.StockSold)
	after, afterLine := turboJob(t, pool)
	late := sendOut(t, pool, after, afterLine, time.Now().AddDate(0, 0, 3))

	record(t, pool, early, workshop.OutsourcedEventInput{Kind: "invoice", AmountMinor: pointerTo(int64(100_000)), InvoiceRef: "S-1"})
	record(t, pool, late, workshop.OutsourcedEventInput{Kind: "invoice", AmountMinor: pointerTo(int64(30_000)), InvoiceRef: "S-2"})
	earlier := cost(workshop.CostReconditioning, 70_000)
	earlier.IncurredOn = time.Now().AddDate(0, 0, -2)
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, earlier); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, cost(workshop.CostTransport, 5_000)); err != nil {
		t.Fatal(err)
	}
	costs, _ := workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
	if costs.BeforeSale != 170_000 || costs.AfterSale != 35_000 {
		t.Errorf("before the sale %d, after it %d; want the early job and the earlier cost before, the late job and today's after",
			costs.BeforeSale, costs.AfterSale)
	}

	// The early job's invoice corrected after the sale: reversed and
	// replaced on the side the work was done.
	record(t, pool, early, workshop.OutsourcedEventInput{Kind: "invoice", AmountMinor: pointerTo(int64(120_000)), InvoiceRef: "S-1b"})
	costs, _ = workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
	if costs.BeforeSale != 190_000 || costs.AfterSale != 35_000 {
		t.Errorf("after the corrected invoice: before the sale %d, after it %d; want 190 000 and 35 000", costs.BeforeSale, costs.AfterSale)
	}

	// The buyer backs out: there was no sale, and nothing is after it.
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	costs, _ = workshop.CostsOfUnit(ctx, pool, buyerScope(), p.StockUnitID)
	if costs.BeforeSale != 225_000 || costs.AfterSale != 0 {
		t.Errorf("after the sale fell through: before %d, after %d; want everything before", costs.BeforeSale, costs.AfterSale)
	}
	for _, c := range costs.Entries {
		if c.PostSale {
			t.Errorf("a %s cost is after a sale that fell through: %+v", c.Source, c)
		}
	}
}
