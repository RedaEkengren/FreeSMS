package workshop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

// What is waiting for somebody is what is true, and stops being on their list
// when somebody deals with it -- not when it is clicked away.

// currentPool is the test's database, so a check reads as one line.
var currentPool *pgxpool.Pool

func waiting(t *testing.T, scope access.Scope, kind string) []workshop.WaitingItem {
	t.Helper()
	pool := currentPool
	items, err := workshop.WaitingFor(context.Background(), pool, scope)
	if err != nil {
		t.Fatalf("WaitingFor(%s): %v", scope.Role, err)
	}
	var out []workshop.WaitingItem
	for _, it := range items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

func TestAPartThatArrivedWaitsForTheTechnicianUntilTheyAreBackOnTheJob(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	addTechnician(t, pool)
	job := newJob(t, pool)
	if err := workshop.RequestParts(ctx, pool, technician(), job, "Styrled vänster", 1); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "part_arrived"); len(got) != 0 {
		t.Fatalf("a part not yet here is waiting: %+v", got)
	}
	if got := waiting(t, partsDesk(), "part_requested"); len(got) != 1 || got[0].Text != "Styrled vänster" {
		t.Errorf("the parts desk's list: %+v", got)
	}

	reqs, _ := workshop.PartRequestsFor(ctx, pool, technician(), job)
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), reqs[0].ID, 1, "delivery"); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, partsDesk(), "part_requested"); len(got) != 0 {
		t.Errorf("an arrived part is still the parts desk's: %+v", got)
	}
	got := waiting(t, technician(), "part_arrived")
	if len(got) != 1 || got[0].JobID != job || got[0].Text != "Styrled vänster" {
		t.Fatalf("the technician's list: %+v", got)
	}
	// Somebody else's request is not theirs to be told about.
	if other := waiting(t, owner(), "part_arrived"); len(other) != 0 {
		t.Errorf("the owner was told about a part they did not ask for: %+v", other)
	}

	if err := workshop.ClockIn(ctx, pool, technician(), job); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "part_arrived"); len(got) != 0 {
		t.Errorf("back on the job and still told: %+v", got)
	}
}

// A finding waits for the counter, for everyone at it at once, and leaves
// all of their lists when one of them deals with it.
func TestAFindingWaitsForTheCounterUntilSomebodyDealsWithIt(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	addTechnician(t, pool)
	job := newJob(t, pool)
	if err := workshop.ReportFinding(ctx, pool, technician(), job, "Spindelled glapp"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []access.Scope{advisor(), owner()} {
		if got := waiting(t, s, "finding"); len(got) != 1 || got[0].Count != 1 || got[0].JobID != job {
			t.Errorf("%s's list: %+v", s.Role, got)
		}
	}
	if got := waiting(t, technician(), "finding"); len(got) != 0 {
		t.Errorf("the technician who reported it is told about it: %+v", got)
	}
	findings, _ := workshop.FindingsFor(ctx, pool, advisor(), job)
	if err := workshop.HandleFinding(ctx, pool, advisor(), findings[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, s := range []access.Scope{advisor(), owner()} {
		if got := waiting(t, s, "finding"); len(got) != 0 {
			t.Errorf("dealt with, and still on %s's list: %+v", s.Role, got)
		}
	}
}

// The customer's yes, once it is on the job, is the technician's go-ahead --
// and it says what was approved, never who approved it.
func TestTheCustomersYesWaitsForTheTechnicianWithoutTheCustomer(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	job, item := answered(t, pool, "approved")
	if got := waiting(t, technician(), "customer_yes"); len(got) != 0 {
		t.Fatalf("not priced yet, and the technician is told to go ahead: %+v", got)
	}
	if got := waiting(t, advisor(), "answer"); len(got) != 1 {
		t.Errorf("the counter is not told to price it: %+v", got)
	}
	if err := workshop.PriceAnswer(ctx, pool, advisor(), job, item, brakes); err != nil {
		t.Fatal(err)
	}
	got := waiting(t, technician(), "customer_yes")
	if len(got) != 1 || got[0].Text != brakes.Description {
		t.Fatalf("the technician's list: %+v", got)
	}
	all, _ := workshop.WaitingFor(ctx, pool, technician())
	for _, it := range all {
		if strings.Contains(it.Text+it.Registration, "Karin") {
			t.Errorf("a customer reached the technician's list: %+v", it)
		}
	}
	if err := workshop.ClockIn(ctx, pool, technician(), job); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "customer_yes"); len(got) != 0 {
		t.Errorf("on it, and still told: %+v", got)
	}
}

// A car booked on somebody that has come in is theirs, until they start.
func TestACarBookedOnATechnicianWaitsForThemWhenItComesIn(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	addTechnician(t, pool)
	id, err := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
		Starts: at("2026-10-15", 8, 0), Ends: at("2026-10-15", 9, 0), What: "Service", TechnicianID: techUserID})
	if err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "booked_on_you"); len(got) != 0 {
		t.Fatalf("a booking with no car is waiting: %+v", got)
	}
	job := newJob(t, pool)
	if err := workshop.ArriveBooking(ctx, pool, advisor(), id, job); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "booked_on_you"); len(got) != 1 || got[0].JobID != job {
		t.Fatalf("the technician's list: %+v", got)
	}
	if err := workshop.ClockIn(ctx, pool, technician(), job); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "booked_on_you"); len(got) != 0 {
		t.Errorf("started, and still told: %+v", got)
	}
}

// A part fitted and never billed is the parts desk's to catch.
func TestAPartTakenOutAndNotPricedWaitsForTheDesk(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	addTechnician(t, pool)
	part := addPart(t, pool, "BP-9", "Bromsbelägg", "each", 10000)
	job := newJob(t, pool)
	if err := workshop.TakeOut(ctx, pool, technician(), job, part, 1); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, partsDesk(), "taken_out_unpriced"); len(got) != 1 || !strings.Contains(got[0].Text, "BP-9") {
		t.Fatalf("the parts desk's list: %+v", got)
	}
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{Kind: "part", PartID: part,
		Description: "Bromsbelägg", QuantityMilli: 1000, UnitPriceMinor: 10000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, partsDesk(), "taken_out_unpriced"); len(got) != 0 {
		t.Errorf("priced, and still waiting: %+v", got)
	}
}
