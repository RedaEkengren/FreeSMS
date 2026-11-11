package workshop_test

import (
	"context"
	"errors"
	"github.com/RedaEkengren/FreeSMS/internal/access"
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

// A booking that fails after its block was written leaves neither: the
// block and the booking are one transaction (#192).
func TestABookingThatFailsHalfwayLeavesNoBlockAndNoBooking(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	for _, q := range []string{
		`CREATE FUNCTION fail_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'broken on purpose'; END $$`,
		`CREATE TRIGGER fail_event BEFORE INSERT ON rental_booking_events FOR EACH ROW EXECUTE FUNCTION fail_event()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Skipf("the test role cannot create a trigger: %v", err)
		}
	}
	dropped := false
	drop := func() {
		if !dropped {
			dropped = true
			_, _ = pool.Exec(ctx, `DROP TRIGGER fail_event ON rental_booking_events`)
			_, _ = pool.Exec(ctx, `DROP FUNCTION fail_event()`)
		}
	}
	defer drop()
	if _, err := v.book(pool, in2031(10, 9), in2031(11, 9)); err == nil {
		t.Fatal("a booking went through a broken event")
	}
	drop()
	var blocks, bookings int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM resource_blocks WHERE resource_id = $1 AND kind = 'rental'),
	                                      (SELECT count(*) FROM rental_bookings)`, v.resource).Scan(&blocks, &bookings); err != nil {
		t.Fatal(err)
	}
	if blocks != 0 || bookings != 0 {
		t.Errorf("a failed booking left %d blocks and %d bookings", blocks, bookings)
	}
	if _, err := v.book(pool, in2031(10, 9), in2031(11, 9)); err != nil {
		t.Errorf("the dates are free after the failure: %v", err)
	}
}

// A renter who did not come is a no-show once the rental was due to start:
// the block goes, the booking and its events stay, and the time is free
// to book again (#192).
func TestANoShowReleasesTheBlockAndKeepsTheBooking(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	from, to := time.Now().Add(-2*time.Hour), time.Now().Add(48*time.Hour)
	id, err := v.book(pool, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelRentalBooking(ctx, pool, rentalDesk(), id, "Kom aldrig", true); err != nil {
		t.Fatalf("a no-show once it was due: %v", err)
	}
	b, err := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	if err != nil || b.State != workshop.RentalNoShow {
		t.Fatalf("the booking after a no-show: %+v, %v", b.State, err)
	}
	if len(b.Events) < 2 || b.Events[len(b.Events)-1].Kind != "no_show" {
		t.Errorf("the events: %+v", b.Events)
	}
	if _, err := v.book(pool, from.Add(time.Hour), to); err != nil {
		t.Errorf("the time after a no-show: %v", err)
	}
}

// One renter may hold two units at once -- a company renting two vans --
// while the same unit and days are taken (#192).
func TestOneRenterBooksTwoUnitsAtOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	second := addUnit(t, pool, workshop.NewRentalUnit{Registration: "DEF 34G", FleetNumber: "V2", Category: "Small van"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), second, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	from, to := in2031(10, 9), in2031(10, 12)
	if _, err := v.book(pool, from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: second, RenterID: v.renter, From: from, To: to}); err != nil {
		t.Errorf("the same renter, a second unit, the same days: %v", err)
	}
	if _, err := v.book(pool, from, to); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("the first unit, the same days: %v", err)
	}
}

// The home page's rental cards (#157): the desk is told of a damage case
// nobody has assessed; the owner of what is out and what was invoiced this
// month; nobody else of either. Read from the rows each time.
func TestTheHomePageCountsRentalDamageAndInvoicingForTheRightRoles(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	dc, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), newDamageOn(t, pool, "GHI 56J", "V3"))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), dc.BookingID, "")
	if err != nil {
		t.Fatal(err)
	}
	cards := func(s access.Scope) map[string]workshop.Card {
		t.Helper()
		home, errs := workshop.HomeFor(ctx, pool, s)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		out := map[string]workshop.Card{}
		for _, c := range append(append([]workshop.Card{}, home.Actions...), home.Information...) {
			out[c.Title] = c
		}
		return out
	}
	const damage, invoiced = "%d rental with damage nobody has assessed", "Rental invoiced this month, before VAT"
	desk := cards(rentalDesk())
	if desk[damage].Count != 1 {
		t.Errorf("the desk's cards: %+v", desk)
	}
	if _, ok := desk[invoiced]; ok {
		t.Error("the rental desk was shown what the shop invoiced")
	}
	boss := cards(rentalOwner())
	var net int64
	if err := pool.QueryRow(ctx, `SELECT net_minor FROM invoices WHERE id = $1`, inv.ID).Scan(&net); err != nil {
		t.Fatal(err)
	}
	if boss[invoiced].Minor != net || boss[invoiced].Count != 1 || net == 0 {
		t.Errorf("the owner's invoiced card: %+v, want %d", boss[invoiced], net)
	}
	for _, s := range []access.Scope{technician(), advisor(), sales()} {
		for title := range cards(s) {
			if title == damage || title == invoiced {
				t.Errorf("%v was shown %q", s.Role, title)
			}
		}
	}
	// Switched off, a module's cards go with it.
	off := rentalOwner()
	off.Modules = workshopOnly
	for title, c := range cards(off) {
		if c.Module == access.Rental {
			t.Errorf("a rental card with Rental off: %q", title)
		}
	}
}

// A unit out on a rental is counted for the owner, and only while it is out.
func TestTheOwnersHomePageCountsTheUnitsOutNow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	checkedOut(t, pool)
	home, errs := workshop.HomeFor(ctx, pool, rentalOwner())
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	found := 0
	for _, c := range home.Information {
		if c.Title == "%d rental unit out now" {
			found = c.Count
		}
	}
	if found != 1 {
		t.Errorf("units out now: %d; cards %+v", found, home.Information)
	}
}
