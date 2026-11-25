package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

var (
	errNoOverride      = fmt.Errorf("%w: that block has not been replanned, or it was put back already", ErrInvalid)
	errOverrideBegun   = fmt.Errorf("%w: what was replanned has begun, and what has been stays as it was", ErrInvalid)
	errOverrideHoldRan = fmt.Errorf("%w: it was a hold, and the hold has run out", ErrInvalid)
)

// ReverseOverrideBlock puts a block back the way it was before the replanning
// somebody regrets: a move gives it its old period again, and an end of a block
// that had not begun gives it back as it stood. It is a new event, with its own
// reason, beside the one it reverses -- the first stays in the trail -- and it
// is refused with the ledger's own sentence when something else has taken the
// time since. Replanning what has begun cannot be undone: what has been, has
// been. Whoever runs the shop, as the override is.
func ReverseOverrideBlock(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, fmt.Errorf("%w: say why it is put back, in 300 characters or fewer", ErrInvalid))
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The block first, locked, so two reversals are told apart.
		if _, _, _, err := blockKindTx(ctx, tx, id); err != nil {
			return err
		}
		var change, fromState string
		var oldFrom time.Time
		var oldTo *time.Time
		err := tx.QueryRow(ctx, `
			SELECT e.detail->>'change', coalesce(e.from_state, ''), (e.detail->>'old_from')::timestamptz, (e.detail->>'old_to')::timestamptz
			FROM audit_events e
			WHERE e.subject_kind = 'resource_block' AND e.subject_id = $1 AND e.event = 'block.overridden'
			  AND NOT EXISTS (SELECT 1 FROM audit_events r WHERE r.subject_kind = 'resource_block' AND r.subject_id = $1
			                  AND r.event = 'block.override_reversed' AND r.seq > e.seq)
			ORDER BY e.seq DESC LIMIT 1`, id).Scan(&change, &fromState, &oldFrom, &oldTo)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoOverride
		}
		if err != nil {
			return fmt.Errorf("read the replanning: %w", err)
		}
		detail := map[string]any{"change": change, "old_from": oldFrom, "old_to": oldTo}
		switch change {
		case "moved":
			if err := auditTx(ctx, tx, scope, id, "block.override_reversed", "moved", fromState, reason, detail); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE resource_blocks SET period = tstzrange($2, $3) WHERE id = $1`, id, oldFrom, oldTo)
			return blockConflict(err)
		case "ended":
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM resource_blocks WHERE id = $1`, id).Scan(&status); err != nil {
				return err
			}
			// A block that had begun was cut short and stays: it kept what was.
			if status != BlockOverridden {
				return errOverrideBegun
			}
			if fromState == BlockHeld {
				return errOverrideHoldRan
			}
			if err := auditTx(ctx, tx, scope, id, "block.override_reversed", "ended", fromState, reason, detail); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE resource_blocks SET status = $2, ended_at = NULL, ended_by = NULL WHERE id = $1`, id, fromState)
			return blockConflict(err)
		}
		return errNoOverride
	})
}
