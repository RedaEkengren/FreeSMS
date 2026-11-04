package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Return charges (#195).

// A charge is frozen with the treatment its kind had when it was
// confirmed, says whether that treatment was confirmed by the shop, and
// keeps it when the setting changes; damage is outside the scope of VAT
// by default, at exactly what was settled.
func TestAReturnChargeKeepsTheVATTreatmentItWasConfirmedWith(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamage(t, pool)
	dc, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	if err != nil {
		t.Fatal(err)
	}
	booking := dc.BookingID
	cleaning := workshop.ChargeClass{Kind: workshop.ChargeCleaning, VATRateBp: 2500, VATCategory: "S", Confirmed: true}
	if err := workshop.SetChargeClass(ctx, pool, rentalDesk(), cleaning); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk set how a charge is taxed: %v", err)
	}
	if err := workshop.SetChargeClass(ctx, pool, rentalOwner(), workshop.ChargeClass{Kind: workshop.ChargeCleaning, VATRateBp: 2500, VATCategory: "O"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("outside the scope of VAT at a rate: %v", err)
	}
	if err := workshop.SetChargeClass(ctx, pool, rentalOwner(), cleaning); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeCleaning,
		Description: "Städning", QuantityMilli: 1000, UnitPriceMinor: 40_000, Key: "c1"}); err != nil {
		t.Fatal(err)
	}
	again, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeCleaning,
		Description: "Städning", QuantityMilli: 1000, UnitPriceMinor: 40_000, Key: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeCongestionTax,
		Description: "Trängselskatt", QuantityMilli: 2000, UnitPriceMinor: 4_500}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetChargeClass(ctx, pool, rentalOwner(), workshop.ChargeClass{Kind: workshop.ChargeCleaning, VATCategory: "O", Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	amount := int64(200_000)
	if _, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeDamage, DamageCaseID: c}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("damage charged before it was settled: %v", err)
	}
	steps(t, pool, c, workshop.DamageStep{Step: "amount_settled", AmountMinor: &amount},
		workshop.DamageStep{Step: "customer_informed", Channel: "email"})
	damage, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeDamage, DamageCaseID: c})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeDamage, DamageCaseID: c}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("one damage charged twice: %v", err)
	}
	charges, err := workshop.RentalCharges(ctx, pool, rentalDesk(), booking)
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 3 || charges[0].ID != again {
		t.Fatalf("the charges: %+v (the key repeated made %s)", charges, again)
	}
	var sum, want int64
	for _, ch := range charges {
		sum += ch.Gross()
	}
	want = 50_000 + 11_250 + 200_000
	switch cl, ct, dm := charges[0], charges[1], charges[2]; {
	case cl.VATRateBp != 2500 || cl.VATCategory != "S" || !cl.ClassConfirmed || cl.Gross() != 50_000:
		t.Errorf("cleaning changed with the setting, or was not frozen confirmed: %+v", cl)
	case ct.VATCategory != "S" || ct.ClassConfirmed || ct.Gross() != 11_250:
		t.Errorf("congestion tax by the unconfirmed default: %+v, gross %d", ct, ct.Gross())
	case dm.ID != damage || dm.VATCategory != "O" || dm.VATRateBp != 0 || dm.Gross() != amount || dm.Description != dc.Description:
		t.Errorf("damage: %+v, gross %d", dm, dm.Gross())
	case sum != want:
		t.Errorf("the lines come to %d, want %d", sum, want)
	}
	// Withdrawn, the case is charged anew.
	if err := workshop.WithdrawCharge(ctx, pool, rentalDesk(), damage, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("withdrawn without a reason: %v", err)
	}
	if err := workshop.WithdrawCharge(ctx, pool, rentalDesk(), damage, "Fel belopp"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.InformCharge(ctx, pool, rentalDesk(), damage, "email"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("told of a withdrawn charge: %v", err)
	}
	if _, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeDamage, DamageCaseID: c}); err != nil {
		t.Errorf("the case charged again after its charge was withdrawn: %v", err)
	}
}

// A charge comes when it can be owed: return charges after the return, a
// cancellation fee after a cancellation; the customer is told once.
func TestAReturnChargeComesAfterWhatItChargesFor(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, out, _ := checkedOut(t, pool)
	fee := workshop.NewCharge{Kind: workshop.ChargeCleaning, Description: "Städning", QuantityMilli: 1000, UnitPriceMinor: 40_000}
	if _, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), out, fee); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a return charge while the unit is out: %v", err)
	}
	c := newDamageOn(t, pool, "GHI 56J", "V3")
	dc, _ := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	if _, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), dc.BookingID, workshop.NewCharge{Kind: workshop.ChargeCancellationFee,
		Description: "Avbokning", QuantityMilli: 1000, UnitPriceMinor: 10_000}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a cancellation fee on a rental that happened: %v", err)
	}
	if _, err := workshop.ConfirmCharge(ctx, pool, technician(), dc.BookingID, fee); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician confirmed a charge: %v", err)
	}
	id, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), dc.BookingID, fee)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.InformCharge(ctx, pool, rentalDesk(), id, "sms"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.InformCharge(ctx, pool, rentalDesk(), id, "sms"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("told twice: %v", err)
	}
}
