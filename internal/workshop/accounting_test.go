package workshop_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

func period() (time.Time, time.Time) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

func TestTheExportContainsTheMonthsInvoices(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	from, to := period()
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if count != 1 {
		t.Errorf("exported %d documents, want 1", count)
	}
	if !bytes.Contains(body, []byte("#SIETYP 4")) {
		t.Error("that is not a SIE 4 file")
	}
	if !bytes.Contains(body, []byte("#FORMAT PC8")) {
		t.Error("the file does not declare its encoding")
	}
	// The receivable carries the gross, the sale the net, the VAT the rest.
	if !bytes.Contains(body, []byte("#TRANS 1510 {} 3522.50")) &&
		!bytes.Contains(body, []byte("#TRANS 1510 {}")) {
		t.Error("there is no receivable entry")
	}
	if !bytes.Contains(body, []byte("#VER \"A\" \"1\"")) {
		t.Errorf("the invoice %s is not in the file", inv.Reference())
	}
}

// An accountant who imports the same file twice produces duplicate
// verifications and a balance nobody can explain.
func TestExportingTwiceIsRefusedUnlessAskedForAgain(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if _, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()

	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); !errors.Is(err, workshop.ErrAlreadyExported) {
		t.Fatalf("second export = %v, want ErrAlreadyExported", err)
	}
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, true); err != nil {
		t.Fatalf("a deliberate re-export was refused: %v", err)
	}
}

// Marking an invoice as exported is bookkeeping about the document, not a
// change to what it says -- and nothing else may move.
func TestExportingDoesNotOtherwiseTouchTheInvoice(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); err != nil {
		t.Fatalf("export: %v", err)
	}

	var gross int64
	var name string
	var exported bool
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT gross_minor, customer_name, accounting_export_id IS NOT NULL
			 FROM invoices WHERE id = $1`, inv.ID).Scan(&gross, &name, &exported)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !exported {
		t.Error("the invoice was not marked as exported")
	}
	if gross != inv.GrossMinor || name != "A Customer" {
		t.Error("exporting changed what the invoice says")
	}

	// And the trigger still refuses everything else.
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE invoices SET gross_minor = 1 WHERE id = $1`, inv.ID)
		return err
	})
	if err == nil {
		t.Error("an invoice could be edited after being exported")
	}
}

// A credit note is its own verification, not a correction of the original.
func TestACreditNoteIsItsOwnVerification(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatalf("CreditNote: %v", err)
	}

	from, to := period()
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if count != 2 {
		t.Errorf("exported %d documents, want 2 -- the invoice and the credit note", count)
	}
	if !bytes.Contains(body, []byte("Kreditfaktura")) {
		t.Error("the credit note is not labelled as one")
	}
	// Two verifications, and the file as a whole nets to nothing.
	if bytes.Count(body, []byte("#VER ")) != 2 {
		t.Errorf("got %d verifications, want 2", bytes.Count(body, []byte("#VER ")))
	}
}

func TestExportingAnEmptyPeriodIsRefused(t *testing.T) {
	pool := setup(t)
	from, to := period()
	if _, _, err := workshop.ExportAccounting(context.Background(), pool, advisor(),
		from.AddDate(-1, 0, 0), to.AddDate(-1, 0, 0), false); !errors.Is(err, workshop.ErrNothingToExport) {
		t.Errorf("an empty period = %v, want ErrNothingToExport", err)
	}
}

// A customer called Öberg must arrive spelled correctly.
func TestASwedishNameSurvivesTheExport(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE people SET display_name = 'Margareta Öberg' WHERE id = '22222222-2222-2222-2222-222222222223'`)
		return err
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if _, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()
	body, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}

	// Ö is one CP437 byte, 153 -- not the two bytes UTF-8 would write.
	if !bytes.Contains(body, append([]byte("Margareta "), 153, 'b', 'e', 'r', 'g')) {
		t.Error("Öberg did not survive the encoding")
	}
	if bytes.Contains(body, []byte("Öberg")) {
		t.Error("the file contains UTF-8; the accountant would see a mangled name")
	}
}

func TestTheExportIsNotForTechnicians(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	from, to := period()
	if _, _, err := workshop.ExportAccounting(context.Background(), pool, technician(), from, to, false); err == nil {
		t.Error("a technician exported the books")
	}
}
