package workshop_test

import (
	"context"
	"errors"
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

	job, already, err := workshop.TakeInBooking(ctx, pool, advisor(), id, "BKG 123", "Service", &km, workshop.Details{})
	if err != nil || already {
		t.Fatalf("first intake: %q, %v, %v", job, already, err)
	}
	b, _, _ := workshop.BookingByID(ctx, pool, advisor(), id)
	if b.Status != "arrived" || b.WorkOrderID != job {
		t.Errorf("the booking after intake: %+v", b)
	}
	again, already, err := workshop.TakeInBooking(ctx, pool, advisor(), id, "BKG 123", "Service", &km, workshop.Details{})
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
			got[i], _, errs[i] = workshop.TakeInBooking(context.Background(), pool, advisor(), id,
				"BKG 123", "Service", nil, workshop.Details{})
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
		if _, _, err := workshop.TakeInBooking(ctx, pool, advisor(), c.id, "BKG 123", "Service", nil, workshop.Details{}); !errors.Is(err, c.want) {
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
	if _, _, err := workshop.TakeInBooking(ctx, pool, advisor(), id, "BKG 123", "Service", &impossible, workshop.Details{}); err == nil {
		t.Fatal("an impossible reading was accepted")
	}
	if jobs, presence, odometer := counts(t, pool); jobs != 0 || presence != 0 || odometer != 0 {
		t.Errorf("a failed intake left %d jobs, %d presence rows, %d readings", jobs, presence, odometer)
	}
	if b, _, _ := workshop.BookingByID(ctx, pool, advisor(), id); b.Status != "booked" {
		t.Errorf("a failed intake left the booking %q", b.Status)
	}
}
