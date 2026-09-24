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

const techUserID = "33333333-3333-3333-3333-333333333334"

func technician() access.Scope {
	return access.Scope{ShopID: shopID, UserID: techUserID, Role: access.RoleTechnician}
}

func partsDesk() access.Scope {
	return access.Scope{ShopID: shopID, UserID: userID, Role: access.RoleParts}
}

// addTechnician gives the fixture shop somebody to hold the spanner.
func addTechnician(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{
			`INSERT INTO people (id, shop_id, display_name) VALUES
			 ('22222222-2222-2222-2222-222222222224','` + shopID + `','A Technician')`,
			`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
			 ('` + techUserID + `','` + shopID + `','22222222-2222-2222-2222-222222222224','technician','x')`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("add technician: %v", err)
	}
}

// working returns a job the technician has started.
func working(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := newJob(t, pool)
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	if err := workshop.ClockIn(context.Background(), pool, technician(), id); err != nil {
		t.Fatalf("ClockIn: %v", err)
	}
	return id
}

// Picking a job up is what assigns it. Asking somebody to assign themselves
// first is a step that gets skipped.
func TestClockingOnAssignsTheJob(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	id := working(t, pool)

	job, _, err := workshop.JobByID(context.Background(), pool, technician(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if !job.AssignedToMe {
		t.Error("clocking on did not assign the job")
	}
	if job.AssignedTo == "" {
		t.Error("the job has no assignee name")
	}
	if job.State != string(workshop.StateInProgress) {
		t.Errorf("state = %s, want in_progress", job.State)
	}
}

// The two moves that belong to the person holding the spanner.
func TestTechnicianCanSayReadyAndNeedsParts(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	id := working(t, pool)
	if err := workshop.SetState(ctx, pool, technician(), id, workshop.StateAwaitingParts); err != nil {
		t.Fatalf("technician could not say it needs parts: %v", err)
	}
	if err := workshop.SetState(ctx, pool, technician(), id, workshop.StateInProgress); err != nil {
		t.Fatalf("technician could not pick it back up: %v", err)
	}
	if err := workshop.SetState(ctx, pool, technician(), id, workshop.StateReady); err != nil {
		t.Fatalf("technician could not say it is ready: %v", err)
	}
}

// Everything that is a conversation with the customer stays at the counter.
func TestTechnicianCannotInvoiceOrDecline(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	id := working(t, pool)
	if err := workshop.SetState(ctx, pool, technician(), id, workshop.StateDeclined); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician declined a job on the customer's behalf: %v", err)
	}
	if _, err := workshop.Issue(ctx, pool, technician(), id); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician issued an invoice: %v", err)
	}

	// And the screen offers them only their own moves.
	for _, s := range workshop.TechnicianStates(workshop.StateInProgress, true) {
		if s == workshop.StateDeclined || s == workshop.StateCancelled {
			t.Errorf("the technician's screen offers %s", s)
		}
	}
}

// The loop: technician asks, parts answers, the job comes back.
func TestAPartRequestTravelsAndReturns(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)

	if err := workshop.RequestPart(ctx, pool, technician(), id, "Track rod end, left front"); err != nil {
		t.Fatalf("RequestPart: %v", err)
	}

	job, _, err := workshop.JobByID(ctx, pool, technician(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.State != string(workshop.StateAwaitingParts) {
		t.Errorf("state = %s, want awaiting_parts -- asking for a part must move the job", job.State)
	}
	if job.OpenRequests != 1 {
		t.Errorf("open requests = %d, want 1", job.OpenRequests)
	}

	open, err := workshop.OpenPartRequests(ctx, pool, partsDesk())
	if err != nil {
		t.Fatalf("OpenPartRequests: %v", err)
	}
	if len(open) != 1 || open[0].Description != "Track rod end, left front" {
		t.Fatalf("the parts desk sees %+v, want the one request", open)
	}

	if err := workshop.MarkPartArrived(ctx, pool, partsDesk(), open[0].ID); err != nil {
		t.Fatalf("MarkPartArrived: %v", err)
	}

	job, _, err = workshop.JobByID(ctx, pool, technician(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.State != string(workshop.StateInProgress) {
		t.Errorf("state = %s, want in_progress -- the part arriving should hand it back", job.State)
	}
	if job.OpenRequests != 0 {
		t.Errorf("open requests = %d, want 0", job.OpenRequests)
	}
}

// A job waiting on three parts must not look workable because one turned up.
func TestOnePartArrivingDoesNotReleaseAJobWaitingOnMore(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)

	for _, what := range []string{"Track rod end", "Ball joint", "Drop link"} {
		if err := workshop.RequestPart(ctx, pool, technician(), id, what); err != nil {
			t.Fatalf("RequestPart(%s): %v", what, err)
		}
	}

	open, err := workshop.OpenPartRequests(ctx, pool, partsDesk())
	if err != nil {
		t.Fatalf("OpenPartRequests: %v", err)
	}
	if len(open) != 3 {
		t.Fatalf("got %d open requests, want 3", len(open))
	}

	if err := workshop.MarkPartArrived(ctx, pool, partsDesk(), open[0].ID); err != nil {
		t.Fatalf("MarkPartArrived: %v", err)
	}
	job, _, err := workshop.JobByID(ctx, pool, technician(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.State != string(workshop.StateAwaitingParts) {
		t.Errorf("state = %s after one of three parts arrived, want awaiting_parts", job.State)
	}

	for _, r := range open[1:] {
		if err := workshop.MarkPartArrived(ctx, pool, partsDesk(), r.ID); err != nil {
			t.Fatalf("MarkPartArrived: %v", err)
		}
	}
	job, _, err = workshop.JobByID(ctx, pool, technician(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.State != string(workshop.StateInProgress) {
		t.Errorf("state = %s once every part arrived, want in_progress", job.State)
	}
}

// A finding is a fact for the front desk, not a line the technician prices.
func TestAFindingReachesTheFrontDesk(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)

	if err := workshop.ReportFinding(ctx, pool, technician(), id, "Other track rod end is going too"); err != nil {
		t.Fatalf("ReportFinding: %v", err)
	}

	board, err := workshop.Board(ctx, pool, advisor())
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	var found bool
	for _, b := range board {
		if b.ID == id && b.OpenFindings == 1 {
			found = true
		}
	}
	if !found {
		t.Error("the finding does not show on the front desk board")
	}

	findings, err := workshop.FindingsFor(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("FindingsFor: %v", err)
	}
	if len(findings) != 1 || findings[0].Handled() {
		t.Fatalf("findings = %+v, want one unhandled", findings)
	}

	// A technician must not be able to close their own question.
	if err := workshop.HandleFinding(ctx, pool, technician(), findings[0].ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician marked a finding dealt with: %v", err)
	}
	if err := workshop.HandleFinding(ctx, pool, advisor(), findings[0].ID); err != nil {
		t.Fatalf("HandleFinding: %v", err)
	}

	findings, _ = workshop.FindingsFor(ctx, pool, advisor(), id)
	if !findings[0].Handled() {
		t.Error("the finding is still open after being dealt with")
	}
}

// Whose job is this today.
func TestTheTechniciansOwnJobsComeFirst(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	other := newJob(t, pool)
	move(t, pool, other, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)

	mine := working(t, pool)

	jobs, err := workshop.OpenJobs(ctx, pool, technician())
	if err != nil {
		t.Fatalf("OpenJobs: %v", err)
	}
	if len(jobs) < 2 {
		t.Fatalf("got %d jobs, want at least 2", len(jobs))
	}
	if jobs[0].ID != mine {
		t.Errorf("the first job is %s, want the one assigned to me (%s)", jobs[0].ID, mine)
	}
	_ = other
}
