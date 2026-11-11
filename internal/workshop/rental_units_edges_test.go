package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The refusal to put a car in the fleet while it is in stock names the
// stock record that stands in the way (#190), so the page can link to it.
func TestTheRefusalToFleetACarInStockNamesTheOpenRecord(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stock := takeIn(t, pool, workshop.StockFromDefleet)
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1"})
	_, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().Add(-time.Hour), "from_stock", "")
	var inStock *workshop.FleetInStockError
	switch {
	case !errors.Is(err, workshop.ErrInvalid):
		t.Fatalf("a car in stock put in the fleet: %v", err)
	case !errors.As(err, &inStock) || inStock.StockUnitID != stock.ID:
		t.Errorf("the refusal names %+v, want stock unit %s", inStock, stock.ID)
	}
}

// A unit's status is never stored: it is the fleet period, the ledger's
// blocks and the bookings, read at the moment (#190). Every status the
// issue lists is reached here from the ledger alone.
func TestEveryStatusOfAUnitIsReadFromTheLedgerAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	status := func(unit string) string {
		t.Helper()
		u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), unit)
		if err != nil {
			t.Fatal(err)
		}
		return u.Status
	}
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1"})
	if got := status(unit); got != workshop.UnitOutOfFleet {
		t.Errorf("before it joined: %q", got)
	}
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().Add(-time.Hour), "bought", ""); err != nil {
		t.Fatal(err)
	}
	if got := status(unit); got != workshop.UnitAvailable {
		t.Errorf("in the fleet with nothing on it: %q", got)
	}
	u, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), unit)
	now := time.Now()
	if _, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
		ResourceID: u.ResourceID, Kind: workshop.BlockService, From: now.Add(-time.Hour), To: now.Add(time.Hour), Reason: "Besiktning", Key: "service-block-0001"}); err != nil {
		t.Fatal(err)
	}
	if got := status(unit); got != workshop.UnitBlocked {
		t.Errorf("blocked for a service: %q", got)
	}
	// A block that has ended says nothing: the status follows the clock.
	later := addUnit(t, pool, workshop.NewRentalUnit{Registration: "DEF 34G", FleetNumber: "V2"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), later, now.Add(-time.Hour), "bought", ""); err != nil {
		t.Fatal(err)
	}
	l, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), later)
	if _, err := workshop.BookResource(ctx, pool, rentalDesk(), workshop.ResourceBooking{
		ResourceID: l.ResourceID, Kind: workshop.BlockService, From: now.Add(time.Hour), To: now.Add(2 * time.Hour), Reason: "Besiktning", Key: "service-block-0002"}); err != nil {
		t.Fatal(err)
	}
	if got := status(later); got != workshop.UnitAvailable {
		t.Errorf("a block that has not begun: %q", got)
	}
}

// Out on a rental, a unit is rented -- from the booking's block, with no
// column to set (#190).
func TestAUnitCheckedOutIsRented(t *testing.T) {
	pool := setup(t)
	v, _, _ := checkedOut(t, pool)
	u, err := workshop.RentalUnitByID(context.Background(), pool, rentalDesk(), v.unit)
	if err != nil || u.Status != workshop.UnitRented {
		t.Errorf("checked out: %q, %v", u.Status, err)
	}
}

// Three identical vans are three units in one category, each with its own
// fleet number and period; a fleet number is not shared (#190).
func TestThreeIdenticalVansAreThreeUnitsInOneCategory(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for i, reg := range []string{"AAA 11A", "BBB 22B", "CCC 33C"} {
		unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: reg, FleetNumber: "VAN-" + string(rune('1'+i)), Category: "Medium van"})
		if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().Add(-time.Hour), "bought", ""); err != nil {
			t.Fatal(err)
		}
	}
	units, err := workshop.RentalUnits(ctx, pool, rentalDesk())
	if err != nil || len(units) != 3 {
		t.Fatalf("the fleet: %d units, %v", len(units), err)
	}
	for _, u := range units {
		if u.Category != "Medium van" || u.InFleet() == nil {
			t.Errorf("a van: %+v", u)
		}
	}
	if _, err := workshop.AddRentalUnit(ctx, pool, rentalDesk(), workshop.NewRentalUnit{Registration: "DDD 44D", FleetNumber: "VAN-1", Category: "Medium van",
		ProfileID: units[0].ProfileID}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a fleet number used twice: %v", err)
	}
}

// A registration changed during a membership leaves the period and the unit
// as they were: the unit follows the vehicle, not the plate (#190).
func TestARegistrationChangedInTheFleetLeavesThePeriodAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().AddDate(0, -1, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	before, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), unit)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ChangeRegistration(ctx, pool, owner(), before.VehicleID, "XYZ 99Z", time.Now(), "Nya skyltar"); err != nil {
		t.Fatal(err)
	}
	after, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), unit)
	switch {
	case err != nil:
		t.Fatal(err)
	case after.VehicleID != before.VehicleID || len(after.Memberships) != 1 || after.InFleet() == nil:
		t.Errorf("the unit after a plate change: %+v", after)
	case workshop.NormaliseRegistration(after.Registration) != "XYZ99Z":
		t.Errorf("the unit shows %q, want the new plate", after.Registration)
	}
}

// Two cars in the fleet are never merged into one: the fleet car is
// neither merged away nor made to absorb another with its own periods.
func TestTwoFleetCarsAreNotMergedIntoOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	a := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1"})
	b := addUnit(t, pool, workshop.NewRentalUnit{Registration: "DEF 34G", FleetNumber: "V2"})
	for _, u := range []string{a, b} {
		if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), u, time.Now().Add(-time.Hour), "bought", ""); err != nil {
			t.Fatal(err)
		}
	}
	ua, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), a)
	ub, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), b)
	for _, pair := range [][2]string{{ua.VehicleID, ub.VehicleID}, {ub.VehicleID, ua.VehicleID}} {
		if p, err := workshop.PreviewVehicleMerge(ctx, pool, owner(), pair[0], pair[1]); err != nil || p.Blocked == "" {
			t.Errorf("merging two fleet cars: %+v, %v", p, err)
		}
	}
}
