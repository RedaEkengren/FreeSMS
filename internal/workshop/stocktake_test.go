package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// Ten in the books, eight on the shelf: the count records -2 and leaves eight.
// It could not be recorded before: the form asked for the difference and
// refused a negative number, so two missing units could only be called a
// write-off or a return, which they were not.
func TestACountBelowTheBooksRecordsTheShortfall(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "OF-1", "Oil filter", "each", 4500)
	if err := workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, part, "", ""); err != nil {
		t.Fatalf("receive: %v", err)
	}

	diff, err := workshop.Stocktake(ctx, pool, partsDeskScope(), part, 8000, "Shelf count, October")
	if err != nil {
		t.Fatalf("Stocktake: %v", err)
	}
	if diff != -2000 {
		t.Errorf("difference = %d thousandths, want -2000", diff)
	}
	if got := onHand(t, pool, part).OnHand; got != 8 {
		t.Errorf("on hand = %v, want the 8 counted", got)
	}
	moves, err := workshop.MovementsFor(ctx, pool, partsDeskScope(), part)
	if err != nil {
		t.Fatalf("MovementsFor: %v", err)
	}
	if moves[0].Kind != "counted" || moves[0].Quantity != -2 {
		t.Errorf("latest movement = %s %v, want counted -2", moves[0].Kind, moves[0].Quantity)
	}
}

// Up as well as down, and in parts of a unit: oil is sold by the litre.
func TestACountCanGoUpAndInFractions(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "OIL-5W30", "Engine oil 5W-30", "litre", 9000)
	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 2.5}, part, "", "")

	diff, err := workshop.Stocktake(ctx, pool, partsDeskScope(), part, 3750, "")
	if err != nil {
		t.Fatalf("Stocktake: %v", err)
	}
	if diff != 1250 {
		t.Errorf("difference = %d, want +1250", diff)
	}
	if got := onHand(t, pool, part).OnHand; got != 3.75 {
		t.Errorf("on hand = %v, want 3.75", got)
	}

	// A count that matches records nothing.
	before, _ := workshop.MovementsFor(ctx, pool, partsDeskScope(), part)
	if diff, err := workshop.Stocktake(ctx, pool, partsDeskScope(), part, 3750, ""); err != nil || diff != 0 {
		t.Errorf("a matching count: %d, %v", diff, err)
	}
	if after, _ := workshop.MovementsFor(ctx, pool, partsDeskScope(), part); len(after) != len(before) {
		t.Error("a count that matched the books recorded a movement of nothing")
	}
}

// A movement that was being booked when the count arrived is counted before
// the difference is worked out, not after: the count waits for the part.
func TestACountWaitsForAMovementInFlight(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	part := addPart(t, pool, "BP-1", "Brake pads", "each", 30000)
	workshop.Move(ctx, pool, partsDeskScope(), workshop.Movement{Kind: "received", Quantity: 10}, part, "", "")

	// Somebody books two sets out and has not committed yet. Taken the way
	// every movement takes it: the part's row, for update.
	release := make(chan struct{})
	held := make(chan error, 1)
	go database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM parts WHERE id = $1 FOR UPDATE`, part); err != nil {
			held <- err
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO stock_movements (shop_id, part_id, kind, quantity, moved_by)
			VALUES ($1, $2, 'consumed', -2, $3)`, shopID, part, userID); err != nil {
			held <- err
			return err
		}
		held <- nil
		<-release
		return nil
	})
	if err := <-held; err != nil {
		t.Fatalf("book two out: %v", err)
	}

	// The shelf has seven: the two booked out are gone, and one more is
	// missing that nobody booked.
	done := make(chan int64, 1)
	go func() {
		diff, err := workshop.Stocktake(ctx, pool, partsDeskScope(), part, 7000, "")
		if err != nil {
			t.Errorf("Stocktake: %v", err)
		}
		done <- diff
	}()
	select {
	case <-done:
		t.Fatal("the count did not wait for the movement in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)

	if diff := <-done; diff != -1000 {
		t.Errorf("difference = %d, want -1000 -- worked out after the two booked out, not before", diff)
	}
	if got := onHand(t, pool, part).OnHand; got != 7 {
		t.Errorf("on hand = %v, want 7", got)
	}
}

// A count says what is there. A negative one is not a count, and a
// technician does not keep the stock.
func TestACountMustBeACount(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	part := addPart(t, pool, "X-1", "Something", "each", 100)

	if _, err := workshop.Stocktake(ctx, pool, partsDeskScope(), part, -1000, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a negative count: %v, want invalid", err)
	}
	if _, err := workshop.Stocktake(ctx, pool, technician(), part, 5000, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician counting: %v, want forbidden", err)
	}
	if _, err := workshop.Stocktake(ctx, pool, partsDeskScope(), "not-a-part", 5000, ""); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("an unknown part: %v, want not found", err)
	}
}
