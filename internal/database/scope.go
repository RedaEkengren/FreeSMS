package database

import (
	"context"
	"fmt"

	"github.com/RedaEkengren/RedaSMS/internal/access"
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

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Rolls back unless the commit below has already happened, in which case
	// it is a no-op.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Drop to the role that cannot change the schema. SET LOCAL lasts until
	// the transaction ends, so the connection returns to the pool unchanged.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE redasms_app`); err != nil {
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
