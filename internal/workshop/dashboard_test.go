package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

func thisMonth() (time.Time, time.Time) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

// One large job moves the average and does not move the middle, which is why
// both are shown.
func TestTheAverageAndTheMiddleAreBothReported(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	// Three ordinary jobs and one that is ten times the size.
	for i := 0; i < 3; i++ {
		id := newJob(t, pool)
		if err := workshop.AddLine(ctx, pool, advisor(), id, workshop.NewLine{
			Kind: "labour", Description: "Service", QuantityMilli: 1000,
			UnitPriceMinor: 100000, VATRateBasis: 2500}); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
		move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
			workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
		if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
			t.Fatalf("Issue: %v", err)
		}
	}
	big := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), big, workshop.NewLine{
		Kind: "labour", Description: "Engine rebuild", QuantityMilli: 1000,
		UnitPriceMinor: 4000000, VATRateBasis: 2500}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	move(t, pool, big, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), big); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	from, to := thisMonth()
	d, err := workshop.Summary(ctx, pool, advisor(), from, to)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if d.Invoices != 4 {
		t.Fatalf("invoices = %d, want 4", d.Invoices)
	}
	if d.MedianMinor != 100000 {
		t.Errorf("middle order = %d, want 100000 -- one big job must not move it", d.MedianMinor)
	}
	if d.MeanMinor <= d.MedianMinor {
		t.Errorf("mean %d is not above median %d; the fixture no longer shows the difference",
			d.MeanMinor, d.MedianMinor)
	}
}

// Leaving credit notes out would make a month look better than the books do,
// which is the one direction a figure must never be wrong in.
func TestCreditNotesReduceTheTotal(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	id := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	from, to := thisMonth()
	before, _ := workshop.Summary(ctx, pool, advisor(), from, to)
	if before.NetMinor != inv.NetMinor {
		t.Fatalf("net = %d, want %d", before.NetMinor, inv.NetMinor)
	}

	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	after, _ := workshop.Summary(ctx, pool, advisor(), from, to)
	if after.NetMinor != 0 {
		t.Errorf("net after crediting = %d, want 0", after.NetMinor)
	}
}

// Somebody who approved and then declined has declined.
func TestApprovalRateCountsOnlyTheLatestAnswer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, token, itemID := shared(t, pool)

	if err := workshop.RecordDecision(ctx, pool, shopID, token, itemID, "approved"); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	from, to := thisMonth()
	d, _ := workshop.Summary(ctx, pool, advisor(), from, to)
	if d.FindingsDecided != 1 || d.ApprovalRate() != 100 {
		t.Fatalf("after approving: decided %d, rate %d%%", d.FindingsDecided, d.ApprovalRate())
	}

	if err := workshop.RecordDecision(ctx, pool, shopID, token, itemID, "declined"); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	d, _ = workshop.Summary(ctx, pool, advisor(), from, to)
	if d.FindingsDecided != 1 {
		t.Errorf("decided = %d, want 1 -- changing an answer is not a second finding", d.FindingsDecided)
	}
	if d.ApprovalRate() != 0 {
		t.Errorf("approval rate = %d%%, want 0 -- the latest answer was no", d.ApprovalRate())
	}
}

// A shop that has not started using inspections should not be shown a nought.
func TestNoFindingsMeansTheSectionIsNotShown(t *testing.T) {
	pool := setup(t)
	from, to := thisMonth()
	d, err := workshop.Summary(context.Background(), pool, advisor(), from, to)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if d.HasFindings() {
		t.Error("a shop with no inspections claims to have findings")
	}
}

func TestTheFiguresAreNotForTechnicians(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	from, to := thisMonth()
	if _, err := workshop.Summary(context.Background(), pool, technician(), from, to); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the shop's figures: %v", err)
	}
}

// A job invoiced outside the period does not count towards it.
func TestThePeriodIsRespected(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	id := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Last month.
	from, _ := thisMonth()
	d, err := workshop.Summary(ctx, pool, advisor(), from.AddDate(0, -1, 0), from)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if d.Invoices != 0 {
		t.Errorf("last month shows %d invoices from a job invoiced today", d.Invoices)
	}
}
