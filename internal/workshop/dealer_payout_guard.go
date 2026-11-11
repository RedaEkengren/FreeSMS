package workshop

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// What a purchase has been paid against what it says is owed (#248): the
// way a payment is held against its invoice (#132), for the money that
// leaves the shop for a car.

// ErrPayoutOverPrice is a payout that would take the money paid for a
// purchase above what the purchase says is owed.
var ErrPayoutOverPrice = fmt.Errorf("%w: that is more than is left to pay for the car; if more was really paid, tick that and say why", ErrInvalid)

// PurchaseBalance is what a purchase is owed and what has left for it.
type PurchaseBalance struct {
	// The price agreed on the purchase, less what a trade-in credits
	// against the sale it was handed over on: the rest of that price never
	// leaves as money (#165).
	OwedMinor int64
	// Paid out so far, every payout together, reversals netted.
	PaidMinor int64
}

// LeftMinor is what may still be paid; negative when more has been paid
// than was owed, which is shown rather than hidden.
func (b PurchaseBalance) LeftMinor() int64 { return b.OwedMinor - b.PaidMinor }

// purchaseBalanceTx reads a purchase's balance. Payouts for one purchase
// take an advisory lock on it first, so two at once are read one after the
// other and the second sees the first (a row lock needs UPDATE on a table
// that is never updated).
func purchaseBalanceTx(ctx context.Context, tx pgx.Tx, purchaseID string) (PurchaseBalance, error) {
	var b PurchaseBalance
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('purchase_payouts:' || $1::text, 0))`, purchaseID); err != nil {
		return b, fmt.Errorf("lock the purchase's payouts: %w", err)
	}
	err := tx.QueryRow(ctx, `
		SELECT p.tax_purchase_price_minor
		       - coalesce((SELECT t.credit_minor - t.loan_minor
		                   FROM vehicle_sale_trade_in_receipts r
		                   JOIN LATERAL (SELECT id FROM vehicle_sale_terms WHERE sale_id = r.sale_id ORDER BY version DESC LIMIT 1) v ON true
		                   JOIN vehicle_sale_trade_ins t ON t.terms_id = v.id AND t.key = r.key
		                   WHERE r.purchase_id = p.id), 0),
		       coalesce((SELECT -sum(x.amount_minor + x.rounding_minor)
		                 FROM invoice_payments x JOIN payouts o ON o.id = x.payout_id
		                 WHERE o.subject_kind = 'purchase' AND o.subject_id = p.id), 0)
		FROM vehicle_purchases p WHERE p.id = $1`, purchaseID).Scan(&b.OwedMinor, &b.PaidMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, fmt.Errorf("read what the purchase is owed: %w", err)
	}
	return b, nil
}
