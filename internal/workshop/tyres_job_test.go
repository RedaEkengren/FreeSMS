package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// measured records a depth on every tyre of a set, as a technician does on the
// way in.
func measured(t *testing.T, pool *pgxpool.Pool, set string) {
	t.Helper()
	ctx := context.Background()
	s, err := workshop.TyreSetByID(ctx, pool, technician(), set)
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range s.Tyres {
		if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), ty.ID, 55, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func aPlace(t *testing.T, pool *pgxpool.Pool, name string, room int) string {
	t.Helper()
	id, err := workshop.CreateStorageLocation(context.Background(), pool, owner(), name, room)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// storedSet is a winter set taken in and put in a place.
func storedSet(t *testing.T, pool *pgxpool.Pool, place string) string {
	t.Helper()
	n := summerSet(customerA, "")
	n.Season, n.Brand = workshop.SeasonWinterFriction, "Nokian"
	id, err := workshop.CreateTyreSet(context.Background(), pool, advisor(), n)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.PlaceTyreSet(context.Background(), pool, technician(), id, place); err != nil {
		t.Fatal(err)
	}
	return id
}

// A seasonal change: the winter set comes out of storage, the summer set comes
// off the car and is measured and put away, the winter set goes on. Nothing is
// moved on paper only: where each set is now is what the rows say.
func TestASeasonalChangeMovesBothSetsAndLeavesTheirTrail(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	rack, shelf := aPlace(t, pool, "Rack", 2), aPlace(t, pool, "Shelf", 2)
	winter := storedSet(t, pool, rack)
	summer, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	if err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)

	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), job, winter); err != nil {
		t.Fatal(err)
	}
	if s, _ := workshop.TyreSetByID(ctx, pool, technician(), winter); s.StoredIn != "" {
		t.Errorf("a set taken out of storage is still in %q", s.StoredIn)
	}
	// The summer set comes off: checked in without being measured is refused.
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, summer, workshop.CheckIn{LocationID: shelf}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set checked in with no tyre measured: %v", err)
	}
	measured(t, pool, summer)
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, summer, workshop.CheckIn{LocationID: shelf}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.FitTyreSet(ctx, pool, technician(), job, winter); err != nil {
		t.Fatal(err)
	}

	w, _ := workshop.TyreSetByID(ctx, pool, technician(), winter)
	s, _ := workshop.TyreSetByID(ctx, pool, technician(), summer)
	if w.VehicleID != vehicleA || w.StoredIn != "" || s.VehicleID != "" || s.StoredIn != "Shelf" {
		t.Errorf("winter: car %q in %q; summer: car %q in %q", w.VehicleID, w.StoredIn, s.VehicleID, s.StoredIn)
	}
	moves, err := workshop.JobTyreMoves(ctx, pool, technician(), job)
	if err != nil || len(moves) != 3 || moves[0].Kind != "checked_out" || moves[1].Kind != "checked_in" || moves[1].Location != "Shelf" || moves[2].Kind != "fitted" {
		t.Fatalf("the job's moves: %+v, %v", moves, err)
	}
	if len(w.Moves) != 1 || w.Moves[0].VehicleID != vehicleA {
		t.Errorf("the winter set's cars: %+v", w.Moves)
	}
	// Nothing says whose they are.
	for _, m := range moves {
		if m.Set == "" {
			t.Errorf("a move with no set: %+v", m)
		}
	}
}

// A set taken out and not fitted goes back to storage, and is then free for
// another job; while one job holds it, no other may take it, however many ask at once.
func TestASetHeldByOneJobCannotBeFittedByAnotherAndGoesBackWhenNotFitted(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	rack := aPlace(t, pool, "Rack", 3)
	winter := storedSet(t, pool, rack)
	first, second := newJob(t, pool), newJob(t, pool)

	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), first, winter); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), second, winter); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set held by one job taken by another: %v", err)
	}
	if err := workshop.FitTyreSet(ctx, pool, technician(), second, winter); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set fitted by a job that does not hold it: %v", err)
	}
	if err := workshop.ReturnTyreSet(ctx, pool, technician(), second, winter, rack); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set returned by a job that does not hold it: %v", err)
	}
	if err := workshop.ReturnTyreSet(ctx, pool, technician(), first, winter, rack); err != nil {
		t.Fatal(err)
	}
	if s, _ := workshop.TyreSetByID(ctx, pool, technician(), winter); s.StoredIn != "Rack" {
		t.Errorf("a set returned is in %q", s.StoredIn)
	}
	if err := workshop.FitTyreSet(ctx, pool, technician(), first, winter); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set fitted after it was returned: %v", err)
	}
	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), second, winter); err != nil {
		t.Errorf("a set that was returned, taken by the next job: %v", err)
	}
}

func TestTwoJobsAskingForTheSameSetAtOnceOneGetsIt(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	rack := aPlace(t, pool, "Rack", 3)
	winter := storedSet(t, pool, rack)
	jobs := []string{newJob(t, pool), newJob(t, pool), newJob(t, pool), newJob(t, pool)}
	results := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = workshop.CheckOutTyreSet(ctx, pool, technician(), j, winter)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("refused for another reason: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d jobs got the set", won)
	}
}

// What a check-in must carry: a place or the word that it was taken home or the
// reason it has none; every tyre measured on this job or the reason it was not;
// and the set must have come off this car or be loose.
func TestACheckInHasAPlaceOrAReasonAndEveryTyreMeasuredOrAReason(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	shelf := aPlace(t, pool, "Shelf", 3)
	job := newJob(t, pool)
	mk := func(vehicle string) string {
		id, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicle))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// The customer's own set, brought in loose: measured, placed, nothing fitted.
	own := mk("")
	measured(t, pool, own)
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, own, workshop.CheckIn{LocationID: shelf}); err != nil {
		t.Errorf("a loose set checked in: %v", err)
	}
	if s, _ := workshop.TyreSetByID(ctx, pool, technician(), own); s.StoredIn != "Shelf" {
		t.Errorf("the customer's own set is in %q", s.StoredIn)
	}

	// Measured but no place and no reason.
	a := mk(vehicleA)
	measured(t, pool, a)
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, a, workshop.CheckIn{}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("no place and no reason: %v", err)
	}
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, a, workshop.CheckIn{LocationID: shelf, TakenHome: true}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a place and taken home: %v", err)
	}
	// Taken home: no place, the car is let go, no storage row.
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, a, workshop.CheckIn{TakenHome: true}); err != nil {
		t.Fatal(err)
	}
	if s, _ := workshop.TyreSetByID(ctx, pool, technician(), a); s.VehicleID != "" || s.StoredIn != "" {
		t.Errorf("a set taken home: car %q in %q", s.VehicleID, s.StoredIn)
	}

	// Not measured: a reason lets it through, and the reason is kept.
	b := mk(vehicleA)
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, b, workshop.CheckIn{LocationID: shelf}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("not measured and no reason: %v", err)
	}
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, b, workshop.CheckIn{LocationID: shelf, Why: "Kunden väntade inte"}); err != nil {
		t.Fatal(err)
	}
	moves, _ := workshop.JobTyreMoves(ctx, pool, technician(), job)
	if last := moves[len(moves)-1]; last.Kind != "checked_in" || last.Why != "Kunden väntade inte" {
		t.Errorf("the last move: %+v", last)
	}

	// A set that is on another car, and a measurement older than the job.
	other := anotherCar(t, pool, customerA)
	c, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, other))
	if err != nil {
		t.Fatal(err)
	}
	measured(t, pool, c)
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, c, workshop.CheckIn{LocationID: shelf}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set from another car: %v", err)
	}
	d := mk(vehicleA)
	measured(t, pool, d)
	if _, err := pool.Exec(ctx, `UPDATE tyre_measurements SET measured_at = measured_at - interval '30 days' WHERE tyre_id IN (SELECT id FROM tyres WHERE tyre_set_id = $1)`, d); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CheckInTyreSet(ctx, pool, technician(), job, d, workshop.CheckIn{LocationID: shelf}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a measurement from before the job: %v", err)
	}
}

// A job that is finished moves no tyres; a set goes only on a car its owner
// has; roles outside the workshop and a shop with the workshop off are refused.
func TestTyreMovesOnAJobAreRefusedWhereTheyShouldBe(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	rack := aPlace(t, pool, "Rack", 3)
	winter := storedSet(t, pool, rack)
	job := newJob(t, pool)

	for _, sc := range []access.Scope{everything(rentalDesk()), everything(sales())} {
		if err := workshop.CheckOutTyreSet(ctx, pool, sc, job, winter); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s checked a set out: %v", sc.Role, err)
		}
		if _, err := workshop.JobTyreMoves(ctx, pool, sc, job); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the moves: %v", sc.Role, err)
		}
	}
	off := technician()
	off.Modules = []access.Module{access.Dealer}
	if err := workshop.CheckOutTyreSet(ctx, pool, off, job, winter); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a set checked out with the workshop off: %v", err)
	}
	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), "nope", winter); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a job that is not an id: %v", err)
	}
	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), "99999999-9999-9999-9999-999999999999", winter); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a job that is not there: %v", err)
	}

	// A set whose owner does not have the job's car is not fitted to it.
	if _, err := pool.Exec(ctx, `UPDATE vehicle_ownership SET owned_to = now() + interval '1 second' WHERE vehicle_id = $1`, vehicleA); err != nil {
		t.Fatal(err)
	}
	var stranger string
	if err := pool.QueryRow(ctx, `INSERT INTO customers (shop_id, kind, company_name) VALUES ($1, 'company', 'Andra AB') RETURNING id`, shopID).Scan(&stranger); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id, owned_from) VALUES ($1, $2, $3, now() + interval '2 seconds')`, shopID, vehicleA, stranger); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), job, winter); err != nil {
		t.Fatal(err)
	}
	// The car is still the first owner's for a second more; fit once it is not.
	if _, err := pool.Exec(ctx, `UPDATE vehicle_ownership SET owned_to = now() - interval '1 second', owned_from = now() - interval '1 hour' WHERE vehicle_id = $1 AND customer_id = $2`, vehicleA, customerA); err != nil {
		t.Fatal(err)
	}
	if err := workshop.FitTyreSet(ctx, pool, technician(), job, winter); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set fitted to a car its owner does not have: %v", err)
	}

	// A finished job moves nothing.
	if _, err := pool.Exec(ctx, `UPDATE work_orders SET state = 'cancelled' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReturnTyreSet(ctx, pool, technician(), job, winter, rack); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set returned on a finished job: %v", err)
	}
}
