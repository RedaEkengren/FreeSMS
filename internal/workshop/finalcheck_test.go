package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
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
