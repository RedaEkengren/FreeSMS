package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	if _, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), d, workshop.LicenceShown{Shown: true, AgeOK: true, Categories: []string{"B"},
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
	_ = third
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelRentalBooking(ctx, pool, rentalDesk(), third, "Ångrade sig", false); err != nil {
		t.Fatal(err)
	}

	register, err := workshop.RentalRegister(ctx, pool, rentalOwner(), workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0)}, "Transportstyrelsens tillsyn")
	// Who had it: the cancelled rental, and one booked and not yet out,
	// had nothing (review r6, #5).
	if _, err := v.book(pool, in2031(27, 9), in2031(28, 9)); err != nil {
		t.Fatal(err)
	}
	register, err = workshop.RentalRegister(ctx, pool, rentalOwner(), workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0)}, "Transportstyrelsens tillsyn")
	if err != nil || len(register) != 2 {
		t.Fatalf("the register: %+v, %v", register, err)
	}
	a, b := register[0], register[1]
	if a.BookingID != first || a.Registration != "ABC 12D" || a.Out == nil || a.In == nil || a.Open() || !a.Ends.Equal(in2031(13, 9)) || len(a.Drivers) != 2 || a.State != workshop.RentalReturned {
		t.Errorf("the first rental: %+v", a)
	}
	if b.BookingID != second || b.Registration != "XYZ 99A" || !b.Open() || b.State != workshop.RentalCheckedOut {
		t.Errorf("the second rental, out: %+v", b)
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
	if err != nil || reads != 6 {
		t.Errorf("%d reads recorded, want 6: %v", reads, err)
	}
	// A check-in recorded before its check-out, by a clock set wrong: who
	// had it at the check-out is the same answer as the register gives
	// (review r8, #2).
	var outAt time.Time
	if err := pool.QueryRow(ctx, `SELECT at FROM rental_handovers WHERE booking_id = $1 AND kind = 'check_out'`, first).Scan(&outAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE rental_handovers SET at = $2::timestamptz - interval '1 minute' WHERE booking_id = $1 AND kind = 'check_in'`, first, outAt); err != nil {
		t.Fatal(err)
	}
	if who, err := workshop.WhoHadRegistration(ctx, pool, rentalOwner(), "ABC 12D", outAt, "Felparkering"); err != nil || who == nil || who.BookingID != first {
		t.Errorf("who had ABC 12D at a check-out its check-in is recorded before: %+v, %v", who, err)
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

// The register is read in round trips that do not grow with its rentals:
// their drivers come in one query (review r6, #6).
func TestTheRegisterIsReadInRoundTripsThatDoNotGrow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl, _ := workshop.SaveChecklistTemplate(ctx, pool, owner(), "", "Tält", []string{"Duk"})
	if err := workshop.SetTemplateSubjects(ctx, pool, everything(owner()), tpl, []string{workshop.SubjectCheckOut, workshop.SubjectCheckIn}); err != nil {
		t.Fatal(err)
	}
	equipment := profileFor(t, pool, workshop.ProfileEquipment)
	rules := equipment.Rules
	rules.CheckOutTemplateID, rules.CheckInTemplateID = tpl, tpl
	if _, err := workshop.SaveRentalProfile(ctx, pool, rentalOwner(), equipment.ID, rules); err != nil {
		t.Fatal(err)
	}
	tent := addUnit(t, pool, workshop.NewRentalUnit{Name: "Tält", FleetNumber: "T1", Category: "Tent", ProfileID: equipment.ID})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), tent, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	dayPlan(t, pool, "Tent", 10000, "at_start")
	renter, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Tina", Role: workshop.PartyRenter})
	rent := func(day int) {
		t.Helper()
		id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: tent, RenterID: renter, From: in2031(day, 9), To: in2031(day, 17)})
		if err != nil {
			t.Fatal(err)
		}
		workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, renter, true)
		for _, kind := range []string{workshop.HandoverOut, workshop.HandoverIn} {
			p, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, kind)
			if err != nil {
				t.Fatal(err)
			}
			if err := workshop.FinishHandover(ctx, pool, rentalDesk(), p, workshop.HandoverReading{RenterPresent: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := pool.Config()
	counter := &countingTracer{}
	cfg.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer counted.Close()
	read := func(want int) int64 {
		t.Helper()
		before := counter.n.Load()
		got, err := workshop.RentalRegister(ctx, counted, rentalOwner(), workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0)}, "Tillsyn")
		if err != nil || len(got) != want {
			t.Fatalf("the register: %d rentals, %v; want %d", len(got), err, want)
		}
		return counter.n.Load() - before
	}
	rent(10)
	// A check-in recorded at the check-out's moment, or before it -- a
	// clock set wrong -- still has the rental in the register (review r7, #4).
	for _, shift := range []string{"0", "-1 minute"} {
		if _, err := pool.Exec(ctx, `UPDATE rental_handovers i SET at = o.at + $1::interval FROM rental_handovers o
			WHERE i.kind = 'check_in' AND o.kind = 'check_out' AND o.booking_id = i.booking_id`, shift); err != nil {
			t.Fatal(err)
		}
		if got, err := workshop.RentalRegister(ctx, pool, rentalOwner(), workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0)}, "Tillsyn"); err != nil || len(got) != 1 {
			t.Errorf("back %s after it went out: %d rentals, %v", shift, len(got), err)
		}
	}
	one := read(1)
	rent(11)
	rent(12)
	if three := read(3); three != one {
		t.Errorf("%d round trips for three rentals, %d for one; want the same", three, one)
	}
}
