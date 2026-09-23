package workshop_test

import (
	"context"
	"testing"

	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One field, whatever the person at the counter happens to be holding.
func TestSearchFindsAVehicleByAnythingToHand(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE vehicles SET vin = 'YV1SW6111234567890' WHERE id = $1`, vehicleA)
		return err
	}); err != nil {
		t.Fatalf("set vin: %v", err)
	}

	for _, c := range []struct{ name, query, matched string }{
		{"the plate as written", "ABC 12D", "registration"},
		{"the plate run together", "abc12d", "registration"},
		{"the plate with a hyphen", "ABC-12D", "registration"},
		{"part of the chassis number", "1234567890", "chassis number"},
		{"the owner's name", "A Customer", "owner"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := workshop.Search(ctx, pool, advisor(), c.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("Search(%q) returned %d results, want 1", c.query, len(got))
			}
			if got[0].VehicleID != vehicleA {
				t.Errorf("found the wrong vehicle")
			}
			if got[0].MatchedOn != c.matched {
				t.Errorf("matched on %q, want %q", got[0].MatchedOn, c.matched)
			}
		})
	}
}

// A technician searching a customer's name must get nothing, and must not be
// shown an owner in a result either.
func TestSearchDoesNotGiveATechnicianNames(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	byName, err := workshop.Search(ctx, pool, technician(), "A Customer")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(byName) != 0 {
		t.Errorf("a technician found %d vehicles by searching a customer's name", len(byName))
	}

	byPlate, err := workshop.Search(ctx, pool, technician(), "ABC 12D")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(byPlate) != 1 {
		t.Fatalf("a technician could not find a vehicle by its plate")
	}
	if byPlate[0].OwnerName != "" {
		t.Errorf("the result carries the owner's name: %q", byPlate[0].OwnerName)
	}
}

// The technical record belongs to the vehicle. Who paid does not.
func TestHistoryFromBeforeTheCurrentOwnerHidesThePersonAndTheMoney(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	// A job, invoiced, under the owner the fixture starts with.
	old := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), old); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// The car is sold: the old ownership ends and a new one begins, after the
	// job above.
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE vehicle_ownership SET owned_to = now() WHERE vehicle_id = $1 AND owned_to IS NULL`,
			vehicleA); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO people (id, shop_id, display_name) VALUES
			 ('22222222-2222-2222-2222-22222222222f', $1, 'The New Owner')`, shopID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO customers (id, shop_id, kind, person_id) VALUES
			 ('44444444-4444-4444-4444-44444444444f', $1, 'private',
			  '22222222-2222-2222-2222-22222222222f')`, shopID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES
			 ($1, $2, '44444444-4444-4444-4444-44444444444f')`, shopID, vehicleA)
		return err
	}); err != nil {
		t.Fatalf("sell the car: %v", err)
	}

	v, err := workshop.VehicleByID(ctx, pool, advisor(), vehicleA)
	if err != nil {
		t.Fatalf("VehicleByID: %v", err)
	}
	if v.OwnerName != "The New Owner" {
		t.Errorf("owner = %q, want The New Owner", v.OwnerName)
	}
	if len(v.History) == 0 {
		t.Fatal("the history did not survive the sale")
	}

	var checked bool
	for _, h := range v.History {
		if !h.PreviousOwner {
			continue
		}
		checked = true
		if h.CustomerName != "" {
			t.Errorf("a job from before the sale still names %q", h.CustomerName)
		}
		if h.Total() != "" {
			t.Errorf("a job from before the sale still shows %q", h.Total())
		}
		if len(h.Lines) == 0 {
			t.Error("the technical record was hidden along with the personal data")
		}
	}
	if !checked {
		t.Fatal("no job was marked as belonging to a previous owner")
	}
}

// A plate that moved away is part of how somebody makes sense of a service
// book that says a different number.
func TestAVehicleKeepsThePlatesItHasCarried(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE vehicle_registrations SET valid_to = now() WHERE vehicle_id = $1`, vehicleA); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised)
			 VALUES ($1, $2, 'NEW 111', 'NEW111')`, shopID, vehicleA)
		return err
	}); err != nil {
		t.Fatalf("change plate: %v", err)
	}

	v, err := workshop.VehicleByID(ctx, pool, advisor(), vehicleA)
	if err != nil {
		t.Fatalf("VehicleByID: %v", err)
	}
	if v.Registration != "NEW 111" {
		t.Errorf("current plate = %q, want NEW 111", v.Registration)
	}
	if len(v.Registrations) != 2 {
		t.Fatalf("got %d plates in the record, want 2", len(v.Registrations))
	}
	if !v.Registrations[0].Current() {
		t.Error("the newest plate is not marked as current")
	}

	// And the old plate still finds the car.
	got, err := workshop.Search(ctx, pool, advisor(), "ABC12D")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].VehicleID != vehicleA {
		t.Error("a plate the car used to carry no longer finds it")
	}
}

// A vehicle with no plate and no chassis number still has to be findable.
func TestAnUnidentifiedVehicleIsFoundByItsLabel(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	var id string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO vehicles (shop_id, label) VALUES ($1, 'Engine on bench, Karlsson') RETURNING id`,
			shopID).Scan(&id)
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := workshop.Search(ctx, pool, advisor(), "bench")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].VehicleID != id {
		t.Fatalf("Search found %d results, want the engine on the bench", len(got))
	}
	if got[0].Describe() != "Engine on bench, Karlsson" {
		t.Errorf("it is described as %q", got[0].Describe())
	}
}

var _ = pgxpool.Pool{}
