package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Rental damage cases (#197).

// newDamage takes a rental out and back with a new scratch, and is the
// damage case the return opened.
func newDamage(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return newDamageOn(t, pool, "ABC 12D", "V1")
}

// newDamageOn is newDamage on a unit with a registration and fleet number
// of its own.
func newDamageOn(t *testing.T, pool *pgxpool.Pool, registration, fleet string) string {
	t.Helper()
	ctx := context.Background()
	_, id := handoverReadyWith(t, pool, registration, fleet)
	out, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverOut)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out, workshop.HandoverReading{OdometerKm: km64(12000), FuelEighths: ptr(8),
		RenterPresent: true, RenterAccepted: true}); err != nil {
		t.Fatal(err)
	}
	in, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
	if err != nil {
		t.Fatal(err)
	}
	recordFound(t, pool, in, map[string]string{"Lack och plåt": "attention"})
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), in, workshop.HandoverReading{OdometerKm: km64(12100), FuelEighths: ptr(8)}); err != nil {
		t.Fatal(err)
	}
	view, err := workshop.RentalHandovers(ctx, pool, rentalDesk(), id)
	if err != nil || len(view.Damage) != 1 {
		t.Fatalf("the damage the return opened: %+v, %v", view.Damage, err)
	}
	return view.Damage[0].ID
}

// A damage is not a debt: it is invoiced or set against the deposit only
// once its amount is settled and the customer told, and the customer is
// told only of an amount settled. Closed, it is reopened by a step.
func TestADamageIsOwedOnlyOnceSettledAndTold(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamage(t, pool)
	step := func(s workshop.DamageStep) error {
		t.Helper()
		if s.Reason == "" {
			s.Reason = "Bedömt vid återlämning"
		}
		return workshop.TakeDamageStep(ctx, pool, rentalDesk(), c, s)
	}
	amount := int64(250_000)
	if err := step(workshop.DamageStep{Step: "outcome", Outcome: "invoiced"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("invoiced as observed: %v", err)
	}
	if err := step(workshop.DamageStep{Step: "customer_informed", Channel: "email"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the customer told of no amount: %v", err)
	}
	for _, s := range []workshop.DamageStep{
		{Step: "new_confirmed"},
		{Step: "liability_assessed", Liability: "renter", Reason: "Repan fanns inte vid utlämning"},
		{Step: "amount_settled", AmountMinor: &amount, Reason: "Verkstadens offert"},
	} {
		if err := step(s); err != nil {
			t.Fatalf("%s: %v", s.Step, err)
		}
	}
	if err := step(workshop.DamageStep{Step: "outcome", Outcome: "set_against_deposit"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("set against the deposit before the customer was told: %v", err)
	}
	if err := step(workshop.DamageStep{Step: "customer_informed", Channel: "email", Reason: "Skickat med bilder och belopp"}); err != nil {
		t.Fatal(err)
	}
	if err := step(workshop.DamageStep{Step: "outcome", Outcome: "invoiced", Reason: "Fakturerad"}); err != nil {
		t.Fatal(err)
	}
	if err := step(workshop.DamageStep{Step: "liability_assessed", Liability: "shop"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a closed case assessed again: %v", err)
	}
	if err := step(workshop.DamageStep{Step: "reopened", Reason: "Kunden hörde av sig med bilder"}); err != nil {
		t.Fatal(err)
	}
	d, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	if err != nil || d.Closed() || d.Chargeable() || len(d.Steps) != 6 {
		t.Errorf("reopened: closed %v, chargeable %v, %d steps, %v", d.Closed(), d.Chargeable(), len(d.Steps), err)
	}
	if _, err := workshop.DamageCaseByID(ctx, pool, everything(technician()), c); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the case and its renter: %v", err)
	}
}

// A disputed damage is set against the deposit only on the word of
// whoever runs the shop, with a reason, an audit event.
func TestADisputedDamageGoesAgainstTheDepositOnlyOnTheOwnersWord(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamage(t, pool)
	amount := int64(100_000)
	for _, s := range []workshop.DamageStep{
		{Step: "liability_assessed", Liability: "renter", Reason: "Ny repa"},
		{Step: "amount_settled", AmountMinor: &amount, Reason: "Självrisken"},
		{Step: "customer_informed", Channel: "sms", Reason: "Skickat"},
		{Step: "customer_response", Response: "disputed", Reason: "Kunden menar att repan fanns"},
	} {
		if err := workshop.TakeDamageStep(ctx, pool, rentalDesk(), c, s); err != nil {
			t.Fatalf("%s: %v", s.Step, err)
		}
	}
	against := workshop.DamageStep{Step: "outcome", Outcome: "set_against_deposit", Reason: "Bilderna visar ny repa"}
	if err := workshop.TakeDamageStep(ctx, pool, rentalDesk(), c, against); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the rental desk set a disputed damage against the deposit: %v", err)
	}
	if err := workshop.TakeDamageStep(ctx, pool, rentalOwner(), c, against); err != nil {
		t.Fatal(err)
	}
	trail, err := workshop.AuditTrail(ctx, pool, rentalOwner(), "damage_case", c)
	if err != nil || !slices.ContainsFunc(trail, func(e workshop.AuditEvent) bool { return e.Event == "damage.set_against_disputed" }) {
		t.Errorf("no audit event for the decision: %+v, %v", trail, err)
	}
}
