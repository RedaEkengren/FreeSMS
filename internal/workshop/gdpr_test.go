package workshop_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// The customer person in the fixture.
const customerPersonID = "22222222-2222-2222-2222-222222222223"

func TestExportGathersEverythingHeldAboutAPerson(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	id := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	e, err := workshop.ExportPerson(ctx, pool, advisor(), customerPersonID)
	if err != nil {
		t.Fatalf("ExportPerson: %v", err)
	}
	if e.Person.Name != "A Customer" {
		t.Errorf("name = %q", e.Person.Name)
	}
	if len(e.Vehicles) == 0 {
		t.Error("the export does not mention the vehicle they own")
	}
	if len(e.Jobs) == 0 {
		t.Error("the export does not mention their jobs")
	}
	if len(e.Invoices) != 1 {
		t.Fatalf("got %d invoices, want 1", len(e.Invoices))
	}
	// The retention is stated per document rather than left to be inferred.
	if !e.Invoices[0].KeptUntil.After(e.Invoices[0].IssuedAt) {
		t.Error("the invoice does not say how long it is kept")
	}

	body, err := workshop.MarshalExport(e)
	if err != nil {
		t.Fatalf("MarshalExport: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatalf("the export is not readable JSON: %v", err)
	}
	if !strings.Contains(string(body), "bookkeeping law") {
		t.Error("the export does not explain why anything is kept")
	}
}

// The right to erasure against a seven-year obligation to keep accounting
// records. Refusing is wrong and deleting is illegal.
func TestErasureClearsThePersonAndKeepsTheInvoice(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	job := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if err := workshop.ErasePerson(ctx, pool, advisor(), customerPersonID, "Asked by email"); err != nil {
		t.Fatalf("ErasePerson: %v", err)
	}

	var name string
	var email, phone, address *string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT display_name, email, phone FROM people WHERE id = $1`, customerPersonID).
			Scan(&name, &email, &phone); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT address_line1 FROM customers WHERE person_id = $1`, customerPersonID).Scan(&address)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != workshop.ErasedName {
		t.Errorf("name = %q, want the placeholder", name)
	}
	if email != nil || phone != nil || address != nil {
		t.Error("contact details survived the erasure")
	}

	// The invoice keeps what it said, because the law requires it.
	var invoiceName string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT customer_name FROM invoices WHERE id = $1`, inv.ID).Scan(&invoiceName)
	}); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if invoiceName != "A Customer" {
		t.Errorf("the invoice now says %q; bookkeeping law requires it to keep the buyer", invoiceName)
	}

	// The vehicle's technical history is untouched.
	v, err := workshop.VehicleByID(ctx, pool, advisor(), vehicleA)
	if err != nil {
		t.Fatalf("VehicleByID: %v", err)
	}
	if len(v.History) == 0 {
		t.Error("erasing the owner took the vehicle's history with it")
	}

	// And it is on record, with both halves stated.
	erasures, err := workshop.Erasures(ctx, pool, advisor())
	if err != nil {
		t.Fatalf("Erasures: %v", err)
	}
	if len(erasures) != 1 {
		t.Fatalf("got %d erasure records, want 1", len(erasures))
	}
	if !strings.Contains(erasures[0].Kept, "bookkeeping law") {
		t.Error("the record does not say why anything was kept")
	}
	if erasures[0].Reason != "Asked by email" {
		t.Errorf("reason = %q", erasures[0].Reason)
	}
}

func TestErasingTwiceIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := workshop.ErasePerson(ctx, pool, advisor(), customerPersonID, ""); err != nil {
		t.Fatalf("ErasePerson: %v", err)
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), customerPersonID, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("erasing twice = %v, want a refusal", err)
	}
}

// Somebody who can still sign in has not been erased, they have been renamed.
func TestAnActiveEmployeeCannotBeErased(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	err := workshop.ErasePerson(ctx, pool, advisor(), "22222222-2222-2222-2222-222222222224", "")
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("ErasePerson on an active account = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "deactivate") {
		t.Errorf("the refusal does not say what to do first: %v", err)
	}
}

func TestOnlyTheFrontDeskCanExportOrErase(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if _, err := workshop.ExportPerson(ctx, pool, technician(), customerPersonID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician exported somebody's personal data: %v", err)
	}
	if err := workshop.ErasePerson(ctx, pool, technician(), customerPersonID, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician erased somebody: %v", err)
	}
}

// Deleting the row alone is not deleting anything: the file is the personal
// data.
func TestDeletingAnAttachmentRemovesTheFile(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// startedInspection adds the technician itself.
	_, inspectionID := startedInspection(t, pool)

	insp, _ := workshop.InspectionsForID(ctx, pool, technician(), inspectionID)
	itemID := insp.Items[0].ID
	if err := workshop.AttachPhoto(ctx, pool, technician(), itemID, "abc123", "image/jpeg", 100, "x.jpg"); err != nil {
		t.Fatalf("AttachPhoto: %v", err)
	}

	var attachmentID string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM attachments WHERE storage_key = 'abc123'`).Scan(&attachmentID)
	}); err != nil {
		t.Fatalf("read attachment: %v", err)
	}

	remover := &recordingRemover{}
	if err := workshop.DeleteAttachment(ctx, pool, advisor(), remover, attachmentID); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if len(remover.removed) != 1 || remover.removed[0] != "abc123" {
		t.Errorf("the file was not removed: %v", remover.removed)
	}

	insp, _ = workshop.InspectionsForID(ctx, pool, technician(), inspectionID)
	if len(insp.Items[0].Photos) != 0 {
		t.Error("the photograph is still on the item")
	}
}

// The policy is applied, not written down and forgotten.
func TestSweepRemovesWhatHasOutlivedItsPurpose(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO login_attempts (shop_id, email, succeeded, attempted_at)
			VALUES ($1, 'old@example.test', false, now() - interval '200 days'),
			       ($1, 'new@example.test', false, now())`, shopID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed attempts: %v", err)
	}

	r, err := workshop.Sweep(ctx, pool, shopID)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if r.LoginAttempts != 1 {
		t.Errorf("swept %d login attempts, want 1 -- only the one past its retention", r.LoginAttempts)
	}

	var left int
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM login_attempts`).Scan(&left)
	})
	if left != 1 {
		t.Errorf("%d attempts left, want the recent one", left)
	}
}

type recordingRemover struct{ removed []string }

func (r *recordingRemover) Remove(key string) error {
	r.removed = append(r.removed, key)
	return nil
}
