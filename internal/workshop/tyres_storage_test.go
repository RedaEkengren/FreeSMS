package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A place holds as many sets as it has room for, a set is in one place at a
// time, moving it keeps where it was, and a place with sets in it cannot be
// retired.
func TestAPlaceHoldsItsCapacityAndASetIsInOnePlaceAtATime(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	hall, err := workshop.CreateStorageLocation(ctx, pool, owner(), "Hall A / Rack 1 / Shelf 2", 2)
	if err != nil {
		t.Fatal(err)
	}
	annex, err := workshop.CreateStorageLocation(ctx, pool, owner(), "Annex", 1)
	if err != nil {
		t.Fatal(err)
	}
	var sets []string
	for i := 0; i < 3; i++ {
		id, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, id)
	}
	for _, id := range sets[:2] {
		if err := workshop.PlaceTyreSet(ctx, pool, technician(), id, hall); err != nil {
			t.Fatal(err)
		}
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), sets[2], hall); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a third set in a place with room for two: %v", err)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), sets[0], hall); err != nil {
		t.Errorf("a set placed where it already is: %v", err)
	}
	places, err := workshop.StorageLocations(ctx, pool, advisor())
	if err != nil || len(places) != 2 || places[0].Name != "Annex" || places[1].Free() != 0 || len(places[1].Sets) != 2 || places[0].Free() != 1 {
		t.Fatalf("the places: %+v, %v", places, err)
	}

	// A move closes the old placement and opens the new; both are kept.
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), sets[0], annex); err != nil {
		t.Fatal(err)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), sets[2], hall); err != nil {
		t.Errorf("the room the move made: %v", err)
	}
	var rows, open int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE removed_at IS NULL) FROM tyre_set_placements WHERE tyre_set_id = $1`, sets[0]).Scan(&rows, &open); err != nil || rows != 2 || open != 1 {
		t.Errorf("placements of the moved set: %d rows, %d open, %v", rows, open, err)
	}
	if s, _ := workshop.TyreSetByID(ctx, pool, technician(), sets[0]); s.StoredIn != "Annex" {
		t.Errorf("where the set stands: %q", s.StoredIn)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), sets[1], annex); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set in a full annex: %v", err)
	}

	// The database itself refuses a set in two places.
	if _, err := pool.Exec(ctx, `INSERT INTO tyre_set_placements (shop_id, tyre_set_id, location_id) VALUES ($1, $2, $3)`, shopID, sets[0], hall); err == nil {
		t.Error("the database let a set be in two places")
	}

	// A place with sets in it cannot be retired; empty, it can, and is not used.
	if err := workshop.SetStorageLocationInUse(ctx, pool, owner(), annex, false); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a place with a set in it retired: %v", err)
	}
	if err := workshop.TakeTyreSetOutOfStorage(ctx, pool, technician(), sets[0]); err != nil {
		t.Fatal(err)
	}
	if err := workshop.TakeTyreSetOutOfStorage(ctx, pool, technician(), sets[0]); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set taken out twice: %v", err)
	}
	if err := workshop.SetStorageLocationInUse(ctx, pool, owner(), annex, false); err != nil {
		t.Fatal(err)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), sets[0], annex); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set put in a retired place: %v", err)
	}
	if err := workshop.SetStorageLocationInUse(ctx, pool, owner(), annex, false); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a place retired twice: %v", err)
	}
	if err := workshop.SetStorageLocationInUse(ctx, pool, owner(), annex, true); err != nil {
		t.Fatal(err)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), sets[0], annex); err != nil {
		t.Errorf("a set put in a place opened again: %v", err)
	}
	// The history of a closed placement cannot be rewritten.
	if _, err := pool.Exec(ctx, `UPDATE tyre_set_placements SET removed_at = clock_timestamp() WHERE removed_at IS NOT NULL`); err == nil {
		t.Error("a placement that had ended was changed")
	}
}

// Two sets going into the last room at once: one gets it.
func TestTwoSetsGoingIntoTheLastRoomAtOnceOneGetsIt(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place, _ := workshop.CreateStorageLocation(ctx, pool, owner(), "Last slot", 1)
	var sets []string
	for i := 0; i < 6; i++ {
		id, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, ""))
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, id)
	}
	var wg sync.WaitGroup
	results := make([]error, len(sets))
	for i, id := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = workshop.PlaceTyreSet(ctx, pool, technician(), id, place)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("a set refused for another reason: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d sets got a slot with room for one", won)
	}
}

// Who may make, retire and fill a place: the owner makes and retires, the
// workshop's people fill; a technician is not shown whose a stored set is;
// the rental desk and a shop with the workshop off see nothing.
func TestWhoMayMakeAndFillAPlaceAndWhoSeesWhoseTheSetsAre(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	for _, s := range []access.Scope{advisor(), technician()} {
		if _, err := workshop.CreateStorageLocation(ctx, pool, s, "X", 1); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s made a place: %v", s.Role, err)
		}
	}
	place, err := workshop.CreateStorageLocation(ctx, pool, owner(), "Shelf", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CreateStorageLocation(ctx, pool, owner(), " Shelf ", 3); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a name used twice: %v", err)
	}
	for _, bad := range []struct {
		name string
		n    int
	}{{"", 1}, {"A", 0}, {"A", 1001}} {
		if _, err := workshop.CreateStorageLocation(ctx, pool, owner(), bad.name, bad.n); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	id, _ := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, vehicleA))
	if err := workshop.PlaceTyreSet(ctx, pool, advisor(), id, place); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetStorageLocationInUse(ctx, pool, advisor(), place, false); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("an advisor retiring a place: %v", err)
	}
	tech, _ := workshop.StorageLocations(ctx, pool, technician())
	if len(tech) != 1 || len(tech[0].Sets) != 1 || tech[0].Sets[0].Owner != "" || tech[0].Sets[0].Vehicle == "" {
		t.Errorf("the technician's places: %+v", tech)
	}
	adv, _ := workshop.StorageLocations(ctx, pool, advisor())
	if adv[0].Sets[0].Owner != "A Customer" {
		t.Errorf("the advisor's places: %+v", adv)
	}
	for _, s := range []access.Scope{everything(rentalDesk()), everything(sales())} {
		if _, err := workshop.StorageLocations(ctx, pool, s); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the places: %v", s.Role, err)
		}
		if err := workshop.PlaceTyreSet(ctx, pool, s, id, place); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s placed a set: %v", s.Role, err)
		}
	}
	off := owner()
	off.Modules = []access.Module{access.Dealer}
	if _, err := workshop.StorageLocations(ctx, pool, off); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("the places with the workshop off: %v", err)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, off, id, place); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a set placed with the workshop off: %v", err)
	}
}
