package workshop_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const otherShop = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"

func oldAttempt(t *testing.T, pool *pgxpool.Pool, shop string) {
	t.Helper()
	err := database.InShop(context.Background(), pool, shop, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO login_attempts (shop_id, email, succeeded, attempted_at)
			VALUES ($1, 'old@example.test', false, now() - interval '200 days')`, shop)
		return err
	})
	if err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
}

func attempts(t *testing.T, pool *pgxpool.Pool, shop string) int {
	t.Helper()
	var n int
	database.InShop(context.Background(), pool, shop, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM login_attempts`).Scan(&n)
	})
	return n
}

// A workshop set up after the process started has its retention applied
// without a restart.
//
// The worker used to be handed the shop once, at start-up. On an empty
// installation that was empty, it stayed empty after setup, and a new
// workshop's login attempts and expired links were kept until the process
// happened to restart.
func TestRetentionStartsWhenTheShopIsSetUp(t *testing.T) {
	pool := setup(t)
	oldAttempt(t, pool, shopID)

	// Another shop in the same database, which this process does not serve.
	if err := database.InShop(context.Background(), pool, otherShop, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO shops (id, name) VALUES ($1, 'Other')`, otherShop)
		return err
	}); err != nil {
		t.Fatalf("other shop: %v", err)
	}
	oldAttempt(t, pool, otherShop)

	var serving atomic.Value
	serving.Store("")
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		workshop.SweepPeriodically(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil)),
			func() string { return serving.Load().(string) }, wake, time.Hour)
		close(done)
	}()

	// Before setup: nothing is anybody's, nothing is swept.
	time.Sleep(200 * time.Millisecond)
	if attempts(t, pool, shopID) != 1 {
		t.Fatal("swept before there was a shop")
	}

	// Setup makes the shop and says so.
	serving.Store(shopID)
	wake <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for attempts(t, pool, shopID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the shop was set up and its retention never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if attempts(t, pool, otherShop) != 1 {
		t.Error("the sweep reached a shop this process does not serve")
	}

	// Shutting down stops it.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not stop when its context was cancelled")
	}
}

// A shop known at start-up is swept at once, not a day later.
func TestRetentionRunsAtStartForAConfiguredShop(t *testing.T) {
	pool := setup(t)
	oldAttempt(t, pool, shopID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go workshop.SweepPeriodically(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() string { return shopID }, nil, time.Hour)

	deadline := time.Now().Add(5 * time.Second)
	for attempts(t, pool, shopID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a configured shop was not swept at start")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
