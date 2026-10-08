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

// A part request was a sentence and a tick; deliveries come in pieces.

func onlyRequest(t *testing.T, pool *pgxpool.Pool) workshop.PartRequest {
	t.Helper()
	open, err := workshop.OpenPartRequests(context.Background(), pool, partsDesk())
	if err != nil || len(open) != 1 {
		t.Fatalf("open requests = %d, %v; want 1", len(open), err)
	}
	return open[0]
}

func jobState(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	job, _, err := workshop.JobByID(context.Background(), pool, technician(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	return job.State
}

func TestHalfADeliveryKeepsTheJobWaitingForTheRest(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)
	if err := workshop.RequestParts(ctx, pool, technician(), id, "Bromsskiva", 4); err != nil {
		t.Fatalf("RequestParts: %v", err)
	}

	req := onlyRequest(t, pool)
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), req.ID, 2, "delivery"); err != nil {
		t.Fatalf("ReceivePart: %v", err)
	}
	req = onlyRequest(t, pool)
	if !req.Partly() || req.Received != 2 || req.Rest() != 2 {
		t.Errorf("after two of four: %+v, want partly here with two to come", req)
	}
	if s := jobState(t, pool, id); s != string(workshop.StateAwaitingParts) {
		t.Errorf("state %s after half a delivery, want awaiting_parts", s)
	}

	// Zero is whatever is still owed.
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), req.ID, 0, "delivery"); err != nil {
		t.Fatalf("ReceivePart: %v", err)
	}
	if s := jobState(t, pool, id); s != string(workshop.StateInProgress) {
		t.Errorf("state %s after the rest came, want in_progress", s)
	}
	requests, _ := workshop.PartRequestsFor(ctx, pool, technician(), id)
	if len(requests) != 1 || requests[0].Open() || requests[0].Received != 4 {
		t.Errorf("requests = %+v, want one, finished, four received", requests)
	}
}

// Five when four were asked for is recorded as it came.
func TestMoreThanWasAskedForIsRecordedAsItCame(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)
	workshop.RequestParts(ctx, pool, technician(), id, "Hjulbult", 4)
	req := onlyRequest(t, pool)
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), req.ID, 5, "delivery"); err != nil {
		t.Fatalf("ReceivePart: %v", err)
	}
	requests, _ := workshop.PartRequestsFor(ctx, pool, technician(), id)
	if requests[0].Open() || requests[0].Received != 5 || requests[0].Rest() != 0 {
		t.Errorf("after five of four: %+v", requests[0])
	}
}

// Off the shelf instead of a delivery: nothing arrives, and the request is
// still finished.
func TestARequestFilledFromTheShelfIsFinished(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)
	workshop.RequestPart(ctx, pool, technician(), id, "Torkarblad")
	req := onlyRequest(t, pool)
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), req.ID, 0, "shelf"); err != nil {
		t.Fatalf("ReceivePart: %v", err)
	}
	if s := jobState(t, pool, id); s != string(workshop.StateInProgress) {
		t.Errorf("state %s, want in_progress", s)
	}
	var source string
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT source FROM part_request_receipts WHERE request_id = $1`, req.ID).Scan(&source)
	})
	if source != "shelf" {
		t.Errorf("source = %q, want shelf", source)
	}
}

// A part that is never coming stops the job waiting -- unless something else
// still is -- and says why.
func TestAPartThatIsNeverComingStopsTheWait(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)
	workshop.RequestPart(ctx, pool, technician(), id, "Styrled")
	workshop.RequestPart(ctx, pool, technician(), id, "Kulled")
	open, _ := workshop.OpenPartRequests(ctx, pool, partsDesk())

	if err := workshop.CancelPartRequest(ctx, pool, partsDesk(), open[0].ID, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("cancelled without a reason: %v", err)
	}
	if err := workshop.CancelPartRequest(ctx, pool, partsDesk(), open[0].ID, "Utgången hos leverantören"); err != nil {
		t.Fatalf("CancelPartRequest: %v", err)
	}
	if s := jobState(t, pool, id); s != string(workshop.StateAwaitingParts) {
		t.Errorf("state %s with another part still coming, want awaiting_parts", s)
	}
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), open[1].ID, 0, "delivery"); err != nil {
		t.Fatal(err)
	}
	if s := jobState(t, pool, id); s != string(workshop.StateInProgress) {
		t.Errorf("state %s once nothing is coming, want in_progress", s)
	}
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), open[0].ID, 1, "delivery"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a cancelled request took an arrival: %v", err)
	}
}

// Who took it in, and when: the fact disputed against a delivery note.
func TestAnArrivalSaysWhoTookItInAndIsNotEdited(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := working(t, pool)
	workshop.RequestParts(ctx, pool, technician(), id, "Bromsskiva", 2)
	req := onlyRequest(t, pool)
	if err := workshop.ReceivePart(ctx, pool, partsDesk(), req.ID, 1, "delivery"); err != nil {
		t.Fatal(err)
	}
	var by string
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT received_by FROM part_request_receipts WHERE request_id = $1`, req.ID).Scan(&by); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE part_request_receipts SET quantity = 2 WHERE request_id = $1`, req.ID)
		return err
	})
	if err == nil {
		t.Error("an arrival was edited")
	}
	if by != partsDesk().UserID {
		t.Errorf("received by %q", by)
	}
}
