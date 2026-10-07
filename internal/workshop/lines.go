package workshop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewLine is a line being added to an order.
type NewLine struct {
	Kind           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
	CostBearer     string

	// Set when the line is for something on the shelf. Pricing it onto a job
	// reserves it; invoicing the job takes it off. Without this the part is
	// sold and never leaves stock.
	PartID string

	// Set when the price is the part's, worked out in the same transaction
	// that adds the line, rather than a number the browser sent. The shelf
	// used to send the part's price in a hidden field -- empty for a part
	// priced from its cost, and empty parsed as zero, so a costed part went
	// onto the job for nothing and the markup bands were never asked.
	PriceFromPart bool

	// Set when the line came from the shop's own time library, so that what
	// was actually clocked can be compared with what was expected. Without it
	// the library never learns and a wrong entry poisons every future
	// estimate quietly.
	LabourTimeID string
}

// AddLine puts a line on an order.
//
// Whether it counts as approved depends on where the order has got to. A line
// added before the customer said yes is part of what they said yes to. A line
// added afterwards is not, and is left unapproved so that it shows as such on
// the invoice -- the alternative is quietly including work nobody agreed to,
// which is the complaint that ends up on a review site.
func AddLine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, line NewLine) error {
	// Pricing is a conversation with the customer, so the front desk's. The
	// handler refused other roles; this did not, and the next caller would not.
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if err := checkLine(&line); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return addLineTx(ctx, tx, scope, jobID, line, lineSource{})
	})
}

func checkLine(line *NewLine) error {
	switch {
	case strings.TrimSpace(line.Description) == "":
		return fmt.Errorf("%w: the line needs a description", ErrInvalid)
	case line.QuantityMilli < 0:
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
	return nil
}

// lineSource is where a line came from when it was not typed in: an item the
// customer approved on their link. Empty for an ordinary line.
type lineSource struct {
	itemID     string
	decisionID string
	// When the customer said yes. Such a line is approved whatever state the
	// order is in: the customer agreed to exactly this, in writing.
	approvedAt *time.Time
}

// addLineTx adds a checked line inside a transaction.
func addLineTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, line NewLine, src lineSource) error {
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

	if line.PriceFromPart && line.CostBearer == "customer" {
		if line.PartID == "" {
			return fmt.Errorf("%w: a price from the part needs a part", ErrInvalid)
		}
		price, number, err := partPriceTx(ctx, tx, line.PartID)
		if err != nil {
			return err
		}
		if price == nil {
			// Nothing is not zero. Selling a part for nothing is something
			// somebody decides, by setting its price to 0.
			return fmt.Errorf("%w: %s has no price and no cost; give it one on the stock page", ErrInvalid, number)
		}
		line.UnitPriceMinor = *price
	}

	// Before approval, the line is part of the quote. After it, it is not --
	// unless the customer approved this very thing.
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
		   unit_price_minor, estimated_unit_price_minor, vat_rate_bp, cost_bearer,
		   approved_at, labour_time_id, part_id, inspection_item_id, customer_decision_id)
		VALUES ($1, $2, $3, $4, $5, ($6::bigint)::numeric / 1000, $7, $8, $9, $10,
		        CASE WHEN $11::timestamptz IS NOT NULL THEN $11::timestamptz
		             WHEN $12 THEN now() ELSE NULL END,
		        nullif($13, '')::uuid, nullif($14, '')::uuid,
		        nullif($15, '')::uuid, nullif($16, '')::uuid)`
	if _, err := tx.Exec(ctx, insert,
		scope.ShopID, jobID, position, line.Kind, strings.TrimSpace(line.Description),
		line.QuantityMilli, line.UnitPriceMinor, estimated, line.VATRateBasis,
		line.CostBearer, src.approvedAt, preApproval, line.LabourTimeID, line.PartID,
		src.itemID, src.decisionID); err != nil {
		if isUniqueViolation(err, "work_order_lines_one_per_item") {
			return ErrAlreadyALine
		}
		return fmt.Errorf("add line: %w", err)
	}

	// Pricing a part onto a job puts it aside. Reserving more than is on
	// the shelf is allowed and shows as a shortfall: the shop may well be
	// ordering more, and refusing here sends somebody to a spreadsheet.
	if line.PartID != "" && line.QuantityMilli > 0 {
		if err := moveTx(ctx, tx, scope,
			Movement{Kind: "reserved", Quantity: float64(line.QuantityMilli) / 1000},
			line.PartID, jobID, ""); err != nil {
			return err
		}
	}
	return nil
}
