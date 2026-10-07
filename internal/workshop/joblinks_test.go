package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The customer could only be told their car was ready on the telephone.

func TestTheCustomersLinkFollowsTheJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := working(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Byte av bromsbelägg", QuantityMilli: 1000,
		UnitPriceMinor: 100000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	token, err := workshop.CreateJobLink(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("CreateJobLink: %v", err)
	}
	status := func() workshop.CustomerStatus {
		t.Helper()
		st, err := workshop.JobStatus(ctx, pool, shopID, token)
		if err != nil {
			t.Fatalf("JobStatus: %v", err)
		}
		return st
	}

	st := status()
	if st.Stage != "in_work" || st.Invoiced || st.GrossMinor != 0 {
		t.Fatalf("while working: %+v, want in work and no money", st)
	}

	move(t, pool, job, workshop.StateInProgress, workshop.StateReady)
	if st := status(); st.Stage != "ready" {
		t.Errorf("ready reads as %q", st.Stage)
	}
	// More work found: the customer is not left reading a stale "ready".
	move(t, pool, job, workshop.StateInProgress)
	if st := status(); st.Stage != "in_work" {
		t.Errorf("back on the lift reads as %q", st.Stage)
	}

	move(t, pool, job, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	st = status()
	if !st.Invoiced || st.GrossMinor != 125000 || st.OutstandingMinor != 125000 || st.Stage != "ready" {
		t.Errorf("after invoicing: %+v, want 1 250,00 owed and ready to collect", st)
	}
}

func TestTheCustomersLinkSaysWhenTheCarIsWithThem(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	back := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC)
	if err := workshop.CarCollected(ctx, pool, advisor(), job, &back); err != nil {
		t.Fatal(err)
	}
	token, _ := workshop.CreateJobLink(ctx, pool, advisor(), job)
	st, err := workshop.JobStatus(ctx, pool, shopID, token)
	if err != nil || !st.CarAway || st.ExpectedBack == nil || !st.ExpectedBack.Equal(back) {
		t.Errorf("status = %+v, %v; want the car with them, due back on the 15th", st, err)
	}
}

// One link per job: making a new one closes the old, which is how a link
// sent to the wrong number is put right.
func TestANewLinkClosesTheOldOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	first, _ := workshop.CreateJobLink(ctx, pool, advisor(), job)
	second, err := workshop.CreateJobLink(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.JobStatus(ctx, pool, shopID, first); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("the old link still opens: %v", err)
	}
	if _, err := workshop.JobStatus(ctx, pool, shopID, second); err != nil {
		t.Errorf("the new link does not open: %v", err)
	}
	if _, err := workshop.JobStatus(ctx, pool, shopID, "made-up"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a made-up token: %v", err)
	}
}

func TestOnlyTheCounterMakesTheCustomersLink(t *testing.T) {
	pool := setup(t)
	job := newJob(t, pool)
	if _, err := workshop.CreateJobLink(context.Background(), pool, technician(), job); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician made a customer link: %v", err)
	}
}
