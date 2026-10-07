package workshop_test

import (
	"context"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
)

// A part put back on the shelf from a job counts as on the shelf, before
// anything in this release writes one: a rollback from the release that
// does must land on code that reads it.
func TestAPartPutBackCountsAsOnTheShelf(t *testing.T) {
	pool := setup(t)
	stockShelf(t, pool, 3)
	job := newJob(t, pool)
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO stock_movements (shop_id, part_id, kind, quantity, work_order_id)
			VALUES ($1, $2, 'consumed', -2, $3), ($1, $2, 'put_back', 1, $3)`, shopID, stockedPart, job); err != nil {
			return err
		}
		// And the sign holds: a put-back takes nothing off the shelf.
		_, err := tx.Exec(ctx, `SAVEPOINT s`)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO stock_movements (shop_id, part_id, kind, quantity) VALUES ($1, $2, 'put_back', -1)`,
			shopID, stockedPart); err == nil {
			t.Error("a negative put-back was accepted")
		}
		_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := shelf(t, pool); p.OnHand != 2 {
		t.Errorf("on hand %v, want 2: three, two taken, one back", p.OnHand)
	}
}
