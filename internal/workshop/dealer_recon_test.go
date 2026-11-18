package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The reconditioning plan (#236): steps as rows, each with the moves it has
// been through.

func stepNamed(t *testing.T, plan workshop.ReconPlan, name string) workshop.ReconStep {
	t.Helper()
	for _, s := range plan.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in %+v", name, plan.Steps)
	return workshop.ReconStep{}
}

// A car's plan is made once from the template, a step is started, finished,
// skipped or reopened and every move is a row; a move that does not follow
// from where the step is, is refused.
func TestAPlanIsMadeOnceAndItsStepsMoveAsRows(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := boughtCar(t, pool)
	if err := workshop.StartReconPlan(ctx, pool, sales(), p.StockUnitID); err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartReconPlan(ctx, pool, sales(), p.StockUnitID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second plan for the car: %v", err)
	}
	plan, err := workshop.ReconPlanOf(ctx, pool, sales(), p.StockUnitID)
	if err != nil || len(plan.Steps) != 5 || plan.Steps[0].Name != "Intake check" || plan.Current() == nil || plan.Current().Name != "Intake check" {
		t.Fatalf("the plan: %+v, %v", plan, err)
	}
	for _, s := range plan.Steps {
		if s.State != workshop.ReconWaiting {
			t.Errorf("%s is %s before anything was done", s.Name, s.State)
		}
	}
	intake, workshopStep := stepNamed(t, plan, "Intake check"), stepNamed(t, plan, "Workshop")
	move := func(id, how string) error {
		return workshop.MoveReconStep(ctx, pool, sales(), id, how, "")
	}
	for _, bad := range []struct{ id, how string }{
		{intake.ID, workshop.ReconFinish}, {intake.ID, workshop.ReconReopen}, {intake.ID, "teleported"},
	} {
		if err := move(bad.id, bad.how); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s of a step that is waiting: %v", bad.how, err)
		}
	}
	for _, how := range []string{workshop.ReconStart, workshop.ReconFinish} {
		if err := move(intake.ID, how); err != nil {
			t.Fatalf("%s: %v", how, err)
		}
	}
	if err := move(intake.ID, workshop.ReconStart); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a finished step started again: %v", err)
	}
	if err := move(workshopStep.ID, workshop.ReconSkip); err != nil {
		t.Fatal(err)
	}
	plan, _ = workshop.ReconPlanOf(ctx, pool, sales(), p.StockUnitID)
	if got := stepNamed(t, plan, "Intake check").State; got != workshop.ReconDone {
		t.Errorf("intake: %s", got)
	}
	if got := stepNamed(t, plan, "Workshop").State; got != workshop.ReconSkipped {
		t.Errorf("a skipped step: %s", got)
	}
	if plan.Current().Name != "Valet" {
		t.Errorf("the current step after one was done and one skipped: %s", plan.Current().Name)
	}
	// Redone: finished and then reopened, with the moves kept.
	if err := move(intake.ID, workshop.ReconReopen); err != nil {
		t.Fatal(err)
	}
	plan, _ = workshop.ReconPlanOf(ctx, pool, sales(), p.StockUnitID)
	if got := stepNamed(t, plan, "Intake check"); got.State != workshop.ReconInProgress || plan.Current().Name != "Intake check" {
		t.Errorf("a reopened step: %+v, current %s", got, plan.Current().Name)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recon_step_events WHERE step_id = $1`, intake.ID).Scan(&events); err != nil || events != 3 {
		t.Errorf("the moves of the step: %d, %v; want started, done, reopened", events, err)
	}
}

// A shop's template replaces the steps new plans start with, and a plan made
// before it is left as it was.
func TestAShopsTemplateIsWhatNewPlansAreMadeFrom(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	first := boughtCar(t, pool)
	if err := workshop.StartReconPlan(ctx, pool, sales(), first.StockUnitID); err != nil {
		t.Fatal(err)
	}
	mine := []workshop.ReconTemplateStep{{Name: "Paint", Kind: workshop.ReconOutside}, {Name: "Photographs", Kind: workshop.ReconInHouse}}
	for _, s := range []access.Scope{buyerScope(), sales(), everything(technician())} {
		if err := workshop.SetReconTemplate(ctx, pool, s, mine); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s set the template: %v", s.Role, err)
		}
	}
	for _, bad := range [][]workshop.ReconTemplateStep{nil, {{Name: "", Kind: workshop.ReconInHouse}}, {{Name: "x", Kind: "magic"}}} {
		if err := workshop.SetReconTemplate(ctx, pool, everything(owner()), bad); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("template %+v accepted: %v", bad, err)
		}
	}
	if err := workshop.SetReconTemplate(ctx, pool, everything(owner()), mine); err != nil {
		t.Fatal(err)
	}
	second := boughtCar(t, pool)
	if err := workshop.StartReconPlan(ctx, pool, sales(), second.StockUnitID); err != nil {
		t.Fatal(err)
	}
	plan, _ := workshop.ReconPlanOf(ctx, pool, sales(), second.StockUnitID)
	if len(plan.Steps) != 2 || plan.Steps[0].Name != "Paint" || plan.Steps[0].Kind != workshop.ReconOutside {
		t.Errorf("the plan from the shop's template: %+v", plan.Steps)
	}
	if old, _ := workshop.ReconPlanOf(ctx, pool, sales(), first.StockUnitID); len(old.Steps) != 5 {
		t.Errorf("a plan made before the template changed: %d steps", len(old.Steps))
	}
	// A step added to one car is that car's.
	if _, err := workshop.AddReconStep(ctx, pool, sales(), second.StockUnitID, workshop.NewReconStep{Name: "Glass", Kind: workshop.ReconOutside, Owner: "Glasmästarn"}); err != nil {
		t.Fatal(err)
	}
	if plan, _ := workshop.ReconPlanOf(ctx, pool, sales(), second.StockUnitID); len(plan.Steps) != 3 || plan.Steps[2].Name != "Glass" || plan.Steps[2].Owner != "Glasmästarn" {
		t.Errorf("the car's plan with a step of its own: %+v", plan.Steps)
	}
	if plan, _ := workshop.ReconPlanOf(ctx, pool, sales(), first.StockUnitID); len(plan.Steps) != 5 {
		t.Errorf("another car's plan got the step: %d", len(plan.Steps))
	}
}

// What was agreed for a step is a cost: stated and read only by whoever may
// see what cars cost, and who may read the plan at all is whoever buys or
// sells cars, in a shop that sells them.
func TestWhatWasAgreedForAStepIsForWhoMaySeeCosts(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := boughtCar(t, pool)
	price := int64(450_000)
	step := workshop.NewReconStep{Name: "Lackering", Kind: workshop.ReconOutside, Owner: "Lackaren AB", AgreedPriceMinor: &price}
	if _, err := workshop.AddReconStep(ctx, pool, sales(), p.StockUnitID, step); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson stating a price: %v", err)
	}
	if _, err := workshop.AddReconStep(ctx, pool, buyerScope(), p.StockUnitID, step); err != nil {
		t.Fatal(err)
	}
	if plan, _ := workshop.ReconPlanOf(ctx, pool, buyerScope(), p.StockUnitID); len(plan.Steps) != 1 || plan.Steps[0].AgreedPriceMinor == nil || *plan.Steps[0].AgreedPriceMinor != price {
		t.Errorf("the buyer's plan: %+v", plan.Steps)
	}
	if plan, _ := workshop.ReconPlanOf(ctx, pool, sales(), p.StockUnitID); len(plan.Steps) != 1 || plan.Steps[0].AgreedPriceMinor != nil {
		t.Errorf("the salesperson was handed the price: %+v", plan.Steps)
	}
	for _, s := range []access.Scope{everything(technician()), everything(advisor()), rentalDesk()} {
		if _, err := workshop.ReconPlanOf(ctx, pool, s, p.StockUnitID); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the plan: %v", s.Role, err)
		}
		if err := workshop.StartReconPlan(ctx, pool, s, p.StockUnitID); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s started a plan: %v", s.Role, err)
		}
		if _, err := workshop.ReconPipeline(ctx, pool, s); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the pipeline: %v", s.Role, err)
		}
	}
	off := buyerScope()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.ReconPipeline(ctx, pool, off); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("the pipeline with Dealer off: %v", err)
	}
}

// The pipeline puts each car in the step it stands in and for how long, the
// car that has stood longest first, and a car that stands too long is one
// item on the queue. A car with its plan finished, or sold off, is not on it.
func TestThePipelineShowsWhereEachCarStandsAndHowLong(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	a, b, c := boughtCar(t, pool), boughtCar(t, pool), boughtCar(t, pool)
	for _, p := range []workshop.Purchase{a, b, c} {
		if err := workshop.StartReconPlan(ctx, pool, sales(), p.StockUnitID); err != nil {
			t.Fatal(err)
		}
	}
	stepOf := func(unit, name string) string {
		plan, _ := workshop.ReconPlanOf(ctx, pool, sales(), unit)
		return stepNamed(t, plan, name).ID
	}
	// b is at the intake check, started 9 days ago; a started a Valet 2 days
	// ago after the first steps were done; c is finished throughout.
	if err := workshop.MoveReconStep(ctx, pool, sales(), stepOf(b.StockUnitID, "Intake check"), workshop.ReconStart, "Väntar på nyckel"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Intake check", "Workshop"} {
		id := stepOf(a.StockUnitID, name)
		for _, how := range []string{workshop.ReconStart, workshop.ReconFinish} {
			if err := workshop.MoveReconStep(ctx, pool, sales(), id, how, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := workshop.MoveReconStep(ctx, pool, sales(), stepOf(a.StockUnitID, "Valet"), workshop.ReconStart, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Intake check", "Workshop", "Valet", "Photographs", "Ad"} {
		id := stepOf(c.StockUnitID, name)
		for _, how := range []string{workshop.ReconStart, workshop.ReconFinish} {
			if err := workshop.MoveReconStep(ctx, pool, sales(), id, how, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	back := func(step string, days int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE recon_step_events SET at = clock_timestamp() - make_interval(days => $2) WHERE step_id = $1`, step, days); err != nil {
			t.Fatal(err)
		}
	}
	back(stepOf(b.StockUnitID, "Intake check"), 9)
	back(stepOf(a.StockUnitID, "Valet"), 2)
	if _, err := pool.Exec(ctx, `UPDATE stock_units SET opened_at = clock_timestamp() - make_interval(days => 15) WHERE id = $1`, b.StockUnitID); err != nil {
		t.Fatal(err)
	}

	rows, err := workshop.ReconPipeline(ctx, pool, sales())
	if err != nil || len(rows) != 2 {
		t.Fatalf("the pipeline: %+v, %v", rows, err)
	}
	if rows[0].UnitID != b.StockUnitID || rows[0].StepName != "Intake check" || rows[0].DaysInStep != 9 || rows[0].DaysSinceBuy != 15 ||
		rows[0].StepsDone != 0 || rows[0].StepsTotal != 5 || !rows[0].Stalled() || rows[0].StepState != workshop.ReconInProgress {
		t.Errorf("the car that has stood longest: %+v", rows[0])
	}
	if rows[1].UnitID != a.StockUnitID || rows[1].StepName != "Valet" || rows[1].DaysInStep != 2 || rows[1].StepsDone != 2 || rows[1].Stalled() {
		t.Errorf("the car under way: %+v", rows[1])
	}

	stalled := func(s access.Scope) (n int) {
		t.Helper()
		items, err := workshop.WaitingFor(ctx, pool, s)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Kind == "recon_stalled" {
				if it.SubjectKind != "pipeline" || it.Text != "" {
					t.Errorf("the item: %+v", it)
				}
				n += it.Count
			}
		}
		return n
	}
	for _, s := range []access.Scope{sales(), buyerScope(), everything(owner())} {
		if n := stalled(s); n != 1 {
			t.Errorf("%s sees %d stalled cars on the queue, want 1", s.Role, n)
		}
	}
	for _, s := range []access.Scope{everything(technician()), everything(advisor())} {
		if n := stalled(s); n != 0 {
			t.Errorf("%s sees %d stalled cars", s.Role, n)
		}
	}

	// Sold off before the plan was finished: it leaves the pipeline and the
	// queue, and its plan stays to read.
	moveStock(t, pool, b.StockUnitID, workshop.StockForSale, workshop.StockSold, workshop.StockDelivered)
	if rows, _ := workshop.ReconPipeline(ctx, pool, sales()); len(rows) != 1 {
		t.Errorf("the pipeline after a car was delivered: %+v", rows)
	}
	if n := stalled(sales()); n != 0 {
		t.Errorf("the queue after the stalled car was delivered: %d", n)
	}
	if plan, _ := workshop.ReconPlanOf(ctx, pool, sales(), b.StockUnitID); len(plan.Steps) != 5 {
		t.Errorf("the delivered car's plan: %+v", plan.Steps)
	}
	if err := workshop.MoveReconStep(ctx, pool, sales(), stepOf(b.StockUnitID, "Workshop"), workshop.ReconStart, ""); err == nil {
		t.Error("a step of a car already out of stock was started")
	}
}
