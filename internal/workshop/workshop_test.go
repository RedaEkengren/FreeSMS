package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/testsupport"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/RedaEkengren/RedaSMS/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	shopID   = "11111111-1111-1111-1111-111111111111"
	userID   = "33333333-3333-3333-3333-333333333333"
	vehicleA = "55555555-5555-5555-5555-555555555555"
)

func advisor() access.Scope {
	return access.Scope{ShopID: shopID, UserID: userID, Role: access.RoleServiceAdvisor}
}

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool := testsupport.FreshPool(t)
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{
			`INSERT INTO shops (id, name) VALUES ('` + shopID + `', 'Verkstaden')`,
			`INSERT INTO people (id, shop_id, display_name) VALUES
			 ('22222222-2222-2222-2222-222222222222','` + shopID + `','An Advisor')`,
			`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
			 ('` + userID + `','` + shopID + `','22222222-2222-2222-2222-222222222222','service_advisor','x')`,
			`INSERT INTO vehicles (id, shop_id, make, model) VALUES
			 ('` + vehicleA + `','` + shopID + `','Volvo','V70')`,
			`INSERT INTO people (id, shop_id, display_name) VALUES
			 ('22222222-2222-2222-2222-222222222223','` + shopID + `','A Customer')`,
			`INSERT INTO customers (id, shop_id, kind, person_id) VALUES
			 ('44444444-4444-4444-4444-444444444444','` + shopID + `','private',
			  '22222222-2222-2222-2222-222222222223')`,
			`INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES
			 ('` + shopID + `','` + vehicleA + `','44444444-4444-4444-4444-444444444444')`,
			// TakeIn finds a vehicle by its current registration, so without
			// this it creates a second one -- which has no owner, and then
			// cannot be invoiced.
			`INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised) VALUES
			 ('` + shopID + `','` + vehicleA + `','ABC 12D','ABC12D')`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool
}

func newJob(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := workshop.TakeIn(context.Background(), pool, advisor(), "ABC 12D", "Grinding", nil)
	if err != nil {
		t.Fatalf("TakeIn: %v", err)
	}
	return id
}

func move(t *testing.T, pool *pgxpool.Pool, id string, states ...workshop.State) {
	t.Helper()
	for _, s := range states {
		if err := workshop.SetState(context.Background(), pool, advisor(), id, s); err != nil {
			t.Fatalf("SetState(%s): %v", s, err)
		}
	}
}

func TestSetStateRefusesAnIllegalMove(t *testing.T) {
	pool := setup(t)
	id := newJob(t, pool)

	err := workshop.SetState(context.Background(), pool, advisor(), id, workshop.StateInvoiced)
	var illegal workshop.ErrIllegalTransition
	if !errors.As(err, &illegal) {
		t.Fatalf("SetState() = %v, want an illegal transition error", err)
	}
	if illegal.From != workshop.StateDraft {
		t.Errorf("error says from %s, want draft", illegal.From)
	}
}

// Two taps on a slow connection must not produce an error.
func TestSetStateToTheCurrentStateIsANoOp(t *testing.T) {
	pool := setup(t)
	id := newJob(t, pool)
	if err := workshop.SetState(context.Background(), pool, advisor(), id, workshop.StateDraft); err != nil {
		t.Errorf("moving to the state it is already in returned %v", err)
	}
}

// Cancelled means it never happened. An order with hours or lines on it did
// happen, and cancelling it writes off work somebody did.
func TestCancellingAnOrderWithWorkIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := newJob(t, pool)

	if err := workshop.AddLine(ctx, pool, advisor(), id, workshop.NewLine{
		Kind: "labour", Description: "Strip and inspect", Quantity: 1,
		UnitPriceMinor: 89500, VATRateBasis: 2500,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}

	err := workshop.SetState(ctx, pool, advisor(), id, workshop.StateCancelled)
	if !errors.Is(err, workshop.ErrWouldLoseWork) {
		t.Fatalf("SetState(cancelled) = %v, want ErrWouldLoseWork", err)
	}
	if !contains(err.Error(), "decline") {
		t.Errorf("the refusal does not point at declining instead: %s", err)
	}
}

// An order nobody has touched is a different matter.
func TestAnEmptyOrderCanBeCancelled(t *testing.T) {
	pool := setup(t)
	id := newJob(t, pool)
	if err := workshop.SetState(context.Background(), pool, advisor(), id, workshop.StateCancelled); err != nil {
		t.Errorf("cancelling an empty draft returned %v", err)
	}
}

// The path the whole thing exists for, end to end.
func TestDeclinedAfterTeardownReachesAnInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := newJob(t, pool)

	move(t, pool, id,
		workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress)

	if err := workshop.SetState(ctx, pool, advisor(), id, workshop.StateDeclined); err != nil {
		t.Fatalf("declining a job in progress: %v", err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), id, workshop.StateInvoiced); err != nil {
		t.Fatalf("invoicing a declined job: %v", err)
	}
}

// A line added before approval is part of what the customer agreed to. One
// added afterwards is not, and must say so rather than being quietly included.
func TestLinesAddedAfterApprovalAreNotApproved(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := newJob(t, pool)

	quoted := workshop.NewLine{Kind: "labour", Description: "Brake pads", Quantity: 1,
		UnitPriceMinor: 89500, VATRateBasis: 2500}
	if err := workshop.AddLine(ctx, pool, advisor(), id, quoted); err != nil {
		t.Fatalf("AddLine before approval: %v", err)
	}

	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress)

	extra := workshop.NewLine{Kind: "part", Description: "Seized bolt, drilled out", Quantity: 1,
		UnitPriceMinor: 24000, VATRateBasis: 2500}
	if err := workshop.AddLine(ctx, pool, advisor(), id, extra); err != nil {
		t.Fatalf("AddLine after approval: %v", err)
	}

	_, lines, err := workshop.JobByID(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if !lines[0].Approved() {
		t.Error("a line quoted before approval is not marked approved")
	}
	if lines[1].Approved() {
		t.Error("a line added after approval is marked approved; the customer never agreed to it")
	}
	if lines[0].EstimatedMinor == nil {
		t.Error("a quoted line did not record what it was quoted at")
	}
	if lines[1].EstimatedMinor != nil {
		t.Error("a line that was never quoted recorded an estimate")
	}
}

// A warranty line is free by definition, and the schema says so too.
func TestAWarrantyLineMustBeFree(t *testing.T) {
	pool := setup(t)
	id := newJob(t, pool)
	err := workshop.AddLine(context.Background(), pool, advisor(), id, workshop.NewLine{
		Kind: "part", Description: "Caliper", Quantity: 1,
		UnitPriceMinor: 145000, VATRateBasis: 2500, CostBearer: "supplier",
	})
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("AddLine() = %v, want ErrInvalid", err)
	}
}

// Nothing is added to a document that has been issued.
func TestNothingCanBeAddedToAnInvoicedOrder(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := newJob(t, pool)

	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady,
		workshop.StateInvoiced)

	err := workshop.AddLine(ctx, pool, advisor(), id, workshop.NewLine{
		Kind: "fee", Description: "One more thing", Quantity: 1, VATRateBasis: 2500,
	})
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("AddLine to an invoiced order = %v, want ErrInvalid", err)
	}
}

// A car taken in with nobody identified -- left overnight, keys through the
// letterbox -- can be worked on and cannot be invoiced, and the refusal says
// which.
func TestAnOrderWithNoCustomerCannotBeInvoiced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	// A plate nobody has seen, so no ownership record follows it.
	id, err := workshop.TakeIn(ctx, pool, advisor(), "ZZZ 999", "Left overnight", nil)
	if err != nil {
		t.Fatalf("TakeIn: %v", err)
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)

	err = workshop.SetState(ctx, pool, advisor(), id, workshop.StateInvoiced)
	if !errors.Is(err, workshop.ErrNoCustomer) {
		t.Fatalf("SetState(invoiced) = %v, want ErrNoCustomer", err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
