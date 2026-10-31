package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// A line put on after the customer said yes is not part of what they agreed
// to. Invoicing it is something somebody decides, not something that happens.
func TestALineNobodyAgreedToIsNotInvoicedUnasked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyToInvoice(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "fee", Description: "Extra", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}

	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrUnagreedLines) {
		t.Fatalf("Issue with an unagreed line = %v, want ErrUnagreedLines", err)
	}
	// Refused whole: no number was used.
	inv, err := workshop.Issue(ctx, pool, advisor(), job, workshop.InvoiceUnagreedLines())
	if err != nil {
		t.Fatalf("Issue when told to include it: %v", err)
	}
	if inv.Reference() != "A-1" {
		t.Errorf("the refusal used a number: first invoice is %s", inv.Reference())
	}
}

// Putting the line to the customer is the other way through.
func TestALineTheCustomerAgreedToLaterIsInvoiced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyToInvoice(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "fee", Description: "Extra", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	agree(t, pool, job)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatalf("Issue after the customer agreed: %v", err)
	}
}

// A payment dated a year before the invoice, or one far above it, is asked
// about; a few days before is ordinary (paid on Friday, written up on Monday).
func TestAPaymentOutOfProportionIsRefusedUntilConfirmed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(100), Method: "bank", PaidOn: time.Now().AddDate(-1, 0, 0)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a payment a year before the invoice = %v, want refused", err)
	}
	ten := inv.GrossMinor * 10
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &ten, Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrOverpayment) {
		t.Errorf("ten times the invoice = %v, want ErrOverpayment", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &ten, Method: "bank", PaidOn: time.Now(), ConfirmedOverpayment: true}); err != nil {
		t.Errorf("a confirmed overpayment = %v", err)
	}
}

// #131, every line unagreed: a job approved with nothing on it for the
// customer to pay, and everything put on afterwards. Issue asks; told to, it
// invoices all of it, and the lines stay without an agreement, so the job and
// the invoice's issuer are the record of who decided it. The credit note of
// such an invoice is an ordinary one: nothing is asked again.
func TestAJobWhoseEveryLineIsUnagreedIsAskedAboutAndCanBeCredited(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress)
	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Found while in there", QuantityMilli: 1000, UnitPriceMinor: 80000, VATRateBasis: 2500},
		{Kind: "part", Description: "And this", QuantityMilli: 2000, UnitPriceMinor: 15000, VATRateBasis: 2500},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), job, l); err != nil {
			t.Fatal(err)
		}
	}
	move(t, pool, job, workshop.StateReady)

	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrUnagreedLines) {
		t.Fatalf("Issue with every line unagreed = %v, want ErrUnagreedLines", err)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job, workshop.InvoiceUnagreedLines())
	if err != nil {
		t.Fatalf("Issue when told to: %v", err)
	}
	if inv.NetMinor != 110000 {
		t.Errorf("net %d, want 110000: both lines", inv.NetMinor)
	}
	var unagreed int
	var by string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_order_lines
			WHERE work_order_id = $1 AND approved_at IS NULL AND cost_bearer = 'customer'`, job).Scan(&unagreed); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT issued_by::text FROM invoices WHERE id = $1`, inv.ID).Scan(&by)
	}); err != nil {
		t.Fatal(err)
	}
	if unagreed != 2 || by != userID {
		t.Errorf("after issuing: %d lines without agreement (want 2), issued by %q (want %s)", unagreed, by, userID)
	}

	credit, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("crediting an invoice of unagreed lines: %v", err)
	}
	if credit.GrossMinor != -inv.GrossMinor {
		t.Errorf("the credit note is %d, want %d", credit.GrossMinor, -inv.GrossMinor)
	}
}

// #132: what is not an overpayment is not asked about. Cash rounded up to the
// krona is fifty öre over; part payments that add up to the total settle it;
// only the one that goes past it by more than a krona asks.
func TestCashRoundingAndPartPaymentsAreNotTakenForAnOverpayment(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// Paid in notes and coins, the öre rounded up to the next krona.
	cash, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if cash.GrossMinor%100 == 0 {
		t.Fatalf("the fixture invoice %d is whole kronor and shows no rounding", cash.GrossMinor)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), cash.ID, workshop.NewPayment{
		AmountMinor: amount((cash.GrossMinor/100 + 1) * 100), Method: "cash", PaidOn: time.Now()}); err != nil {
		t.Errorf("cash rounded up to the krona = %v, want recorded unasked", err)
	}

	pool = setup(t)
	parts, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	for _, minor := range []int64{100000, 100000, parts.GrossMinor - 200000} {
		if _, err := workshop.RecordPayment(ctx, pool, advisor(), parts.ID, workshop.NewPayment{
			AmountMinor: amount(minor), Method: "bank", PaidOn: time.Now()}); err != nil {
			t.Fatalf("a part payment of %d that the total still covers = %v", minor, err)
		}
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), parts.ID, workshop.NewPayment{
		AmountMinor: amount(50000), Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrOverpayment) {
		t.Errorf("a part payment past the total = %v, want ErrOverpayment", err)
	}
}

// #133: money may be dated before its invoice -- up to a month, for a deposit
// or the counter paid before the invoice was written -- but not further back,
// and not ahead. A payment dated into a day that was signed off is recorded,
// and the sign-off says the day has changed since.
func TestAPaymentDateIsCheckedAgainstTheInvoiceAndASignedOffDayIsToldOfIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	issuedOn := inv.IssuedAt.In(shopZone())
	day := func(back int) time.Time {
		d := issuedOn.AddDate(0, 0, -back)
		return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, shopZone())
	}

	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(1000), Method: "bank", PaidOn: day(32)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("32 days before the invoice = %v, want refused", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(1000), Method: "bank", PaidOn: time.Now().AddDate(0, 0, 2)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("two days ahead = %v, want refused", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(1000), Method: "bank", PaidOn: day(31)}); err != nil {
		t.Errorf("31 days before the invoice = %v, want recorded", err)
	}

	signed := day(3).Format("2006-01-02")
	if _, err := workshop.ReviewDay(ctx, pool, owner(), signed, "Nothing that day"); err != nil {
		t.Fatalf("ReviewDay: %v", err)
	}
	if d := dayOf(t, pool, signed); d.Review == nil || d.Stale {
		t.Fatalf("the signed-off day before the payment: review %v, stale %v", d.Review, d.Stale)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(2000), Method: "card", PaidOn: day(3)}); err != nil {
		t.Fatalf("a payment dated into a signed-off day = %v, want recorded", err)
	}
	if d := dayOf(t, pool, signed); !d.Stale || method(d, "card").Received != 2000 {
		t.Errorf("the signed-off day after a payment dated into it: stale %v, card %d", d.Stale, method(d, "card").Received)
	}
}
