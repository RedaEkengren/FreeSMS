package workshop_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Congestion tax, parking fees and tolls matched to the rental (#198).

// rentedAcrossTheClockChange is a rental of GHI 56J out at noon on 24
// October 2026 and back at noon on the 26th -- across the night summer
// time ends -- by the handovers' own times, with the plate registered
// since 2020. It is the booking.
func rentedAcrossTheClockChange(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	c := newDamageOn(t, pool, "GHI 56J", "V3")
	dc, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	if err != nil {
		t.Fatal(err)
	}
	out := time.Date(2026, 10, 24, 12, 0, 0, 0, stockholm)
	in := time.Date(2026, 10, 26, 12, 0, 0, 0, stockholm)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE rental_handovers SET at = $2 WHERE booking_id = $1 AND kind = 'check_out'`, []any{dc.BookingID, out}},
		{`UPDATE rental_handovers SET at = $2 WHERE booking_id = $1 AND kind = 'check_in'`, []any{dc.BookingID, in}},
		{`UPDATE vehicle_registrations SET valid_from = '2020-01-01' WHERE normalised = $1`, []any{workshop.NormaliseRegistration("GHI 56J")}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return dc.BookingID
}

func fineAt(plate, ref string, at time.Time) workshop.NewFine {
	return workshop.NewFine{Kind: "congestion_tax", Registration: plate, OccurredAt: at, AmountMinor: 4_500,
		Issuer: "Transportstyrelsen", Reference: ref}
}

// fineNamed is the recorded fine with the reference.
func fineNamed(t *testing.T, pool *pgxpool.Pool, ref string) workshop.Fine {
	t.Helper()
	fines, err := workshop.Fines(context.Background(), pool, rentalDesk())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(fines, func(f workshop.Fine) bool { return f.Reference == ref })
	if i < 0 {
		t.Fatalf("no fine %s in %+v", ref, fines)
	}
	return fines[i]
}

// A charge is matched by the moment against the handovers' actual times
// and the registration valid then: inside the rental across the night the
// clocks go back, a minute after the check-in too near to say, an hour
// after the shop's own, and a plate that had moved on to nothing.
func TestAFineIsMatchedByTheHandoversAndThePlateThen(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	booking := rentedAcrossTheClockChange(t, pool)
	in := time.Date(2026, 10, 26, 12, 0, 0, 0, stockholm)
	// The file says 02:30 on the night summer time ends: that hour happens
	// twice, so the line is refused until it says which; with its offset,
	// the first 02:30, 00:30 UTC, is inside the rental.
	head := "kind,registration,when,amount,issuer,reference,due\n"
	if _, err := workshop.ParseFinesCSV(strings.NewReader(head+"congestion_tax,GHI 56 J,2026-10-25 02:30,45,Transportstyrelsen,DST-1,\n"), stockholm); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an hour that happens twice read without its offset: %v", err)
	}
	parsed, err := workshop.ParseFinesCSV(strings.NewReader(head+"congestion_tax,GHI 56 J,2026-10-25T02:30:00+02:00,45,Transportstyrelsen,DST-1,2026-11-30\n"), stockholm)
	if err != nil || len(parsed) != 1 || !parsed[0].OccurredAt.Equal(time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)) || parsed[0].AmountMinor != 4_500 {
		t.Fatalf("the file read as %+v, %v", parsed, err)
	}
	fines := append(parsed,
		fineAt("GHI56J", "AFTER-1M", in.Add(time.Minute)),
		fineAt("GHI56J", "AFTER-2H", in.Add(2*time.Hour)),
		fineAt("XYZ 999", "NOBODY", in.Add(-time.Hour)))
	if n, err := workshop.RecordFines(ctx, pool, rentalDesk(), "imported", fines); err != nil || n != 4 {
		t.Fatalf("recorded %d, %v", n, err)
	}
	if n, err := workshop.RecordFines(ctx, pool, rentalDesk(), "imported", fines); err != nil || n != 0 {
		t.Errorf("the same file again recorded %d, %v; want none", n, err)
	}
	for ref, want := range map[string]string{"DST-1": workshop.FineMatched, "AFTER-1M": workshop.FineAmbiguous,
		"AFTER-2H": workshop.FineOwn, "NOBODY": workshop.FineUnknownUnit} {
		if f := fineNamed(t, pool, ref); f.Match != want {
			t.Errorf("%s: %s, want %s (candidates %v)", ref, f.Match, want, f.Candidates)
		}
	}
	if f := fineNamed(t, pool, "DST-1"); len(f.Candidates) != 1 || f.Candidates[0] != booking {
		t.Errorf("the rental matched: %v, want %s", f.Candidates, booking)
	}

	// The plate moves to another number at noon on the 25th: the old one
	// then names nothing, the new one the same car.
	change := time.Date(2026, 10, 25, 12, 0, 0, 0, stockholm)
	var vehicle string
	if err := pool.QueryRow(ctx, `UPDATE vehicle_registrations SET valid_to = $2 WHERE normalised = $1 RETURNING vehicle_id::text`,
		workshop.NormaliseRegistration("GHI 56J"), change).Scan(&vehicle); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised, valid_from) VALUES ($1, $2, 'NEW 11A', $3, $4)`,
		shopID, vehicle, workshop.NormaliseRegistration("NEW 11A"), change); err != nil {
		t.Fatal(err)
	}
	later := change.Add(time.Hour)
	if _, err := workshop.RecordFines(ctx, pool, rentalDesk(), "typed", []workshop.NewFine{fineAt("GHI 56J", "OLD-PLATE", later), fineAt("NEW 11A", "NEW-PLATE", later)}); err != nil {
		t.Fatal(err)
	}
	if f := fineNamed(t, pool, "OLD-PLATE"); f.Match != workshop.FineUnknownUnit {
		t.Errorf("the old plate after it moved on: %s", f.Match)
	}
	if f := fineNamed(t, pool, "NEW-PLATE"); f.Match != workshop.FineMatched {
		t.Errorf("the new plate on the rented car: %s", f.Match)
	}
}

// A person passes a fine to a rental that had the car -- choosing among
// those an ambiguous one could be, never another -- and it becomes a
// return charge at what the authority charged; one due soon is on the
// rental desk's queue until the shop has paid it.
func TestAFineIsPassedOnlyToARentalThatHadTheCar(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	booking := rentedAcrossTheClockChange(t, pool)
	// Another rental, returned too, that never had this car.
	otherCase, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), newDamageOn(t, pool, "DEF 34G", "V2"))
	if err != nil {
		t.Fatal(err)
	}
	other := otherCase.BookingID
	in := time.Date(2026, 10, 26, 12, 0, 0, 0, stockholm)
	due := time.Now().AddDate(0, 0, 3)
	near := fineAt("GHI 56J", "NEAR", in.Add(time.Minute))
	near.DueOn = &due
	if _, err := workshop.RecordFines(ctx, pool, rentalDesk(), "typed", []workshop.NewFine{near}); err != nil {
		t.Fatal(err)
	}
	f := fineNamed(t, pool, "NEAR")
	if _, err := workshop.PassFineToRenter(ctx, pool, rentalDesk(), f.ID, other); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("passed to a rental that never had the car: %v", err)
	}
	charge, err := workshop.PassFineToRenter(ctx, pool, rentalDesk(), f.ID, booking)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.PassFineToRenter(ctx, pool, rentalDesk(), f.ID, booking); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("passed twice: %v", err)
	}
	charges, err := workshop.RentalCharges(ctx, pool, rentalDesk(), booking)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(charges, func(c workshop.RentalCharge) bool { return c.ID == charge })
	if i < 0 || charges[i].Kind != workshop.ChargeCongestionTax || charges[i].Net() != 4_500 || charges[i].InformedAt != nil {
		t.Errorf("the charge passed on: %+v", charges)
	}
	due2 := func() bool {
		items, err := workshop.WaitingFor(ctx, pool, rentalDesk())
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(items, func(it workshop.ActionItem) bool { return it.Kind == "fine_due" && it.Link() == "/rental/fines" })
	}
	if !due2() {
		t.Errorf("a charge due in three days is not on the queue")
	}
	if err := workshop.FinePaid(ctx, pool, rentalDesk(), f.ID); err != nil {
		t.Fatal(err)
	}
	if due2() {
		t.Errorf("a charge paid is still on the queue")
	}
}

// rentalTransitionsAudited makes each of Rental's transitions that the
// trail reads from its own table (#151): a car out and back, a deposit
// asked for and a hold taken and let go, a charge confirmed, told and
// withdrawn, and fines passed on, kept and paid.
func rentalTransitionsAudited(t *testing.T, pool *pgxpool.Pool, made func(event, kind, id string)) {
	t.Helper()
	ctx := context.Background()
	dc, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), newDamageOn(t, pool, "AUD 151", "V151"))
	if err != nil {
		t.Fatal(err)
	}
	booking := dc.BookingID
	made("rental.handed_out", "rental_booking", booking)
	made("rental.returned", "rental_booking", booking)

	if err := workshop.RequireRentalDeposit(ctx, pool, rentalDesk(), booking, "", 300_000, "card_hold"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.HoldRentalDeposit(ctx, pool, rentalDesk(), booking, 300_000, "T-151", time.Now().AddDate(0, 0, 7)); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReleaseRentalDepositHold(ctx, pool, rentalDesk(), booking, "Tillbaka utan anmärkning"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"rental_deposit.required", "rental_deposit.held", "rental_deposit.hold_released"} {
		made(e, "rental_booking", booking)
	}

	charge, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeCleaning,
		Description: "Städning", QuantityMilli: 1000, UnitPriceMinor: 40_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.InformCharge(ctx, pool, rentalDesk(), charge, "sms"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.WithdrawCharge(ctx, pool, rentalDesk(), charge, "Kunden hade städat"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"rental_charge.confirmed", "rental_charge.informed", "rental_charge.withdrawn"} {
		made(e, "rental_booking", booking)
	}

	// The rental had the car from three hours ago to one; the plate was
	// its long before.
	now := time.Now()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE rental_handovers SET at = $2 WHERE booking_id = $1 AND kind = 'check_out'`, []any{booking, now.Add(-3 * time.Hour)}},
		{`UPDATE rental_handovers SET at = $2 WHERE booking_id = $1 AND kind = 'check_in'`, []any{booking, now.Add(-time.Hour)}},
		{`UPDATE vehicle_registrations SET valid_from = '2020-01-01' WHERE normalised = $1`, []any{workshop.NormaliseRegistration("AUD 151")}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := workshop.RecordFines(ctx, pool, rentalDesk(), "typed", []workshop.NewFine{
		fineAt("AUD 151", "AUD-1", now.Add(-2*time.Hour)), fineAt("AUD 151", "AUD-2", now.Add(-2*time.Hour))}); err != nil {
		t.Fatal(err)
	}
	passed, kept := fineNamed(t, pool, "AUD-1"), fineNamed(t, pool, "AUD-2")
	if _, err := workshop.PassFineToRenter(ctx, pool, rentalDesk(), passed.ID, booking); err != nil {
		t.Fatal(err)
	}
	if err := workshop.KeepFine(ctx, pool, rentalDesk(), kept.ID, "Vår egen körning"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.FinePaid(ctx, pool, rentalDesk(), passed.ID); err != nil {
		t.Fatal(err)
	}
	made("fine.passed_to_renter", "vehicle_fine", passed.ID)
	made("fine.kept", "vehicle_fine", kept.ID)
	made("fine.paid", "vehicle_fine", passed.ID)
}
