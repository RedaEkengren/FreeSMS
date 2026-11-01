package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runningNothing is a scope as the session loads it for a shop that has
// switched every module off.
func runningNothing(s access.Scope) access.Scope {
	s.Modules = nil
	return s
}

// shopModules is what the shop runs, as a session would load it.
func shopModules(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var out []string
	if err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ARRAY(SELECT module FROM shop_modules ORDER BY module)`).Scan(&out)
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A shop nothing has recorded a choice for runs the workshop: every shop the
// migration found, and one set up by the release before after it ran. Its
// first change writes that down before anything else, so switching the
// workshop off does not depend on the rule that kept it on (#143).
func TestAShopWithNoRecordedChoiceRunsTheWorkshop(t *testing.T) {
	pool := setup(t)
	if got := shopModules(t, pool); len(got) != 1 || got[0] != "workshop" {
		t.Fatalf("a shop with no recorded choice runs %v, want [workshop]", got)
	}
	if err := workshop.SwitchModule(context.Background(), pool, owner(), access.Workshop, false, "", true); err != nil {
		t.Fatalf("SwitchModule: %v", err)
	}
	if got := shopModules(t, pool); len(got) != 0 {
		t.Errorf("after switching the workshop off the shop runs %v", got)
	}
	changes, err := workshop.ModuleChanges(context.Background(), pool, owner())
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Enabled || !changes[1].Enabled || !changes[1].Ours() {
		t.Errorf("changes = %+v; want the workshop as it was, then switched off", changes)
	}
}

// Switched off, the workshop's operations refuse with ErrModuleOff and Core's
// go on: what the workshop recorded is read, and paid, through Core (#143).
func TestASwitchedOffWorkshopRefusesNewWorkAndKeepsItsHistory(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	open := newJob(t, pool)

	// Open work is listed, and nothing changes until somebody says they mean it.
	err = workshop.SwitchModule(ctx, pool, owner(), access.Workshop, false, "Closing", false)
	ob, ok := workshop.AsOpenBusiness(err)
	if !ok || len(ob.Items) == 0 || ob.Items[0].Kind != "job" || ob.Items[0].Link != "/jobs/"+open {
		t.Fatalf("switching off over an open job: %v (%+v); want it refused, listing the job", err, ob)
	}
	if got := shopModules(t, pool); len(got) != 1 {
		t.Fatalf("a refused switch changed what the shop runs: %v", got)
	}
	if err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, false, "Closing", true); err != nil {
		t.Fatalf("SwitchModule, confirmed: %v", err)
	}

	off := runningNothing(advisor())
	refused := map[string]error{}
	refused["TakeIn"] = func() error { _, err := workshop.TakeIn(ctx, pool, off, "XYZ 999", "Noise", nil); return err }()
	refused["JobByID"] = func() error { _, _, err := workshop.JobByID(ctx, pool, off, open); return err }()
	refused["AddLine"] = workshop.AddLine(ctx, pool, off, open, workshop.NewLine{Kind: "labour", Description: "x",
		QuantityMilli: 1000, UnitPriceMinor: 100, VATRateBasis: 2500, CostBearer: "customer"})
	refused["Issue"] = func() error { _, err := workshop.Issue(ctx, pool, off, open); return err }()
	refused["Board"] = func() error { _, err := workshop.Board(ctx, pool, off); return err }()
	refused["ClockIn"] = workshop.ClockIn(ctx, pool, runningNothing(technician()), open)
	for name, err := range refused {
		if !errors.Is(err, access.ErrModuleOff) {
			t.Errorf("%s with the workshop off: %v, want ErrModuleOff", name, err)
		}
	}

	if doc, err := workshop.DocumentByID(ctx, pool, off, inv.ID); err != nil || doc.Reference() != inv.Reference() {
		t.Errorf("the workshop's invoice with the workshop off: %v", err)
	}
	if v, err := workshop.VehicleByID(ctx, pool, off, vehicleA); err != nil || len(v.History) == 0 {
		t.Errorf("the vehicle's history with the workshop off: %v (%d entries)", err, len(v.History))
	}
	if _, err := workshop.CustomerByID(ctx, pool, off, "44444444-4444-4444-4444-444444444444"); err != nil {
		t.Errorf("the customer with the workshop off: %v", err)
	}
	amount := int64(100)
	if _, err := workshop.RecordPayment(ctx, pool, off, inv.ID, workshop.NewPayment{AmountMinor: &amount, Method: "bank",
		PaidOn: inv.IssuedAt}); err != nil {
		t.Errorf("a payment against the workshop's invoice with the workshop off: %v", err)
	}
	if items, err := workshop.WaitingFor(ctx, pool, off); err != nil || len(items) != 0 {
		t.Errorf("waiting with the workshop off: %v, %d items; a switched-off module puts nothing in the inbox", err, len(items))
	}

	// On again: the job is where it was.
	if err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, true, "", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.JobByID(ctx, pool, advisor(), open); err != nil {
		t.Errorf("the open job after switching back on: %v", err)
	}
}

// Who may switch, what may be switched on, and that a change is a row
// nobody can rewrite (#143).
func TestSwitchingAModuleIsTheOwnersAndIsKept(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, who := range []access.Scope{advisor(), technician(), partsDesk()} {
		if err := workshop.SwitchModule(ctx, pool, who, access.Workshop, false, "", true); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s switching the workshop off: %v, want ErrForbidden", who.Role, err)
		}
		if _, err := workshop.ModulesFor(ctx, pool, who); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s reading the modules: %v, want ErrForbidden", who.Role, err)
		}
	}
	for _, m := range []access.Module{access.Dealer, access.Rental, "bakery"} {
		if err := workshop.SwitchModule(ctx, pool, owner(), m, true, "", false); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("switching on %q: %v; this release has nothing behind it", m, err)
		}
	}
	// Switching on what is on records nothing.
	if err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, true, "", false); err != nil {
		t.Fatal(err)
	}
	if changes, _ := workshop.ModuleChanges(ctx, pool, owner()); len(changes) != 0 {
		t.Errorf("switching on what was on recorded %d changes", len(changes))
	}
	if err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, false, "Selling the business", true); err != nil {
		t.Fatal(err)
	}
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE shop_module_changes SET enabled = true`)
		return err
	})
	if err == nil {
		t.Error("a module change was rewritten in place; it is the record of who switched what")
	}
	states, err := workshop.ModulesFor(ctx, pool, owner())
	if err != nil || len(states) != 1 || states[0].Module != access.Workshop || states[0].On {
		t.Errorf("modules = %+v, %v; want the workshop, off", states, err)
	}
}

// A clock left running when the workshop is switched off could not be
// stopped -- clocking out is the workshop's, and refused -- and would close
// weeks later with every hour in between counted as work. It is listed with
// what is open, and stops at the moment the workshop does.
func TestSwitchingTheWorkshopOffStopsTheRunningClocks(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	job := newJob(t, pool)
	if err := workshop.ClockIn(ctx, pool, technician(), job); err != nil {
		t.Fatalf("ClockIn: %v", err)
	}

	err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, false, "Closing", false)
	ob, ok := workshop.AsOpenBusiness(err)
	if !ok {
		t.Fatalf("switching off with a clock running: %v, want the open business listed", err)
	}
	var listed bool
	for _, it := range ob.Items {
		listed = listed || (it.Kind == "clock" && it.Link == "/jobs/"+job)
	}
	if !listed {
		t.Errorf("the running clock is not listed: %+v", ob.Items)
	}
	if err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, false, "Closing", true); err != nil {
		t.Fatal(err)
	}
	var running int
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM time_entries WHERE ended_at IS NULL`).Scan(&running)
	}); err != nil {
		t.Fatal(err)
	}
	if running != 0 {
		t.Errorf("%d clocks still running after the workshop was switched off", running)
	}
}
