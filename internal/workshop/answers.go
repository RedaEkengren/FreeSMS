package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// A customer's yes on their link used to stop at the inspection: stored, and
// seen only by somebody who opened it and read each item. These bring it to
// the job, where the front desk prices it, and to the board, where they see
// that there is something to price.

// ErrAlreadyALine is returned when an approved item has been made a line
// already -- by somebody else, a moment ago.
var ErrAlreadyALine = errors.New("workshop: that has already been made a line")

// CustomerAnswer is the customer's latest answer about one inspection item.
type CustomerAnswer struct {
	ItemID       string
	InspectionID string
	Label        string
	Note         string
	Decision     string // "approved" or "declined"
	DecisionID   string
	DecidedAt    time.Time
	// From the link, or said at the counter and recorded by somebody.
	FromLink bool
	// Set once the item has been made a line.
	LineID string
}

// Priceable is an approved answer nobody has made a line of yet.
func (a CustomerAnswer) Priceable() bool { return a.Decision == "approved" && a.LineID == "" }

// Description is what a line made from this item says: the item, and the
// technician's note when there is one.
func (a CustomerAnswer) Description() string {
	if note := strings.TrimSpace(a.Note); note != "" {
		return a.Label + ": " + note
	}
	return a.Label
}

// CustomerAnswers lists what the customer has answered on a job's
// inspections, the latest answer per item.
//
// The front desk's: the customer's decisions are part of the conversation
// about money, and a technician does not need them to do the work.
func CustomerAnswers(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]CustomerAnswer, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []CustomerAnswer
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT it.id, it.inspection_id, it.label, coalesce(it.note, ''),
			       d.decision, d.id, d.decided_at, d.share_id IS NOT NULL,
			       coalesce(l.id::text, '')
			FROM inspection_items it
			JOIN inspections i ON i.id = it.inspection_id
			-- The latest answer counts. Earlier ones stay on the inspection,
			-- which is the record of what was said when.
			JOIN LATERAL (
			    SELECT id, decision, decided_at, share_id FROM inspection_decisions
			    WHERE item_id = it.id ORDER BY decided_at DESC, id DESC LIMIT 1
			) d ON true
			LEFT JOIN work_order_lines l ON l.inspection_item_id = it.id
			WHERE i.work_order_id = $1
			ORDER BY i.started_at, it.position`, jobID)
		if err != nil {
			return fmt.Errorf("customer answers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a CustomerAnswer
			if err := rows.Scan(&a.ItemID, &a.InspectionID, &a.Label, &a.Note,
				&a.Decision, &a.DecisionID, &a.DecidedAt, &a.FromLink, &a.LineID); err != nil {
				return fmt.Errorf("scan customer answer: %w", err)
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// PriceAnswer makes an item the customer approved into a line on its job.
//
// The line is approved as of the customer's answer, whatever state the order
// is in, and carries which answer it was priced on. An item becomes one line:
// two people pricing it at once get one line and ErrAlreadyALine.
func PriceAnswer(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, itemID string, line NewLine) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if err := checkLine(&line); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The order first, as every line does: it serialises two people
		// pricing the same item, so the second sees the first one's line.
		var state State
		if err := tx.QueryRow(ctx,
			`SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}

		var decision, decisionID string
		var decidedAt time.Time
		var already bool
		err := tx.QueryRow(ctx, `
			SELECT d.decision, d.id, d.decided_at,
			       EXISTS (SELECT 1 FROM work_order_lines l WHERE l.inspection_item_id = it.id)
			FROM inspection_items it
			JOIN inspections i ON i.id = it.inspection_id
			JOIN LATERAL (
			    SELECT id, decision, decided_at FROM inspection_decisions
			    WHERE item_id = it.id ORDER BY decided_at DESC, id DESC LIMIT 1
			) d ON true
			WHERE it.id = $1 AND i.work_order_id = $2`, itemID, jobID).
			Scan(&decision, &decisionID, &decidedAt, &already)
		if err == pgx.ErrNoRows {
			// Not this job's, or nobody has answered it.
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the answer: %w", err)
		}
		if already {
			return ErrAlreadyALine
		}
		if decision != "approved" {
			return fmt.Errorf("%w: the customer has not approved this", ErrInvalid)
		}
		return addLineTx(ctx, tx, scope, jobID, line, lineSource{
			itemID: itemID, decisionID: decisionID, approvedAt: &decidedAt,
		})
	})
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
