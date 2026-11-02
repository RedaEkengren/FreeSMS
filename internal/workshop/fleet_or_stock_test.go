package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// A car is in the rental fleet or in stock for sale, never both at once:
// the dealer cannot take in a car the rental desk still rents out, and the
// rental desk cannot put a car the dealer is selling in the fleet. Moving
// one over is ending its time on one side and then starting it on the
// other. The database refuses the overlap, whichever side comes second.

// From the fleet's side: a car in the fleet is not taken into stock until
// its time in the fleet has ended.
func TestACarInTheFleetIsNotTakenIntoStock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool) // vehicle A, in the fleet since a year ago
	if _, err := workshop.TakeIntoStock(ctx, pool, buyerScope(), vehicleA, workshop.StockFromDefleet, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("a car in the fleet taken into stock: %v", err)
	}
	u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), v.unit)
	if err != nil || len(u.Memberships) != 1 {
		t.Fatalf("the unit: %+v, %v", u, err)
	}
	if err := workshop.EndFleetMembership(ctx, pool, rentalDesk(), u.Memberships[0].ID, time.Now(), "to_stock", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.TakeIntoStock(ctx, pool, buyerScope(), vehicleA, workshop.StockFromDefleet, ""); err != nil {
		t.Errorf("a car taken out of the fleet, then into stock: %v", err)
	}
}

// From the stock's side: a car in stock does not join the fleet, from any
// moment its cycle in stock covers, until the cycle has ended.
func TestACarInStockDoesNotJoinTheFleet(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stock := takeIn(t, pool, workshop.StockFromDefleet)
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Small van"})
	for _, from := range []time.Time{time.Now().AddDate(-1, 0, 0), time.Now().Add(time.Hour)} {
		if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, from, "from_stock", ""); !errors.Is(err, workshop.ErrInvalid) {
			t.Fatalf("a car in stock put in the fleet from %v: %v", from, err)
		}
	}
	moveStock(t, pool, stock.ID, workshop.StockClosed)
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().Add(time.Minute), "from_stock", ""); err != nil {
		t.Errorf("a car whose cycle in stock has ended, put in the fleet: %v", err)
	}
}

// The database decides, not the code before it: two transactions that
// each find the other side empty do not both stand. The second waits for
// the first and is refused once it has committed.
func TestTheFleetAndStockAreOneQueueForACar(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Small van"})
	u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), unit)
	if err != nil {
		t.Fatal(err)
	}
	inShop := func() pgx.Tx {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `SELECT set_config('app.current_shop', $1, true)`, shopID); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	fleet := inShop()
	if _, err := fleet.Exec(ctx, `INSERT INTO fleet_memberships (shop_id, unit_id, vehicle_id, joined_at, joined_how) VALUES ($1, $2, $3, now(), 'bought')`,
		shopID, unit, vehicleA); err != nil {
		t.Fatal(err)
	}
	stock := start(func() error {
		_, err := inShop().Exec(ctx, `INSERT INTO stock_units (shop_id, vehicle_id, resource_id, reason) VALUES ($1, $2, $3, 'defleet')`,
			shopID, vehicleA, u.ResourceID)
		return err
	})
	waitForWaiters(t, pool, 1, stock, "the stock unit")
	if err := fleet.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := stock.result(t); err == nil {
		t.Error("a car put in the fleet and in stock at once: both stand")
	}
}
