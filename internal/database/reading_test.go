package database

import (
	"context"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/jackc/pgx/v5"
)

// A page's reads share one transaction under Reading, and nothing else about
// them changes: the shop each sees, what a failure costs, what may be written.

func TestReadsUnderReadingShareOneTransaction(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	seedSecondShop(t, pool)
	ctx := context.Background()
	scope := access.Scope{ShopID: shopID, UserID: "33333333-3333-3333-3333-333333333333", Role: access.RoleOwner}
	other := access.Scope{ShopID: otherShopID, UserID: "33333333-3333-3333-3333-333333333333", Role: access.RoleOwner}

	// now() is when the transaction began: the same for every statement in
	// one, different for two.
	started := func(ctx context.Context, s access.Scope) (time.Time, int) {
		t.Helper()
		var at time.Time
		var orders int
		err := InScope(ctx, pool, s, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT now(), (SELECT count(*) FROM work_orders)`).Scan(&at, &orders)
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return at, orders
	}
	broke := func(ctx context.Context) error {
		return InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT 1/0`)
			return err
		})
	}

	Reading(ctx, pool, scope, func(ctx context.Context) {
		first, _ := started(ctx, scope)
		time.Sleep(5 * time.Millisecond)
		second, _ := started(ctx, scope)
		if !first.Equal(second) {
			t.Error("two reads under Reading ran in two transactions")
		}

		// Another shop's read in the same request is not given this shop's
		// transaction: it sees its own one work order.
		if _, orders := started(ctx, other); orders != 1 {
			t.Errorf("another shop's read saw %d work orders", orders)
		}

		// A write is refused rather than committed.
		err := InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE work_orders SET complaint = 'written' WHERE true`)
			return err
		})
		if err == nil {
			t.Error("a write under Reading was accepted")
		}

		// Postgres refuses everything after an error; the next read still
		// answers, in a transaction of its own.
		if err := broke(ctx); err == nil {
			t.Fatal("dividing by zero did not fail")
		}
		after, orders := started(ctx, scope)
		if orders != 1 || after.Equal(first) {
			t.Errorf("after a failed read: %d orders, own transaction %v", orders, !after.Equal(first))
		}
	})

	// Outside Reading, every read is its own transaction, as before.
	a, _ := started(ctx, scope)
	time.Sleep(5 * time.Millisecond)
	if b, _ := started(ctx, scope); a.Equal(b) {
		t.Error("reads outside Reading shared a transaction")
	}
}
