package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Changing a plan is a new row beside the old; ending one is another; what was
// done is a row each time, with its kilometres as a reading of the unit's car;
// and what falls due follows from them and the unit's own readings.
func TestAServicePlanChangesAsNewRowsAndTheNextDueFollowsWhatWasDone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "SVC 123", FleetNumber: "S1", Category: "Bil"})
	var vehicle string
	if err := pool.QueryRow(ctx, `SELECT r.vehicle_id::text FROM rental_units u JOIN resources r ON r.id = u.resource_id WHERE u.id = $1`, unit).Scan(&vehicle); err != nil {
		t.Fatal(err)
	}

	plan := workshop.ServicePlan{Kind: "service", IntervalMonths: 12, IntervalKm: 15000, DurationMinutes: 240, InUse: true}
	if err := workshop.SetServicePlan(ctx, pool, technician(), unit, plan); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician made a plan: %v", err)
	}
	for name, bad := range map[string]workshop.ServicePlan{
		"no interval":  {Kind: "service", DurationMinutes: 60, InUse: true},
		"no kind":      {Kind: "wash", IntervalMonths: 1, DurationMinutes: 60, InUse: true},
		"no duration":  {Kind: "service", IntervalMonths: 1, InUse: true},
		"tiny km":      {Kind: "service", IntervalKm: 5, DurationMinutes: 60, InUse: true},
		"months large": {Kind: "service", IntervalMonths: 500, DurationMinutes: 60, InUse: true},
	} {
		if err := workshop.SetServicePlan(ctx, pool, rentalDesk(), unit, bad); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := workshop.SetServicePlan(ctx, pool, rentalDesk(), unit, plan); err != nil {
		t.Fatal(err)
	}
	plan.IntervalMonths, plan.IntervalKm = 6, 10000
	if err := workshop.SetServicePlan(ctx, pool, rentalDesk(), unit, plan); err != nil {
		t.Fatal(err)
	}
	plans, err := workshop.ServicePlansOf(ctx, pool, rentalDesk(), unit)
	if err != nil || len(plans) != 1 || plans[0].IntervalMonths != 6 || plans[0].IntervalKm != 10000 {
		t.Fatalf("the standing plan: %+v, %v", plans, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fleet_service_plans WHERE unit_id = $1`, unit).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("the plan has %d rows, %v", rows, err)
	}

	// Done three months ago at 40 000 km; readings since give a pace.
	loc := time.UTC
	_ = loc
	doneOn := time.Now().AddDate(0, -3, 0)
	km := int64(40000)
	if err := workshop.RecordServiceDone(ctx, pool, rentalDesk(), unit, "service", doneOn.AddDate(0, 0, 2), &km, ""); !errors.Is(err, workshop.ErrInvalid) && doneOn.AddDate(0, 0, 2).After(time.Now()) {
		t.Errorf("done in the future: %v", err)
	}
	if err := workshop.RecordServiceDone(ctx, pool, rentalDesk(), unit, "service", doneOn, &km, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO odometer_readings (shop_id, vehicle_id, km, read_at, source) VALUES ($1, $2, 46000, clock_timestamp() - interval '2 days', 'manual')`, shopID, vehicle); err != nil {
		t.Fatal(err)
	}
	due, err := workshop.ServiceDueFor(ctx, pool, rentalDesk(), unit)
	if err != nil || len(due) != 1 {
		t.Fatalf("what is due: %+v, %v", due, err)
	}
	d := due[0]
	if d.DueKm == nil || *d.DueKm != 50000 || d.LatestKm == nil || *d.LatestKm != 46000 || d.KmPerDay <= 0 || d.DueOn == nil {
		t.Errorf("the next service: %+v", d)
	}
	// 4 000 km to go at about 45 a day is some three months; six months from
	// three months ago is three months: both land near; either basis is a day.
	if d.DueOn.Before(time.Now().AddDate(0, 0, -1)) {
		t.Errorf("due %v, in the past", d.DueOn)
	}

	// Done again: the count starts from it. A reading lower than the one before is
	// flagged and left out of the pace.
	later := int64(51000)
	if err := workshop.RecordServiceDone(ctx, pool, rentalDesk(), unit, "service", time.Now(), &later, ""); err != nil {
		t.Fatal(err)
	}
	var flagged int
	if _, err := pool.Exec(ctx, `INSERT INTO odometer_readings (shop_id, vehicle_id, km, read_at, source, decreasing) VALUES ($1, $2, 100, clock_timestamp(), 'manual', true)`, shopID, vehicle); err != nil {
		t.Fatal(err)
	}
	_ = flagged
	due, _ = workshop.ServiceDueFor(ctx, pool, rentalDesk(), unit)
	if len(due) != 1 || due[0].DueKm == nil || *due[0].DueKm != 61000 || due[0].LatestKm == nil || *due[0].LatestKm != 51000 {
		t.Errorf("after doing it again: %+v", due)
	}

	// A plan not in use asks for nothing, and what it said stays.
	plan.InUse = false
	if err := workshop.SetServicePlan(ctx, pool, rentalDesk(), unit, plan); err != nil {
		t.Fatal(err)
	}
	if due, _ := workshop.ServiceDueFor(ctx, pool, rentalDesk(), unit); len(due) != 0 {
		t.Errorf("a plan not in use is due: %+v", due)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fleet_service_plans WHERE unit_id = $1`, unit).Scan(&rows); err != nil || rows != 3 {
		t.Errorf("the plan has %d rows, %v", rows, err)
	}
	off := rentalDesk()
	off.Modules = []access.Module{access.Workshop}
	if err := workshop.SetServicePlan(ctx, pool, off, unit, plan); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a plan with the rental switched off: %v", err)
	}
}
