package workshop

import (
	"context"
	"fmt"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MarkPerformed says that the work on a line was done, or takes that back.
//
// What a declined job is billed for is what was performed, so this is the
// record the decline is made from. Both the person who did the work and the
// front desk may say so; neither the parts desk nor anybody outside the shop.
//
// Marking a line the customer declined performed makes it billable again --
// the desk found out afterwards that the diagnosis had been done. Taking it
// back on a declined job declines the line, which is what it was.
func MarkPerformed(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, position int, performed bool) error {
	if scope.Role != access.RoleTechnician && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var state State
		if err := tx.QueryRow(ctx,
			`SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}
		switch state {
		case StateInvoiced, StateClosed, StateCancelled:
			return fmt.Errorf("%w: a %s order's lines are settled", ErrInvalid, state)
		}
		var tag int64
		if performed {
			ct, err := tx.Exec(ctx, `
				UPDATE work_order_lines
				   SET performed_at = coalesce(performed_at, now()), declined_at = NULL, updated_at = now()
				 WHERE work_order_id = $1 AND position = $2`, jobID, position)
			if err != nil {
				return fmt.Errorf("mark performed: %w", err)
			}
			tag = ct.RowsAffected()
		} else {
			ct, err := tx.Exec(ctx, `
				UPDATE work_order_lines
				   SET performed_at = NULL,
				       declined_at = CASE WHEN $3 THEN coalesce(declined_at, now()) END,
				       updated_at = now()
				 WHERE work_order_id = $1 AND position = $2`, jobID, position, state == StateDeclined)
			if err != nil {
				return fmt.Errorf("unmark performed: %w", err)
			}
			tag = ct.RowsAffected()
		}
		if tag == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Decline records that the customer said no, and which lines were performed
// before they did. Those stay and are billed; every other line is declined.
//
// One transaction, so a job is never declined with the performed work still
// to be marked: the invoice of a declined job is made of exactly this.
// Positions that are not lines on the job are refused rather than ignored,
// since a tick that quietly did nothing would leave work unbilled.
func Decline(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, performed []int) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var from State
		if err := tx.QueryRow(ctx,
			`SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&from); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}
		if !MayTransition(scope.Role, from, StateDeclined) {
			if CanTransition(from, StateDeclined) {
				return access.ErrForbidden
			}
			return ErrIllegalTransition{From: from, To: StateDeclined}
		}
		if len(performed) > 0 {
			ct, err := tx.Exec(ctx, `
				UPDATE work_order_lines SET performed_at = coalesce(performed_at, now()), updated_at = now()
				WHERE work_order_id = $1 AND position = ANY($2)`, jobID, performed)
			if err != nil {
				return fmt.Errorf("mark performed: %w", err)
			}
			if int(ct.RowsAffected()) != len(uniqueInts(performed)) {
				return fmt.Errorf("%w: some of those lines are not on this order", ErrInvalid)
			}
		}
		return setStateInScopeTx(ctx, tx, scope, jobID, StateDeclined)
	})
}

func uniqueInts(in []int) map[int]struct{} {
	m := make(map[int]struct{}, len(in))
	for _, v := range in {
		m[v] = struct{}{}
	}
	return m
}
