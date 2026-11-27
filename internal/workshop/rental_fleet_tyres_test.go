package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A fleet of three: one on winter tyres, one on summer, one with no set on record.
// The list for winter puts the two that do not suit first; a job is opened for
// each picked, and a car a rental holds is returned with the others opened.
func TestTheFleetsTyreChangeListsWhatIsOnTheWrongSeasonAndOpensJobs(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	add := func(reg, number string) string {
		unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: reg, FleetNumber: number, Category: "Bil"})
		if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
			t.Fatal(err)
		}
		return unit
	}
	a, b, c, d := add("FLT 001", "F1"), add("FLT 002", "F2"), add("FLT 003", "F3"), add("FLT 004", "F4")
	vehicleOf := func(unit string) string {
		var v string
		if err := pool.QueryRow(ctx, `SELECT r.vehicle_id::text FROM rental_units u JOIN resources r ON r.id = u.resource_id WHERE u.id = $1`, unit).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for unit, season := range map[string]string{a: workshop.SeasonWinterFriction, b: workshop.SeasonSummer, d: workshop.SeasonAllSeason} {
		n := summerSet("", vehicleOf(unit))
		n.Season = season
		if _, err := workshop.CreateShopTyreSet(ctx, pool, rentalOwner(), n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := workshop.FleetTyreChange(ctx, pool, technician(), "winter"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the fleet's tyres: %v", err)
	}
	if _, err := workshop.FleetTyreChange(ctx, pool, rentalDesk(), "spring"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a season that is not one: %v", err)
	}
	winter, err := workshop.FleetTyreChange(ctx, pool, rentalDesk(), "winter")
	if err != nil || len(winter) != 4 {
		t.Fatalf("the fleet for winter: %+v, %v", winter, err)
	}
	if !winter[0].NeedsChange || !winter[1].NeedsChange || winter[2].NeedsChange || winter[3].NeedsChange {
		t.Errorf("the wrong ones first, and an all-season set does for winter: %+v", winter)
	}
	summer, _ := workshop.FleetTyreChange(ctx, pool, rentalDesk(), "summer")
	needing := 0
	for _, r := range summer {
		if r.NeedsChange {
			needing++
		}
	}
	if needing != 2 {
		t.Errorf("for summer %d need a change, want the winter one and the car with none; all-season does: %+v", needing, summer)
	}

	// A rental holds the third car, from now, for a long time: its job is refused.
	var holdRes string
	if err := pool.QueryRow(ctx, `SELECT resource_id::text FROM rental_units WHERE id = $1`, c).Scan(&holdRes); err != nil {
		t.Fatal(err)
	}
	if _, err := rent(pool, rentalDesk(), holdRes, time.Now().Add(-time.Hour), time.Now().AddDate(0, 0, 30)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.OpenFleetTyreJobs(ctx, pool, technician(), []string{b}, "Change to winter tyres"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician opened jobs: %v", err)
	}
	if _, _, err := workshop.OpenFleetTyreJobs(ctx, pool, rentalDesk(), nil, "x"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("no unit picked: %v", err)
	}
	opened, refused, err := workshop.OpenFleetTyreJobs(ctx, pool, rentalDesk(), []string{b, c}, "Change to winter tyres")
	if err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE complaint = 'Change to winter tyres'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if opened != 1 || len(refused) != 1 || refused[0] != c || jobs != 1 {
		t.Errorf("opened %d, refused %v, %d jobs", opened, refused, jobs)
	}
	// The job blocks its car: a rental over it is refused.
	var res string
	_ = pool.QueryRow(ctx, `SELECT resource_id::text FROM rental_units WHERE id = $1`, b).Scan(&res)
	if _, err := rent(pool, rentalDesk(), res, time.Now().Add(24*time.Hour), time.Now().Add(48*time.Hour)); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("a rental over a car in for its tyres: %v", err)
	}
}
