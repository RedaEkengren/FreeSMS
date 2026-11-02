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

// Stock units (#160). The fixture's vehicleA carries the plate ABC 12D.

// takeIn opens a cycle for vehicleA as the buyer.
func takeIn(t *testing.T, pool *pgxpool.Pool, reason string) workshop.StockUnit {
	t.Helper()
	u, err := workshop.TakeIntoStock(context.Background(), pool, buyerScope(), vehicleA, reason, "")
	if err != nil {
		t.Fatalf("TakeIntoStock: %v", err)
	}
	return u
}

// moveStock moves a unit through statuses as whoever runs the shop.
func moveStock(t *testing.T, pool *pgxpool.Pool, id string, to ...workshop.StockStatus) {
	t.Helper()
	for _, s := range to {
		if err := workshop.MoveStockUnit(context.Background(), pool, everything(owner()), id, s, "Skrotad"); err != nil {
			t.Fatalf("MoveStockUnit(%s): %v", s, err)
		}
	}
}

// Bought, sold, delivered and bought again: two cycles of one vehicle, on
// one resource that left use with the first and came back with the second.
func TestACarBoughtTwiceIsTwoStockUnitsOfOneVehicle(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	bought := buy(t, pool, workshop.StockFromPurchase)
	first, err := workshop.StockUnitByID(ctx, pool, buyerScope(), bought.StockUnitID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != workshop.StockInPreparation || first.ClosedAt != nil || !first.Owned() {
		t.Fatalf("a unit just taken in: %+v", first)
	}
	moveStock(t, pool, first.ID, workshop.StockForSale, workshop.StockReserved, workshop.StockSold, workshop.StockDelivered)
	boughtBack := buy(t, pool, workshop.StockFromRepurchase)
	again, err := workshop.StockUnitByID(ctx, pool, buyerScope(), boughtBack.StockUnitID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Reason != workshop.StockFromRepurchase || again.ID == first.ID || again.VehicleID != first.VehicleID || again.ResourceID != first.ResourceID {
		t.Errorf("bought again: %+v, first %+v; want a new unit of the same vehicle and resource", again, first)
	}
	units, err := workshop.StockUnitsOfVehicle(ctx, pool, sales(), vehicleA)
	if err != nil || len(units) != 2 || units[0].ID != first.ID || units[1].ID != again.ID {
		t.Fatalf("the vehicle's cycles: %+v, %v", units, err)
	}
	if units[0].Status != workshop.StockDelivered || units[0].ClosedAt == nil || units[0].FirstForSaleAt == nil {
		t.Errorf("the first cycle after delivery: %+v", units[0])
	}
	if units[1].FirstForSaleAt != nil {
		t.Errorf("the second cycle starts its stock days again: %+v", units[1])
	}
	var retired bool
	if err := pool.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM resources WHERE id = $1`, again.ResourceID).Scan(&retired); err != nil || retired {
		t.Errorf("the car bought back is not in use again: %v, %v", retired, err)
	}
	var periods int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM resource_retirements WHERE resource_id = $1`, again.ResourceID).Scan(&periods); err != nil || periods != 1 {
		t.Errorf("%d periods out of use, want the one between the cycles (%v)", periods, err)
	}

	// Every move is on the unit's trail, in order.
	trail, err := workshop.AuditTrail(ctx, pool, buyerScope(), "stock_unit", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	var moves []string
	for _, e := range trail {
		moves = append(moves, e.Event+":"+e.To)
	}
	want := []string{"stock_unit.opened:in_preparation", "stock_unit.moved:for_sale", "stock_unit.moved:reserved",
		"stock_unit.moved:sold", "stock_unit.moved:delivered"}
	if len(moves) != len(want) {
		t.Fatalf("trail %v, want %v", moves, want)
	}
	for i := range want {
		if moves[i] != want[i] {
			t.Errorf("trail %v, want %v", moves, want)
			break
		}
	}
}

// Two buyers taking the same car in at the same moment: the database lets
// one cycle open, and the other is told the car is in stock already.
func TestTwoAttemptsToOpenAUnitAtOnceLeaveOne(t *testing.T) {
	pool := setup(t)
	const tries = 8
	var wg sync.WaitGroup
	errs := make([]error, tries)
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = workshop.TakeIntoStock(context.Background(), pool, buyerScope(), vehicleA, workshop.StockFromDefleet, "")
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, workshop.ErrInvalid) || err.Error() != "workshop: invalid: that car is already in stock; finish its cycle before starting another":
			t.Errorf("a losing attempt: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d of %d attempts opened a unit, want 1", ok, tries)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM stock_units`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d units, want 1 (%v)", n, err)
	}
}

// A car bought comes into stock through its purchase, which records what
// was paid and on what evidence; only a car with nothing to record is taken
// in on its own.
func TestACarBoughtComesInThroughItsPurchase(t *testing.T) {
	pool := setup(t)
	for _, reason := range []string{workshop.StockFromPurchase, workshop.StockFromTradeIn, workshop.StockFromRepurchase,
		workshop.StockFromConsignment, workshop.StockFromImport} {
		if _, err := workshop.TakeIntoStock(context.Background(), pool, buyerScope(), vehicleA, reason, ""); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("taken in as %s with nothing recorded: %v", reason, err)
		}
	}
}

// A request replayed from a phone finds the unit it opened, not a refusal.
func TestATakeInReplayedFindsItsUnit(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	a, err := workshop.TakeIntoStock(ctx, pool, buyerScope(), vehicleA, workshop.StockFromDefleet, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := workshop.TakeIntoStock(ctx, pool, buyerScope(), vehicleA, workshop.StockFromDefleet, "req-1")
	if err != nil || b.ID != a.ID {
		t.Errorf("replayed: %+v, %v; want %s", b, err, a.ID)
	}
}

// Moves that are not a move are refused with a sentence; an ended cycle
// moves no more; ending without a sale says why; and a car with something
// booked on it from now on cannot leave.
func TestAStockUnitMovesOnlyAsADealCan(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	u := takeIn(t, pool, workshop.StockFromDefleet)
	all := everything(owner())
	if err := workshop.MoveStockUnit(ctx, pool, all, u.ID, workshop.StockSold, ""); !errors.Is(err, workshop.ErrInvalid) ||
		err.Error() != "workshop: invalid: a car cannot go straight from there to there" {
		t.Errorf("sold straight from preparation: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, all, u.ID, workshop.StockInPreparation, ""); err != nil {
		t.Errorf("to where it already is: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, all, u.ID, workshop.StockWrittenOff, " "); !errors.Is(err, workshop.ErrStockNoReason) {
		t.Errorf("written off with no reason: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, all, u.ID, "lost", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a status there is not: %v", err)
	}

	// A test drive booked ahead keeps the car from leaving stock.
	block, err := workshop.BookResource(ctx, pool, all, workshop.ResourceBooking{ResourceID: u.ResourceID,
		Kind: workshop.BlockTestDrive, From: in2031(1, 10), To: in2031(1, 11)})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, all, u.ID, workshop.StockWrittenOff, "Stulen"); !errors.Is(err, workshop.ErrInvalid) ||
		err.Error() != "workshop: invalid: something is still booked on the car; release or replan that first" {
		t.Errorf("written off with a test drive ahead: %v", err)
	}
	if err := workshop.ReleaseBlock(ctx, pool, all, block); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, all, u.ID, workshop.StockWrittenOff, "Stulen"); err != nil {
		t.Fatalf("written off: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, all, u.ID, workshop.StockForSale, ""); !errors.Is(err, workshop.ErrInvalid) ||
		err.Error() != "workshop: invalid: that cycle is over; a car that comes back starts a new one" {
		t.Errorf("an ended cycle moved: %v", err)
	}
	trail, _ := workshop.AuditTrail(ctx, pool, all, "stock_unit", u.ID)
	if last := trail[len(trail)-1]; last.To != string(workshop.StockWrittenOff) || last.Reason != "Stulen" || last.Actor == "" {
		t.Errorf("written off on the trail as %+v", last)
	}
}

// A car known by neither its chassis number nor a plate it carries cannot
// come into stock: the margin scheme is accounted car by car.
func TestACarWithNoChassisNumberOrPlateCannotComeIntoStock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	const bare = "55555555-5555-5555-5555-555555555556"
	if _, err := pool.Exec(ctx, `INSERT INTO vehicles (id, shop_id, make, label) VALUES ($1, $2, 'Volvo', 'den blå')`, bare, shopID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.TakeIntoStock(ctx, pool, buyerScope(), bare, workshop.StockFromDefleet, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a car with neither: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE vehicles SET vin = 'YV1SW61R021234567' WHERE id = $1`, bare); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.TakeIntoStock(ctx, pool, buyerScope(), bare, workshop.StockFromDefleet, ""); err != nil {
		t.Errorf("a car with its chassis number: %v", err)
	}
}

// Who moves a unit where, and only in a shop that sells cars; with car
// sales switched off the history stays readable to those who may read it.
func TestStockUnitsByRoleAndModule(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	u := takeIn(t, pool, workshop.StockFromDefleet)
	for name, s := range map[string]access.Scope{"the front desk": everything(advisor()), "a technician": everything(technician()),
		"the rental desk": rentalDesk()} {
		if _, err := workshop.TakeIntoStock(ctx, pool, s, vehicleA, workshop.StockFromDefleet, ""); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s took a car in: %v", name, err)
		}
		if _, err := workshop.StockUnitByID(ctx, pool, s, u.ID); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read a stock unit: %v", name, err)
		}
		if err := workshop.MoveStockUnit(ctx, pool, s, u.ID, workshop.StockForSale, ""); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s moved a stock unit: %v", name, err)
		}
		if _, err := workshop.AuditTrail(ctx, pool, s, "stock_unit", u.ID); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read a stock unit's trail: %v", name, err)
		}
	}
	if _, err := workshop.TakeIntoStock(ctx, pool, sales(), vehicleA, workshop.StockFromDefleet, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson took a car in: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, sales(), u.ID, workshop.StockForSale, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson offered a car the buyer has not: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, buyerScope(), u.ID, workshop.StockForSale, ""); err != nil {
		t.Fatalf("the buyer offered it: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, buyerScope(), u.ID, workshop.StockReserved, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer reserved it: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, sales(), u.ID, workshop.StockReserved, ""); err != nil {
		t.Errorf("the salesperson reserved it: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, sales(), u.ID, workshop.StockForSale, ""); err != nil {
		t.Errorf("the reservation fell through: %v", err)
	}

	off := buyerScope()
	off.Modules = []access.Module{access.Workshop, access.Rental}
	if _, err := workshop.TakeIntoStock(ctx, pool, off, vehicleA, workshop.StockFromDefleet, ""); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("taken in with car sales off: %v", err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, off, u.ID, workshop.StockInPreparation, ""); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("moved with car sales off: %v", err)
	}
	if got, err := workshop.StockUnitByID(ctx, pool, off, u.ID); err != nil || got.ID != u.ID {
		t.Errorf("read with car sales off: %+v, %v", got, err)
	}
	if units, err := workshop.StockUnitsOfVehicle(ctx, pool, off, vehicleA); err != nil || len(units) != 1 {
		t.Errorf("the history with car sales off: %d, %v", len(units), err)
	}
	if trail, err := workshop.AuditTrail(ctx, pool, off, "stock_unit", u.ID); err != nil || len(trail) == 0 {
		t.Errorf("the trail with car sales off: %d, %v", len(trail), err)
	}
}

// The table keeps what a cycle was: never deleted, never moved to another
// car, its opening never rewritten, as the application.
func TestAStockUnitKeepsWhatItWas(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	takeIn(t, pool, workshop.StockFromDefleet)
	for _, stmt := range []string{
		`DELETE FROM stock_units`,
		`UPDATE stock_units SET vehicle_id = vehicle_id`,
		`UPDATE stock_units SET opened_at = now()`,
		`UPDATE stock_units SET reason = 'import'`,
	} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}
