package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A dent found at return becomes a job on the same car in one step, for the
// shop's own customer, with the description as its complaint and the case and
// the job linked. It is not the renter's bill.
func TestADamageBecomesAJobOnTheSameCarForTheShopItself(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamage(t, pool)

	if _, err := workshop.CreateDamageJob(ctx, pool, technician(), c); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician opened a job from a damage case: %v", err)
	}
	if _, err := workshop.CreateDamageJob(ctx, pool, rentalDesk(), "11111111-1111-1111-1111-111111111111"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a case that is not there: %v", err)
	}
	// While a rental still holds the car then, the ledger's own sentence refuses
	// the job and nothing is made: replanning is for whoever runs the shop.
	if _, err := workshop.CreateDamageJob(ctx, pool, rentalDesk(), c); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Fatalf("a job on a car a rental holds: %v", err)
	}
	if jobs, _ := workshop.DamageJobsOf(ctx, pool, rentalDesk(), c); len(jobs) != 0 {
		t.Fatalf("a refused job left %d rows", len(jobs))
	}
	if _, err := pool.Exec(ctx, `UPDATE resource_blocks SET status = 'released', ended_at = clock_timestamp() WHERE kind = 'rental'`); err != nil {
		t.Fatal(err)
	}
	job, err := workshop.CreateDamageJob(ctx, pool, rentalDesk(), c)
	if err != nil {
		t.Fatal(err)
	}
	var complaint, customer, vehicle, caseVehicle string
	var own string
	if err := pool.QueryRow(ctx, `SELECT complaint, coalesce(customer_id::text, ''), vehicle_id::text FROM work_orders WHERE id = $1`, job).Scan(&complaint, &customer, &vehicle); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT customer_id::text FROM shop_own_customers`).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT vehicle_id::text FROM damage_cases WHERE id = $1`, c).Scan(&caseVehicle); err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	if complaint != d.Description || customer != own || vehicle != caseVehicle {
		t.Errorf("the job: complaint %q, customer %q (own %q), car %q (case %q)", complaint, customer, own, vehicle, caseVehicle)
	}
	jobs, err := workshop.DamageJobsOf(ctx, pool, rentalDesk(), c)
	if err != nil || len(jobs) != 1 || jobs[0].WorkOrderID != job || jobs[0].State != "draft" {
		t.Errorf("the case's jobs: %+v, %v", jobs, err)
	}
	// The renter was not charged by this: the case still has no amount settled.
	if d.Settled() != nil || d.Chargeable() {
		t.Errorf("the case was settled by opening a job: %+v", d.Steps)
	}
	// Repaired once the job is ready: the register no longer carries the damage,
	// and the case with its history is still there.
	var booking string
	if err := pool.QueryRow(ctx, `SELECT booking_id::text FROM damage_cases WHERE id = $1`, c).Scan(&booking); err != nil {
		t.Fatal(err)
	}
	open := func() int {
		v, err := workshop.RentalHandovers(ctx, pool, rentalDesk(), booking)
		if err != nil {
			t.Fatal(err)
		}
		return len(v.Damage)
	}
	rows := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM damage_cases WHERE id = $1`, c).Scan(&n)
		return n
	}
	if open() != 1 {
		t.Errorf("before the repair the register holds %d", open())
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE work_orders DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE work_orders SET state = 'ready' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE work_orders ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if open() != 0 || rows() != 1 {
		t.Errorf("after the repair the register holds %d and the case rows are %d", open(), rows())
	}
	// With the workshop switched off the action is not there.
	off := rentalDesk()
	off.Modules = []access.Module{access.Rental}
	if _, err := workshop.CreateDamageJob(ctx, pool, off, c); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a job with the workshop switched off: %v", err)
	}
}
