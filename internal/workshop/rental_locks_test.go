package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every write to a unit, rental or Core, takes the unit's resources row
// first. These tests put two writes into the order that deadlocks when one
// of them does not, by holding the row in a transaction of their own the
// way a rental write holds it, and watching who waits for it.

// holdTheUnit opens a transaction holding a resource's row, as every write
// to it takes it first. The caller commits or rolls it back.
func holdTheUnit(t *testing.T, pool *pgxpool.Pool, resource string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_shop', $1, true)`, shopID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM resources WHERE id = $1 FOR UPDATE`, resource); err != nil {
		t.Fatal(err)
	}
	return tx
}

// pending is an operation running on its own connection.
type pending chan error

func start(fn func() error) pending {
	p := make(pending, 1)
	go func() { p <- fn() }()
	return p
}

func (p pending) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-p:
		return err
	case <-time.After(60 * time.Second):
		t.Fatal("it never finished")
		return nil
	}
}

// waitForWaiters waits until n connections to this test's database are
// waiting for a lock. It fails the test if p finishes first: it did not
// wait for what is held.
func waitForWaiters(t *testing.T, pool *pgxpool.Pool, n int, p pending, what string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if len(p) > 0 {
			t.Fatalf("%s did not wait for the unit: %v", what, <-p)
		}
		var waiting int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never waited for the unit", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// aDriverOnABooking is a booking on a van with its renter named to drive.
func aDriverOnABooking(t *testing.T, pool *pgxpool.Pool) (rentable, string, string) {
	t.Helper()
	v := rentableVan(t, pool)
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := workshop.AddRentalDriver(context.Background(), pool, rentalDesk(), id, v.renter, true)
	if err != nil {
		t.Fatal(err)
	}
	return v, id, driver
}

func licenceShown() workshop.LicenceShown {
	return workshop.LicenceShown{Shown: true, Categories: []string{"B"}, ValidUntil: day("2035-01-01"), Method: workshop.LicenceCardInspected}
}

// A driver taken off while their licence is being checked: the check, in
// line for the unit first, goes through, and the removal after it. Neither
// is lost to a deadlock (review 3 #1).
func TestRemovingADriverWhileTheirLicenceIsCheckedDoesNotDeadlock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v, _, driver := aDriverOnABooking(t, pool)
	held := holdTheUnit(t, pool, v.resource)
	check := start(func() error {
		_, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, licenceShown())
		return err
	})
	waitForWaiters(t, pool, 1, check, "the licence check")
	remove := start(func() error { return workshop.RemoveRentalDriver(ctx, pool, rentalDesk(), driver, "Kör inte") })
	waitForWaiters(t, pool, 2, remove, "the removal")
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := check.result(t); err != nil {
		t.Errorf("the licence check: %v", err)
	}
	if err := remove.result(t); err != nil {
		t.Errorf("the removal: %v", err)
	}
}

// A licence check waiting for the unit while the driver is taken off is
// refused once it has the unit: it reads the driver again, and records
// nothing for somebody no longer driving (review 3 #3).
func TestALicenceCheckWaitingBehindARemovalFindsTheDriverGone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v, _, driver := aDriverOnABooking(t, pool)
	held := holdTheUnit(t, pool, v.resource)
	remove := start(func() error { return workshop.RemoveRentalDriver(ctx, pool, rentalDesk(), driver, "Kör inte") })
	waitForWaiters(t, pool, 1, remove, "the removal")
	check := start(func() error {
		_, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, licenceShown())
		return err
	})
	waitForWaiters(t, pool, 2, check, "the licence check")
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := remove.result(t); err != nil {
		t.Fatalf("the removal: %v", err)
	}
	if err := check.result(t); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a licence check for a driver taken off meanwhile: %v", err)
	}
	var checks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rental_licence_checks WHERE driver_id = $1`, driver).Scan(&checks); err != nil || checks != 0 {
		t.Errorf("%d checks kept for a driver taken off (%v)", checks, err)
	}
}

// A unit with a rental hold on it that has run out.
func aRunOutHold(t *testing.T, pool *pgxpool.Pool) rentable {
	t.Helper()
	ctx := context.Background()
	v := rentableVan(t, pool)
	until := time.Now().Add(time.Hour)
	held, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
		From: in2031(10, 9), To: in2031(11, 9), HoldUntil: &until})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE resource_blocks SET expires_at = now() - interval '1 minute' WHERE source_id = $1`, held); err != nil {
		t.Fatal(err)
	}
	return v
}

// A service booked over a rental hold that has run out, while a rental
// write holding the unit lets the hold go: the booking waits for the unit
// before it touches the hold, so the two do not wait on each other
// (review 3 #2).
func TestBookingAResourceBesideARentalWriteDoesNotDeadlock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := aRunOutHold(t, pool)
	held := holdTheUnit(t, pool, v.resource)
	book := start(func() error {
		_, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{ResourceID: v.resource,
			Kind: workshop.BlockService, From: in2031(10, 12), To: in2031(10, 14)})
		return err
	})
	waitForWaiters(t, pool, 1, book, "the booking")
	// What a rental write does next, holding the unit.
	if _, err := held.Exec(ctx, `SELECT release_expired_holds($1, tstzrange(NULL, NULL))`, v.resource); err != nil {
		t.Fatalf("the rental write: %v", err)
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := book.result(t); err != nil {
		t.Errorf("the booking: %v", err)
	}
}

// Confirming, letting go and replanning a block wait for its unit, as
// booking it does: one order for every write to a unit (review 3 #2).
func TestChangingABlockWaitsForItsUnit(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	block := func(day int) string {
		t.Helper()
		until := time.Now().Add(time.Hour)
		id, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{ResourceID: v.resource,
			Kind: workshop.BlockService, From: in2031(day, 9), To: in2031(day, 12), HoldUntil: &until})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	confirm, release, override := block(10), block(11), block(12)
	for _, c := range []struct {
		what string
		do   func() error
	}{
		{"confirming", func() error { return workshop.ConfirmBlock(ctx, pool, rentalDesk(), confirm) }},
		{"letting go", func() error { return workshop.ReleaseBlock(ctx, pool, rentalDesk(), release) }},
		{"replanning", func() error {
			return workshop.OverrideBlock(ctx, pool, rentalOwner(), override, in2031(13, 9), in2031(13, 12), "Verkstaden flyttade tiden")
		}},
	} {
		held := holdTheUnit(t, pool, v.resource)
		p := start(c.do)
		waitForWaiters(t, pool, 1, p, c.what)
		if err := held.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := p.result(t); err != nil {
			t.Errorf("%s: %v", c.what, err)
		}
	}
}

// A unit's time in the fleet starts with the unit held, as a booking on it
// is made: the booking reads the fleet as the start left it, not halfway
// (review 3 #8).
func TestStartingAFleetPeriodWaitsForTheUnit(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Small van"})
	u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), unit)
	if err != nil {
		t.Fatal(err)
	}
	held := holdTheUnit(t, pool, u.ResourceID)
	join := start(func() error {
		_, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().AddDate(-1, 0, 0), "bought", "")
		return err
	})
	waitForWaiters(t, pool, 1, join, "starting the fleet period")
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := join.result(t); err != nil {
		t.Errorf("starting the fleet period: %v", err)
	}
}

// Taking a rental's unit is one statement, not a read and then a lock:
// every rental write pays for it. Confirming a hold and ending a fleet
// period are each counted whole -- the transaction, the scope, the lock,
// the work (review 3 #9).
func TestTakingARentalsUnitIsOneRoundTrip(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	until := time.Now().Add(time.Hour)
	held, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
		From: in2031(10, 9), To: in2031(11, 9), HoldUntil: &until})
	if err != nil {
		t.Fatal(err)
	}
	other := addUnit(t, pool, workshop.NewRentalUnit{Registration: "XYZ 99A", FleetNumber: "V2", Category: "Small van"})
	membership, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), other, time.Now().AddDate(-1, 0, 0), "bought", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	counter := &countingTracer{}
	cfg.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer counted.Close()
	for _, c := range []struct {
		what   string
		budget int64
		do     func() error
	}{
		{"confirming a hold", 8, func() error { return workshop.ConfirmRentalBooking(ctx, counted, rentalDesk(), held) }},
		{"ending a fleet period", 9, func() error {
			return workshop.EndFleetMembership(ctx, counted, rentalDesk(), membership, time.Now(), "sold", "")
		}},
	} {
		before := counter.n.Load()
		if err := c.do(); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if n := counter.n.Load() - before; n > c.budget {
			t.Errorf("%s: %d statements, over its %d", c.what, n, c.budget)
		} else {
			t.Logf("%s: %d statements", c.what, n)
		}
	}
}
