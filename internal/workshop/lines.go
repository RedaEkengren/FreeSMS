package workshop

import (
	"context"
	"fmt"
	"strings"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewLine is a line being added to an order.
type NewLine struct {
	Kind           string
	Description    string
	Quantity       float64
	UnitPriceMinor int64
	VATRateBasis   int
	CostBearer     string
}

// AddLine puts a line on an order.
//
// Whether it counts as approved depends on where the order has got to. A line
// added before the customer said yes is part of what they said yes to. A line
// added afterwards is not, and is left unapproved so that it shows as such on
// the invoice -- the alternative is quietly including work nobody agreed to,
// which is the complaint that ends up on a review site.
func AddLine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, line NewLine) error {
	switch {
	case strings.TrimSpace(line.Description) == "":
		return fmt.Errorf("%w: the line needs a description", ErrInvalid)
	case line.Quantity < 0:
		return fmt.Errorf("%w: quantity cannot be negative", ErrInvalid)
	case line.UnitPriceMinor < 0:
		return fmt.Errorf("%w: a negative price is a credit note, not a line", ErrInvalid)
	}
	switch line.Kind {
	case "labour", "part", "sublet", "fee":
	default:
		return fmt.Errorf("%w: %q is not labour, part, sublet or fee", ErrInvalid, line.Kind)
	}
	if line.CostBearer == "" {
		line.CostBearer = "customer"
	}
	if line.CostBearer != "customer" && line.UnitPriceMinor != 0 {
		return fmt.Errorf("%w: a line the customer is not paying for must be free", ErrInvalid)
	}

	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var state State
		if err := tx.QueryRow(ctx,
			`SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("read state: %w", err)
		}
		switch state {
		case StateInvoiced, StateClosed, StateCancelled:
			return fmt.Errorf("%w: nothing can be added to a %s order", ErrInvalid, state)
		}

		// Before approval, the line is part of the quote. After it, it is not.
		preApproval := state == StateDraft || state == StateEstimated || state == StateAwaitingApproval

		var position int
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(max(position), 0) + 1 FROM work_order_lines WHERE work_order_id = $1`,
			jobID).Scan(&position); err != nil {
			return fmt.Errorf("next position: %w", err)
		}

		// The quoted price is only meaningful for a line that was quoted.
		var estimated *int64
		if preApproval {
			price := line.UnitPriceMinor
			estimated = &price
		}

		const insert = `
			INSERT INTO work_order_lines
			  (shop_id, work_order_id, position, kind, description, quantity,
			   unit_price_minor, estimated_unit_price_minor, vat_rate_bp, cost_bearer, approved_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			        CASE WHEN $11 THEN now() ELSE NULL END)`
		if _, err := tx.Exec(ctx, insert,
			scope.ShopID, jobID, position, line.Kind, strings.TrimSpace(line.Description),
			line.Quantity, line.UnitPriceMinor, estimated, line.VATRateBasis,
			line.CostBearer, preApproval); err != nil {
			return fmt.Errorf("add line: %w", err)
		}
		return nil
	})
}
