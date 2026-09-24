package workshop_test

import (
	"context"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const stockedPart = "eeeeeeee-0000-0000-0000-000000000001"

func stockShelf(t *testing.T, pool *pgxpool.Pool, quantity float64) {
	t.Helper()
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO parts (id, shop_id, number, name, unit, cost_minor)
			VALUES ($1, $2, 'BP-100', 'Brake pad set, front', 'each', 45000)
			ON CONFLICT DO NOTHING`, stockedPart, shopID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO stock_movements (shop_id, part_id, kind, quantity, unit_cost_minor)
			VALUES ($1, $2, 'received', $3, 45000)`, shopID, stockedPart, quantity)
		return err
	})
	if err != nil {
		t.Fatalf("stock the shelf: %v", err)
	}
}

func shelf(t *testing.T, pool *pgxpool.Pool) workshop.Part {
	t.Helper()
	parts, err := workshop.Parts(context.Background(), pool, advisor())
	if err != nil {
		t.Fatalf("Parts: %v", err)
	}
	for _, p := range parts {
		if p.ID == stockedPart {
			return p
		}
	}
	t.Fatal("the part is not in the list")
	return workshop.Part{}
}

// The morning that found this: a brake pad set priced onto a job, invoiced,
// and still on the shelf afterwards.
func TestPricingAPartReservesItAndInvoicingTakesItOff(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 6)

	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "part", Description: "Brake pad set, front (BP-100)", QuantityMilli: 1000,
		UnitPriceMinor: 65250, VATRateBasis: 2500, PartID: stockedPart,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}

	p := shelf(t, pool)
	if p.OnHand != 6 {
		t.Errorf("on hand = %v after pricing, want 6 -- pricing does not take it off the shelf", p.OnHand)
	}
	if p.Reserved != 1 {
		t.Errorf("reserved = %v, want 1", p.Reserved)
	}
	if p.Available() != 5 {
		t.Errorf("available = %v, want 5", p.Available())
	}

	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	p = shelf(t, pool)
	if p.OnHand != 5 {
		t.Errorf("on hand = %v after invoicing, want 5 -- the part was sold and never left stock", p.OnHand)
	}
	if p.Reserved != 0 {
		t.Errorf("reserved = %v after invoicing, want 0", p.Reserved)
	}
	if p.Available() != 5 {
		t.Errorf("available = %v, want 5 -- not 4, which is what double counting gives", p.Available())
	}
}

// A warranty replacement costs the customer nothing and the part left the
// shelf all the same.
func TestAWarrantyPartStillLeavesTheShelf(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 4)

	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "part", Description: "Brake pad set (warranty)", QuantityMilli: 1000,
		UnitPriceMinor: 0, VATRateBasis: 2500, CostBearer: "supplier", PartID: stockedPart,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	// Something to charge for, or there is no invoice.
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Fitting", QuantityMilli: 1000,
		UnitPriceMinor: 89500, VATRateBasis: 2500,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}

	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got := shelf(t, pool).OnHand; got != 3 {
		t.Errorf("on hand = %v, want 3 -- whoever is paying, the part is gone", got)
	}
}

// A declined job releases what it had put aside and consumes nothing.
func TestADeclinedJobReturnsItsPartsToTheShelf(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 6)

	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "part", Description: "Brake pad set", QuantityMilli: 2000,
		UnitPriceMinor: 65250, VATRateBasis: 2500, PartID: stockedPart,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	if got := shelf(t, pool).Available(); got != 4 {
		t.Fatalf("available = %v, want 4", got)
	}

	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateDeclined)

	p := shelf(t, pool)
	if p.Available() != 6 || p.OnHand != 6 {
		t.Errorf("after declining: %v on hand, %v available; want 6 and 6", p.OnHand, p.Available())
	}
}

// The shop may well be ordering more. Refusing sends somebody to a
// spreadsheet.
func TestReservingMoreThanIsThereIsAllowedAndVisible(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 1)

	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "part", Description: "Brake pad set", QuantityMilli: 3000,
		UnitPriceMinor: 65250, VATRateBasis: 2500, PartID: stockedPart,
	}); err != nil {
		t.Fatalf("AddLine was refused: %v", err)
	}
	if got := shelf(t, pool).Available(); got != -2 {
		t.Errorf("available = %v, want -2 -- the shortfall has to be visible", got)
	}
}

// Handing the car back is the one moment somebody is certainly finished.
func TestSayingReadyStopsTheCallersClock(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	job := working(t, pool)
	entries, _ := workshop.TimeFor(ctx, pool, advisor(), job)
	if len(entries) != 1 || !entries[0].Running() {
		t.Fatalf("expected one running entry, got %+v", entries)
	}

	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Fatalf("SetState(ready): %v", err)
	}

	entries, _ = workshop.TimeFor(ctx, pool, advisor(), job)
	if entries[0].Running() {
		t.Error("the clock is still running after the car was handed back")
	}
	if entries[0].Flagged {
		t.Error("a short stretch was flagged")
	}
}

// A second technician on the same gearbox has not finished because this one
// handed the keys over.
func TestSayingReadyLeavesSomebodyElsesClockAlone(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	job := working(t, pool)
	// The advisor is also on it -- a small shop, the owner turns spanners.
	if err := workshop.ClockIn(ctx, pool, advisor(), job); err != nil {
		t.Fatalf("ClockIn: %v", err)
	}
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Fatalf("SetState(ready): %v", err)
	}

	entries, _ := workshop.TimeFor(ctx, pool, advisor(), job)
	var running int
	for _, e := range entries {
		if e.Running() {
			running++
		}
	}
	if running != 1 {
		t.Errorf("%d clocks running, want the other person's still going", running)
	}

	// And the job page says so.
	job2, _, err := workshop.JobByID(ctx, pool, technician(), job)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job2.OthersRunning == "" {
		t.Error("the page does not say somebody else is still on it")
	}
}

// A day in progress read as nothing, which is wrong for the period somebody
// actually looks at.
func TestTheFiguresCountAClockThatIsStillRunning(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	working(t, pool)
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE time_entries SET started_at = now() - interval '3 hours' WHERE ended_at IS NULL`)
		return err
	}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	d, err := workshop.Summary(ctx, pool, advisor(), from, from.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if d.HoursClocked < 2.9 || d.HoursClocked > 3.1 {
		t.Errorf("hours clocked = %.2f, want about 3 -- a running clock counts up to now", d.HoursClocked)
	}
	if len(d.Technicians) == 0 || d.Technicians[0].Clocked < 2.9 {
		t.Errorf("per person = %+v, want the running hours", d.Technicians)
	}
}

var _ = access.RoleParts
