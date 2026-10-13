package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A car went straight from "it is ready" to the board, with nobody asked to
// check the work.

func requireFinalCheck(t *testing.T, pool *pgxpool.Pool, byAnother bool) {
	t.Helper()
	addTemplate(t, pool)
	if err := workshop.SaveFinalCheck(context.Background(), pool, owner(),
		workshop.FinalCheckSettings{TemplateID: templateID, ByAnother: byAnother}); err != nil {
		t.Fatalf("SaveFinalCheck: %v", err)
	}
}

// finalCheck runs the shop's final check on a job as somebody, failing the
// first item when told to.
func finalCheck(t *testing.T, pool *pgxpool.Pool, who access.Scope, job string, fail bool) {
	t.Helper()
	ctx := context.Background()
	id, err := workshop.StartInspection(ctx, pool, who, job, templateID)
	if err != nil {
		t.Fatalf("StartInspection: %v", err)
	}
	insp, _ := workshop.InspectionsForID(ctx, pool, who, id)
	for i, it := range insp.Items {
		status := "pass"
		if fail && i == 0 {
			status = "fail"
		}
		if err := workshop.SetItem(ctx, pool, who, it.ID, status, "Glapp i spindelleden"); err != nil {
			t.Fatalf("SetItem: %v", err)
		}
	}
	if err := workshop.CompleteInspection(ctx, pool, who, id); err != nil {
		t.Fatalf("CompleteInspection: %v", err)
	}
}

func TestReadyWaitsForTheFinalCheckWhereTheShopRequiresOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, false)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)

	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("ready without the final check: %v", err)
	}
	fc, _ := workshop.FinalCheckFor(ctx, pool, technician(), job)
	if !fc.Required || fc.Done || fc.Missing == "" {
		t.Errorf("final check = %+v, want required and missing", fc)
	}

	finalCheck(t, pool, technician(), job, false)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Fatalf("ready after the final check: %v", err)
	}
	if b := boardEntry(t, pool, job); b.FinalCheckedBy != "A Technician" {
		t.Errorf("the board says the work was signed off by %q", b.FinalCheckedBy)
	}
}

// A failed item does not let the car be ready, and goes back as a finding.
func TestAFailedFinalCheckSendsTheJobBackWithAFinding(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, false)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)

	finalCheck(t, pool, technician(), job, true)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("ready after a failed final check: %v", err)
	}
	findings, err := workshop.FindingsFor(ctx, pool, technician(), job)
	if err != nil || len(findings) != 1 || findings[0].Note != "Slutkontroll: Front brakes: Glapp i spindelleden" {
		t.Errorf("findings = %+v, %v; want the failed item as one", findings, err)
	}

	// Put right and checked again, it passes.
	finalCheck(t, pool, technician(), job, false)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Errorf("ready after a passed second check: %v", err)
	}
}

func TestAFinalCheckBySomebodyElseWhenTheShopSaysSo(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, true)
	job := working(t, pool) // clocked on, so the technician's job
	move(t, pool, job, workshop.StateInProgress)

	finalCheck(t, pool, technician(), job, false)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("signed off by whoever did the work: %v", err)
	}
	finalCheck(t, pool, advisor(), job, false)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Errorf("ready after somebody else checked: %v", err)
	}
}

// Off by default: nothing changes for a shop that does not use it.
func TestReadyNeedsNoFinalCheckUnlessTheShopAsks(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Errorf("ready with no final check required: %v", err)
	}
}

func TestOnlyWhoeverRunsTheShopSetsTheFinalCheck(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTemplate(t, pool)
	if err := workshop.SaveFinalCheck(ctx, pool, advisor(), workshop.FinalCheckSettings{TemplateID: templateID}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk set the final check: %v", err)
	}
	if err := workshop.SaveFinalCheck(ctx, pool, owner(), workshop.FinalCheckSettings{TemplateID: "dddddddd-0000-0000-0000-000000000099"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a checklist that does not exist: %v", err)
	}
}

// answer gives a started check's items these results, in order; "" leaves
// one unanswered.
func answer(t *testing.T, pool *pgxpool.Pool, who access.Scope, id string, results ...string) {
	t.Helper()
	insp, _ := workshop.InspectionsForID(context.Background(), pool, who, id)
	for i, it := range insp.Items {
		if i < len(results) && results[i] != "" {
			if err := workshop.SetItem(context.Background(), pool, who, it.ID, results[i], ""); err != nil {
				t.Fatalf("SetItem: %v", err)
			}
		}
	}
}

// Finishing a final check is saying every checkpoint was looked at. With
// none answered, or some, it is not finished, and says which are left.
func TestAFinalCheckIsNotFinishedUntilEveryCheckpointHasAResult(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, false)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)
	id, err := workshop.StartInspection(ctx, pool, technician(), job, templateID)
	if err != nil {
		t.Fatal(err)
	}

	for _, results := range [][]string{nil, {"pass"}, {"pass", "", "pass"}} {
		answer(t, pool, technician(), id, results...)
		err := workshop.CompleteInspection(ctx, pool, technician(), id)
		if !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "Rear brakes") {
			t.Errorf("finished with %v answered: %v", results, err)
		}
		if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("ready with %v answered: %v", results, err)
		}
	}

	answer(t, pool, technician(), id, "pass", "pass", "pass")
	if err := workshop.CompleteInspection(ctx, pool, technician(), id); err != nil {
		t.Fatalf("finished with every checkpoint answered: %v", err)
	}
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Errorf("ready after a full final check: %v", err)
	}
}

// A check finished unanswered before the shop required it -- or by any
// other road -- does not count once it does. The gate reads the results, not
// the timestamp, and names what is missing.
func TestTheGateCountsUnansweredCheckpointsWhicheverWayTheCheckWasFinished(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	addTemplate(t, pool)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)
	id, err := workshop.StartInspection(ctx, pool, technician(), job, templateID)
	if err != nil {
		t.Fatal(err)
	}
	answer(t, pool, technician(), id, "pass")
	if err := workshop.CompleteInspection(ctx, pool, technician(), id); err != nil {
		t.Fatalf("an ordinary checklist could not be finished part answered: %v", err)
	}
	if err := workshop.SaveFinalCheck(ctx, pool, owner(), workshop.FinalCheckSettings{TemplateID: templateID}); err != nil {
		t.Fatal(err)
	}

	fc, _ := workshop.FinalCheckFor(ctx, pool, technician(), job)
	if fc.Done || fc.Missing == "" || strings.Join(fc.Unanswered, ",") != "Rear brakes,Tyres" {
		t.Errorf("final check = %+v, want not done with two unanswered", fc)
	}
	err = workshop.SetState(ctx, pool, technician(), job, workshop.StateReady)
	if !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "Tyres") {
		t.Errorf("ready on a part answered check: %v", err)
	}
}

// Attention is "it works, and will need doing": the car may go back, and
// the customer is told. It is a decision, not pass by accident.
func TestAttentionSignsOffButFailDoesNot(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, false)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)
	id, err := workshop.StartInspection(ctx, pool, technician(), job, templateID)
	if err != nil {
		t.Fatal(err)
	}
	answer(t, pool, technician(), id, "attention", "attention", "pass")
	if err := workshop.CompleteInspection(ctx, pool, technician(), id); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Errorf("ready with checkpoints marked attention: %v", err)
	}
}

// A checklist with nothing on it certifies nothing: it cannot be chosen,
// and a check with no checkpoints does not sign work off.
func TestAnEmptyChecklistDoesNotSignWorkOff(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	const empty = "dddddddd-0000-0000-0000-0000000000ee"
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO inspection_templates (id, shop_id, name) VALUES ($1, $2, 'Empty')`, empty, shopID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SaveFinalCheck(ctx, pool, owner(), workshop.FinalCheckSettings{TemplateID: empty}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an empty checklist was chosen to sign work off: %v", err)
	}

	// Chosen anyway, by a path that does not ask.
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE shops SET final_check_template_id = $1 WHERE id = $2`, empty, shopID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)
	id, err := workshop.StartInspection(ctx, pool, technician(), job, empty)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CompleteInspection(ctx, pool, technician(), id); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an empty final check was finished: %v", err)
	}
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("ready on an empty final check: %v", err)
	}
}

// A final check signs off the work done before it, not work done after.
// Back to work from ready, the next ready needs a check of its own -- and
// the old one stays in the job's history as it was.
func TestAFinalCheckDoesNotCoverWorkAfterIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, false)
	job := working(t, pool)
	move(t, pool, job, workshop.StateInProgress)

	finalCheck(t, pool, technician(), job, false)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Fatalf("ready after a passed check: %v", err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateInProgress); err != nil {
		t.Fatalf("back to work: %v", err)
	}
	err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady)
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("ready again on the check from before it went back: %v", err)
	}
	if fc, _ := workshop.FinalCheckFor(ctx, pool, technician(), job); fc.Done || fc.Missing == "" {
		t.Errorf("final check = %+v, want not done", fc)
	}

	// An older pass does not stand behind a newer fail.
	finalCheck(t, pool, technician(), job, true)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("ready on a failed check with a passed one before it: %v", err)
	}
	finalCheck(t, pool, technician(), job, false)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Errorf("ready after a fresh pass: %v", err)
	}

	insp, err := workshop.InspectionsFor(ctx, pool, technician(), job)
	if err != nil || len(insp) != 3 {
		t.Fatalf("the job's checks: %d, %v; want all three kept", len(insp), err)
	}
	if insp[0].CompletedAt == nil || insp[0].PerformedBy != "A Technician" || insp[0].Items[0].Status != "pass" {
		t.Errorf("the first check lost its record: %+v", insp[0])
	}
	// The way back is a row, with who took it.
	var back int
	var by string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*), coalesce(max(changed_by::text), '') FROM work_order_state_changes
			WHERE work_order_id = $1 AND from_state = 'ready' AND to_state = 'in_progress'`, job).Scan(&back, &by)
	}); err != nil {
		t.Fatal(err)
	}
	if back != 1 || by != advisor().UserID {
		t.Errorf("going back to work recorded %d times, by %q", back, by)
	}
}

// Work added while the job is still in progress counts the same: a part
// taken out, or a line of work, after the check started. A fee is not work
// on the car and leaves the check standing.
func TestWorkAddedAfterTheCheckNeedsTheCheckAgain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	stockShelf(t, pool, 6)
	requireFinalCheck(t, pool, false)

	for _, c := range []struct {
		name  string
		after func(job string) error
		again bool
	}{
		{"a part taken out", func(job string) error {
			return workshop.TakeOut(ctx, pool, technician(), job, stockedPart, 1)
		}, true},
		{"a line of labour", func(job string) error {
			return workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{Kind: "labour",
				Description: "Byte av bromsskiva", QuantityMilli: 1000, UnitPriceMinor: 80000, VATRateBasis: 2500})
		}, true},
		{"a fee", func(job string) error {
			return workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{Kind: "fee",
				Description: "Miljöavgift", QuantityMilli: 1000, UnitPriceMinor: 5000, VATRateBasis: 2500})
		}, false},
	} {
		job := working(t, pool)
		move(t, pool, job, workshop.StateInProgress)
		finalCheck(t, pool, technician(), job, false)
		if err := c.after(job); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady)
		if c.again && !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("ready on a check from before %s: %v", c.name, err)
		}
		if !c.again && err != nil {
			t.Errorf("%s undid the check: %v", c.name, err)
		}
	}
}
