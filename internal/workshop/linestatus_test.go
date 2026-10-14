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

func quote(t *testing.T, pool *pgxpool.Pool, job, what string, hours int64) {
	t.Helper()
	if err := workshop.AddLine(context.Background(), pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: what, QuantityMilli: hours,
		UnitPriceMinor: 89500, VATRateBasis: 2500,
	}); err != nil {
		t.Fatalf("AddLine %s: %v", what, err)
	}
}

func statuses(t *testing.T, pool *pgxpool.Pool, job string) []string {
	t.Helper()
	_, lines, err := workshop.JobByID(context.Background(), pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range lines {
		out = append(out, l.Status())
	}
	return out
}

// Writing a line is not the customer agreeing to it. The yes is recorded when
// it is given, and covers the quote as it stood.
func TestAQuotedLineIsNotApprovedUntilTheCustomerSaysYes(t *testing.T) {
	pool := setup(t)
	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1500)
	if got := statuses(t, pool, job); len(got) != 1 || got[0] != "quoted" {
		t.Fatalf("a line written in a draft is %v, want quoted", got)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval)
	if got := statuses(t, pool, job); got[0] != "quoted" {
		t.Errorf("waiting for the customer, the line is %v", got)
	}
	move(t, pool, job, workshop.StateApproved)
	if got := statuses(t, pool, job); got[0] != "approved" {
		t.Errorf("after the yes, the line is %v", got)
	}
}

// The issue's case: the customer says no before anything is done. Nothing can
// be invoiced, and the job can be cancelled.
func TestADeclinedQuoteWithNoWorkDoneIsNotInvoicedAndCanBeCancelled(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1500)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateDeclined)

	if got := statuses(t, pool, job); got[0] != "declined" {
		t.Errorf("the declined quote line is %v", got)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Fatalf("invoicing a declined quote = %v, want ErrNothingToInvoice", err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateCancelled); err != nil {
		t.Errorf("cancelling a declined quote with no work on it: %v", err)
	}
	// The offer stays on the record.
	if got := statuses(t, pool, job); len(got) != 1 {
		t.Errorf("the quote line was deleted: %v", got)
	}
}

// A quote approved and then declined before the car is touched is the same.
func TestAnApprovedQuoteDeclinedBeforeWorkIsNotInvoiced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1500)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	if err := workshop.Decline(ctx, pool, advisor(), job, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("invoicing = %v, want ErrNothingToInvoice", err)
	}
}

// After teardown the customer declines the repair. What was done is billed;
// what was only offered is not.
func TestDecliningAfterTeardownBillsOnlyWhatWasPerformed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Diagnosis", 1000)
	quote(t, pool, job, "Replace gearbox", 8000)
	quote(t, pool, job, "Reassembly", 500)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress)

	// The desk names a line that is not on the job: refused, and nothing moves.
	if err := workshop.Decline(ctx, pool, advisor(), job, []int{1, 9}); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("declining with a line that is not there = %v, want ErrInvalid", err)
	}
	if got := statuses(t, pool, job); got[0] != "approved" {
		t.Errorf("a refused decline left the lines %v", got)
	}
	// A technician owns "ready", not the conversation with the customer.
	if err := workshop.Decline(ctx, pool, technician(), job, []int{1}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician declined the job: %v", err)
	}

	if err := workshop.Decline(ctx, pool, advisor(), job, []int{1, 3}); err != nil {
		t.Fatal(err)
	}
	got := statuses(t, pool, job)
	if got[0] != "performed" || got[1] != "declined" || got[2] != "performed" {
		t.Fatalf("the lines are %v", got)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	// 1.5 hours at 895.00 excluding VAT.
	if inv.NetMinor != 134250 {
		t.Errorf("the invoice is %d net, want 134250: diagnosis and reassembly only", inv.NetMinor)
	}
}

// The desk can correct it afterwards, either way, until the invoice exists.
func TestAPerformedMarkCanBeChangedOnADeclinedJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Diagnosis", 1000)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateDeclined)

	if err := workshop.MarkPerformed(ctx, pool, partsDesk(), job, 1, true); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the parts desk marked a line done: %v", err)
	}
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 7, true); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("marking a line that is not there = %v", err)
	}
	if err := workshop.MarkPerformed(ctx, pool, technician(), job, 1, true); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, pool, job); got[0] != "performed" {
		t.Errorf("after marking: %v", got)
	}
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 1, false); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, pool, job); got[0] != "declined" {
		t.Errorf("taking the mark back on a declined job leaves %v, want declined", got)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("invoicing = %v", err)
	}
	// Once there is a document the lines are settled.
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 1, false); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("changing a line after the invoice = %v, want ErrInvalid", err)
	}
}

// Work added on the lift after the customer agreed is not in their yes; when
// they decline, only the line marked performed survives.
func TestDecliningKeepsOnlyThePerformedLines(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1000)
	quote(t, pool, job, "Wheel alignment", 1000)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress)
	quote(t, pool, job, "Seized bolt, drilled out", 500)
	if got := statuses(t, pool, job); got[2] != "quoted" {
		t.Fatalf("an added line is %v", got)
	}
	if err := workshop.Decline(ctx, pool, advisor(), job, []int{1}); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM work_order_lines WHERE work_order_id = $1 AND declined_at IS NULL`, job).Scan(&kept)
	}); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Errorf("%d lines survive the decline, want only the performed one", kept)
	}
}
