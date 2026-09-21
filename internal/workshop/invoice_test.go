package workshop_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readyToInvoice returns a work order with two priced lines and a warranty
// line, sitting at ready.
func readyToInvoice(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	id := newJob(t, pool)

	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Replace front pads", QuantityMilli: 2500,
			UnitPriceMinor: 89500, VATRateBasis: 2500},
		{Kind: "part", Description: "Engine oil 5W-30", QuantityMilli: 4500,
			UnitPriceMinor: 12900, VATRateBasis: 2500},
		{Kind: "part", Description: "Caliper (warranty)", QuantityMilli: 1000,
			UnitPriceMinor: 0, VATRateBasis: 2500, CostBearer: "supplier"},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), id, l); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	return id
}

func TestIssueSnapshotsTheCustomerAndTheLines(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := readyToInvoice(t, pool)

	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if inv.Reference() != "A-1" {
		t.Errorf("reference = %q, want A-1", inv.Reference())
	}
	if inv.CustomerName != "A Customer" {
		t.Errorf("customer = %q, want the name at the time of issue", inv.CustomerName)
	}

	// 2.5 x 895.00 = 2237.50, plus 4.5 x 129.00 = 580.50. The warranty line is
	// on the order and not on the invoice.
	if inv.NetMinor != 281800 {
		t.Errorf("net = %d, want 281800", inv.NetMinor)
	}
	if inv.GrossMinor != inv.NetMinor+inv.VATMinor {
		t.Error("gross is not net plus VAT")
	}

	var lines int
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM invoice_lines WHERE invoice_id = $1`, inv.ID).Scan(&lines)
	}); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if lines != 2 {
		t.Errorf("the invoice has %d lines, want 2 -- the warranty line must not be charged", lines)
	}

	// Renaming the customer afterwards must not reach the document.
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE people SET display_name = 'Renamed Later'
		                        WHERE id = '22222222-2222-2222-2222-222222222223'`)
		return err
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var stored string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT customer_name FROM invoices WHERE id = $1`, inv.ID).Scan(&stored)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "A Customer" {
		t.Errorf("the invoice now says %q; it must keep what was true when it was issued", stored)
	}
}

// The property the whole numbering design exists for.
func TestNumbersAreGapFreeAndUniqueUnderConcurrency(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	const n = 8
	ids := make([]string, n)
	for i := range ids {
		ids[i] = readyToInvoice(t, pool)
	}

	var wg sync.WaitGroup
	numbers := make([]int64, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inv, err := workshop.Issue(ctx, pool, advisor(), ids[i])
			numbers[i], errs[i] = inv.Number, err
		}(i)
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("issue %d failed: %v", i, err)
		}
		if seen[numbers[i]] {
			t.Fatalf("number %d was issued twice", numbers[i])
		}
		seen[numbers[i]] = true
	}
	for want := int64(1); want <= n; want++ {
		if !seen[want] {
			t.Errorf("number %d is missing; the series has a gap", want)
		}
	}
}

// A transaction that fails after allocating must not spend the number.
func TestAFailedIssueLeavesNoGap(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	first := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), first); err != nil {
		t.Fatalf("first issue: %v", err)
	}

	// An order with nothing chargeable on it: the state moves, the number is
	// allocated, and then it fails.
	empty := newJob(t, pool)
	move(t, pool, empty, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), empty); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Fatalf("issuing an empty order = %v, want ErrNothingToInvoice", err)
	}

	third := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), third)
	if err != nil {
		t.Fatalf("third issue: %v", err)
	}
	if inv.Number != 2 {
		t.Errorf("the next invoice is number %d, want 2 -- the failed issue spent a number", inv.Number)
	}
}

// "We never update invoices" is a convention. This is a refusal.
func TestAnIssuedInvoiceCannotBeChanged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	for _, stmt := range []string{
		`UPDATE invoices SET gross_minor = 1 WHERE id = '%s'`,
		`DELETE FROM invoices WHERE id = '%s'`,
	} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(stmt, inv.ID))
			return err
		})
		if err == nil {
			t.Errorf("%q was allowed", stmt)
			continue
		}
		if !contains(err.Error(), "credit note") {
			t.Errorf("the refusal does not say what to do instead: %v", err)
		}
	}
}

// A credit note reverses exactly, to the minor unit.
func TestCreditNoteReversesTheInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	note, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	if note.Number != inv.Number+1 {
		t.Errorf("credit note is %s, want the next number in the same series", note.Reference())
	}
	if note.CreditOfID == nil || *note.CreditOfID != inv.ID {
		t.Error("the credit note does not name what it reverses")
	}
	if inv.GrossMinor+note.GrossMinor != 0 {
		t.Errorf("invoice %d and credit note %d leave %d behind",
			inv.GrossMinor, note.GrossMinor, inv.GrossMinor+note.GrossMinor)
	}
	if inv.VATMinor+note.VATMinor != 0 {
		t.Error("the VAT does not reverse exactly")
	}

	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err == nil {
		t.Error("the same invoice was credited twice")
	}
}

func TestAnOrderCannotBeInvoicedTwice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), id); !errors.Is(err, workshop.ErrAlreadyInvoiced) {
		t.Fatalf("second Issue = %v, want ErrAlreadyInvoiced", err)
	}
}
