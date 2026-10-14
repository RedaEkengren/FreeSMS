package workshop_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A booking becomes one job. Taking it in twice -- a second tab, a second
// person, a replay after the answer was lost -- is the same job, never a new
// one.

func booked(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := workshop.CreateBooking(context.Background(), pool, advisor(), workshop.NewBooking{
		Starts: at("2026-10-15", 8, 0), Ends: at("2026-10-15", 9, 0), What: "Service", Registration: "BKG 123"})
	if err != nil {
		t.Fatalf("CreateBooking: %v", err)
	}
	return id
}

// counts is how many of each thing taking a car in writes.
func counts(t *testing.T, pool *pgxpool.Pool) (jobs, presence, odometer int) {
	t.Helper()
	if err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM work_orders), (SELECT count(*) FROM vehicle_presence),
		                               (SELECT count(*) FROM odometer_readings)`).Scan(&jobs, &presence, &odometer)
	}); err != nil {
		t.Fatal(err)
	}
	return
}

func TestABookingTakenInTwiceIsOneJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := booked(t, pool)
	km := int64(18500)

	job, already, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id, Registration: "BKG 123", Complaint: "Service", OdometerKm: &km})
	if err != nil || already {
		t.Fatalf("first intake: %q, %v, %v", job, already, err)
	}
	b, _, _ := workshop.BookingByID(ctx, pool, advisor(), id)
	if b.Status != "arrived" || b.WorkOrderID != job {
		t.Errorf("the booking after intake: %+v", b)
	}
	again, already, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id, Registration: "BKG 123", Complaint: "Service", OdometerKm: &km})
	if err != nil || !already || again != job {
		t.Errorf("second intake: %q (already %v), %v; want the first job %q", again, already, err, job)
	}
	if jobs, presence, odometer := counts(t, pool); jobs != 1 || presence != 1 || odometer != 1 {
		t.Errorf("after two intakes: %d jobs, %d presence rows, %d readings; want one of each", jobs, presence, odometer)
	}
}

// Several people at once still make one job.
func TestABookingTakenInByManyAtOnceIsOneJob(t *testing.T) {
	pool := setup(t)
	id := booked(t, pool)
	var wg sync.WaitGroup
	got := make([]string, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _, errs[i] = workshop.TakeInBooking(context.Background(), pool, advisor(),
				workshop.BookingIntake{BookingID: id, Registration: "BKG 123", Complaint: "Service"})
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] {
			t.Errorf("intake %d: %q, %v; want %q", i, got[i], errs[i], got[0])
		}
	}
	if jobs, _, _ := counts(t, pool); jobs != 1 {
		t.Errorf("%d jobs from one booking", jobs)
	}
}

// A booking that ended while the form was open, or one that is not there,
// is said so; nothing is created on its behalf.
func TestABookingThatIsNotThereOrHasEndedMakesNoJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	cancelled, noShow := booked(t, pool), booked(t, pool)
	if err := workshop.CancelBooking(ctx, pool, advisor(), cancelled, "Kunden ringde"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.BookingNoShow(ctx, pool, advisor(), noShow); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   string
		want error
	}{
		{cancelled, workshop.ErrBookingEnded},
		{noShow, workshop.ErrBookingEnded},
		{"not-a-booking", workshop.ErrNotFound},
		{"dddddddd-0000-0000-0000-0000000000b0", workshop.ErrNotFound}, // none, or another shop's
	} {
		if _, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: c.id, Registration: "BKG 123", Complaint: "Service"}); !errors.Is(err, c.want) {
			t.Errorf("booking %s: %v, want %v", c.id, err, c.want)
		}
	}
	if jobs, presence, _ := counts(t, pool); jobs != 0 || presence != 0 {
		t.Errorf("%d jobs and %d presence rows from bookings that could not be taken in", jobs, presence)
	}
}

// A failure part way leaves nothing behind: no job, no car left with us,
// and the booking still waiting.
func TestAnIntakeThatFailsLeavesTheBookingAsItWas(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := booked(t, pool)
	impossible := int64(-1) // refused by the database, after the job is written
	if _, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id, Registration: "BKG 123", Complaint: "Service", OdometerKm: &impossible}); err == nil {
		t.Fatal("an impossible reading was accepted")
	}
	if jobs, presence, odometer := counts(t, pool); jobs != 0 || presence != 0 || odometer != 0 {
		t.Errorf("a failed intake left %d jobs, %d presence rows, %d readings", jobs, presence, odometer)
	}
	if b, _, _ := workshop.BookingByID(ctx, pool, advisor(), id); b.Status != "booked" {
		t.Errorf("a failed intake left the booking %q", b.Status)
	}
}

// What the booking knew goes onto the job: who is to be paid and rung, and
// who was to do the work -- confirmed by the desk, never guessed.

func bookedFor(t *testing.T, pool *pgxpool.Pool, reg, name, phone, tech string) string {
	t.Helper()
	id, err := workshop.CreateBooking(context.Background(), pool, advisor(), workshop.NewBooking{
		Starts: at("2026-10-15", 8, 0), Ends: at("2026-10-15", 9, 0), What: "Service", Registration: reg,
		CustomerName: name, CustomerPhone: phone, TechnicianID: tech})
	if err != nil {
		t.Fatalf("CreateBooking: %v", err)
	}
	return id
}

type jobFacts struct {
	Customer, Phone, Technician string
	Owner                       int
}

func factsOf(t *testing.T, pool *pgxpool.Pool, job string) jobFacts {
	t.Helper()
	var f jobFacts
	if err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT coalesce(p.display_name, ''), coalesce(p.phone, ''), coalesce(w.assigned_to::text, ''),
			       (SELECT count(*) FROM vehicle_ownership o WHERE o.vehicle_id = w.vehicle_id AND o.owned_to IS NULL)
			FROM work_orders w LEFT JOIN customers c ON c.id = w.customer_id LEFT JOIN people p ON p.id = c.person_id
			WHERE w.id = $1`, job).Scan(&f.Customer, &f.Phone, &f.Technician, &f.Owner)
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTheBookingsCustomerAndTechnicianComeWithTheCar(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	id := bookedFor(t, pool, "NEW 111", "Bo Kund", "070-111 22 33", techUserID)
	job, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id,
		Registration: "NEW 111", Complaint: "Service", Customer: "new", TechnicianID: techUserID})
	if err != nil {
		t.Fatal(err)
	}
	f := factsOf(t, pool, job)
	if f.Customer != "Bo Kund" || f.Phone != "070-111 22 33" || f.Technician != techUserID {
		t.Errorf("the job after intake: %+v", f)
	}
	// Payer and owner are two facts: a customer made from a booking owns
	// nothing unless the desk says so.
	if f.Owner != 0 {
		t.Errorf("the booking's contact was made the car's owner without being asked: %+v", f)
	}

	id2 := bookedFor(t, pool, "NEW 222", "Cia Kund", "070-999 88 77", "")
	job2, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id2,
		Registration: "NEW 222", Complaint: "Service", Customer: "new", AlsoOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	if f := factsOf(t, pool, job2); f.Customer != "Cia Kund" || f.Owner != 1 || f.Technician != "" {
		t.Errorf("owner asked for, nobody booked: %+v", f)
	}
}

// Choosing nothing is allowed -- the car is here and the customer is not
// yet known -- and leaves the job showing it has no customer.
func TestABookingTakenInWithoutDecidingWhoPaysLeavesItOpen(t *testing.T) {
	pool := setup(t)
	id := bookedFor(t, pool, "NEW 333", "Dan Kund", "070-123 45 67", "")
	job, _, err := workshop.TakeInBooking(context.Background(), pool, advisor(), workshop.BookingIntake{BookingID: id,
		Registration: "NEW 333", Complaint: "Service"})
	if err != nil {
		t.Fatal(err)
	}
	if f := factsOf(t, pool, job); f.Customer != "" || f.Technician != "" {
		t.Errorf("a customer or technician nobody confirmed: %+v", f)
	}
}

// An existing customer is chosen by the desk, not matched by a name. The
// car's owner stays as it was.
func TestAnExistingCustomerIsChosenAndOwnershipIsLeftAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// The car has an owner already, who is not paying this time.
	firstJob := newJob(t, pool)
	owner, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "Leasing Ägare"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.AssignCustomer(ctx, pool, advisor(), firstJob, owner, true); err != nil {
		t.Fatal(err)
	}
	payer, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "Bo Kund", Phone: "070-111 22 33"})
	if err != nil {
		t.Fatal(err)
	}
	lookalike, _ := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "Bo Kund", Phone: "070-000 00 00"})
	_ = lookalike

	id := bookedFor(t, pool, "ABC 12D", "Bo Kund", "070-111 22 33", "")
	job, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id,
		Registration: "ABC 12D", Complaint: "Service", Customer: "existing", CustomerID: payer})
	if err != nil {
		t.Fatal(err)
	}
	if f := factsOf(t, pool, job); f.Customer != "Bo Kund" || f.Phone != "070-111 22 33" || f.Owner != 1 {
		t.Errorf("the job after intake: %+v", f)
	}
	var owned string
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT customer_id::text FROM vehicle_ownership WHERE owned_to IS NULL`).Scan(&owned)
	})
	if owned != owner {
		t.Errorf("the car's owner became %s, want %s", owned, owner)
	}

	// One that is not ours, or not there, makes no job.
	id2 := bookedFor(t, pool, "ABC 12D", "Bo", "", "")
	for _, bad := range []string{"dddddddd-0000-0000-0000-0000000000c0", "nope", ""} {
		_, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id2,
			Registration: "ABC 12D", Complaint: "Service", Customer: "existing", CustomerID: bad})
		if err == nil {
			t.Errorf("customer %q was accepted", bad)
		}
	}
	if b, _, _ := workshop.BookingByID(ctx, pool, advisor(), id2); b.Status != "booked" {
		t.Errorf("a refused customer left the booking %q", b.Status)
	}
}

// A technician who has left since the booking is not given the job; one the
// desk puts in their place is, and the booking says so.
func TestAReplacedTechnicianIsExplainedOnTheBooking(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	id := bookedFor(t, pool, "NEW 444", "Eva Kund", "", techUserID)
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET active = false, deactivated_at = now() WHERE id = $1`, techUserID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id,
		Registration: "NEW 444", Complaint: "Service", TechnicianID: techUserID})
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("a technician who has left was given the job: %v", err)
	}

	addSecondTechnician(t, pool)
	job, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id,
		Registration: "NEW 444", Complaint: "Service", TechnicianID: secondTechID})
	if err != nil {
		t.Fatal(err)
	}
	if f := factsOf(t, pool, job); f.Technician != secondTechID {
		t.Errorf("the replacement was not given the job: %+v", f)
	}
	b, events, _ := workshop.BookingByID(ctx, pool, advisor(), id)
	last := events[len(events)-1]
	if b.TechnicianID != secondTechID || last.Event != "arrived" || !strings.Contains(last.Note, "Another Technician") || !strings.Contains(last.Note, "A Technician") {
		t.Errorf("the booking after the swap: %+v, last event %+v", b, last)
	}
}

// A replay is the first answer: what was chosen the second time is not
// applied behind it.
func TestAReplayChangesNothingOnTheJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := bookedFor(t, pool, "NEW 555", "Fia Kund", "070-555 55 55", "")
	first, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id,
		Registration: "NEW 555", Complaint: "Service"})
	if err != nil {
		t.Fatal(err)
	}
	again, already, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: id,
		Registration: "NEW 555", Complaint: "Service", Customer: "new"})
	if err != nil || !already || again != first {
		t.Fatalf("replay: %q, %v, %v", again, already, err)
	}
	if f := factsOf(t, pool, first); f.Customer != "" {
		t.Errorf("a replay attached a customer: %+v", f)
	}
}
