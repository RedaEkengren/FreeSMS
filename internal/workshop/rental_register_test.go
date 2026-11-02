package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The rental register (#200).

// A month of a van: a rental that went out, was extended, took an extra
// driver and came back; the van's plate changed; a second rental out on
// the new plate; a third cancelled. The register says who had it when,
// under which plate, by the actual times, and only the owner reads it.
func TestTheRentalRegisterSaysWhoHadTheVehicleWhen(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v, first := handoverReady(t, pool)
	out, err := workshop.StartHandover(ctx, pool, rentalDesk(), first, workshop.HandoverOut)
	if err != nil {
		t.Fatal(err)
	}
	recordFound(t, pool, out, nil)
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out, workshop.HandoverReading{OdometerKm: km64(1000), RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), first, in2031(13, 9), "Behöver en dag till"); err != nil {
		t.Fatal(err)
	}
	extra, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Erik Extra", Role: workshop.PartyRenter})
	if _, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), first, extra, false); err != nil {
		t.Fatal(err)
	}
	whileOut := time.Now()
	in, err := workshop.StartHandover(ctx, pool, rentalDesk(), first, workshop.HandoverIn)
	if err != nil {
		t.Fatal(err)
	}
	recordFound(t, pool, in, nil)
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), in, workshop.HandoverReading{OdometerKm: km64(1100), RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	// The cleaning after it let go, and the van given a new plate.
	for _, s := range []string{
		`UPDATE resource_blocks SET status = 'released' WHERE id = (SELECT block_id FROM rental_bookings WHERE id = '` + first + `')`,
		`UPDATE vehicle_registrations SET valid_to = clock_timestamp() WHERE vehicle_id = '` + vehicleA + `' AND valid_to IS NULL`,
		`INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised, valid_from) VALUES ('` + shopID + `', '` + vehicleA + `', 'XYZ 99A', 'XYZ99A', clock_timestamp())`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	second, err := v.book(pool, in2031(20, 9), in2031(21, 9))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.AddRentalDriver(ctx, pool, rentalDesk(), second, v.renter, true)
	if _, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), d, workshop.LicenceShown{Shown: true, Categories: []string{"B"},
		ValidUntil: day("2035-01-01"), Method: workshop.LicenceCardInspected}); err != nil {
		t.Fatal(err)
	}
	out2, err := workshop.StartHandover(ctx, pool, rentalDesk(), second, workshop.HandoverOut)
	if err != nil {
		t.Fatal(err)
	}
	recordFound(t, pool, out2, nil)
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out2, workshop.HandoverReading{OdometerKm: km64(1200), RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	third, err := v.book(pool, in2031(25, 9), in2031(26, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelRentalBooking(ctx, pool, rentalDesk(), third, "Ångrade sig", false); err != nil {
		t.Fatal(err)
	}

	register, err := workshop.RentalRegister(ctx, pool, rentalOwner(), workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0)}, "Transportstyrelsens tillsyn")
	if err != nil || len(register) != 3 {
		t.Fatalf("the register: %+v, %v", register, err)
	}
	a, b, c := register[0], register[1], register[2]
	if a.BookingID != first || a.Registration != "ABC 12D" || a.Out == nil || a.In == nil || a.Open() || !a.Ends.Equal(in2031(13, 9)) || len(a.Drivers) != 2 || a.State != workshop.RentalReturned {
		t.Errorf("the first rental: %+v", a)
	}
	if b.BookingID != second || b.Registration != "XYZ 99A" || !b.Open() || b.State != workshop.RentalCheckedOut {
		t.Errorf("the second rental, out: %+v", b)
	}
	if c.BookingID != third || c.Out != nil || c.State != workshop.RentalCancelled {
		t.Errorf("the cancelled rental: %+v", c)
	}
	if one, _ := workshop.RentalRegister(ctx, pool, rentalOwner(), workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0), Registration: "abc12d"}, "Polisen"); len(one) != 1 || one[0].BookingID != first {
		t.Errorf("by the old plate: %+v", one)
	}

	who, err := workshop.WhoHadRegistration(ctx, pool, rentalOwner(), "ABC 12D", whileOut, "Felparkering")
	if err != nil || who == nil || who.BookingID != first {
		t.Errorf("who had ABC 12D while the first rental was out: %+v, %v", who, err)
	}
	if who, err := workshop.WhoHadRegistration(ctx, pool, rentalOwner(), "XYZ 99A", time.Now(), "Trängselskatt"); err != nil || who == nil || who.BookingID != second {
		t.Errorf("who has XYZ 99A now: %+v, %v", who, err)
	}
	if who, err := workshop.WhoHadRegistration(ctx, pool, rentalOwner(), "ABC 12D", time.Now(), "Trängselskatt"); err != nil || who != nil {
		t.Errorf("the old plate now: %+v, %v", who, err)
	}

	// Only the owner reads it, never without a purpose, and each read is
	// recorded with its purpose.
	if _, err := workshop.RentalRegister(ctx, pool, rentalDesk(), workshop.RegisterFilter{From: time.Now(), To: in2031(31, 0)}, "x"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk reading the register: %v", err)
	}
	if _, err := workshop.RentalRegister(ctx, pool, rentalOwner(), workshop.RegisterFilter{From: time.Now(), To: in2031(31, 0)}, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("read with no purpose: %v", err)
	}
	events, err := workshop.AuditTrail(ctx, pool, rentalOwner(), "shop", shopID)
	reads := 0
	for _, e := range events {
		if e.Event == "rental_register.read" && e.Reason != "" && e.Actor != "" {
			reads++
		}
	}
	if err != nil || reads != 5 {
		t.Errorf("%d reads recorded, want 5: %v", reads, err)
	}
	// Switched off, the register is still there for the owner.
	off := rentalOwner()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.RentalRegister(ctx, pool, off, workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0)}, "Tillsyn"); err != nil {
		t.Errorf("the register with Rental off: %v", err)
	}
}

// How long the register is kept is the shop's decision: missing until it
// is made, and only whoever runs the shop makes it.
func TestTheRegistersRetentionIsMissingUntilSet(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if r, err := workshop.RentalRegisterRetention(ctx, pool, rentalDesk()); err != nil || r != nil {
		t.Errorf("before any decision: %+v, %v", r, err)
	}
	if err := workshop.SetRentalRegisterRetention(ctx, pool, rentalDesk(), 24, "Råd från jurist"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk deciding: %v", err)
	}
	if err := workshop.SetRentalRegisterRetention(ctx, pool, rentalOwner(), 0, "x"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("zero months: %v", err)
	}
	if err := workshop.SetRentalRegisterRetention(ctx, pool, rentalOwner(), 24, "Råd från jurist"); err != nil {
		t.Fatal(err)
	}
	if r, err := workshop.RentalRegisterRetention(ctx, pool, rentalDesk()); err != nil || r == nil || r.Months != 24 {
		t.Errorf("after the decision: %+v, %v", r, err)
	}
}
