package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A priced line says what the customer pays; it cannot say that the brakes
// are done and the exhaust waits for a part. These are the steps, and what
// ticking one is not allowed to mean.

func tasksOf(t *testing.T, pool *pgxpool.Pool, job string) []workshop.Task {
	t.Helper()
	tasks, err := workshop.TasksFor(context.Background(), pool, technician(), job)
	if err != nil {
		t.Fatalf("TasksFor: %v", err)
	}
	return tasks
}

func taskMove(t *testing.T, pool *pgxpool.Pool, who access.Scope, id string, m workshop.TaskMove) {
	t.Helper()
	if err := workshop.MoveTask(context.Background(), pool, who, id, m); err != nil {
		t.Fatalf("MoveTask(%s): %v", m.To, err)
	}
}

// inProgress is a job with the given operations quoted, agreed and on the lift.
func inProgress(t *testing.T, pool *pgxpool.Pool, what ...string) string {
	t.Helper()
	job := newJob(t, pool)
	for i, w := range what {
		quote(t, pool, job, w, int64(i+1)*1000)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress)
	return job
}

func TestAThreeOperationJobKnowsWhatIsDoneWaitingAndDeclined(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs", "Replace exhaust")

	made, err := workshop.TasksFromLines(ctx, pool, advisor(), job)
	if err != nil || made != 2 {
		t.Fatalf("TasksFromLines = %d, %v; want 2", made, err)
	}
	// Running it again adds nothing.
	if again, _ := workshop.TasksFromLines(ctx, pool, advisor(), job); again != 0 {
		t.Errorf("a second TasksFromLines added %d", again)
	}

	// New work found: a third operation, offered and turned down.
	quote(t, pool, job, "Wheel alignment", 500)
	if _, err := workshop.AddTask(ctx, pool, advisor(), job, "Wheel alignment", 3, ""); err != nil {
		t.Fatal(err)
	}
	q, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID,
		workshop.QuoteAnswer{Decision: "declined", Via: "counter"}); err != nil {
		t.Fatal(err)
	}

	before := lineOf(t, pool, job, 1)
	tasks := tasksOf(t, pool, job)
	taskMove(t, pool, technician(), tasks[0].ID, workshop.TaskMove{To: workshop.TaskDone})
	taskMove(t, pool, technician(), tasks[1].ID, workshop.TaskMove{To: workshop.TaskBlocked, Blocker: workshop.BlockerPart})
	// Somebody ticks the alignment too, not knowing it was turned down.
	taskMove(t, pool, technician(), tasks[2].ID, workshop.TaskMove{To: workshop.TaskDone})

	// The counter is told what stands in the way, and only that.
	err = workshop.SetState(ctx, pool, technician(), job, workshop.StateReady)
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("ready with the exhaust waiting = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "Replace exhaust") || !strings.Contains(err.Error(), "waiting for a part") {
		t.Errorf("the refusal does not say what is in the way: %v", err)
	}
	if strings.Contains(err.Error(), "brake") || strings.Contains(err.Error(), "alignment") {
		t.Errorf("the refusal names work that is not in the way: %v", err)
	}
	c := workshop.CountTasks(tasksOf(t, pool, job))
	if c.Total != 3 || c.Done != 2 || c.Outstanding != 1 || c.Blocked != 1 {
		t.Errorf("counts = %+v", c)
	}

	// Done on a task is the work being done -- and nothing else. The shelf is
	// untouched and the customer's yes is exactly what it was.
	after := lineOf(t, pool, job, 1)
	if after.PerformedAt == nil {
		t.Error("the brake line was not marked performed by its task")
	}
	if after.ApprovedAt == nil || !after.ApprovedAt.Equal(*before.ApprovedAt) {
		t.Errorf("approval moved: %v -> %v", before.ApprovedAt, after.ApprovedAt)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE work_order_id = $1`, job); n != 0 {
		t.Errorf("ticking tasks wrote %d stock movements", n)
	}
	// The declined line stays declined; a technician's tick did not bill it.
	if l := lineOf(t, pool, job, 3); l.DeclinedAt == nil || l.PerformedAt != nil {
		t.Errorf("the declined line is %s", l.Status())
	}

	// Part arrives, job is finished, car goes back.
	taskMove(t, pool, technician(), tasks[1].ID, workshop.TaskMove{To: workshop.TaskDone})
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Fatalf("ready when everything is done: %v", err)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	// Brake discs 1h and exhaust 2h at 895.00; the alignment is not billed.
	if inv.NetMinor != 268500 {
		t.Errorf("invoice net = %d, want 268500", inv.NetMinor)
	}
}

func TestDiagnosisThatWasDoneIsBilledWhenTheRepairIsDeclined(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Diagnosis", "Replace gearbox")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	tasks := tasksOf(t, pool, job)
	taskMove(t, pool, technician(), tasks[0].ID, workshop.TaskMove{To: workshop.TaskDone})
	taskMove(t, pool, technician(), tasks[1].ID, workshop.TaskMove{To: workshop.TaskUnderway})

	if err := workshop.Decline(ctx, pool, advisor(), job, nil); err != nil {
		t.Fatal(err)
	}
	got := tasksOf(t, pool, job)
	if got[0].Status != workshop.TaskDone || got[1].Status != workshop.TaskOmitted {
		t.Errorf("after the decline the tasks are %s and %s", got[0].Status, got[1].Status)
	}
	if got[1].NeedsCustomer() {
		t.Error("the customer's own no left a decision to be put to them")
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if inv.NetMinor != 89500 {
		t.Errorf("invoice net = %d, want 89500: the diagnosis only", inv.NetMinor)
	}
}

func TestNewWorkAfterTheFinalCheckSendsTheCarBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, false)
	job := inProgress(t, pool, "Replace brake discs")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	taskMove(t, pool, technician(), tasksOf(t, pool, job)[0].ID, workshop.TaskMove{To: workshop.TaskDone})
	finalCheck(t, pool, technician(), job, false)
	if err := workshop.SetState(ctx, pool, technician(), job, workshop.StateReady); err != nil {
		t.Fatalf("ready after the check: %v", err)
	}

	// The technician sees something else while the car is waiting.
	if _, err := workshop.AddTask(ctx, pool, technician(), job, "Replace wiper blades", 0, ""); err != nil {
		t.Fatal(err)
	}
	j, _, _ := workshop.JobByID(ctx, pool, technician(), job)
	if j.State != string(workshop.StateInProgress) {
		t.Errorf("the job is %s with new work on it, want in_progress", j.State)
	}
	fc, _ := workshop.FinalCheckFor(ctx, pool, technician(), job)
	if fc.Done {
		t.Error("the final check still stands after work it did not see")
	}
}

func TestTheFinalCheckDoesNotCoverTasksDoneAfterIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	requireFinalCheck(t, pool, false)
	job := inProgress(t, pool, "Replace brake discs", "Replace exhaust")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	tasks := tasksOf(t, pool, job)
	taskMove(t, pool, technician(), tasks[0].ID, workshop.TaskMove{To: workshop.TaskDone})
	finalCheck(t, pool, technician(), job, false)
	taskMove(t, pool, technician(), tasks[1].ID, workshop.TaskMove{To: workshop.TaskDone})
	if fc, _ := workshop.FinalCheckFor(ctx, pool, technician(), job); fc.Done {
		t.Error("a task done after the check left the check standing")
	}
}

func TestAReopenedTaskTakesTheMarkOffItsLine(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	id := tasksOf(t, pool, job)[0].ID
	taskMove(t, pool, technician(), id, workshop.TaskMove{To: workshop.TaskDone})
	taskMove(t, pool, technician(), id, workshop.TaskMove{To: workshop.TaskPending})
	if l := lineOf(t, pool, job, 1); l.PerformedAt != nil {
		t.Error("the line is still performed after its task was reopened")
	}
	if got := tasksOf(t, pool, job)[0]; len(got.History) != 3 {
		t.Errorf("history has %d events, want 3 (added, done, reopened)", len(got.History))
	}
}

func TestMarkingALinePerformedFinishesItsTask(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := inProgress(t, pool, "Replace brake discs")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 1, true); err != nil {
		t.Fatal(err)
	}
	if got := tasksOf(t, pool, job)[0]; got.Status != workshop.TaskDone {
		t.Errorf("the task is %s after its line was marked performed", got.Status)
	}
}

func TestSeveralPeopleLeaveTheirNameOnATask(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs")
	id, err := workshop.AddTask(context.Background(), pool, technician(), job, "Bleed the brakes", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	taskMove(t, pool, technician(), id, workshop.TaskMove{To: workshop.TaskUnderway})
	// Same state twice is a retried tap, not a second event.
	taskMove(t, pool, technician(), id, workshop.TaskMove{To: workshop.TaskUnderway})
	taskMove(t, pool, advisor(), id, workshop.TaskMove{To: workshop.TaskDone})
	h := tasksOf(t, pool, job)[0].History
	if len(h) != 3 {
		t.Fatalf("history has %d events, want 3", len(h))
	}
	if h[1].By == "" || h[2].By == "" || h[1].By == h[2].By {
		t.Errorf("who did what is lost: %q then %q", h[1].By, h[2].By)
	}
}

func TestLeavingATaskOutIsTheDesksAndNeedsAReason(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs")
	id, err := workshop.AddTask(ctx, pool, technician(), job, "Bleed the brakes", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	omit := workshop.TaskMove{To: workshop.TaskOmitted, Note: "Fluid was fine", NeedsDecision: true}
	if err := workshop.MoveTask(ctx, pool, technician(), id, omit); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician left a task out: %v", err)
	}
	if err := workshop.MoveTask(ctx, pool, advisor(), id, workshop.TaskMove{To: workshop.TaskOmitted}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("left out with no reason = %v, want ErrInvalid", err)
	}
	taskMove(t, pool, advisor(), id, omit)

	// It does not stand in the way of ready, but the customer has to hear.
	if got := tasksOf(t, pool, job)[0]; !got.NeedsCustomer() || got.Outstanding() {
		t.Errorf("omitted task: needs customer %v, outstanding %v", got.NeedsCustomer(), got.Outstanding())
	}
	// The technician cannot put it back, nor record what the customer said.
	if err := workshop.MoveTask(ctx, pool, technician(), id, workshop.TaskMove{To: workshop.TaskPending}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician put an omitted task back: %v", err)
	}
	if err := workshop.RecordTaskDecision(ctx, pool, technician(), id, "They agreed"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician recorded the customer's decision: %v", err)
	}
	if err := workshop.RecordTaskDecision(ctx, pool, advisor(), id, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a decision with nothing said = %v, want ErrInvalid", err)
	}
	if err := workshop.RecordTaskDecision(ctx, pool, advisor(), id, "Rang them, fine to skip"); err != nil {
		t.Fatal(err)
	}
	if tasksOf(t, pool, job)[0].NeedsCustomer() {
		t.Error("the decision is still open after it was recorded")
	}
}

func TestABlockerSaysWhatItIsWaitingFor(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs")
	id, _ := workshop.AddTask(ctx, pool, technician(), job, "Bleed the brakes", 0, "")
	for _, m := range []workshop.TaskMove{
		{To: workshop.TaskBlocked},
		{To: workshop.TaskBlocked, Blocker: "gremlins"},
		{To: workshop.TaskBlocked, Blocker: workshop.BlockerOther},
		{To: "finished"},
	} {
		if err := workshop.MoveTask(ctx, pool, technician(), id, m); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("MoveTask(%+v) = %v, want ErrInvalid", m, err)
		}
	}
	taskMove(t, pool, technician(), id, workshop.TaskMove{To: workshop.TaskBlocked, Blocker: workshop.BlockerOther, Note: "Bleed nipple seized"})
	got := tasksOf(t, pool, job)[0]
	if got.Why() != "blocked" || got.Note != "Bleed nipple seized" {
		t.Errorf("blocked task reads %q / %q", got.Why(), got.Note)
	}
	// Moving on clears what it was waiting for.
	taskMove(t, pool, technician(), id, workshop.TaskMove{To: workshop.TaskUnderway})
	if got := tasksOf(t, pool, job)[0]; got.Blocker != "" {
		t.Errorf("still blocked by %q after it moved on", got.Blocker)
	}
}

func TestOnlyTheShopFloorAndCounterKeepAWorkList(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := inProgress(t, pool, "Replace brake discs")
	if _, err := workshop.AddTask(ctx, pool, partsDesk(), job, "Anything", 0, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the parts desk added a task: %v", err)
	}
	if _, err := workshop.TasksFromLines(ctx, pool, technician(), job); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician made tasks of the priced lines: %v", err)
	}
	if _, err := workshop.AddTask(ctx, pool, advisor(), job, "  ", 0, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an empty title = %v, want ErrInvalid", err)
	}
	if _, err := workshop.AddTask(ctx, pool, advisor(), job, "x", 9, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a line that is not there = %v, want ErrInvalid", err)
	}
}

func TestAnInvoicedJobsListIsSettled(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := inProgress(t, pool, "Replace brake discs")
	id, _ := workshop.AddTask(ctx, pool, advisor(), job, "Road test", 0, "")
	taskMove(t, pool, advisor(), id, workshop.TaskMove{To: workshop.TaskDone})
	move(t, pool, job, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveTask(ctx, pool, advisor(), id, workshop.TaskMove{To: workshop.TaskPending}); !errors.Is(err, workshop.ErrFinished) {
		t.Errorf("reopening a task on an invoiced job = %v, want ErrFinished", err)
	}
	if _, err := workshop.AddTask(ctx, pool, advisor(), job, "More", 0, ""); !errors.Is(err, workshop.ErrFinished) {
		t.Errorf("adding to an invoiced job = %v, want ErrFinished", err)
	}
}
