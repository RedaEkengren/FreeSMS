package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Vehicle lookup (#155). No test reaches a real provider: fakeLookup
// answers as one would.

type fakeLookup struct {
	v     vehicledata.Vehicle
	err   error
	calls int
}

func (f *fakeLookup) Lookup(context.Context, string) (vehicledata.Vehicle, error) {
	f.calls++
	return f.v, f.err
}

// A lookup is a row -- who, which registration, why, which provider, what
// it said -- and its answer is read again for a day rather than asked for,
// unless a refresh is; a plate moved to another car since is asked again.
// A typed VIN that differs is a conflict. Found, not found and failed are
// all answers, never an error.
func TestALookupIsARowWithItsSourceAndAge(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := advisor()
	f := &fakeLookup{v: vehicledata.Vehicle{Make: "Polestar", Model: "2", VIN: vehicledata.Of("YV1SW58D"),
		DrivingBan: vehicledata.Of(false), OdometerKm: vehicledata.Of(int64(184500))}}

	got, err := workshop.LookUpVehicle(ctx, pool, desk, f, "Bilregistret", "abc 12 d", workshop.LookupIntake, "", false)
	if err != nil || got.Outcome != workshop.LookupFound || got.Cached || got.Vehicle != f.v || got.Provider != "Bilregistret" {
		t.Fatalf("first lookup: %+v, %v", got, err)
	}
	again, err := workshop.LookUpVehicle(ctx, pool, desk, f, "Bilregistret", "ABC12D", workshop.LookupIntake, "YV1XXXXX", false)
	if err != nil || !again.Cached || f.calls != 1 || again.Vehicle != f.v || again.At.IsZero() {
		t.Errorf("the same plate again: %+v, %v, provider asked %d times", again, err, f.calls)
	}
	if !again.VINConflict {
		t.Error("a typed VIN unlike the provider's is no conflict")
	}
	if _, err := workshop.LookUpVehicle(ctx, pool, desk, f, "Bilregistret", "ABC12D", workshop.LookupIntake, "", true); err != nil || f.calls != 2 {
		t.Errorf("a refresh: %v, provider asked %d times", err, f.calls)
	}

	f.err = vehicledata.ErrNotFound
	if got, err := workshop.LookUpVehicle(ctx, pool, desk, f, "Bilregistret", "DE 123 X", workshop.LookupIntake, "", false); err != nil || got.Outcome != workshop.LookupNotFound {
		t.Errorf("a foreign plate: %+v, %v", got, err)
	}
	f.err = vehicledata.ErrRateLimited
	if got, err := workshop.LookUpVehicle(ctx, pool, desk, f, "Bilregistret", "XYZ987", workshop.LookupIntake, "", false); err != nil || got.Outcome != workshop.LookupFailed {
		t.Errorf("a provider holding back: %+v, %v", got, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM vehicle_lookups WHERE looked_up_by IS NOT NULL AND purpose = 'intake' AND provider = 'Bilregistret'`); n != 5 {
		t.Errorf("%d lookups recorded, want 5", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM vehicle_lookups WHERE outcome = 'found' AND facts->>'make' = 'Polestar' AND facts->>'driving_ban' = 'false' AND NOT facts ? 'tax'`); n != 2 {
		t.Errorf("%d lookups with the facts as given, want 2 (a fact not given is no key)", n)
	}
	// Nothing a lookup says is written into the vehicle a person typed.
	if n := countRows(t, pool, `SELECT count(*) FROM vehicles WHERE make = 'Polestar'`); n != 0 {
		t.Errorf("%d vehicles written from a lookup", n)
	}

	// The plate moves to another car: the old answer is not this car's.
	if _, err := pool.Exec(ctx, `
		INSERT INTO vehicles (id, shop_id) VALUES ('99999999-0000-0000-0000-000000000155', '`+shopID+`');
		UPDATE vehicle_registrations SET valid_to = now() WHERE normalised = 'ABC12D' AND valid_to IS NULL;
		INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised, valid_from)
		VALUES ('`+shopID+`', '99999999-0000-0000-0000-000000000155', 'ABC 12D', 'ABC12D', now())`); err != nil {
		t.Fatal(err)
	}
	f.err = nil
	if got, err := workshop.LookUpVehicle(ctx, pool, desk, f, "Bilregistret", "ABC12D", workshop.LookupIntake, "", false); err != nil || got.Cached || f.calls != 5 {
		t.Errorf("after the plate moved: %+v, %v, provider asked %d times", got, err, f.calls)
	}

	// The log, for whoever runs the shop.
	if _, err := workshop.VehicleLookupLog(ctx, pool, desk, "ABC12D"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk read the lookup log: %v", err)
	}
	if log, err := workshop.VehicleLookupLog(ctx, pool, owner(), "abc12d"); err != nil || len(log) != 4 || log[0].By == "" {
		t.Errorf("the lookup log: %+v, %v", log, err)
	}
}

// Who may look up, and for what: the front desk at intake in the
// workshop, the buyer for a purchase, whoever keeps the fleet for a rental
// unit -- each only in a shop that runs the module. A refusal asks nobody.
func TestALookupIsItsModulesAndItsRoles(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	f := &fakeLookup{v: vehicledata.Vehicle{Make: "Volvo"}}
	for name, c := range map[string]struct {
		s       access.Scope
		purpose string
		want    error
	}{
		"technician, intake":       {technician(), workshop.LookupIntake, access.ErrForbidden},
		"sales, a purchase":        {otherRoles(access.Of(access.RoleSales)), workshop.LookupPurchase, access.ErrForbidden},
		"buyer, a purchase, off":   {func() access.Scope { s := buyerScope(); s.Modules = []access.Module{access.Workshop}; return s }(), workshop.LookupPurchase, access.ErrModuleOff},
		"front desk, intake, off":  {func() access.Scope { s := advisor(); s.Modules = []access.Module{access.Rental}; return s }(), workshop.LookupIntake, access.ErrModuleOff},
		"buyer, a purchase":        {buyerScope(), workshop.LookupPurchase, nil},
		"rental desk, a unit":      {otherRoles(access.Of(access.RoleRentalAgent)), workshop.LookupRentalUnit, nil},
		"rental desk, no such why": {otherRoles(access.Of(access.RoleRentalAgent)), "curiosity", workshop.ErrInvalid},
	} {
		before := f.calls
		_, err := workshop.LookUpVehicle(ctx, pool, c.s, f, "Bilregistret", "ABC12D", c.purpose, "", true)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if c.want != nil && f.calls != before {
			t.Errorf("%s: refused, and the provider was asked anyway", name)
		}
	}
	// With no provider there is nobody to ask: not an outcome, the shop has
	// not set it up.
	if _, err := workshop.LookUpVehicle(ctx, pool, advisor(), vehicledata.None{}, "", "ABC12D", workshop.LookupIntake, "", false); !errors.Is(err, vehicledata.ErrNotConfigured) {
		t.Errorf("no provider: %v", err)
	}
}

// A list goes through the outbox, one call each, at the worker's pace;
// the provider rate limiting is retried, not lost; the record is as if it
// had been looked up by whoever queued it. The record is kept a year.
func TestAListOfLookupsIsQueuedNotHammered(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	f := &fakeLookup{err: vehicledata.ErrRateLimited}
	n, err := workshop.QueueVehicleLookups(ctx, pool, buyerScope(), workshop.LookupPurchase, []string{"ABC 12D", "abc12d", "XYZ 987", ""})
	if err != nil || n != 2 {
		t.Fatalf("queued %d, %v; want the two plates once each", n, err)
	}
	if _, err := workshop.QueueVehicleLookups(ctx, pool, advisor(), workshop.LookupIntake, []string{"ABC12D"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a list at intake: %v", err)
	}
	adapters := map[string]workshop.OutboxAdapter{"vehicle_lookup": workshop.VehicleLookupAdapter{Pool: pool, Lookup: f, Provider: "Bilregistret"}}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); err != nil || run.Retried != 2 || run.Sent != 0 {
		t.Errorf("with the provider holding back: %+v, %v", run, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET retry_at = now() - interval '1 second' WHERE event = 'retry_scheduled'`); err != nil {
		t.Fatal(err)
	}
	f.err, f.v = nil, vehicledata.Vehicle{Make: "Volvo"}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); err != nil || run.Sent != 2 {
		t.Errorf("once it answers: %+v, %v", run, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM vehicle_lookups WHERE purpose = 'purchase' AND outcome = 'found' AND looked_up_by IS NOT NULL`); n != 2 {
		t.Errorf("%d lookups recorded from the list, want 2", n)
	}
	if _, err := pool.Exec(ctx, `UPDATE vehicle_lookups SET looked_up_at = now() - interval '366 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.Sweep(ctx, pool, shopID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM vehicle_lookups`); n != 0 {
		t.Errorf("%d lookups kept past a year", n)
	}
}
