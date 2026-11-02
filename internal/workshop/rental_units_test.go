package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

func addUnit(t *testing.T, pool *pgxpool.Pool, n workshop.NewRentalUnit) string {
	t.Helper()
	if n.ProfileID == "" {
		base := workshop.ProfileCar
		if n.Registration == "" {
			base = workshop.ProfileEquipment
		}
		n.ProfileID = profileFor(t, pool, base).ID
	}
	if n.Category == "" {
		n.Category = "Small van"
	}
	id, err := workshop.AddRentalUnit(context.Background(), pool, rentalDesk(), n)
	if err != nil {
		t.Fatalf("AddRentalUnit: %v", err)
	}
	return id
}

// A vehicle the workshop already knows is the same vehicle as a unit, its
// history one history; a thing with no plate is a unit too. A unit is in
// the fleet only for its memberships, and a vehicle cannot be in it twice
// at once -- the database refuses it whatever the code does.
func TestARentalUnitIsInTheFleetForItsPeriods(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	van := addUnit(t, pool, workshop.NewRentalUnit{Registration: "abc12d", FleetNumber: "V1"})
	tent := addUnit(t, pool, workshop.NewRentalUnit{Name: "Partytält 6x12", FleetNumber: "T1", Category: "Tent"})

	u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), van)
	if err != nil || u.VehicleID != vehicleA || u.Registration != "ABC 12D" || u.Status != workshop.UnitOutOfFleet {
		t.Fatalf("the van is the workshop's vehicle, out of the fleet: %+v, %v", u, err)
	}
	if _, err := workshop.AddRentalUnit(ctx, pool, rentalDesk(), workshop.NewRentalUnit{Registration: "ABC 12 D",
		FleetNumber: "V2", Category: "x", ProfileID: u.ProfileID}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same vehicle made a unit twice: %v", err)
	}
	if _, err := workshop.AddRentalUnit(ctx, pool, rentalDesk(), workshop.NewRentalUnit{Name: "Släp", FleetNumber: "v1",
		Category: "x", ProfileID: u.ProfileID}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second unit numbered V1: %v", err)
	}

	start := time.Now().Add(-48 * time.Hour)
	first, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), van, start, "bought", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), van, start.Add(time.Hour), "other", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("in the fleet twice at once: %v", err)
	}
	if u, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), van); u.Status != workshop.UnitAvailable || u.InFleet() == nil {
		t.Errorf("the van in the fleet: %+v", u)
	}
	// Sold, and bought back: two periods, one unit, one vehicle.
	if err := workshop.EndFleetMembership(ctx, pool, rentalDesk(), first, start.Add(24*time.Hour), "sold", "Såld till Karlsson"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.EndFleetMembership(ctx, pool, rentalDesk(), first, start.Add(30*time.Hour), "other", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a period ended twice: %v", err)
	}
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), van, start.Add(36*time.Hour), "bought", "Återköpt"); err != nil {
		t.Fatal(err)
	}
	u, _ = workshop.RentalUnitByID(ctx, pool, rentalDesk(), van)
	if len(u.Memberships) != 2 || u.Memberships[1].LeftHow != "sold" || u.Memberships[0].LeftAt != nil {
		t.Errorf("two periods, the first sold: %+v", u.Memberships)
	}
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), tent, start, "already_owned", ""); err != nil {
		t.Errorf("a tent joining the fleet: %v", err)
	}
	// The database's own refusal, past the code.
	if err := asApp(ctx, pool, `INSERT INTO fleet_memberships (shop_id, unit_id, vehicle_id, joined_at, joined_how)
		VALUES ('`+shopID+`', '`+tent+`', '`+vehicleA+`', now(), 'other')`); err == nil {
		t.Error("the database let the van's vehicle be in the fleet twice at once")
	}
	if err := asApp(ctx, pool, `UPDATE fleet_memberships SET left_note = 'x' WHERE left_at IS NOT NULL`); err == nil {
		t.Error("the database let an ended period be changed")
	}
}

// A period in the fleet does not end under a rental booked after it; the
// refusal lists what is in the way.
func TestAFleetPeriodDoesNotEndUnderABooking(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	van := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1"})
	m, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), van, time.Now().Add(-time.Hour), "bought", "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), van)
	if _, err := rent(pool, rentalDesk(), u.ResourceID, in2031(10, 9), in2031(12, 9)); err != nil {
		t.Fatal(err)
	}
	err = workshop.EndFleetMembership(ctx, pool, rentalDesk(), m, in2031(1, 9), "sold", "")
	if f, ok := workshop.AsFutureBookings(err); !ok || len(f.Blocks) != 1 || !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("ending the period with a rental booked after: %v", err)
	}
	if err := workshop.EndFleetMembership(ctx, pool, rentalDesk(), m, in2031(20, 9), "sold", ""); err != nil {
		t.Errorf("ending it after the rental: %v", err)
	}
}

// A car in the workshop reads as at the workshop; the desk is the only one
// that reads units, and with Rental switched off nobody does.
func TestAUnitsStatusIsReadFromTheLedger(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	van := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), van, time.Now().Add(-time.Hour), "bought", ""); err != nil {
		t.Fatal(err)
	}
	newJob(t, pool) // the van, taken into the workshop: its job blocks it
	units, err := workshop.RentalUnits(ctx, pool, rentalDesk())
	if err != nil || len(units) != 1 || units[0].Status != workshop.UnitWorkshop {
		t.Errorf("the van in the workshop: %+v, %v", units, err)
	}
	for _, s := range []access.Scope{renting(access.RoleTechnician), renting(access.RoleSales), advisor()} {
		if _, err := workshop.RentalUnits(ctx, pool, s); err == nil {
			t.Errorf("%v read the units", s.Role)
		}
	}
	off := rentalDesk()
	off.Modules = workshopOnly
	if _, err := workshop.RentalUnits(ctx, pool, off); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("reading units with Rental off: %v", err)
	}
}

// A duplicate of a fleet car is merged into it, keeping its periods; the
// fleet car is never the one merged away (Core refuses that, 0070).
func TestMergingIntoAFleetVehicleKeepsItsPeriods(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	van := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), van, time.Now().Add(-time.Hour), "bought", ""); err != nil {
		t.Fatal(err)
	}
	var dup string
	if err := pool.QueryRow(ctx, `INSERT INTO vehicles (shop_id, make) VALUES ($1, 'Volvo') RETURNING id`, shopID).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if p, err := workshop.PreviewVehicleMerge(ctx, pool, owner(), dup, vehicleA); err != nil || p.Blocked == "" {
		t.Errorf("merging the fleet car away: %+v, %v", p, err)
	}
	p, err := workshop.PreviewVehicleMerge(ctx, pool, owner(), vehicleA, dup)
	if err != nil || p.Blocked != "" {
		t.Fatalf("merging the duplicate into the fleet car: %+v, %v", p, err)
	}
	if _, err := workshop.MergeVehicles(ctx, pool, owner(), vehicleA, dup, p.KeepRevision, p.DropRevision, "Samma bil"); err != nil {
		t.Fatal(err)
	}
	if u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), van); err != nil || len(u.Memberships) != 1 || u.VehicleID != vehicleA {
		t.Errorf("the unit after the merge: %+v, %v", u, err)
	}
}
