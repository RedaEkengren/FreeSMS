package workshop_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/RedaEkengren/FreeSMS/migrations"
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

// An overlap that got in anyway -- written by the release before the
// refusal, which a rollback may run over it -- can still be ended from
// either side. Only a period made longer is checked again: ending one,
// or a status move that leaves its end as it was, is not refused for an
// overlap it does not add to.
func TestAnOverlapAlreadyThereCanBeEnded(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool) // vehicle A, in the fleet since a year ago
	for _, s := range []string{`ALTER TABLE stock_units DISABLE TRIGGER vehicle_fleet_or_stock`} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	stock := takeIn(t, pool, workshop.StockFromDefleet)
	if _, err := pool.Exec(ctx, `ALTER TABLE stock_units ENABLE TRIGGER vehicle_fleet_or_stock`); err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, stock.ID, workshop.StockForSale)
	// Made longer, it is checked again and refused. (Not something the
	// application role may do; the owner can, and the trigger still runs.)
	_, err := pool.Exec(ctx, `UPDATE stock_units SET opened_at = opened_at - interval '1 day' WHERE id = $1`, stock.ID)
	if err == nil || !strings.Contains(err.Error(), "23P01") {
		t.Errorf("a stock cycle made longer over a fleet period: %v", err)
	}
	u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), v.unit)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.EndFleetMembership(ctx, pool, rentalDesk(), u.Memberships[0].ID, time.Now(), "to_stock", ""); err != nil {
		t.Errorf("ending the fleet side of an overlap: %v", err)
	}
	moveStock(t, pool, stock.ID, workshop.StockClosed)
}

// A database that already holds a car in both, written before the refusal
// existed, is not migrated over it in silence: the migration stops and
// names the car, so the overlap is ended by a person who knows which side
// is right.
func TestTheRefusalIsNotAddedOverAnOverlapAlreadyThere(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	refusal := ""
	for _, n := range names {
		if strings.HasSuffix(n, "_fleet_or_stock.sql") {
			refusal = n
		}
	}
	if refusal == "" {
		t.Fatal("no fleet_or_stock migration")
	}
	before := fstest.MapFS{}
	for _, n := range names {
		if n < refusal {
			body, err := fs.ReadFile(migrations.FS, n)
			if err != nil {
				t.Fatal(err)
			}
			before[n] = &fstest.MapFile{Data: body}
		}
	}
	if err := database.Migrate(ctx, pool, before); err != nil {
		t.Fatal(err)
	}
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{
			`INSERT INTO shops (id, name) VALUES ('` + shopID + `', 'Verkstaden')`,
			`INSERT INTO vehicles (id, shop_id) VALUES ('` + vehicleA + `', '` + shopID + `')`,
			`INSERT INTO resources (id, shop_id, kind, vehicle_id) VALUES ('66666666-6666-6666-6666-666666666666', '` + shopID + `', 'vehicle', '` + vehicleA + `')`,
			`INSERT INTO rental_profiles (id, shop_id, base, name) VALUES ('77777777-7777-7777-7777-777777777777', '` + shopID + `', 'car', 'Bil')`,
			`INSERT INTO rental_units (id, shop_id, resource_id, profile_id, fleet_number, category)
			 VALUES ('88888888-8888-8888-8888-888888888888', '` + shopID + `', '66666666-6666-6666-6666-666666666666', '77777777-7777-7777-7777-777777777777', 'V1', 'Van')`,
			`INSERT INTO fleet_memberships (shop_id, unit_id, vehicle_id, joined_at, joined_how)
			 VALUES ('` + shopID + `', '88888888-8888-8888-8888-888888888888', '` + vehicleA + `', now() - interval '1 year', 'bought')`,
			`INSERT INTO stock_units (shop_id, vehicle_id, resource_id, reason)
			 VALUES ('` + shopID + `', '` + vehicleA + `', '66666666-6666-6666-6666-666666666666', 'defleet')`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = database.Migrate(ctx, pool, migrations.FS)
	if err == nil || !strings.Contains(err.Error(), vehicleA) || !strings.Contains(err.Error(), "fleet") {
		t.Errorf("migrated over a car in the fleet and in stock: %v", err)
	}
}

// Closing a stock cycle takes the car's resource out of use. Joining the
// fleet from stock brings it back, so the car the dealer handed over can
// be rented -- whether it was a rental unit before or is made one now.
func TestACarFromStockJoinsTheFleetAndIsRented(t *testing.T) {
	ctx := context.Background()
	t.Run("a unit before", func(t *testing.T) {
		pool := setup(t)
		v := rentableVan(t, pool)
		u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), v.unit)
		if err != nil {
			t.Fatal(err)
		}
		if err := workshop.EndFleetMembership(ctx, pool, rentalDesk(), u.Memberships[0].ID, time.Now(), "to_stock", ""); err != nil {
			t.Fatal(err)
		}
		stock := takeIn(t, pool, workshop.StockFromDefleet)
		moveStock(t, pool, stock.ID, workshop.StockClosed)
		if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), v.unit, time.Now(), "from_stock", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := v.book(pool, in2031(10, 9), in2031(12, 9)); err != nil {
			t.Errorf("a car back in the fleet from stock, booked: %v", err)
		}
	})
	t.Run("a unit made after", func(t *testing.T) {
		pool := setup(t)
		stock := takeIn(t, pool, workshop.StockFromDefleet)
		moveStock(t, pool, stock.ID, workshop.StockClosed)
		unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Small van"})
		if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now(), "from_stock", ""); err != nil {
			t.Fatal(err)
		}
		renter, err := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Rita Hyrare", Role: workshop.PartyRenter})
		if err != nil {
			t.Fatal(err)
		}
		v := rentable{unit: unit, renter: renter, plan: dayPlan(t, pool, "Small van", 40000, "at_start")}
		if _, err := v.book(pool, in2031(10, 9), in2031(12, 9)); err != nil {
			t.Errorf("a car from stock made a unit and put in the fleet, booked: %v", err)
		}
	})
}
