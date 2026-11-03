package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A rental's deposit (#196).

// depositOn asks and receives a deposit of amount, in cash, on the
// booking the damage case c came from, and is the booking and the payment
// received.
func depositOn(t *testing.T, pool *pgxpool.Pool, c string, amount int64) (string, workshop.Payment) {
	t.Helper()
	ctx := context.Background()
	dc, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RequireRentalDeposit(ctx, pool, rentalDesk(), dc.BookingID, "", amount, "cash"); err != nil {
		t.Fatal(err)
	}
	d, err := workshop.RentalDepositOf(ctx, pool, rentalDesk(), dc.BookingID)
	if err != nil {
		t.Fatal(err)
	}
	in, err := workshop.RecordDepositPayment(ctx, pool, rentalDesk(), d.DepositID, workshop.NewPayment{AmountMinor: minor(amount), Method: "cash", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return dc.BookingID, in
}

// steps takes the damage case's steps as the rental desk.
func steps(t *testing.T, pool *pgxpool.Pool, c string, ss ...workshop.DamageStep) {
	t.Helper()
	for _, s := range ss {
		if s.Reason == "" {
			s.Reason = "Enligt rutin"
		}
		if err := workshop.TakeDamageStep(context.Background(), pool, rentalDesk(), c, s); err != nil {
			t.Fatalf("%s: %v", s.Step, err)
		}
	}
}

// A deposit is applied to a claim only once its amount is settled, the
// customer told and they accepted it, never beyond it, and never by the
// journal's own ApplyDeposit. What is left is the customer's, given back,
// and the deposit, the journal and the invoice then agree to the öre.
func TestARentalDepositSettlesOnlyAnAcceptedClaimAndTheRestGoesBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamageOn(t, pool, "GHI 56J", "V3")
	booking, in := depositOn(t, pool, c, 500_000)
	_, invoice := issued(t, pool)
	inv := invoice.ID
	before, err := workshop.BalanceFor(ctx, pool, advisor(), inv)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(amount int64) error {
		return workshop.ApplyRentalDeposit(ctx, pool, rentalDesk(), booking, c, inv, amount, "")
	}
	d, _ := workshop.RentalDepositOf(ctx, pool, rentalDesk(), booking)
	if err := workshop.ApplyDeposit(ctx, pool, advisor(), d.DepositID, inv, 1000, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a rental's deposit applied past its damage case: %v", err)
	}
	if err := apply(1000); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("applied to a damage only observed: %v", err)
	}
	amount := int64(200_000)
	tomorrow := time.Now().AddDate(0, 0, 1)
	steps(t, pool, c, workshop.DamageStep{Step: "new_confirmed"},
		workshop.DamageStep{Step: "liability_assessed", Liability: "renter"},
		workshop.DamageStep{Step: "amount_settled", AmountMinor: &amount},
		workshop.DamageStep{Step: "customer_informed", Channel: "email", RespondBy: &tomorrow})
	if err := apply(1000); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("applied while the customer may still object: %v", err)
	}
	steps(t, pool, c, workshop.DamageStep{Step: "customer_response", Response: "accepted"})
	if err := apply(amount + 1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("more applied than the case settled: %v", err)
	}
	if err := apply(amount); err != nil {
		t.Fatal(err)
	}
	if err := apply(1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the case taken from twice: %v", err)
	}
	if _, err := workshop.RefundPayment(ctx, pool, rentalDesk(), in.ID, workshop.NewPayment{AmountMinor: minor(300_001), Method: "bank",
		PaidOn: time.Now()}, "Resten tillbaka"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("more given back than is left: %v", err)
	}
	if _, err := workshop.RefundPayment(ctx, pool, rentalDesk(), in.ID, workshop.NewPayment{AmountMinor: minor(300_000), Method: "bank",
		PaidOn: time.Now()}, "Resten tillbaka"); err != nil {
		t.Fatal(err)
	}
	d, err = workshop.RentalDepositOf(ctx, pool, rentalDesk(), booking)
	if err != nil {
		t.Fatal(err)
	}
	after, err := workshop.BalanceFor(ctx, pool, advisor(), inv)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case d.Balance.Received != 500_000 || d.Balance.Applied != amount || d.Balance.Refunded != 300_000 || d.OwedBack() != 0:
		t.Errorf("the deposit: %+v, owed back %d", d.Balance, d.OwedBack())
	case before.OutstandingMinor()-after.OutstandingMinor() != amount:
		t.Errorf("the invoice was settled by %d, want %d", before.OutstandingMinor()-after.OutstandingMinor(), amount)
	}
}

// Undisputed and past the day to object, a claim is settled from the
// deposit without an answer; disputed, only once whoever runs the shop has
// set it against the deposit.
func TestADepositWaitsForTheTimeToObjectOrTheOwnersWord(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	quiet, disputed := newDamageOn(t, pool, "GHI 56J", "V3"), newDamageOn(t, pool, "DEF 34G", "V2")
	amount := int64(50_000)
	past := time.Now().AddDate(0, 0, -2)
	for _, c := range []string{quiet, disputed} {
		steps(t, pool, c, workshop.DamageStep{Step: "liability_assessed", Liability: "renter"},
			workshop.DamageStep{Step: "amount_settled", AmountMinor: &amount},
			workshop.DamageStep{Step: "customer_informed", Channel: "letter", RespondBy: &past})
	}
	_, invoice := issued(t, pool)
	inv := invoice.ID
	b1, _ := depositOn(t, pool, quiet, 100_000)
	if err := workshop.ApplyRentalDeposit(ctx, pool, rentalDesk(), b1, quiet, inv, amount, ""); err != nil {
		t.Errorf("past the day to object: %v", err)
	}
	b2, _ := depositOn(t, pool, disputed, 100_000)
	if err := workshop.ApplyRentalDeposit(ctx, pool, rentalDesk(), b2, quiet, inv, 1000, ""); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another booking's damage case settled from this deposit: %v", err)
	}
	steps(t, pool, disputed, workshop.DamageStep{Step: "customer_response", Response: "disputed"})
	if err := workshop.ApplyRentalDeposit(ctx, pool, rentalDesk(), b2, disputed, inv, amount, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a disputed claim taken from the deposit: %v", err)
	}
	if err := workshop.TakeDamageStep(ctx, pool, rentalOwner(), disputed, workshop.DamageStep{Step: "outcome", Outcome: "set_against_deposit",
		Reason: "Bilderna visar ny repa"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ApplyRentalDeposit(ctx, pool, rentalDesk(), b2, disputed, inv, amount, ""); err != nil {
		t.Errorf("on the owner's word: %v", err)
	}
}

// A hold on the card moves no money: it is not in the journal, it is
// released with a reason, one stands at a time, and one that runs out
// before the rental is due back says so.
func TestAHoldOnTheCardIsNoMoneyAndSaysWhenItRunsOutTooSoon(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamage(t, pool)
	dc, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	if err != nil {
		t.Fatal(err)
	}
	booking := dc.BookingID
	if err := workshop.HoldRentalDeposit(ctx, pool, rentalDesk(), booking, 300_000, "T-1", time.Now().Add(time.Hour)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a hold on a deposit never asked for: %v", err)
	}
	if err := workshop.RequireRentalDeposit(ctx, pool, technician(), booking, "", 300_000, "card_hold"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician asked a deposit: %v", err)
	}
	if err := workshop.RequireRentalDeposit(ctx, pool, rentalDesk(), booking, "", 300_000, "card_hold"); err != nil {
		t.Fatal(err)
	}
	d, err := workshop.RentalDepositOf(ctx, pool, rentalDesk(), booking)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.HoldRentalDeposit(ctx, pool, rentalDesk(), booking, 300_000, "T-1", time.Now().Add(-time.Minute)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a hold already run out: %v", err)
	}
	if err := workshop.HoldRentalDeposit(ctx, pool, rentalDesk(), booking, 300_000, "T-1", d.BookingEnds.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := workshop.HoldRentalDeposit(ctx, pool, rentalDesk(), booking, 300_000, "T-2", time.Now().AddDate(0, 0, 7)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("two holds at once: %v", err)
	}
	d, err = workshop.RentalDepositOf(ctx, pool, rentalDesk(), booking)
	switch {
	case err != nil:
		t.Fatal(err)
	case d.Hold == nil || d.Hold.AmountMinor != 300_000 || d.Short() != 0:
		t.Errorf("the hold: %+v, short %d", d.Hold, d.Short())
	case len(d.Balance.Payments) != 0 || d.OwedBack() != 0:
		t.Errorf("a hold in the journal: %+v", d.Balance)
	case d.HoldEndsTooSoon():
		t.Errorf("a hold lasting past the return said to end too soon")
	}
	d.Hold.ValidUntil = d.BookingEnds.Add(-time.Hour)
	if !d.HoldEndsTooSoon() {
		t.Errorf("a hold ending before the return not said to")
	}
	if err := workshop.ReleaseRentalDepositHold(ctx, pool, rentalDesk(), booking, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("released without a reason: %v", err)
	}
	if err := workshop.ReleaseRentalDepositHold(ctx, pool, rentalDesk(), booking, "Bilen tillbaka utan anmärkning"); err != nil {
		t.Fatal(err)
	}
	if d, err = workshop.RentalDepositOf(ctx, pool, rentalDesk(), booking); err != nil || d.Hold != nil || d.Short() != 300_000 {
		t.Errorf("released: %+v, short %d, %v", d.Hold, d.Short(), err)
	}
}

// A rental over whose deposit is still held is on the rental desk's
// queue, but not while a damage case of it is open: the case decides first.
func TestARentalOverWithItsDepositHeldIsOnTheQueue(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamage(t, pool)
	booking, _ := depositOn(t, pool, c, 100_000)
	waiting := func() bool {
		t.Helper()
		items, err := workshop.WaitingFor(ctx, pool, rentalDesk())
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(items, func(it workshop.ActionItem) bool {
			return it.Kind == "deposit_to_return" && it.SubjectID == booking
		})
	}
	if waiting() {
		t.Errorf("the deposit on the queue while its damage case is open")
	}
	steps(t, pool, c, workshop.DamageStep{Step: "pre_existing"},
		workshop.DamageStep{Step: "outcome", Outcome: "closed_not_renter"})
	if !waiting() {
		t.Errorf("a rental over with its deposit held is not on the queue")
	}
}
