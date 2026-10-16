package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Work sent to a supplier: a promise, a result, a cost and an invoice number,
// kept apart from the job's state and from the shop's own acceptance.

func pointerTo[T any](v T) *T { return &v }

// turboJob is a job in work with a sublet line for the turbocharger overhaul
// and a step on the work list for it.
func turboJob(t *testing.T, pool *pgxpool.Pool) (job, line string) {
	t.Helper()
	ctx := context.Background()
	job = newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "sublet", Description: "Overhaul the turbocharger", QuantityMilli: 1000, UnitPriceMinor: 650000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	if j, _, _ := workshop.JobByID(ctx, pool, advisor(), job); workshop.State(j.State) != workshop.StateInProgress {
		move(t, pool, job, workshop.StateInProgress)
	}
	line = lineID(t, pool, job, "Overhaul the turbocharger")
	return
}

func sendOut(t *testing.T, pool *pgxpool.Pool, job, line string, back time.Time) string {
	t.Helper()
	id, err := workshop.RequestOutsourced(context.Background(), pool, advisor(), workshop.OutsourcedInput{
		JobID: job, LineID: line, Supplier: "Turbo-Verkstan AB", Contact: "Lars, 08-123 45", Scope: "Overhaul, balance and test",
		ExternalRef: "TV-2291", Item: "Turbocharger", ItemKind: "component", ExpectedBack: &back, EstimateMinor: pointerTo(int64(450000))})
	if err != nil {
		t.Fatalf("RequestOutsourced: %v", err)
	}
	return id
}

func record(t *testing.T, pool *pgxpool.Pool, id string, in workshop.OutsourcedEventInput) {
	t.Helper()
	if err := workshop.RecordOutsourced(context.Background(), pool, advisor(), id, in); err != nil {
		t.Fatalf("RecordOutsourced %s: %v", in.Kind, err)
	}
}

func theOnlyOne(t *testing.T, pool *pgxpool.Pool, job string) workshop.Outsourced {
	t.Helper()
	list, err := workshop.OutsourcedFor(context.Background(), pool, advisor(), job)
	if err != nil || len(list) != 1 {
		t.Fatalf("OutsourcedFor = %+v, %v", list, err)
	}
	return list[0]
}

func readyIsRefused(t *testing.T, pool *pgxpool.Pool, job, want string) {
	t.Helper()
	err := workshop.SetState(context.Background(), pool, advisor(), job, workshop.StateReady)
	if !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Fatalf("ready = %v, want a refusal mentioning %q", err, want)
	}
}

func TestASubcontractedComponentFromRequestToSupplierInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, line := turboJob(t, pool)
	yesterday := time.Now().AddDate(0, 0, -1)
	id := sendOut(t, pool, job, line, yesterday)

	o := theOnlyOne(t, pool, job)
	if o.Status != "requested" || o.Away() || o.Supplier != "Turbo-Verkstan AB" || o.EstimateMinor == nil || *o.EstimateMinor != 450000 {
		t.Fatalf("after the request: %+v", o)
	}
	if o.Charge() != 650000 {
		t.Errorf("the customer's price on the line = %d, want 650000", o.Charge())
	}

	// Sent: the step on the work list is blocked by it, and it is late.
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "sent"})
	tasks, _ := workshop.TasksFor(ctx, pool, advisor(), job)
	if len(tasks) != 1 || tasks[0].Status != workshop.TaskBlocked {
		t.Fatalf("the step after sending: %+v", tasks)
	}
	o = theOnlyOne(t, pool, job)
	if !o.Away() || !o.Overdue {
		t.Errorf("sent and due yesterday: away %v overdue %v", o.Away(), o.Overdue)
	}
	items, _ := workshop.WaitingFor(ctx, pool, advisor())
	found := false
	for _, it := range items {
		found = found || it.Kind == "outsourced_overdue"
	}
	if !found {
		t.Error("an overdue result is not in the counter's queue")
	}

	// A delay with a new date: no longer overdue, and the history says so.
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "waiting", Note: "Balancing machine is down", ExpectedBack: pointerTo(time.Now().AddDate(0, 0, 3))})
	if o = theOnlyOne(t, pool, job); o.Overdue || o.Status != "waiting" {
		t.Errorf("after a delay: %+v", o)
	}
	items, _ = workshop.WaitingFor(ctx, pool, advisor())
	for _, it := range items {
		if it.Kind == "outsourced_overdue" {
			t.Error("a delayed item with a new date is still in the queue")
		}
	}

	// It is not ready while it is out, nor while it is merely back.
	readyIsRefused(t, pool, job, "Turbo-Verkstan AB")
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "returned", Outcome: "repaired", Note: "Cartridge replaced, balanced to spec"})
	readyIsRefused(t, pool, job, "not back and checked")
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateReady); err == nil {
		t.Fatal("receipt was taken for acceptance")
	}
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "checked", Note: "Spins free, no play"})
	tasks, _ = workshop.TasksFor(ctx, pool, advisor(), job)
	if tasks[0].Status != workshop.TaskDone {
		t.Errorf("the step after the shop accepted it: %v", tasks[0].Status)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateReady); err != nil {
		t.Fatalf("ready after acceptance: %v", err)
	}

	// The money: what it cost, what they invoiced, and handing it on.
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "cost", AmountMinor: pointerTo(int64(470000))})
	o = theOnlyOne(t, pool, job)
	if !o.CostDiffers() || o.Cost() != 470000 {
		t.Errorf("cost %d, differs %v", o.Cost(), o.CostDiffers())
	}
	if err := workshop.RecordOutsourced(ctx, pool, advisor(), id, workshop.OutsourcedEventInput{Kind: "reconciled"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("handing over an invoice nobody noted = %v", err)
	}
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "invoice", InvoiceRef: "F-8841", AmountMinor: pointerTo(int64(470000))})
	if o = theOnlyOne(t, pool, job); !o.InvoiceWaiting() || o.InvoiceRef != "F-8841" {
		t.Errorf("noted invoice: %+v", o)
	}
	items, _ = workshop.WaitingFor(ctx, pool, advisor())
	waiting := false
	for _, it := range items {
		waiting = waiting || it.Kind == "outsourced_invoice"
	}
	if !waiting {
		t.Error("an invoice nobody handed on is not in the queue")
	}
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "reconciled", Note: "Booked as verification 118"})
	if o = theOnlyOne(t, pool, job); o.InvoiceWaiting() || !o.Reconciled {
		t.Errorf("handed over: %+v", o)
	}
	if err := workshop.RecordOutsourced(ctx, pool, advisor(), id, workshop.OutsourcedEventInput{Kind: "reconciled"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("handing over twice = %v", err)
	}
}

func TestAReturnedItemThatFailsTheShopsCheckKeepsTheJobFromReady(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, line := turboJob(t, pool)
	id := sendOut(t, pool, job, line, time.Now().AddDate(0, 0, 2))
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "sent"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "returned", Outcome: "repaired", Note: "Done"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "check_failed", Note: "Shaft play at the bench"})
	if o := theOnlyOne(t, pool, job); o.Status != "check_failed" || !o.Unresolved() {
		t.Fatalf("after the failed check: %+v", o)
	}
	readyIsRefused(t, pool, job, "Turbo-Verkstan AB")
	// It goes back, comes back, and is accepted.
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "sent", ExpectedBack: pointerTo(time.Now().AddDate(0, 0, 4))})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "returned", Outcome: "repaired", Note: "Re-done under warranty"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "checked"})
	// Accepted, then found wanting at the final check: it is open again.
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "check_failed", Note: "Oil weeps at the flange on the road test"})
	readyIsRefused(t, pool, job, "Turbo-Verkstan AB")
	var kinds []string
	for _, e := range theOnlyOne(t, pool, job).Events {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "requested,sent,returned,check_failed,sent,returned,checked,check_failed" {
		t.Errorf("history = %s", got)
	}
	_ = ctx
}

func TestAFinalCheckCannotCertifyWorkThatIsStillOutOrCameBackAfterIt(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	requireFinalCheck(t, pool, false)
	// No operation and no step: only the outsourced record says it is out.
	job := approvedJob(t, pool)
	move(t, pool, job, workshop.StateInProgress)
	id, err := workshop.RequestOutsourced(ctx, pool, advisor(), workshop.OutsourcedInput{
		JobID: job, Supplier: "Turbo-Verkstan AB", Scope: "Overhaul", Item: "Turbocharger", ItemKind: "component"})
	if err != nil {
		t.Fatal(err)
	}
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "sent"})

	// A check made with the turbocharger at the specialist proves nothing.
	finalCheck(t, pool, technician(), job, false)
	fc, _ := workshop.FinalCheckFor(ctx, pool, technician(), job)
	if fc.Done || !strings.Contains(fc.Missing, "supplier") {
		t.Fatalf("final check while the work is out = %+v", fc)
	}
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "returned", Outcome: "repaired", Note: "Done"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "checked"})
	// It is back and accepted, but the check on file was made before that.
	if fc, _ = workshop.FinalCheckFor(ctx, pool, technician(), job); fc.Done || !strings.Contains(fc.Missing, "since the final check") {
		t.Fatalf("final check from before it came back = %+v", fc)
	}
	finalCheck(t, pool, technician(), job, false)
	if fc, _ = workshop.FinalCheckFor(ctx, pool, technician(), job); !fc.Done {
		t.Fatalf("final check after it came back = %+v", fc)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateReady); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

func TestACarAtASupplierDoesNotHoldALiftButAPartDoes(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	addSecondTechnician(t, pool)
	ctx := context.Background()
	if err := workshop.SetLifts(ctx, pool, owner(), 1); err != nil {
		t.Fatal(err)
	}
	away, other := approvedJob(t, pool), approvedJob(t, pool)
	window := func(version int) workshop.PlanInput {
		return plan(at("2030-03-05", 8, 0), at("2030-03-05", 16, 0), version)
	}
	if _, err := workshop.PlanJob(ctx, pool, advisor(), away, window(0)); err != nil {
		t.Fatal(err)
	}
	if clashes, _ := workshop.CheckPlan(ctx, pool, advisor(), other, window(0)); len(clashes) != 1 {
		t.Fatalf("one lift, two cars: %+v", clashes)
	}

	// A part off the car leaves the car where it is.
	part, err := workshop.RequestOutsourced(ctx, pool, advisor(), workshop.OutsourcedInput{
		JobID: away, Supplier: "Turbo-Verkstan AB", Scope: "Overhaul", Item: "Turbocharger", ItemKind: "component"})
	if err != nil {
		t.Fatal(err)
	}
	record(t, pool, part, workshop.OutsourcedEventInput{Kind: "sent"})
	if clashes, _ := workshop.CheckPlan(ctx, pool, advisor(), other, window(0)); len(clashes) != 1 {
		t.Errorf("a part at a supplier freed the lift: %+v", clashes)
	}
	record(t, pool, part, workshop.OutsourcedEventInput{Kind: "cancelled", Note: "Changed our mind"})

	// The car itself is somewhere else, and so is not on the lift, though the
	// customer has not collected it and the job is not "away" in that sense.
	car, err := workshop.RequestOutsourced(ctx, pool, advisor(), workshop.OutsourcedInput{
		JobID: away, Supplier: "Kaross-Centrum", Scope: "Straighten the sub-frame", Item: "The car", ItemKind: "vehicle"})
	if err != nil {
		t.Fatal(err)
	}
	record(t, pool, car, workshop.OutsourcedEventInput{Kind: "sent"})
	if clashes, _ := workshop.CheckPlan(ctx, pool, advisor(), other, window(0)); len(clashes) != 0 {
		t.Errorf("a car at a supplier still holds the lift: %+v", clashes)
	}
	if pr, _ := workshop.PresenceFor(ctx, pool, advisor(), away); !pr.Here {
		t.Error("the supplier's work was written as the customer collecting the car")
	}
	record(t, pool, car, workshop.OutsourcedEventInput{Kind: "returned", Outcome: "repaired", Note: "Straightened"})
	if clashes, _ := workshop.CheckPlan(ctx, pool, advisor(), other, window(0)); len(clashes) != 1 {
		t.Errorf("a car back from the supplier does not hold the lift: %+v", clashes)
	}
}

func TestWhatASupplierDidNotDoOrDidInstead(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// Nothing done, and a bill for the look at it.
	job, line := turboJob(t, pool)
	id := sendOut(t, pool, job, line, time.Now().AddDate(0, 0, 2))
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "sent"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "returned", Outcome: "no_work", Note: "Customer declined the quote; they only diagnosed it"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "cost", AmountMinor: pointerTo(int64(60000))})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "invoice", InvoiceRef: "F-9001", AmountMinor: pointerTo(int64(60000))})
	if o := theOnlyOne(t, pool, job); o.Outcome != "no_work" || o.Cost() != 60000 || !o.Unresolved() {
		t.Errorf("a diagnosis billed, nothing fixed: %+v", o)
	}
	// The shop accepts that nothing was done and cancels the rest: the bill stays.
	if err := workshop.RecordOutsourced(ctx, pool, advisor(), id, workshop.OutsourcedEventInput{Kind: "cancelled", Note: "Not repaired"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("cancelling something that came back = %v, want it checked or failed first", err)
	}
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "checked", Note: "Report seen; no work to check"})
	o := theOnlyOne(t, pool, job)
	if o.Unresolved() || o.Cost() != 60000 || o.InvoiceRef != "F-9001" {
		t.Errorf("accepted: %+v", o)
	}

	// A replacement unit is a different result from a repair.
	job2, line2 := turboJob(t, pool)
	id2 := sendOut(t, pool, job2, line2, time.Now().AddDate(0, 0, 2))
	record(t, pool, id2, workshop.OutsourcedEventInput{Kind: "sent"})
	record(t, pool, id2, workshop.OutsourcedEventInput{Kind: "returned", Outcome: "replaced", Note: "Exchange unit, serial 5521"})
	if o := theOnlyOne(t, pool, job2); o.Outcome != "replaced" {
		t.Errorf("outcome = %q", o.Outcome)
	}

	// Cancelled before it left: the step is free again and nothing blocks ready.
	job3, line3 := turboJob(t, pool)
	id3 := sendOut(t, pool, job3, line3, time.Now().AddDate(0, 0, 2))
	record(t, pool, id3, workshop.OutsourcedEventInput{Kind: "sent"})
	record(t, pool, id3, workshop.OutsourcedEventInput{Kind: "cancelled", Note: "We will do it ourselves"})
	if tasks, _ := workshop.TasksFor(ctx, pool, advisor(), job3); tasks[0].Status != workshop.TaskPending {
		t.Errorf("the step after cancelling: %v", tasks[0].Status)
	}
	if o := theOnlyOne(t, pool, job3); o.Unresolved() {
		t.Error("a cancelled piece of work still blocks")
	}
}

func TestOutsourcedWorkNeedsItsReasonsAndTheRightOrder(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	job, line := turboJob(t, pool)
	id := sendOut(t, pool, job, line, time.Now().AddDate(0, 0, 2))
	bad := func(name string, in workshop.OutsourcedEventInput) {
		t.Helper()
		if err := workshop.RecordOutsourced(ctx, pool, advisor(), id, in); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	bad("returned before it was sent", workshop.OutsourcedEventInput{Kind: "returned", Outcome: "repaired", Note: "x"})
	bad("checked before it came back", workshop.OutsourcedEventInput{Kind: "checked"})
	bad("waiting with nothing said", workshop.OutsourcedEventInput{Kind: "waiting"})
	bad("an invented event", workshop.OutsourcedEventInput{Kind: "teleported"})
	bad("cost without an amount", workshop.OutsourcedEventInput{Kind: "cost"})
	bad("invoice without a number", workshop.OutsourcedEventInput{Kind: "invoice"})
	bad("a note without words", workshop.OutsourcedEventInput{Kind: "note"})
	bad("cancelled without a reason", workshop.OutsourcedEventInput{Kind: "cancelled"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "sent"})
	bad("returned without a report", workshop.OutsourcedEventInput{Kind: "returned", Outcome: "repaired"})
	bad("returned without a result", workshop.OutsourcedEventInput{Kind: "returned", Note: "x"})
	bad("a negative cost", workshop.OutsourcedEventInput{Kind: "cost", AmountMinor: pointerTo(int64(-1))})
	bad("put on nobody", workshop.OutsourcedEventInput{Kind: "assigned", Assignee: "11111111-1111-1111-1111-111111111111"})

	// Who is chasing it is a row, and changes.
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "assigned", Assignee: userID})
	if o := theOnlyOne(t, pool, job); o.Responsible != "An Advisor" {
		t.Errorf("responsible = %q", o.Responsible)
	}

	// The shop floor sees where it is and nothing about money.
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "cost", AmountMinor: pointerTo(int64(470000))})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "invoice", InvoiceRef: "F-1"})
	tech, err := workshop.OutsourcedFor(ctx, pool, technician(), job)
	if err != nil || len(tech) != 1 {
		t.Fatalf("OutsourcedFor as a technician = %+v, %v", tech, err)
	}
	o := tech[0]
	if !o.Away() || o.MoneyVisible || o.EstimateMinor != nil || o.CostMinor != nil || o.InvoiceRef != "" || o.ChargeMinor != nil {
		t.Errorf("the technician was handed money: %+v", o)
	}
	for _, e := range o.Events {
		if e.Kind == "cost" || e.Kind == "invoice" || e.Kind == "reconciled" {
			t.Errorf("the technician sees a %s", e.Kind)
		}
	}
	if _, err := workshop.RequestOutsourced(ctx, pool, technician(), workshop.OutsourcedInput{JobID: job, Supplier: "x", Scope: "x", Item: "x", ItemKind: "component"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("request as a technician = %v", err)
	}
	if err := workshop.RecordOutsourced(ctx, pool, technician(), id, workshop.OutsourcedEventInput{Kind: "note", Note: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("record as a technician = %v", err)
	}
	if _, err := workshop.OutsourcedOpen(ctx, pool, technician()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the counter's list as a technician = %v", err)
	}
}

func TestChangingTheCustomersChargeForOutsourcedWorkNeedsTheirYesAgain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, line := turboJob(t, pool)
	sendOut(t, pool, job, line, time.Now().AddDate(0, 0, 2))
	// The supplier cost more; raising the customer's line is the ordinary edit,
	// and the ordinary edit takes the customer's agreement back.
	_, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	if lines[0].ApprovedAt == nil {
		t.Fatal("the line was not approved to begin with")
	}
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, lines[0].Position, lines[0].Revision, workshop.LineEdit{
		Description: lines[0].Description, QuantityMilli: lines[0].QuantityMilli, UnitPriceMinor: 720000,
		VATRateBasis: lines[0].VATRateBasis, CostBearer: "customer", Reason: "The supplier charged more"}); err != nil {
		t.Fatalf("EditLine: %v", err)
	}
	_, lines, _ = workshop.JobByID(ctx, pool, advisor(), job)
	if lines[0].ApprovedAt != nil {
		t.Error("a higher price kept the customer's earlier yes")
	}
}
