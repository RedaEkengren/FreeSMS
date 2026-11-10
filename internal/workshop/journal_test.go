package workshop_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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
	dep, err := workshop.CreateDeposit(ctx, pool, desk, workshop.NewHolding{PartyID: party, Purpose: "other"})
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
	payout := purchasePayout(t, pool, buyerScope(), seller)
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

// rawPayment writes a payment row as the application would, past every
// check in Go: what the database alone lets through.
func rawPayment(t *testing.T, pool *pgxpool.Pool, cols string, args ...any) error {
	t.Helper()
	return rawPaymentBy(t, pool, "bank", cols, args...)
}

// rawPaymentBy is rawPayment by a way of paying: number first in args,
// then cols' values.
func rawPaymentBy(t *testing.T, pool *pgxpool.Pool, method, cols string, args ...any) error {
	t.Helper()
	return database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO invoice_payments (shop_id, series, number, method, paid_on, recorded_by, `+cols+`)
			VALUES ($1, 'B', $2, $3, now(), $4`+placeholders(5, len(args)-1)+`)`, append([]any{shopID, args[0], method, userID}, args[1:]...)...)
		return err
	})
}

func placeholders(from, n int) string {
	var s string
	for i := range n {
		s += fmt.Sprintf(", $%d", from+i)
	}
	return s
}

func depositWith(t *testing.T, pool *pgxpool.Pool, amount int64) (string, workshop.Payment) {
	t.Helper()
	return depositFor(t, pool, amount, "other")
}

// depositFor is depositWith for a purpose: a rental's is applied only from
// its damage case (#196).
func depositFor(t *testing.T, pool *pgxpool.Pool, amount int64, purpose string) (string, workshop.Payment) {
	t.Helper()
	ctx := context.Background()
	desk := everything(advisor())
	dep, err := workshop.CreateDeposit(ctx, pool, desk, workshop.NewHolding{PartyID: partyOf(t, pool, fixtureCustomer), Purpose: purpose})
	if err != nil {
		t.Fatal(err)
	}
	in, err := workshop.RecordDepositPayment(ctx, pool, desk, dep, workshop.NewPayment{AmountMinor: minor(amount), Method: "swish", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return dep, in
}

// Money handed back for a payment -- its refunds and its reversal -- never
// adds up to more than it was, and a deposit never holds less than
// nothing: in Go, in the database, and with two people at it at once. A
// reversed payment is not refunded; one with refunds standing is reversed
// only once they are, because a reversal undoes the whole payment.
func TestMoneyHandedBackNeverExceedsWhatCameIn(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := everything(advisor())

	// Reversed, then refunded: once handed back already.
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	p, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReversePayment(ctx, pool, advisor(), p.ID, "Fel faktura"); err != nil {
		t.Fatal(err)
	}
	if err := rawPayment(t, pool, "invoice_id, amount_minor, refunds_id, reason", 990, inv.ID, -p.AmountMinor, p.ID, "x"); err == nil {
		t.Error("the database let a reversed payment be refunded")
	}
	if _, err := workshop.RefundPayment(ctx, pool, advisor(), p.ID, workshop.NewPayment{
		AmountMinor: minor(p.AmountMinor), Method: "bank", PaidOn: time.Now()}, "Tillbaka"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a reversed payment refunded: %v", err)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID); b.OutstandingMinor() != inv.GrossMinor {
		t.Errorf("the invoice owes %d, want %d", b.OutstandingMinor(), inv.GrossMinor)
	}

	// Refunded in part, then reversed: refused until the refund is.
	dep, in := depositWith(t, pool, 500000)
	back, err := workshop.RefundPayment(ctx, pool, desk, in.ID, workshop.NewPayment{AmountMinor: minor(300000), Method: "bank", PaidOn: time.Now()}, "Del tillbaka")
	if err != nil {
		t.Fatal(err)
	}
	if err := rawPayment(t, pool, "deposit_id, amount_minor, reverses_id, reason", 991, dep, -500000, in.ID, "x"); err == nil {
		t.Error("the database let a refunded payment be reversed whole")
	}
	if err := workshop.ReversePayment(ctx, pool, desk, in.ID, "Fel belopp"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a refunded payment reversed whole: %v", err)
	}
	if d, _ := workshop.DepositBalanceFor(ctx, pool, desk, dep); d.Held() != 200000 {
		t.Errorf("the deposit holds %d, want 200000", d.Held())
	}
	if err := workshop.ReversePayment(ctx, pool, desk, back.ID, "Återbetalningen var fel"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReversePayment(ctx, pool, desk, in.ID, "Fel belopp"); err != nil {
		t.Errorf("reversed once its refund was: %v", err)
	}
	if d, _ := workshop.DepositBalanceFor(ctx, pool, desk, dep); d.Held() != 0 {
		t.Errorf("the deposit holds %d after both reversals, want 0", d.Held())
	}

	// A deposit partly applied: its receipt cannot be reversed whole, and
	// nothing takes it below nothing.
	dep, in = depositWith(t, pool, 500000)
	inv2, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ApplyDeposit(ctx, pool, desk, dep, inv2.ID, 200000, ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReversePayment(ctx, pool, desk, in.ID, "Fel belopp"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a deposit's receipt reversed with part of it applied: %v", err)
	}
	if err := rawPayment(t, pool, "deposit_id, amount_minor", 992, dep, -300001); err == nil {
		t.Error("the database let a deposit hold less than nothing")
	}
	if d, _ := workshop.DepositBalanceFor(ctx, pool, desk, dep); d.Held() != 300000 {
		t.Errorf("the deposit holds %d, want 300000", d.Held())
	}

	// A reversal undoes exactly one payment, and not a reversal.
	p3, err := workshop.RecordPayment(ctx, pool, advisor(), inv2.ID, workshop.NewPayment{AmountMinor: minor(1000), Method: "bank", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := rawPayment(t, pool, "invoice_id, amount_minor, reverses_id, reason", 993, inv2.ID, -999, p3.ID, "x"); err == nil {
		t.Error("the database let a reversal undo a different amount")
	}

	// Two at once: one refunds the whole payment, the other reverses it.
	// One wins; the money is handed back once.
	for round := range 4 {
		dep, in := depositWith(t, pool, 100000)
		errs := make(chan error, 2)
		go func() {
			_, err := workshop.RefundPayment(ctx, pool, desk, in.ID, workshop.NewPayment{AmountMinor: minor(100000), Method: "bank", PaidOn: time.Now()}, "Tillbaka")
			errs <- err
		}()
		go func() { errs <- workshop.ReversePayment(ctx, pool, desk, in.ID, "Fel") }()
		var ok int
		for range 2 {
			if err := <-errs; err == nil {
				ok++
			} else if !errors.Is(err, workshop.ErrInvalid) {
				t.Errorf("round %d: %v", round, err)
			}
		}
		d, _ := workshop.DepositBalanceFor(ctx, pool, desk, dep)
		if ok != 1 || d.Held() != 0 {
			t.Errorf("round %d: %d of the two went through and the deposit holds %d; want one, and nothing", round, ok, d.Held())
		}
	}
}

// applicationHalves is the two rows of the one application on a deposit
// and an invoice: out of the deposit, and into the invoice.
func applicationHalves(t *testing.T, pool *pgxpool.Pool, dep, inv string) (out, in string) {
	t.Helper()
	ctx := context.Background()
	d, err := workshop.DepositBalanceFor(ctx, pool, everything(advisor()), dep)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range d.Payments {
		if p.Method == "deposit" && p.Reverses == "" {
			out = p.ID
		}
	}
	b, err := workshop.BalanceFor(ctx, pool, advisor(), inv)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range b.Payments {
		if p.Method == "deposit" && p.Reverses == "" {
			in = p.ID
		}
	}
	if out == "" || in == "" {
		t.Fatalf("the application's halves: %q, %q", out, in)
	}
	return out, in
}

// A deposit applied to an invoice is one fact in two rows. Reversed from
// either half, both go: the deposit holds the money again and the invoice
// owes it again, so it neither exists twice nor vanishes. The database
// refuses a reversal of one half alone.
func TestADepositApplicationIsReversedWhole(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := everything(advisor())
	for _, side := range []string{"deposit", "invoice"} {
		dep, _ := depositWith(t, pool, 500000)
		inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
		if err != nil {
			t.Fatal(err)
		}
		if err := workshop.ApplyDeposit(ctx, pool, desk, dep, inv.ID, 200000, ""); err != nil {
			t.Fatal(err)
		}
		out, in := applicationHalves(t, pool, dep, inv.ID)
		// One half alone, past Go: refused when the transaction commits.
		if err := rawPaymentBy(t, pool, "deposit", "deposit_id, amount_minor, reverses_id, reason", 980, dep, 200000, out, "x"); err == nil {
			t.Errorf("%s side: the database let the deposit's half be reversed alone", side)
		}
		if err := rawPaymentBy(t, pool, "deposit", "invoice_id, amount_minor, reverses_id, reason", 981, inv.ID, -200000, in, "x"); err == nil {
			t.Errorf("%s side: the database let the invoice's half be reversed alone", side)
		}
		which := out
		if side == "invoice" {
			which = in
		}
		if err := workshop.ReversePayment(ctx, pool, desk, which, "Fel deposition"); err != nil {
			t.Fatalf("%s side: %v", side, err)
		}
		d, _ := workshop.DepositBalanceFor(ctx, pool, desk, dep)
		b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
		if d.Held() != 500000 || b.OutstandingMinor() != inv.GrossMinor {
			t.Errorf("%s side: the deposit holds %d and the invoice owes %d; want 500000 and %d", side, d.Held(), b.OutstandingMinor(), inv.GrossMinor)
		}
		for _, again := range []string{out, in} {
			if err := workshop.ReversePayment(ctx, pool, desk, again, "Igen"); !errors.Is(err, workshop.ErrInvalid) {
				t.Errorf("%s side: an application reversed twice: %v", side, err)
			}
		}
	}
}

// A request's key is checked under the locks of what it moves money on,
// and names one request: the same key again -- a double click, at once or
// after -- records nothing more and succeeds, and the same key for
// anything else is refused rather than answered with a payment it did not
// make.
func TestARequestKeyNamesOneRequest(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := everything(advisor())
	dep, _ := depositWith(t, pool, 500000)
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	other, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	applications := func() int {
		return countRows(t, pool, `SELECT count(*) FROM invoice_payments WHERE method = 'deposit' AND deposit_id = $1`, dep)
	}

	// A double click: both at once, one application.
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- workshop.ApplyDeposit(ctx, pool, desk, dep, inv.ID, 100000, "apply-twice") }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("a double click on the application: %v", err)
		}
	}
	if n := applications(); n != 1 {
		t.Errorf("%d applications from one request, want 1", n)
	}
	// The same key for another invoice, another amount, or another kind of
	// request: refused, and nothing moves.
	for name, apply := range map[string]func() error{
		"another invoice": func() error { return workshop.ApplyDeposit(ctx, pool, desk, dep, other.ID, 100000, "apply-twice") },
		"another amount":  func() error { return workshop.ApplyDeposit(ctx, pool, desk, dep, inv.ID, 50000, "apply-twice") },
	} {
		if err := apply(); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("the key again for %s: %v, want refused", name, err)
		}
	}
	paid, err := workshop.RecordPayment(ctx, pool, advisor(), other.ID, workshop.NewPayment{AmountMinor: minor(1000), Method: "bank", PaidOn: time.Now(), Key: "pay-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ApplyDeposit(ctx, pool, desk, dep, other.ID, 1000, "pay-1"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an application under a payment's key: %v, want refused", err)
	}
	if n := applications(); n != 1 {
		t.Errorf("%d applications after the refusals, want 1", n)
	}
	if r, err := workshop.RefundPayment(ctx, pool, advisor(), paid.ID, workshop.NewPayment{AmountMinor: minor(1000), Method: "bank", PaidOn: time.Now(), Key: "pay-1"}, "Tillbaka"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a refund under the payment's own key: %+v, %v; want refused", r, err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), other.ID, workshop.NewPayment{AmountMinor: minor(2000), Method: "bank", PaidOn: time.Now(), Key: "pay-1"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the payment's key again for another amount: %v, want refused", err)
	}
	if again, err := workshop.RecordPayment(ctx, pool, advisor(), other.ID, workshop.NewPayment{AmountMinor: minor(1000), Method: "bank", PaidOn: time.Now(), Key: "pay-1"}); err != nil || again.ID != paid.ID {
		t.Errorf("the payment repeated: %s, %v; want %s", again.ID, err, paid.ID)
	}
}

// Money a customer has paid in is always returnable: a rental deposit
// still held is refunded after Rental is switched off, by the rental desk
// that took it or by the front desk. Taking a new one is refused.
func TestAHeldDepositIsRefundedWithRentalSwitchedOff(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	dep, in := depositFor(t, pool, 300000, "rental")
	off := func(s access.Scope) access.Scope { s.Modules = []access.Module{access.Workshop}; return s }
	desk, front := off(otherRoles(access.Of(access.RoleRentalAgent))), off(advisor())
	if _, err := workshop.CreateDeposit(ctx, pool, desk, workshop.NewHolding{PartyID: partyOf(t, pool, fixtureCustomer), Purpose: "rental"}); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a new rental deposit with Rental off: %v", err)
	}
	for _, s := range []access.Scope{desk, front} {
		if _, err := workshop.RefundPayment(ctx, pool, s, in.ID, workshop.NewPayment{AmountMinor: minor(100000), Method: "bank", PaidOn: time.Now()}, "Hyran avslutad"); err != nil {
			t.Errorf("%v refunding a held deposit with Rental off: %v", s.Role, err)
		}
	}
	if d, err := workshop.DepositBalanceFor(ctx, pool, desk, dep); err != nil || d.Held() != 100000 {
		t.Errorf("the deposit after both refunds: %+v, %v", d, err)
	}
}

// An application one half of which was reversed alone before 0082 linked
// the two -- the money then held by the deposit again and still paid on
// the invoice, or neither -- is put right by reversing the half that is
// left, from either half; after that it is reversed whole and refused
// again.
func TestAnApplicationHalfReversedBefore0082IsPutRight(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := everything(advisor())
	for i, side := range []string{"deposit", "invoice"} {
		dep, _ := depositWith(t, pool, 500000)
		inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
		if err != nil {
			t.Fatal(err)
		}
		if err := workshop.ApplyDeposit(ctx, pool, desk, dep, inv.ID, 200000, ""); err != nil {
			t.Fatal(err)
		}
		out, in := applicationHalves(t, pool, dep, inv.ID)
		// As the release before 0082 wrote it: one half reversed alone,
		// with the checks that came later not there.
		lone, target, amount := out, "deposit_id", int64(200000)
		other := in
		if side == "invoice" {
			lone, target, amount, other = in, "invoice_id", -200000, out
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO invoice_payments (shop_id, series, number, method, paid_on, recorded_by, `+target+`, amount_minor, reverses_id, reason)
			SELECT $1, 'B', $2, 'deposit', now(), $3, coalesce(invoice_id, deposit_id), $4, id, 'before 0082' FROM invoice_payments WHERE id = $5`,
			shopID, 970+i, userID, amount, lone); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		// From either half: the one reversed before, or the one left.
		first, second := lone, other
		if side == "invoice" {
			first, second = other, lone
		}
		if err := workshop.ReversePayment(ctx, pool, desk, first, "Halva var redan återförd"); err != nil {
			t.Errorf("%s half reversed before: reversing the rest: %v", side, err)
		}
		if err := workshop.ReversePayment(ctx, pool, desk, second, "Igen"); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s half reversed before: reversed again once whole: %v", side, err)
		}
		d, _ := workshop.DepositBalanceFor(ctx, pool, desk, dep)
		b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
		if d.Held() != 500000 || b.OutstandingMinor() != inv.GrossMinor {
			t.Errorf("%s half reversed before: the deposit holds %d and the invoice owes %d; want 500000 and %d", side, d.Held(), b.OutstandingMinor(), inv.GrossMinor)
		}
	}
}

// A payment through the bank with no line on the statement five days on is
// one item on the owner's queue (#158); matched, it leaves; the front desk
// never has it.
func TestAnUnmatchedPaymentWaitsForTheOwnerUntilItIsMatched(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	p, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now().AddDate(0, 0, -10)})
	if err != nil {
		t.Fatal(err)
	}
	items := waiting(t, owner(), "payments_unmatched")
	if len(items) != 1 || items[0].Count != 1 || items[0].Link() != "/reconcile" {
		t.Fatalf("the owner's queue: %+v", items)
	}
	if got := waiting(t, advisor(), "payments_unmatched"); len(got) != 0 {
		t.Errorf("the front desk has it: %+v", got)
	}
	if _, err := workshop.AddStatementLines(ctx, pool, owner(), "imported", []workshop.StatementLine{
		{BookedOn: time.Now(), AmountMinor: p.AmountMinor, Reference: "OCR 1", ExternalID: "bank-1"}}); err != nil {
		t.Fatal(err)
	}
	r, _ := workshop.Unreconciled(ctx, pool, owner())
	if err := workshop.MatchPayment(ctx, pool, owner(), p.ID, r.Lines[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, owner(), "payments_unmatched"); len(got) != 0 {
		t.Errorf("matched, and still waiting: %+v", got)
	}
}

// A deposit received and a payout are money that moved, so the day counts
// them under their way of paying, each pointing at what it was for; they
// are not sales, and the accounting export leaves them out until the
// accounts they use are decided (G2, #207, #221).
func TestADepositAndAPayoutAreInTheDayAndNotInTheAccountingExport(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, in := depositWith(t, pool, 1234500)
	seller := privateSeller(t, pool)
	payout := purchasePayout(t, pool, buyerScope(), seller)
	out, err := workshop.RecordPayout(ctx, pool, buyerScope(), payout, workshop.NewPayment{
		AmountMinor: minor(8500000), Method: "bank", PaidOn: time.Now(), Reference: "Köp ABC 12D"})
	if err != nil {
		t.Fatal(err)
	}

	d := dayOf(t, pool, today())
	if d.InvoicedGross != 0 || d.CreditedGross != 0 {
		t.Errorf("a deposit or a payout counted as a sale: invoiced %d credited %d", d.InvoicedGross, d.CreditedGross)
	}
	swish, bank := method(d, "swish"), method(d, "bank")
	if swish.Received != in.AmountMinor || len(swish.Entries) != 1 || swish.Entries[0].Target != "deposit" {
		t.Errorf("the deposit in the day: %+v", swish)
	}
	if bank.PaidOut != -out.AmountMinor || len(bank.Entries) != 1 || bank.Entries[0].Target != "payout" {
		t.Errorf("the payout in the day: %+v", bank)
	}

	from := time.Now().AddDate(0, 0, -1)
	to := time.Now().AddDate(0, 0, 1)
	// The only money in the period is a deposit and a payout, so there is
	// nothing to export.
	_, n, err := workshop.ExportAccounting(ctx, pool, owner(), from, to, false)
	if !errors.Is(err, workshop.ErrNothingToExport) {
		t.Fatalf("exported a deposit or a payout: %d rows, %v", n, err)
	}
	var exported int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoice_payments WHERE accounting_export_id IS NOT NULL`).Scan(&exported); err != nil {
		t.Fatal(err)
	}
	if exported != 0 {
		t.Errorf("%d deposit or payout rows were exported (the export reported %d)", exported, n)
	}
}

// A payment may be dated a little before its invoice (the counter paid on
// Friday, the invoice written on Monday) and not a month before (a year
// typed wrong); and a finance company paying for a buyer is a payment
// against the buyer's invoice that says who paid in its reference (#152).
func TestAPaymentDatedBeforeItsInvoiceAndAThirdPartyPayer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	half := inv.GrossMinor / 2
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &half, Method: "bank", PaidOn: time.Now().AddDate(0, 0, -60)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a payment dated two months before the invoice: %v", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &half, Method: "bank", PaidOn: time.Now().AddDate(0, 0, 5)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a payment dated five days ahead: %v", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &half, Method: "bank", PaidOn: time.Now().AddDate(0, 0, -3)}); err != nil {
		t.Errorf("a payment dated three days before the invoice: %v", err)
	}
	rest := inv.GrossMinor - half
	p, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &rest, Method: "bank", PaidOn: time.Now(), Reference: "Finansbolaget AB, köparens finansiering"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Reference != "Finansbolaget AB, köparens finansiering" {
		t.Errorf("the payer is not on the payment: %q", p.Reference)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID); !b.Settled() {
		t.Errorf("paid by two payers and not settled: %d outstanding", b.OutstandingMinor())
	}
}
