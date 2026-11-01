package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The payment journal (#152). The shop runs every module by its scope.

func minor(n int64) *int64 { return &n }

// A rental deposit received by Swish, part of it applied to the damage
// invoiced after, and part given back by bank: the deposit's balance and
// the invoice's agree with what moved, and every figure is a row.
func TestAPartlyRefundedDepositReconcilesWithBothBalances(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := everything(advisor())
	party := partyOf(t, pool, fixtureCustomer)
	dep, err := workshop.CreateDeposit(ctx, pool, desk, workshop.NewHolding{PartyID: party, Purpose: "rental"})
	if err != nil {
		t.Fatal(err)
	}
	in, err := workshop.RecordDepositPayment(ctx, pool, desk, dep, workshop.NewPayment{
		AmountMinor: minor(500000), Method: "swish", PaidOn: time.Now(), Provider: "swish", ExternalID: "SW-1"})
	if err != nil {
		t.Fatal(err)
	}
	// The same Swish payment recorded again is one payment.
	if _, err := workshop.RecordDepositPayment(ctx, pool, desk, dep, workshop.NewPayment{
		AmountMinor: minor(500000), Method: "swish", PaidOn: time.Now(), Provider: "swish", ExternalID: "SW-1"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same Swish payment recorded twice: %v", err)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ApplyDeposit(ctx, pool, desk, dep, inv.ID, inv.GrossMinor+1, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("more applied than is owed: %v", err)
	}
	if err := workshop.ApplyDeposit(ctx, pool, desk, dep, inv.ID, inv.GrossMinor, "apply-1"); err != nil {
		t.Fatal(err)
	}
	// Repeated, the same request applies nothing more.
	if err := workshop.ApplyDeposit(ctx, pool, desk, dep, inv.ID, inv.GrossMinor, "apply-1"); err != nil {
		t.Fatal(err)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, desk, inv.ID); !b.Settled() {
		t.Errorf("the invoice after the deposit was applied: %d outstanding", b.OutstandingMinor())
	}
	held := 500000 - inv.GrossMinor
	if _, err := workshop.RefundPayment(ctx, pool, desk, in.ID, workshop.NewPayment{
		AmountMinor: minor(held + 1), Method: "bank", PaidOn: time.Now()}, "Hyran slut"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("more given back than is held: %v", err)
	}
	if _, err := workshop.RefundPayment(ctx, pool, desk, in.ID, workshop.NewPayment{
		AmountMinor: minor(held - 10000), Method: "bank", PaidOn: time.Now()}, ""); !errors.Is(err, workshop.ErrRefundNoReason) {
		t.Errorf("a refund with no reason: %v", err)
	}
	back, err := workshop.RefundPayment(ctx, pool, desk, in.ID, workshop.NewPayment{
		AmountMinor: minor(held - 10000), Method: "bank", PaidOn: time.Now(), Key: "refund-1"}, "Hyran slut, inga fler skador")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.RefundPayment(ctx, pool, desk, in.ID, workshop.NewPayment{
		AmountMinor: minor(held - 10000), Method: "bank", PaidOn: time.Now(), Key: "refund-1"}, "Hyran slut, inga fler skador"); err != nil || again.ID != back.ID {
		t.Errorf("the refund repeated: %s, %v; want %s", again.ID, err, back.ID)
	}
	d, err := workshop.DepositBalanceFor(ctx, pool, desk, dep)
	if err != nil {
		t.Fatal(err)
	}
	if d.Received != 500000 || d.Applied != inv.GrossMinor || d.Refunded != held-10000 || d.Held() != 10000 {
		t.Errorf("deposit: received %d, applied %d, refunded %d, held %d", d.Received, d.Applied, d.Refunded, d.Held())
	}
	// The database holds the refunds within the payment too.
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoice_payments (shop_id, deposit_id, series, number, amount_minor, method, paid_on, recorded_by, refunds_id)
		VALUES ($1, $2, 'B', 999, -500000, 'bank', now(), $3, $4)`, shopID, dep, userID, in.ID); err == nil {
		t.Error("a refund of more than was paid was written")
	}
	// Every movement is in the deposit's trail, the refund with its reason.
	events, err := workshop.AuditTrail(ctx, pool, desk, "deposit", dep)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.Event]++
		if e.Event == "payment.refunded" && e.Reason == "" {
			t.Error("the refund's reason is not in the trail")
		}
	}
	if kinds["payment.recorded"] != 1 || kinds["payment.applied"] != 1 || kinds["payment.refunded"] != 1 {
		t.Errorf("the deposit's trail: %v", kinds)
	}
}

// What a payment is against is exactly one thing, and its direction is the
// sign rules as they always were: money in positive, a refund or a
// reversal negative.
func TestAPaymentIsAgainstOneThingAndSaysWhichWay(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	p, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReversePayment(ctx, pool, advisor(), p.ID, ""); !errors.Is(err, workshop.ErrReversalNoReason) {
		t.Errorf("a reversal with no reason: %v", err)
	}
	if err := workshop.ReversePayment(ctx, pool, advisor(), p.ID, "Fel faktura"); err != nil {
		t.Fatal(err)
	}
	directions := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT CASE WHEN reverses_id IS NULL THEN 'payment' ELSE 'reversal' END, direction FROM invoice_payments`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, d string
		if err := rows.Scan(&k, &d); err != nil {
			t.Fatal(err)
		}
		directions[k] = d
	}
	rows.Close()
	if directions["payment"] != "in" || directions["reversal"] != "out" {
		t.Errorf("directions: %v", directions)
	}
	dep, err := workshop.CreateDeposit(ctx, pool, everything(advisor()), workshop.NewHolding{PartyID: partyOf(t, pool, fixtureCustomer), Purpose: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoice_payments (shop_id, invoice_id, deposit_id, series, number, amount_minor, method, paid_on, recorded_by)
		VALUES ($1, $2, $3, 'B', 998, 100, 'bank', now(), $4)`, shopID, inv.ID, dep, userID); err == nil {
		t.Error("a payment against an invoice and a deposit at once was written")
	}
}

// A seller is paid out by the buyer, who agreed the price; the salesperson
// may not, and is not shown it. A payout is money out.
func TestAPayoutIsTheBuyersAndMoneyOut(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seller := privateSeller(t, pool)
	if _, err := workshop.CreatePayout(ctx, pool, otherRoles(access.Of(access.RoleSales)), workshop.NewHolding{PartyID: seller, Purpose: "purchase"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson opened a payout: %v", err)
	}
	payout, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: seller, Purpose: "purchase"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := workshop.RecordPayout(ctx, pool, buyerScope(), payout, workshop.NewPayment{
		AmountMinor: minor(8500000), Method: "bank", PaidOn: time.Now(), Reference: "Köp ABC 12D"})
	if err != nil {
		t.Fatal(err)
	}
	if out.AmountMinor != -8500000 {
		t.Errorf("the payout is recorded as %d", out.AmountMinor)
	}
	if _, err := workshop.AuditTrail(ctx, pool, otherRoles(access.Of(access.RoleSales)), "payout", payout); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson read a payout's trail: %v", err)
	}
	if events, err := workshop.AuditTrail(ctx, pool, buyerScope(), "payout", payout); err != nil || len(events) != 1 || events[0].Event != "payment.paid_out" {
		t.Errorf("the payout's trail: %+v, %v", events, err)
	}
}

// Matched to the bank's statement, line by line; what is not matched, on
// either side, is in front of the owner. Imported twice, a line is one.
func TestPaymentsAreMatchedToTheBanksStatement(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	p, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	lines := []workshop.StatementLine{
		{BookedOn: time.Now(), AmountMinor: p.AmountMinor, Reference: "OCR 1", ExternalID: "bank-1"},
		{BookedOn: time.Now(), AmountMinor: 12345, Reference: "Okänd", ExternalID: "bank-2"},
	}
	if n, err := workshop.AddStatementLines(ctx, pool, owner(), "imported", lines); err != nil || n != 2 {
		t.Fatalf("imported %d, %v", n, err)
	}
	if n, err := workshop.AddStatementLines(ctx, pool, owner(), "imported", lines); err != nil || n != 0 {
		t.Errorf("imported again: %d new, %v", n, err)
	}
	if _, err := workshop.Unreconciled(ctx, pool, advisor()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk read the reconciliation: %v", err)
	}
	r, err := workshop.Unreconciled(ctx, pool, owner())
	if err != nil || len(r.Payments) != 1 || len(r.Lines) != 2 {
		t.Fatalf("before matching: %+v, %v", r, err)
	}
	var first, other string
	for _, l := range r.Lines {
		if l.ExternalID == "bank-1" {
			first = l.ID
		} else {
			other = l.ID
		}
	}
	if err := workshop.MatchPayment(ctx, pool, owner(), p.ID, other); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("matched to a line of another amount: %v", err)
	}
	if err := workshop.MatchPayment(ctx, pool, owner(), p.ID, first); err != nil {
		t.Fatal(err)
	}
	if r, _ := workshop.Unreconciled(ctx, pool, owner()); len(r.Payments) != 0 || len(r.Lines) != 1 {
		t.Errorf("after matching: %+v", r)
	}
	if err := workshop.UndoMatch(ctx, pool, owner(), p.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := workshop.Unreconciled(ctx, pool, owner()); len(r.Payments) != 1 {
		t.Errorf("after undoing the match: %+v", r)
	}
	if events, _ := workshop.AuditTrail(ctx, pool, owner(), "payment", p.ID); len(events) != 2 {
		t.Errorf("the payment's trail: %+v", events)
	}
}

// The application may not delete or rewrite the journal's new rows.
func TestTheJournalsRowsStay(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM deposits`, `UPDATE deposits SET purpose = 'other'`,
		`DELETE FROM payouts`, `UPDATE payouts SET purpose = 'other'`,
		`DELETE FROM bank_statement_lines`, `UPDATE bank_statement_lines SET amount_minor = 1`,
		`DELETE FROM payment_matches`, `UPDATE payment_matches SET payment_id = payment_id`,
	} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("%q as the application: %v, want refused for want of the privilege", stmt, err)
		}
	}
}

// The shop's own channels: the manual provider asks nobody and records
// nothing a person has not.
func TestTheManualProviderOnlyRecords(t *testing.T) {
	var p workshop.PaymentProvider = workshop.ManualProvider{}
	ctx := context.Background()
	if id, err := p.CreateRequest(ctx, workshop.PaymentRequest{AmountMinor: 100}); id != "" || err != nil {
		t.Errorf("CreateRequest: %q, %v", id, err)
	}
	if s, err := p.Status(ctx, "x"); s != workshop.ProviderUnknown || err != nil {
		t.Errorf("Status: %q, %v", s, err)
	}
	if _, err := p.VerifyWebhook(nil, []byte("{}")); !errors.Is(err, workshop.ErrNoWebhooks) {
		t.Errorf("VerifyWebhook: %v", err)
	}
}
