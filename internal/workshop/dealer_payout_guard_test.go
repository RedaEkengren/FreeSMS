package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/ops"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Money paid out for a purchase is held against what the purchase says is
// owed, all payouts together (#248): paying it in full and then again asks;
// a split between the lender and the seller up to the price does not; a
// payout reversed frees its amount; an owner who says why may pay more, and
// that is on the trail; the same request again finds its payment.
func TestMoneyPaidOutForAPurchaseIsHeldAgainstItsPrice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := boughtCarFrom(t, pool, theSeller(t, pool))
	passEveryCheck(t, pool, p.StockUnitID)
	price := p.TaxPurchasePriceMinor
	open := func(party string) string {
		t.Helper()
		id, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: party, Purpose: "purchase", SubjectKind: "purchase", SubjectID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pay := func(payout string, minor int64, n workshop.NewPayment) (workshop.Payment, error) {
		n.AmountMinor, n.Method, n.PaidOn = &minor, "bank", time.Now()
		return workshop.RecordPayout(ctx, pool, buyerScope(), payout, n)
	}
	seller := open(p.SellerPartyID)

	// A split up to the price passes without a question: the lender what the
	// loan was settled at, the seller the rest.
	lender, err := workshop.CreateParty(ctx, pool, buyerScope(), workshop.NewParty{Kind: "company", Name: "Bilfinans AB", OrgNumber: "556677-8899", City: "Solna", Role: workshop.PartySupplier})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordPurchaseLoan(ctx, pool, buyerScope(), p.ID, workshop.PurchaseLoan{LenderPartyID: lender,
		AmountMinor: 5_000_000, SettlementReference: "Lösen 4711"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pay(open(lender), 5_000_000, workshop.NewPayment{}); err != nil {
		t.Fatalf("the lender's part: %v", err)
	}
	first, err := pay(seller, price-5_000_000, workshop.NewPayment{Key: "payout-seller-0001"})
	if err != nil {
		t.Fatalf("the seller's part: %v", err)
	}
	// The same request again finds its payment, whatever the balance says now.
	again, err := pay(seller, price-5_000_000, workshop.NewPayment{Key: "payout-seller-0001"})
	if err != nil || again.ID != first.ID {
		t.Errorf("the same request again: %+v, %v; want payment %s", again, err, first.ID)
	}

	// Paid in full; paying again is asked about, not silently accepted.
	if _, err := pay(seller, price, workshop.NewPayment{}); !errors.Is(err, workshop.ErrPayoutOverPrice) {
		t.Fatalf("paying a purchase twice: %v", err)
	}
	// Confirmed without a reason is still refused; with one it is recorded
	// and is on the trail.
	if _, err := pay(seller, 500_000, workshop.NewPayment{ConfirmedOverpayment: true}); !errors.Is(err, workshop.ErrOverpaymentNoReason) {
		t.Errorf("more than is owed, confirmed with no reason: %v", err)
	}
	extra, err := pay(seller, 500_000, workshop.NewPayment{ConfirmedOverpayment: true, OverpaymentReason: "Säljaren fick betalt för vinterhjulen också"})
	if err != nil {
		t.Fatalf("more than is owed, with a reason: %v", err)
	}
	trail, err := workshop.AuditTrail(ctx, pool, everything(owner()), "payment", extra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(trail) == 0 || trail[len(trail)-1].Event != "payment.overpayment_confirmed" || trail[len(trail)-1].Reason == "" {
		t.Errorf("the overpayment on the trail: %+v", trail)
	}
	// -verify reports it: a decision with a reason is worth a look, and one
	// nobody confirmed fails the check.
	verdict := func() ops.Check {
		t.Helper()
		rep, err := ops.Verify(ctx, pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range rep.Shops {
			for _, c := range s.Checks {
				if c.Name == "purchase payouts" {
					return c
				}
			}
		}
		t.Fatalf("no check for purchase payouts in %s", rep.Text())
		return ops.Check{}
	}
	if c := verdict(); c.Level != ops.Warn {
		t.Errorf("a confirmed overpayment: %+v", c)
	}
	// An overpayment already made is shown: the balance is below nothing.
	// The next payout is refused rather than hidden by it.
	if _, err := pay(seller, 100_000, workshop.NewPayment{}); !errors.Is(err, workshop.ErrPayoutOverPrice) {
		t.Errorf("paying on after an overpayment: %v", err)
	}
	// A payment that nobody confirmed (a restore that lost the decision) fails.
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_events WHERE event = 'payment.overpayment_confirmed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if c := verdict(); c.Level != ops.Fail {
		t.Errorf("an overpayment nobody confirmed: %+v", c)
	}
	// A payout reversed (a bank return) frees its amount again.
	if err := workshop.ReversePayment(ctx, pool, buyerScope(), extra.ID, "Banken returnerade betalningen"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReversePayment(ctx, pool, buyerScope(), first.ID, "Banken returnerade betalningen"); err != nil {
		t.Fatal(err)
	}
	if _, err := pay(seller, price-5_000_000, workshop.NewPayment{}); err != nil {
		t.Errorf("paying again once the return freed it: %v", err)
	}
}

// Payouts for one purchase are read one after the other (#248): while
// another transaction holds the purchase's payout lock a payout waits, and
// once it is let go the payout reads what was paid and goes through. Two at
// once for the whole price leave one standing.
func TestPayoutsForOnePurchaseWaitForEachOther(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := boughtCarFrom(t, pool, theSeller(t, pool))
	passEveryCheck(t, pool, p.StockUnitID)
	var payouts [2]string
	for i := range payouts {
		id, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: p.SellerPartyID, Purpose: "purchase", SubjectKind: "purchase", SubjectID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		payouts[i] = id
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('purchase_payouts:' || $1::text, 0))`, p.ID); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, id := range payouts {
		go func() {
			minor := p.TaxPurchasePriceMinor
			_, err := workshop.RecordPayout(ctx, pool, buyerScope(), id, workshop.NewPayment{AmountMinor: &minor, Method: "bank", PaidOn: time.Now()})
			errs <- err
		}()
	}
	select {
	case err := <-errs:
		t.Fatalf("a payout went through while the purchase's payouts were locked: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	paid, refused := 0, 0
	for range payouts {
		switch err := <-errs; {
		case err == nil:
			paid++
		case errors.Is(err, workshop.ErrPayoutOverPrice):
			refused++
		default:
			t.Errorf("a payout: %v", err)
		}
	}
	if paid != 1 || refused != 1 {
		t.Errorf("%d paid, %d refused; want one of each", paid, refused)
	}
}
