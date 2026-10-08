package database

import (
	"context"
	"fmt"
	"sync"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InScope runs fn inside a transaction that has the caller's shop established,
// as the restricted application role.
//
// Everything touching tenant data goes through here. That is not a convention
// -- it is the only place the session settings that row level security depends
// on are set, and those settings are transaction-local. A query run outside
// this wrapper gets a pooled connection with no scope, every policy denies,
// and the result is zero rows with no error. Making that impossible is the
// point of the wrapper.
func InScope(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fn func(context.Context, pgx.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return InShop(ctx, pool, scope.ShopID, fn)
}

// InShop is InScope without a user.
//
// It exists for the one thing that has to happen before there is a user to
// scope by: signing in. The shop is known -- resolved from the installation
// or, later, from the host -- but the role is not, because establishing it is
// what the caller is about to do.
//
// Everything else uses InScope. This is not a way to skip the role check; it
// is the narrow path that runs before a role exists, and the isolation it
// provides is the same.
func InShop(ctx context.Context, pool *pgxpool.Pool, shopID string, fn func(context.Context, pgx.Tx) error) error {
	if shopID == "" {
		return access.ErrNoScope
	}
	if sh, ok := ctx.Value(readingKey{}).(*reading); ok && sh.shopID == shopID {
		if done, err := sh.run(ctx, fn); done {
			return err
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Rolls back unless the commit below has already happened, in which case
	// it is a no-op.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Drop to the role that cannot change the schema. SET LOCAL lasts until
	// the transaction ends, so the connection returns to the pool unchanged.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE freesms_app`); err != nil {
		return fmt.Errorf("assume application role: %w", err)
	}

	// set_config rather than SET, because SET does not accept bind parameters
	// and building the statement by hand would put a value from a session
	// cookie into SQL text. The third argument makes it transaction-local.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_shop', $1, true)`, shopID); err != nil {
		return fmt.Errorf("set shop scope: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Reading runs fn with every InScope call made with its context sharing one
// transaction: begun, scoped and ended once, rather than once a read.
//
// A page that makes eighteen reads otherwise pays for eighteen transactions,
// five round trips each where one does the work -- and on a phone at the far
// end of the workshop's wifi, each round trip is the network's latency again.
//
// The reads are unchanged, so each keeps its own role check: sharing the
// transaction does not share permissions. It is read only, so a page that
// writes fails where it is written rather than committing by accident, and
// repeatable read, so every part of the page sees the same moment.
//
// A read that fails is still that read's failure alone. Postgres refuses
// everything after an error in a transaction, so once one has, the reads
// after it go back to transactions of their own: slower, never lost. If the
// shared transaction cannot be begun at all, fn runs without it.
func Reading(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fn func(context.Context)) {
	if scope.Validate() != nil {
		fn(ctx)
		return
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		fn(ctx)
		return
	}
	// Nothing to commit: it only read.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE freesms_app`); err != nil {
		fn(ctx)
		return
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_shop', $1, true)`, scope.ShopID); err != nil {
		fn(ctx)
		return
	}
	fn(context.WithValue(ctx, readingKey{}, &reading{shopID: scope.ShopID, tx: tx}))
}

type readingKey struct{}

// reading is the transaction Reading shares.
type reading struct {
	shopID string
	// A pgx transaction is one connection and not safe for two goroutines.
	mu     sync.Mutex
	tx     pgx.Tx
	broken bool
}

// run runs fn in the shared transaction, and reports false when it could
// not, so the caller begins its own.
func (r *reading) run(ctx context.Context, fn func(context.Context, pgx.Tx) error) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.broken {
		return false, nil
	}
	err := fn(ctx, r.tx)
	if err != nil {
		// Not found and refused leave the transaction usable; an error from
		// Postgres does not. Ask, rather than guess from the error.
		if _, perr := r.tx.Exec(ctx, `SELECT 1`); perr != nil {
			r.broken = true
		}
	}
	return true, err
}
