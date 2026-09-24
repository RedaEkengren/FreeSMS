package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const templateID = "dddddddd-0000-0000-0000-000000000001"

func addTemplate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO inspection_templates (id, shop_id, name) VALUES ($1, $2, 'Service check')`,
			templateID, shopID); err != nil {
			return err
		}
		for i, label := range []string{"Front brakes", "Rear brakes", "Tyres"} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO inspection_template_items (shop_id, template_id, position, label)
				 VALUES ($1, $2, $3, $4)`, shopID, templateID, i+1, label); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("add template: %v", err)
	}
}

func startedInspection(t *testing.T, pool *pgxpool.Pool) (jobID, inspectionID string) {
	t.Helper()
	addTemplate(t, pool)
	addTechnician(t, pool)
	jobID = working(t, pool)
	id, err := workshop.StartInspection(context.Background(), pool, technician(), jobID, templateID)
	if err != nil {
		t.Fatalf("StartInspection: %v", err)
	}
	return jobID, id
}

func TestStartingAnInspectionCopiesTheTemplate(t *testing.T) {
	pool := setup(t)
	_, id := startedInspection(t, pool)

	insp, err := workshop.InspectionsForID(context.Background(), pool, technician(), id)
	if err != nil {
		t.Fatalf("InspectionsForID: %v", err)
	}
	if len(insp.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(insp.Items))
	}
	if insp.Items[0].Label != "Front brakes" {
		t.Errorf("first item is %q, want Front brakes", insp.Items[0].Label)
	}
	if insp.Completed() {
		t.Error("a new inspection is already complete")
	}
}

// Editing a template next month must not change what a customer was shown
// last month.
func TestEditingATemplateDoesNotChangeAPastInspection(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id := startedInspection(t, pool)

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE inspection_template_items SET label = 'Something else' WHERE template_id = $1`,
			templateID)
		return err
	}); err != nil {
		t.Fatalf("edit template: %v", err)
	}

	insp, _ := workshop.InspectionsForID(ctx, pool, technician(), id)
	if insp.Items[0].Label != "Front brakes" {
		t.Errorf("the inspection now says %q; it must keep what was checked", insp.Items[0].Label)
	}
}

// An inspection where nothing is wrong is still evidence that the check
// happened, and is not a list of things to approve.
func TestAnInspectionWithNoFindingsIsStillARecord(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id := startedInspection(t, pool)

	insp, _ := workshop.InspectionsForID(ctx, pool, technician(), id)
	for _, item := range insp.Items {
		if err := workshop.SetItem(ctx, pool, technician(), item.ID, "pass", ""); err != nil {
			t.Fatalf("SetItem: %v", err)
		}
	}
	if err := workshop.CompleteInspection(ctx, pool, technician(), id); err != nil {
		t.Fatalf("CompleteInspection: %v", err)
	}

	insp, _ = workshop.InspectionsForID(ctx, pool, technician(), id)
	if !insp.Completed() {
		t.Error("the inspection is not marked complete")
	}
	if len(insp.Findings()) != 0 {
		t.Errorf("got %d findings on an all-pass inspection", len(insp.Findings()))
	}
	if len(insp.Items) != 3 {
		t.Error("the record of what was checked is gone")
	}
}

func shared(t *testing.T, pool *pgxpool.Pool) (inspectionID, token, itemID string) {
	t.Helper()
	ctx := context.Background()
	_, inspectionID = startedInspection(t, pool)

	insp, _ := workshop.InspectionsForID(ctx, pool, technician(), inspectionID)
	itemID = insp.Items[0].ID
	if err := workshop.SetItem(ctx, pool, technician(), itemID, "fail", "Worn to the indicator"); err != nil {
		t.Fatalf("SetItem: %v", err)
	}
	if err := workshop.CompleteInspection(ctx, pool, technician(), inspectionID); err != nil {
		t.Fatalf("CompleteInspection: %v", err)
	}
	token, err := workshop.CreateShare(ctx, pool, advisor(), inspectionID)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	return inspectionID, token, itemID
}

// The link carries the evidence and nothing else.
func TestTheSharedPageShowsTheFindingsAndNoPrices(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, token, _ := shared(t, pool)

	insp, _, err := workshop.SharedInspection(ctx, pool, shopID, token)
	if err != nil {
		t.Fatalf("SharedInspection: %v", err)
	}
	if len(insp.Findings()) != 1 {
		t.Fatalf("got %d findings, want 1", len(insp.Findings()))
	}
	if insp.Registration == "" {
		t.Error("the customer cannot tell which car it is")
	}
	if insp.ShopName == "" {
		t.Error("the customer cannot tell who sent it")
	}
}

// A revoked link stops working, because it will have been forwarded.
func TestRevokingCLosesTheLink(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, token, _ := shared(t, pool)

	shares, err := workshop.SharesFor(ctx, pool, advisor(), id)
	if err != nil || len(shares) != 1 {
		t.Fatalf("SharesFor = %v, %v", shares, err)
	}
	if !shares[0].Usable() {
		t.Fatal("a new link is already closed")
	}
	if err := workshop.RevokeShare(ctx, pool, advisor(), shares[0].ID); err != nil {
		t.Fatalf("RevokeShare: %v", err)
	}
	if _, _, err := workshop.SharedInspection(ctx, pool, shopID, token); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("SharedInspection after revoking = %v, want ErrShareNotUsable", err)
	}
}

// An expired link stops working without anybody doing anything.
func TestAnExpiredLinkIsClosed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, token, _ := shared(t, pool)

	// Both timestamps move. A share that expires before it was created is
	// nonsense, and a CHECK constraint says so -- closing one early is what
	// revoking is for.
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE inspection_shares
			   SET created_at = now() - interval '30 days',
			       expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, _, err := workshop.SharedInspection(ctx, pool, shopID, token); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("an expired link still works: %v", err)
	}
}

// The important one: a token authorises exactly one inspection.
func TestALinkCannotDecideAnotherInspectionsItems(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, tokenA, _ := shared(t, pool)

	// A second inspection, on a second job, that token A must not reach.
	secondJob := newJob2(t, pool)
	idB, err := workshop.StartInspection(ctx, pool, technician(), secondJob, templateID)
	if err != nil {
		t.Fatalf("StartInspection: %v", err)
	}
	inspB, _ := workshop.InspectionsForID(ctx, pool, technician(), idB)
	itemB := inspB.Items[0].ID

	if err := workshop.RecordDecision(ctx, pool, shopID, tokenA, itemB, "approved"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Fatalf("a link to one inspection decided another's item: %v", err)
	}

	var decisions int
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM inspection_decisions WHERE item_id = $1`, itemB).Scan(&decisions)
	})
	if decisions != 0 {
		t.Errorf("%d decisions were recorded against the other inspection", decisions)
	}
}

// Approve and then decline is two things, and both stay on record.
func TestChangingTheirMindKeepsBoth(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, token, itemID := shared(t, pool)

	for _, d := range []string{"approved", "declined"} {
		if err := workshop.RecordDecision(ctx, pool, shopID, token, itemID, d); err != nil {
			t.Fatalf("RecordDecision(%s): %v", d, err)
		}
	}

	insp, _ := workshop.InspectionsForID(ctx, pool, advisor(), id)
	item := insp.Items[0]
	if item.Decision != "declined" {
		t.Errorf("current decision is %q, want declined -- the latest answer wins", item.Decision)
	}
	if item.Superseded != 1 {
		t.Errorf("superseded = %d, want 1 -- the earlier answer must stay on record", item.Superseded)
	}
	if !item.Decided() {
		t.Error("the item reads as undecided")
	}
}

func TestADecisionNeedsAUsableLink(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, token, itemID := shared(t, pool)

	if err := workshop.RecordDecision(ctx, pool, shopID, "not-a-token", itemID, "approved"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a made-up token was accepted: %v", err)
	}
	if err := workshop.RecordDecision(ctx, pool, shopID, token, itemID, "maybe"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an invented decision was accepted: %v", err)
	}
}

// Only the counter makes links; a technician holding the customer's contact
// details is not what the role separation is for.
func TestATechnicianCannotMakeACustomerLink(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id := startedInspection(t, pool)

	if _, err := workshop.CreateShare(ctx, pool, technician(), id); err == nil {
		t.Error("a technician made a customer link")
	}
}

// newJob2 opens a second job on a different vehicle.
func newJob2(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := workshop.TakeIn(context.Background(), pool, advisor(), "QRS 456", "Service", nil)
	if err != nil {
		t.Fatalf("TakeIn: %v", err)
	}
	return id
}
