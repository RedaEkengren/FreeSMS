package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Who is on a job and when the rest of it is to be done. A morning plan
// survives a technician being sick, a repair taking two days and a car
// waiting for a part overnight, without anybody opening a second job.

func approvedJob(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := newJob(t, pool)
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	return id
}

func plan(from, to time.Time, version int) workshop.PlanInput {
	m := 480
	return workshop.PlanInput{Version: version, Starts: &from, Ends: &to, RemainingMinutes: &m, OnLift: true}
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestASickTechnicianLeavesTheJobToSomebodyElseAndKeepsTheirHours(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	addSecondTechnician(t, pool)
	job := approvedJob(t, pool)

	// Hours already clocked by the first technician.
	if err := workshop.ClockIn(ctx, pool, technician(), job); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ClockOut(ctx, pool, technician(), job); err != nil {
		t.Fatal(err)
	}
	p, err := workshop.PlanFor(ctx, pool, advisor(), job)
	if err != nil || len(p.On) != 1 || p.On[0].UserID != techUserID {
		t.Fatalf("picking the job up put them on it: %+v %v", p.On, err)
	}
	if _, err := workshop.PlanJob(ctx, pool, advisor(), job, plan(at("2026-10-15", 8, 0), at("2026-10-15", 16, 0), 0)); err != nil {
		t.Fatalf("PlanJob: %v", err)
	}

	// They ring in sick.
	if err := workshop.SetAbsence(ctx, pool, owner(), techUserID, at("2026-10-15", 0, 0), "sick"); err != nil {
		t.Fatal(err)
	}
	p, _ = workshop.PlanFor(ctx, pool, advisor(), job)
	if len(p.Clashes) != 1 || p.Clashes[0].Kind != "absent" || p.Clashes[0].Who != "A Technician" {
		t.Fatalf("the plan against the absence: %+v", p.Clashes)
	}
	// The counter is told that they are away, not why.
	if p.Clashes[0].Detail != "" {
		t.Errorf("the clash carries a reason: %+v", p.Clashes[0])
	}
	backlog, _ := workshop.Backlog(ctx, pool, advisor())
	if len(backlog) != 1 || backlog[0].Kind != "away" || backlog[0].Who != "A Technician" {
		t.Fatalf("backlog: %+v", backlog)
	}

	// Handed over; the first technician's hours stay theirs.
	if err := workshop.ReassignJob(ctx, pool, advisor(), job, techUserID, secondTechID, "off sick"); err != nil {
		t.Fatalf("ReassignJob: %v", err)
	}
	p, _ = workshop.PlanFor(ctx, pool, advisor(), job)
	if len(p.On) != 1 || p.On[0].UserID != secondTechID || len(p.Before) != 1 ||
		p.Before[0].UserID != techUserID || p.Before[0].Reason != "off sick" || p.Before[0].Released == nil {
		t.Fatalf("who is on it: on %+v before %+v", p.On, p.Before)
	}
	if len(p.Clashes) != 0 {
		t.Errorf("the new technician is in: %+v", p.Clashes)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM time_entries WHERE work_order_id = $1 AND user_id = $2`, job, techUserID); n != 1 {
		t.Errorf("the first technician's clocked time was %d entries", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM time_entries WHERE work_order_id = $1 AND user_id = $2`, job, secondTechID); n != 0 {
		t.Errorf("hours were put on the second technician: %d", n)
	}
	var lead string
	pool.QueryRow(ctx, `SELECT assigned_to::text FROM work_orders WHERE id = $1`, job).Scan(&lead)
	if lead != secondTechID {
		t.Errorf("answerable: %s", lead)
	}
	if backlog, _ = workshop.Backlog(ctx, pool, advisor()); len(backlog) != 0 {
		t.Errorf("still in the backlog: %+v", backlog)
	}

	// The hand-over reaches the new technician's queue, and a later move of
	// the plan does too.
	kindsOf := func(who access.Scope) []string {
		var kinds []string
		items, _ := workshop.WaitingFor(ctx, pool, who)
		for _, it := range items {
			kinds = append(kinds, it.Kind)
		}
		return kinds
	}
	if k := kindsOf(secondTechnician()); len(k) != 1 || k[0] != "assigned_to_you" {
		t.Errorf("the new technician's queue: %v", k)
	}
	if _, err := workshop.PlanJob(ctx, pool, advisor(), job, plan(at("2026-10-16", 8, 0), at("2026-10-16", 16, 0), 1)); err != nil {
		t.Fatal(err)
	}
	if k := kindsOf(secondTechnician()); len(k) != 2 || k[1] != "plan_changed" && k[0] != "plan_changed" {
		t.Errorf("after the plan moved: %v", k)
	}
	items, _ := workshop.WaitingFor(ctx, pool, technician())
	for _, it := range items {
		if it.Kind == "assigned_to_you" || it.Kind == "plan_changed" {
			t.Errorf("the one handed over from is told it is theirs: %+v", it)
		}
	}
}

func TestAJobNobodyIsOnIsInTheBacklogAndSeveralCanBeOnOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	addSecondTechnician(t, pool)
	job := approvedJob(t, pool)

	backlog, _ := workshop.Backlog(ctx, pool, technician())
	if len(backlog) != 1 || backlog[0].Kind != "unstaffed" {
		t.Fatalf("an approved job with nobody on it: %+v", backlog)
	}
	items, _ := workshop.WaitingFor(ctx, pool, advisor())
	found := false
	for _, it := range items {
		found = found || it.Kind == "unstaffed"
	}
	if !found {
		t.Errorf("the counter was not told: %+v", items)
	}

	// Not the technician's to decide.
	if err := workshop.AssignTechnician(ctx, pool, technician(), job, techUserID, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician assigned: %v", err)
	}
	if err := workshop.AssignTechnician(ctx, pool, advisor(), job, userID, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("somebody who is not a technician: %v", err)
	}
	for _, who := range []string{techUserID, secondTechID, techUserID} {
		if err := workshop.AssignTechnician(ctx, pool, advisor(), job, who, "two on a gearbox"); err != nil {
			t.Fatalf("AssignTechnician: %v", err)
		}
	}
	p, _ := workshop.PlanFor(ctx, pool, technician(), job)
	if len(p.On) != 2 {
		t.Fatalf("two technicians, asked for three times: %+v", p.On)
	}
	if backlog, _ = workshop.Backlog(ctx, pool, advisor()); len(backlog) != 0 {
		t.Errorf("staffed and still in the backlog: %+v", backlog)
	}

	// The second one steps off; the job keeps the first.
	if err := workshop.ReleaseTechnician(ctx, pool, advisor(), job, secondTechID, ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReleaseTechnician(ctx, pool, advisor(), job, secondTechID, ""); !errors.Is(err, workshop.ErrPlanChanged) {
		t.Errorf("releasing somebody who is not on it: %v", err)
	}
	// Handing over from somebody who is no longer on it writes nothing.
	if err := workshop.ReassignJob(ctx, pool, advisor(), job, secondTechID, techUserID, ""); !errors.Is(err, workshop.ErrPlanChanged) {
		t.Errorf("the second planner's hand-over: %v", err)
	}
	p, _ = workshop.PlanFor(ctx, pool, technician(), job)
	if len(p.On) != 1 || p.On[0].UserID != techUserID {
		t.Errorf("after: %+v", p.On)
	}
}

func TestATwoDayJobCarriesOverWithoutANewJobOrBooking(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := approvedJob(t, pool)
	if err := workshop.AssignTechnician(ctx, pool, advisor(), job, techUserID, ""); err != nil {
		t.Fatal(err)
	}
	promise := at("2026-10-09", 16, 0)
	if err := workshop.SetPromise(ctx, pool, advisor(), job, &promise); err != nil {
		t.Fatal(err)
	}
	jobs := countRows(t, pool, `SELECT count(*) FROM work_orders`)
	bookings := countRows(t, pool, `SELECT count(*) FROM bookings`)

	// Monday to Tuesday noon, long ago: it ran over.
	if _, err := workshop.PlanJob(ctx, pool, advisor(), job, plan(at("2026-10-05", 8, 0), at("2026-10-06", 12, 0), 0)); err != nil {
		t.Fatal(err)
	}
	backlog, _ := workshop.Backlog(ctx, pool, advisor())
	if len(backlog) != 1 || backlog[0].Kind != "overdue" || backlog[0].Version != 1 {
		t.Fatalf("the plan ran out: %+v", backlog)
	}

	// Another planner moved it first: the second one is told.
	if _, err := workshop.CarryOver(ctx, pool, advisor(), job, 0, ""); !errors.Is(err, workshop.ErrPlanChanged) {
		t.Errorf("carried over from a version that was not the latest: %v", err)
	}
	if _, err := workshop.CarryOver(ctx, pool, advisor(), job, 1, ""); err != nil {
		t.Fatalf("CarryOver: %v", err)
	}
	p, _ := workshop.PlanFor(ctx, pool, advisor(), job)
	if p.Current.Version != 2 || !p.Current.Ends.After(time.Now()) || len(p.Earlier) != 1 || p.Earlier[0].Version != 1 {
		t.Fatalf("plans: current %+v earlier %+v", p.Current, p.Earlier)
	}
	// Same wall clock, whole days: eight o'clock stays eight o'clock.
	if h, m, _ := p.Current.Starts.In(stockholm).Clock(); h != 8 || m != 0 {
		t.Errorf("starts at %v", p.Current.Starts.In(stockholm))
	}
	s, e := p.Current.Starts.In(stockholm), p.Current.Ends.In(stockholm)
	if h, m, _ := e.Clock(); h != 12 || m != 0 || e.YearDay() != s.YearDay()+1 {
		t.Errorf("still Monday eight to Tuesday noon by the wall clock: %v to %v", s, e)
	}
	if countRows(t, pool, `SELECT count(*) FROM work_orders`) != jobs || countRows(t, pool, `SELECT count(*) FROM bookings`) != bookings {
		t.Error("carrying over made a job or a booking")
	}
	j, _, _ := workshop.JobByID(ctx, pool, advisor(), job)
	if j.PromisedAt == nil || !j.PromisedAt.Equal(promise) {
		t.Errorf("the plan moved the promise made to the customer: %v", j.PromisedAt)
	}
	if backlog, _ = workshop.Backlog(ctx, pool, advisor()); len(backlog) != 0 {
		t.Errorf("carried over and still late: %+v", backlog)
	}
}

// A plan that ran out years ago, say a job forgotten over a long closure, is
// carried to the next open day, not a few hundred days short of it.
func TestAPlanThatRanOutYearsAgoIsCarriedToTheFuture(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := approvedJob(t, pool)
	if _, err := workshop.PlanJob(ctx, pool, advisor(), job, plan(at("2019-01-07", 8, 0), at("2019-01-07", 16, 0), 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CarryOver(ctx, pool, advisor(), job, 1, ""); err != nil {
		t.Fatal(err)
	}
	p, _ := workshop.PlanFor(ctx, pool, advisor(), job)
	if !p.Current.Ends.After(time.Now()) || p.Current.Ends.Sub(time.Now()) > 49*time.Hour {
		t.Errorf("carried to %v", p.Current.Ends)
	}
}

func TestACarWaitingForAPartNoLongerTakesALift(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	addSecondTechnician(t, pool)
	if err := workshop.SetLifts(ctx, pool, owner(), 1); err != nil {
		t.Fatal(err)
	}
	waiting, other := approvedJob(t, pool), approvedJob(t, pool)
	window := func(version int) workshop.PlanInput {
		return plan(at("2030-03-05", 8, 0), at("2030-03-05", 16, 0), version)
	}
	if _, err := workshop.PlanJob(ctx, pool, advisor(), waiting, window(0)); err != nil {
		t.Fatal(err)
	}
	clashes, err := workshop.CheckPlan(ctx, pool, advisor(), other, window(0))
	if err != nil || len(clashes) != 1 || clashes[0].Kind != "lifts" {
		t.Fatalf("one lift, two cars: %+v %v", clashes, err)
	}

	// The part is not coming until Thursday, and the car is pushed out: its
	// plan is off the lift.
	off := window(1)
	off.OnLift = false
	if _, err := workshop.PlanJob(ctx, pool, advisor(), waiting, off); err != nil {
		t.Fatal(err)
	}
	if clashes, _ = workshop.CheckPlan(ctx, pool, advisor(), other, window(0)); len(clashes) != 0 {
		t.Errorf("a car that does not need the lift still holds it: %+v", clashes)
	}

	// Or the car goes home, whatever the plan says.
	back := window(2)
	if _, err := workshop.PlanJob(ctx, pool, advisor(), waiting, back); err != nil {
		t.Fatal(err)
	}
	if clashes, _ = workshop.CheckPlan(ctx, pool, advisor(), other, window(0)); len(clashes) != 1 {
		t.Fatalf("back on the lift: %+v", clashes)
	}
	if err := workshop.CarCollected(ctx, pool, advisor(), waiting, nil); err != nil {
		t.Fatal(err)
	}
	if clashes, _ = workshop.CheckPlan(ctx, pool, advisor(), other, window(0)); len(clashes) != 0 {
		t.Errorf("a car that went home holds a lift: %+v", clashes)
	}
}

func TestGoingAheadAgainstAClashNeedsAReasonAndKeepsIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := approvedJob(t, pool)
	if err := workshop.AssignTechnician(ctx, pool, advisor(), job, techUserID, ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CloseDay(ctx, pool, owner(), at("2030-06-21", 0, 0), "Midsommarafton"); err != nil {
		t.Fatal(err)
	}
	in := plan(at("2030-06-20", 13, 0), at("2030-06-21", 10, 0), 0)
	clashes, err := workshop.PlanJob(ctx, pool, advisor(), job, in)
	if !errors.Is(err, workshop.ErrPlanClash) || len(clashes) != 1 || clashes[0].Kind != "closed" {
		t.Fatalf("a clash with no reason: %+v %v", clashes, err)
	}
	if p, _ := workshop.PlanFor(ctx, pool, advisor(), job); p.Current != nil {
		t.Fatal("a refused plan was written")
	}
	in.Override = "the customer is collecting it themselves"
	if _, err := workshop.PlanJob(ctx, pool, advisor(), job, in); err != nil {
		t.Fatalf("with a reason: %v", err)
	}
	p, _ := workshop.PlanFor(ctx, pool, technician(), job)
	if p.Current == nil || p.Current.Override != in.Override || len(p.Current.Clashes) != 1 || p.Current.Clashes[0].Detail != "Midsommarafton" {
		t.Errorf("the override as the next person reads it: %+v", p.Current)
	}
	// A reason with nothing to override is not kept as if there were.
	ok := plan(at("2030-06-24", 8, 0), at("2030-06-24", 12, 0), 1)
	ok.Override = "just in case"
	if _, err := workshop.PlanJob(ctx, pool, advisor(), job, ok); err != nil {
		t.Fatal(err)
	}
	if p, _ = workshop.PlanFor(ctx, pool, advisor(), job); p.Current.Override != "" {
		t.Errorf("an override with no clash: %+v", p.Current)
	}
}

func TestTwoPlannersAtOnceDoNotOverwriteEachOther(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := approvedJob(t, pool)
	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			day := at("2030-04-01", 8+i, 0)
			_, results[i] = workshop.PlanJob(ctx, pool, advisor(), job, plan(day, day.Add(time.Hour), 0))
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, workshop.ErrPlanChanged):
			t.Errorf("a loser got %v, not the plan changed", err)
		}
	}
	if won != 1 {
		t.Errorf("%d planners won from the same version", won)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM job_plans WHERE work_order_id = $1`, job); n != 1 {
		t.Errorf("%d plans written", n)
	}
}

func TestOnlyTheCounterPlansAndAFinishedJobCannotBe(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := approvedJob(t, pool)
	for _, s := range []access.Scope{technician(), partsDesk()} {
		if _, err := workshop.PlanJob(ctx, pool, s, job, plan(at("2030-04-01", 8, 0), at("2030-04-01", 9, 0), 0)); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s planned: %v", s.Role, err)
		}
		if _, err := workshop.CarryOver(ctx, pool, s, job, 0, ""); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s carried over: %v", s.Role, err)
		}
		if _, err := workshop.PlannedJobs(ctx, pool, s, at("2030-04-01", 0, 0), at("2030-04-02", 0, 0)); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the planner: %v", s.Role, err)
		}
	}
	bad := plan(at("2030-04-01", 9, 0), at("2030-04-01", 8, 0), 0)
	if _, err := workshop.PlanJob(ctx, pool, advisor(), job, bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a plan that ends before it starts: %v", err)
	}
	gone := newJob(t, pool)
	if err := workshop.SetState(ctx, pool, advisor(), gone, workshop.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AssignTechnician(ctx, pool, advisor(), gone, techUserID, ""); !errors.Is(err, workshop.ErrFinished) {
		t.Errorf("assigned on a finished job: %v", err)
	}
	if _, err := workshop.PlanJob(ctx, pool, advisor(), gone, plan(at("2030-04-01", 8, 0), at("2030-04-01", 9, 0), 0)); !errors.Is(err, workshop.ErrFinished) {
		t.Errorf("planned a finished job: %v", err)
	}
}
