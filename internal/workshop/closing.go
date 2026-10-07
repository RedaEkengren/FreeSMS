package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Closing a job used to be a button somebody had to remember, the last one in
// the state machine. A job that was collected and paid stayed open on the
// board. Now a job closes itself when it is finished in every sense: the
// invoice is issued, the car is collected, and nothing is owed. The owner is
// not a step in every job; the owner reads the result.

// closeIfFinishedTx closes a job that is finished in every sense, and leaves
// any other alone. Called from the moments that can finish one: money
// arriving and the car being collected, in the same transaction.
func closeIfFinishedTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
		return fmt.Errorf("read order: %w", err)
	}
	if state != StateInvoiced {
		return nil
	}
	p, err := presenceTx(ctx, tx, jobID)
	if err != nil || p.Here {
		return err
	}
	owed, ok, err := outstandingTx(ctx, tx, jobID)
	if err != nil || !ok || owed != 0 {
		return err
	}
	return setStateTx(ctx, tx, jobID, StateClosed)
}

// outstandingTx is what is still owed on a job's invoice, and whether it has
// one.
func outstandingTx(ctx context.Context, tx pgx.Tx, jobID string) (int64, bool, error) {
	var invoiceID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM invoices WHERE work_order_id = $1 AND credit_of_id IS NULL
		ORDER BY issued_at DESC LIMIT 1`, jobID).Scan(&invoiceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read invoice: %w", err)
	}
	b, err := balanceTx(ctx, tx, invoiceID, false)
	if err != nil {
		return 0, false, err
	}
	return b.OutstandingMinor(), true, nil
}

// CloseJob closes an invoiced job by hand. With money still owing it needs a
// reason, kept with the job: closing it is then a decision -- a debt written
// off -- and the receivable is still followed up where money owed is.
func CloseJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return fmt.Errorf("%w: keep the reason under 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read order: %w", err)
		}
		if !MayTransition(scope.Role, state, StateClosed) {
			return ErrIllegalTransition{From: state, To: StateClosed}
		}
		owed, _, err := outstandingTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if owed > 0 && reason == "" {
			return fmt.Errorf("%w: money is still owed on the invoice; say why it is closed anyway", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `UPDATE work_orders SET closed_reason = nullif($2, '') WHERE id = $1`,
			jobID, reason); err != nil {
			return fmt.Errorf("record reason: %w", err)
		}
		return setStateTx(ctx, tx, jobID, StateClosed)
	})
}
