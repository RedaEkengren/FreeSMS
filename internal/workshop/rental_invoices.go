package workshop

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// A rental invoiced (#199): through the same gap-free series and the same
// frozen document as the workshop's, from what was frozen on the booking
// (#191) and the return charges a person confirmed and told the customer
// of (#195). Nothing is repriced. Each line and charge is invoiced at most
// once while its invoice stands (rental_invoice_items); a credit note
// releases it. A deposit applied to it is a settlement, not a line
// (ApplyRentalDeposit). The invoice is to the renter, or to whoever pays
// for them, frozen on it.
//
// This release invoices once the rental is over: after the return, with
// its rent and its charges; after a cancellation or a no-show, its fee
// alone. Invoices per period of a running rental, and an advance invoice
// at booking, wait on gate G2 (#221): when VAT arises on a prepayment.

var (
	errRentalNotOver  = fmt.Errorf("%w: a rental is invoiced once it is over: returned, cancelled or not collected", ErrInvalid)
	errChargesNotTold = fmt.Errorf("%w: the customer has not been told of every charge; tell them, or withdraw it, first", ErrInvalid)
)

// rentalItem is a booking line or a charge not invoiced while an invoice
// of it stands, with the round its invoice will be.
type rentalItem struct {
	lineID, chargeID string
	round            int
	line             SourcedLine
}

// openRentalItemsTx is what of a booking is there to invoice: its frozen
// lines when withRent, and its standing charges, each not on an invoice
// that stands. An invoice stands until a credit note is made of it.
func openRentalItemsTx(ctx context.Context, tx pgx.Tx, bookingID string, withRent bool) ([]rentalItem, error) {
	const rounds = `
		(SELECT count(*) FROM rental_invoice_items x JOIN invoices i ON i.id = x.invoice_id
		  WHERE x.%[1]s = %[2]s AND EXISTS (SELECT 1 FROM invoices c WHERE c.credit_of_id = i.id))::int,
		EXISTS (SELECT 1 FROM rental_invoice_items x JOIN invoices i ON i.id = x.invoice_id
		  WHERE x.%[1]s = %[2]s AND NOT EXISTS (SELECT 1 FROM invoices c WHERE c.credit_of_id = i.id))`
	var out []rentalItem
	if withRent {
		rows, err := tx.Query(ctx, `
			SELECT l.id::text, l.description, l.unit, l.quantity_milli, l.unit_price_minor, l.vat_rate_bp, l.vat_category, `+
			fmt.Sprintf(rounds, "booking_line_id", "l.id")+`
			FROM rental_booking_lines l WHERE l.booking_id = $1 ORDER BY l.position`, bookingID)
		if err != nil {
			return nil, fmt.Errorf("read the booking's lines: %w", err)
		}
		for rows.Next() {
			var it rentalItem
			var standing bool
			l := &it.line
			if err := rows.Scan(&it.lineID, &l.Description, &l.Unit, &l.QuantityMilli, &l.UnitPriceMinor, &l.VATRateBasis, &l.VATCategory,
				&it.round, &standing); err != nil {
				rows.Close()
				return nil, err
			}
			if !standing {
				l.Kind = LineRent
				out = append(out, it)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	charges, err := rentalChargesTx(ctx, tx, bookingID)
	if err != nil {
		return nil, err
	}
	for _, c := range charges {
		if !c.Stands() {
			continue
		}
		if c.InformedAt == nil {
			return nil, errChargesNotTold
		}
		var round int
		var standing bool
		if err := tx.QueryRow(ctx, `SELECT `+fmt.Sprintf(rounds, "charge_id", "$1::uuid"), c.ID).Scan(&round, &standing); err != nil {
			return nil, fmt.Errorf("read the charge's invoices: %w", err)
		}
		if standing {
			continue
		}
		l := SourcedLine{Kind: LineRentalCharge, Description: c.Description, VATRateBasis: c.VATRateBp, VATCategory: c.VATCategory}
		if c.GrossGiven != nil {
			l.QuantityMilli, l.GrossMinor = 1000, *c.GrossGiven
		} else {
			l.QuantityMilli, l.UnitPriceMinor = c.QuantityMilli, *c.UnitPriceMinor
		}
		out = append(out, rentalItem{chargeID: c.ID, round: round, line: l})
	}
	return out, nil
}

// IssueRentalInvoice invoices what of the rental is not invoiced yet, to
// payerID -- the renter when empty, or a company paying for them. For the
// rental desk and the front desk. Refused while the rental is not over, or
// while a charge the customer has not been told of stands.
func IssueRentalInvoice(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, payerID string) (Invoice, error) {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) || (payerID != "" && !looksLikeUUID(payerID)) {
		return Invoice{}, ErrNotFound
	}
	var inv Invoice
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inv, err = issueRentalInvoiceTx(ctx, tx, scope, bookingID, payerID)
		return err
	})
	return inv, err
}

// IssueRentalInvoiceAndPay is IssueRentalInvoice paid at the counter as it
// is issued, a cash invoice (#156): the rent and the charges paid at the
// return, in one transaction. Either the invoice and its payments exist,
// or neither does.
func IssueRentalInvoiceAndPay(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, payerID string, sale CashSale) (Invoice, error) {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) || (payerID != "" && !looksLikeUUID(payerID)) {
		return Invoice{}, ErrNotFound
	}
	if err := checkCashSale(sale); err != nil {
		return Invoice{}, err
	}
	var inv Invoice
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if inv, err = issueRentalInvoiceTx(ctx, tx, scope, bookingID, payerID); err != nil {
			return err
		}
		return payOnIssueTx(ctx, tx, scope, inv, sale)
	})
	return inv, err
}

func issueRentalInvoiceTx(ctx context.Context, tx pgx.Tx, scope access.Scope, bookingID, payerID string) (Invoice, error) {
	// The charges' lock: nothing is confirmed, told or withdrawn while the
	// invoice is made of them.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_charges:' || $1::text, 0))`, bookingID); err != nil {
		return Invoice{}, fmt.Errorf("lock the charges: %w", err)
	}
	var state, renter string
	err := tx.QueryRow(ctx, `SELECT `+rentalState+`, b.renter_party_id::text FROM rental_bookings b JOIN resource_blocks k ON k.id = b.block_id
		WHERE b.id = $1`, bookingID).Scan(&state, &renter)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("read the booking: %w", err)
	}
	if state != RentalReturned && state != RentalCancelled && state != RentalNoShow {
		return Invoice{}, errRentalNotOver
	}
	items, err := openRentalItemsTx(ctx, tx, bookingID, state == RentalReturned)
	if err != nil {
		return Invoice{}, err
	}
	if len(items) == 0 {
		return Invoice{}, ErrNothingToInvoice
	}
	if payerID == "" {
		payerID = renter
	} else if err := partySeenTx(ctx, tx, scope, payerID); err != nil {
		return Invoice{}, err
	}
	customer, err := customerForPartyTx(ctx, tx, scope, payerID)
	if err != nil {
		return Invoice{}, err
	}
	lines := make([]SourcedLine, len(items))
	for i, it := range items {
		lines[i] = it.line
	}
	inv, err := issueSourcedTx(ctx, tx, scope, sourced{source: SourceRental, rentalID: bookingID, customerID: customer}, lines)
	if err != nil {
		return Invoice{}, err
	}
	for _, it := range items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO rental_invoice_items (shop_id, invoice_id, booking_line_id, charge_id, round)
			VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5)`,
			scope.ShopID, inv.ID, it.lineID, it.chargeID, it.round); err != nil {
			return Invoice{}, fmt.Errorf("record what the invoice charges: %w", err)
		}
	}
	return inv, nil
}

// RentalInvoice is an invoice of a rental, as its booking's page lists it.
type RentalInvoice struct {
	ID         string
	Reference  string
	GrossMinor int64
	Credited   bool
	CreditNote bool
}

// RentalInvoices reads a booking's invoices and credit notes, oldest
// first. The rental desk and the front desk; reading is Core's.
func RentalInvoices(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) ([]RentalInvoice, error) {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return nil, ErrNotFound
	}
	var out []RentalInvoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT i.id::text, i.series || '-' || i.number, i.gross_minor, i.credit_of_id IS NOT NULL,
			       EXISTS (SELECT 1 FROM invoices c WHERE c.credit_of_id = i.id)
			FROM invoices i WHERE i.source = 'rental' AND i.rental_id = $1 ORDER BY i.issued_at, i.number`, bookingID)
		if err != nil {
			return fmt.Errorf("read the rental's invoices: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r RentalInvoice
			if err := rows.Scan(&r.ID, &r.Reference, &r.GrossMinor, &r.CreditNote, &r.Credited); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// rentalInvoiceOfTx refuses an invoice that is not one of the booking's,
// standing.
func rentalInvoiceOfTx(ctx context.Context, tx pgx.Tx, bookingID, invoiceID string) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invoices WHERE id = $1 AND source = 'rental' AND rental_id = $2 AND credit_of_id IS NULL)`,
		invoiceID, bookingID).Scan(&ok); err != nil {
		return fmt.Errorf("read the invoice: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: a rental's deposit settles an invoice of the same rental", ErrInvalid)
	}
	return nil
}
