package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The estimate is the labour lines, warranty included and parts not; the
// clocked time is everybody's, a running clock counted up to now; and the
// promise is what the front desk set.
func TestProgressPutsThePromiseTheHoursAndTheClockTogether(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	addSecondTechnician(t, pool)
	ctx := context.Background()

	job := newJob(t, pool)
	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Kamrem", QuantityMilli: 1500, UnitPriceMinor: 89500, VATRateBasis: 2500, CostBearer: "customer"},
		{Kind: "labour", Description: "Vattenpump, garanti", QuantityMilli: 500, UnitPriceMinor: 0, VATRateBasis: 2500, CostBearer: "supplier"},
		{Kind: "part", Description: "Remsats", QuantityMilli: 1000, UnitPriceMinor: 120000, VATRateBasis: 2500, CostBearer: "customer"},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), job, l); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)

	p, err := workshop.ProgressFor(ctx, pool, technician(), job)
	if err != nil {
		t.Fatalf("ProgressFor: %v", err)
	}
	if p.EstimateMinutes != 120 || p.ClockedMinutes != 0 || p.Verdict() != "" {
		t.Errorf("before work: %+v, want 120 minutes estimated, nothing clocked, nothing promised", p)
	}

	// One technician did an hour; the other is on it now.
	workshop.ClockIn(ctx, pool, technician(), job)
	workshop.ClockOut(ctx, pool, technician(), job)
	entries, _ := workshop.TimeFor(ctx, pool, advisor(), job)
	start := time.Now().Add(-3 * time.Hour)
	if err := workshop.CorrectTime(ctx, pool, advisor(), entries[0].ID, start, start.Add(time.Hour), "as it was"); err != nil {
		t.Fatalf("CorrectTime: %v", err)
	}
	if err := workshop.ClockIn(ctx, pool, secondTechnician(), job); err != nil {
		t.Fatalf("ClockIn: %v", err)
	}

	promise := time.Now().Add(30 * time.Minute)
	if err := workshop.SetPromise(ctx, pool, advisor(), job, &promise); err != nil {
		t.Fatalf("SetPromise: %v", err)
	}
	p, _ = workshop.ProgressFor(ctx, pool, technician(), job)
	if p.ClockedMinutes != 60 || !p.Running || p.Percent() != 50 {
		t.Errorf("clocked %d (running %v, %d%%), want both technicians' 60 so far and a clock running", p.ClockedMinutes, p.Running, p.Percent())
	}
	// An hour left, half an hour to the promise.
	if p.Verdict() != "at risk" {
		t.Errorf("verdict %q, want at risk", p.Verdict())
	}

	if err := workshop.SetPromise(ctx, pool, advisor(), job, nil); err != nil {
		t.Fatalf("clear the promise: %v", err)
	}
	if p, _ = workshop.ProgressFor(ctx, pool, technician(), job); p.PromisedAt != nil {
		t.Error("the promise was not cleared")
	}
}

// The promise is a conversation with the customer: the front desk's.
func TestOnlyTheFrontDeskPromises(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	job := newJob(t, pool)
	when := time.Now().Add(time.Hour)
	for _, s := range []access.Scope{technician(), partsDeskScope()} {
		if err := workshop.SetPromise(ctx, pool, s, job, &when); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("SetPromise as %s: %v, want forbidden", s.Role, err)
		}
	}
	if p, _ := workshop.ProgressFor(ctx, pool, advisor(), job); p.PromisedAt != nil {
		t.Error("a refused promise was recorded")
	}
}
