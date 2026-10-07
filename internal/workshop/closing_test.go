package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A job that was collected and paid stayed open on the board until somebody
// remembered the last button.

func invoicedJob(t *testing.T, pool *pgxpool.Pool) (job, invoiceID string) {
	t.Helper()
	ctx := context.Background()
	addTechnician(t, pool)
	job = working(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Byte av bromsbelägg", QuantityMilli: 1000,
		UnitPriceMinor: 100000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	move(t, pool, job, workshop.StateInProgress, workshop.StateReady)
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return job, inv.ID
}

func stateOf(t *testing.T, pool *pgxpool.Pool, job string) workshop.State {
	t.Helper()
	var s workshop.State
	database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, job).Scan(&s)
	})
	return s
}

func pay(t *testing.T, pool *pgxpool.Pool, invoiceID string) workshop.Payment {
	t.Helper()
	p, err := workshop.RecordPayment(context.Background(), pool, advisor(), invoiceID,
		workshop.NewPayment{Method: "card", PaidOn: time.Now()})
	if err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}
	return p
}

func TestAJobClosesWhenTheCarIsCollectedAndThenPaid(t *testing.T) {
	pool := setup(t)
	job, inv := invoicedJob(t, pool)
	if err := workshop.CarCollected(context.Background(), pool, advisor(), job, nil); err != nil {
		t.Fatal(err)
	}
	if s := stateOf(t, pool, job); s != workshop.StateInvoiced {
		t.Fatalf("collected and unpaid is %s, want invoiced", s)
	}
	pay(t, pool, inv)
	if s := stateOf(t, pool, job); s != workshop.StateClosed {
		t.Errorf("collected and paid is %s, want closed", s)
	}
}

func TestAJobClosesWhenItIsPaidAndThenCollected(t *testing.T) {
	pool := setup(t)
	job, inv := invoicedJob(t, pool)
	pay(t, pool, inv)
	if s := stateOf(t, pool, job); s != workshop.StateInvoiced {
		t.Fatalf("paid with the car still here is %s, want invoiced", s)
	}
	if err := workshop.CarCollected(context.Background(), pool, advisor(), job, nil); err != nil {
		t.Fatal(err)
	}
	if s := stateOf(t, pool, job); s != workshop.StateClosed {
		t.Errorf("paid and collected is %s, want closed", s)
	}
}

// By hand with money owing is a decision, and it is kept.
func TestClosingByHandWithMoneyOwedNeedsAReason(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, _ := invoicedJob(t, pool)

	if err := workshop.CloseJob(ctx, pool, advisor(), job, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("closed with money owed and no reason: %v", err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateClosed); err == nil {
		t.Error("SetState closed a job without asking why")
	}
	if err := workshop.CloseJob(ctx, pool, advisor(), job, "Kunden har gått i konkurs"); err != nil {
		t.Fatalf("CloseJob: %v", err)
	}
	var reason string
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT closed_reason FROM work_orders WHERE id = $1`, job).Scan(&reason)
	})
	if stateOf(t, pool, job) != workshop.StateClosed || reason != "Kunden har gått i konkurs" {
		t.Errorf("state %s, reason %q", stateOf(t, pool, job), reason)
	}
}

func TestClosingASettledJobByHandNeedsNoReason(t *testing.T) {
	pool := setup(t)
	job, inv := invoicedJob(t, pool)
	pay(t, pool, inv)
	if err := workshop.CloseJob(context.Background(), pool, advisor(), job, ""); err != nil {
		t.Errorf("CloseJob: %v", err)
	}
}

// A closed job is never edited. A payment reversed afterwards leaves it
// closed, and the money owed shows where receivables are followed up.
func TestAReversalAfterClosingReopensNothing(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, inv := invoicedJob(t, pool)
	if err := workshop.CarCollected(ctx, pool, advisor(), job, nil); err != nil {
		t.Fatal(err)
	}
	p := pay(t, pool, inv)
	if err := workshop.ReversePayment(ctx, pool, advisor(), p.ID); err != nil {
		t.Fatalf("ReversePayment: %v", err)
	}
	if s := stateOf(t, pool, job); s != workshop.StateClosed {
		t.Errorf("the reversal moved the job to %s", s)
	}
	owed, err := workshop.Receivables(ctx, pool, advisor())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range owed {
		if b.InvoiceID == inv && b.OutstandingMinor() > 0 {
			found = true
		}
	}
	if !found {
		t.Error("the money owed after the reversal is not on the receivables")
	}
}
