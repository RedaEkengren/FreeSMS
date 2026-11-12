package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A check keeps the document it rests on (#162): the evidence is carried
// onto the row, a document that does not exist is refused and leaves no
// check, and a check made again is a row beside the first, the first
// untouched.
func TestACheckKeepsItsEvidenceAndARecheckIsANewRow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	doc := contractFor(t, pool, p.ID)

	before, err := workshop.StockChecks(ctx, pool, buyerScope(), p.StockUnitID)
	if err != nil {
		t.Fatal(err)
	}
	missing := "00000000-0000-4000-8000-000000000001"
	err = workshop.RecordStockCheck(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCheck{
		Kind: workshop.CheckCreditPurchase, Result: workshop.CheckPass, EvidenceID: missing})
	if err == nil {
		t.Error("a check resting on a document that does not exist was recorded")
	}
	if after, _ := workshop.StockChecks(ctx, pool, buyerScope(), p.StockUnitID); len(after) != len(before) {
		t.Errorf("a refused check left %d rows, had %d", len(after), len(before))
	}

	if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCheck{
		Kind: workshop.CheckCreditPurchase, Result: workshop.CheckFail, Note: "Kreditköp hos banken"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCheck{
		Kind: workshop.CheckCreditPurchase, Result: workshop.CheckPass, EvidenceID: doc, Note: "Lånet är löst"}); err != nil {
		t.Fatal(err)
	}
	// StockChecks shows the standing check of each kind; the table holds both.
	var fails, passes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE result = 'fail' AND evidence_document_id IS NULL),
			count(*) FILTER (WHERE result = 'pass' AND evidence_document_id = $2 AND source = 'manual')
		FROM stock_checks WHERE stock_unit_id = $1 AND kind = $3`, p.StockUnitID, doc, workshop.CheckCreditPurchase).Scan(&fails, &passes); err != nil {
		t.Fatal(err)
	}
	if fails != 1 || passes != 1 {
		t.Errorf("after a re-check: %d failed rows and %d passed rows with the evidence; want one of each", fails, passes)
	}
	standing, err := workshop.StockChecks(ctx, pool, buyerScope(), p.StockUnitID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range standing {
		if c.Kind == workshop.CheckCreditPurchase && (c.Result != workshop.CheckPass || c.EvidenceID != doc) {
			t.Errorf("the standing check is %+v; want the re-check", c)
		}
	}
}

// A trade-in is a purchase and goes through the same checks (#162): its loan
// is not paid off to the lender while a required check is outstanding, and
// the owner's override frees it the way it frees any purchase.
func TestATradeInsPayoutWaitsForTheSameChecks(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := contractWith(t, pool, eligibleCar(t, pool), driver(t, pool), tradeIn(4_000_000, 1_000_000))
	if _, err := workshop.ReceiveTradeIn(ctx, pool, buyerScope(), d.ID, "TRD123", time.Now()); err != nil {
		t.Fatal(err)
	}
	var purchase, seller, unit string
	if err := pool.QueryRow(ctx, `SELECT p.id, p.seller_party_id, p.stock_unit_id FROM vehicle_sale_trade_in_receipts r
		JOIN vehicle_purchases p ON p.id = r.purchase_id WHERE r.sale_id = $1`, d.ID).Scan(&purchase, &seller, &unit); err != nil {
		t.Fatal(err)
	}
	payout, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: seller, Purpose: "purchase", SubjectKind: "purchase", SubjectID: purchase})
	if err != nil {
		t.Fatal(err)
	}
	pay := func() error {
		minor := int64(1_000_000)
		_, err := workshop.RecordPayout(ctx, pool, buyerScope(), payout, workshop.NewPayment{AmountMinor: &minor, Method: "bank", PaidOn: time.Now()})
		return err
	}
	if err := pay(); !errors.Is(err, workshop.ErrChecksOutstanding) {
		t.Fatalf("a trade-in's loan paid with its checks outstanding: %v", err)
	}
	if err := workshop.OverrideStockChecks(ctx, pool, everything(owner()), unit, "Kontrollerat per telefon"); err != nil {
		t.Fatal(err)
	}
	if err := pay(); err != nil {
		t.Errorf("paid after the owner let the checks through: %v", err)
	}
}
