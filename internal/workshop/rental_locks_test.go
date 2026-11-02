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
