package workshop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Correcting an issued invoice without altering it.
//
// An invoice is frozen, so a correction is another document: a credit note that
// takes back some of it, and when the buyer was wrong, a credit of the whole
// and a new invoice to the right one. Each credit is bounded by what is left of
// the original, line by line and in total, under a lock, so two people
// correcting the same invoice cannot between them credit more than was billed.
//
// A price correction is money. It does not put anything back on a shelf: a part
// that physically comes back is its own movement, confirmed on its own.

// ErrNothingToCredit is returned when there is nothing left of an invoice to
// credit.
var ErrNothingToCredit = ErrAlreadyCredited

// CreditLine is a quantity of one line of the original to take back.
type CreditLine struct {
	// The position on the original invoice.
	Position int
	// Thousandths to take back; zero means everything that is left of it.
	QuantityMilli int64
}

// Adjustment is a sum to credit that belongs to no line: goodwill, a discount
// agreed afterwards.
type Adjustment struct {
	Description string
	// What to credit, excluding VAT, in minor units (positive).
	NetMinor     int64
	VATRateBasis int
}

// CreditRequest says what to credit and why.
type CreditRequest struct {
	Lines       []CreditLine
	Adjustments []Adjustment
	Reason      string
	// The form's own key. Asking again with the same one returns the credit
	// note it made.
	Key string
}

// Eligible is a line of an invoice and how much of it can still be credited.
type Eligible struct {
	Position       int
	Description    string
	QuantityMilli  int64
	CreditedMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
	NetMinor       int64
}

// RemainingMilli is what can still be taken back.
func (e Eligible) RemainingMilli() int64 { return e.QuantityMilli - e.CreditedMilli }

// Remaining renders it the way a line renders its quantity.
func (e Eligible) Remaining() string { return Line{QuantityMilli: e.RemainingMilli()}.Quantity() }

// Ordered renders what the line was for.
func (e Eligible) Ordered() string { return Line{QuantityMilli: e.QuantityMilli}.Quantity() }

// CreditPreview is what a credit would come to, before it is issued.
type CreditPreview struct {
	Lines []DocumentLine
	Bands []money.VATBand
	// Negative, as the credit note will be.
	NetMinor, VATMinor, GrossMinor int64
	// What could be credited before and after, including VAT.
	LeftBefore, LeftAfter int64
	// The combined position on the invoice: positive is owed by the customer,
	// negative is owed back, before and after.
	OwedBefore, OwedAfter int64
	// The whole of what is left.
	Everything bool
}

type creditPlan struct {
	original   Invoice
	address    *string
	orgNumber  *string
	vatNumber  *string
	buyer      Buyer
	lines      []invoiceLine
	left       int64 // gross still creditable
	grossBack  int64 // gross this request credits, positive
	owed       int64
	everything bool
}

func newKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// planCreditTx works out the credit note a request would make, and refuses one
// that would take back more than is left. With lock the invoice is held until
// the transaction ends.
func planCreditTx(ctx context.Context, tx pgx.Tx, invoiceID string, req CreditRequest, lock bool) (creditPlan, error) {
	var p creditPlan
	q := `SELECT id, series, number, work_order_id, credit_of_id IS NOT NULL, customer_name,
	             customer_address, customer_org_number, customer_vat_number,
	             net_minor, vat_minor, gross_minor,
	             customer_postal_code, customer_city, customer_country,
	             customer_endpoint_id, customer_reference
	      FROM invoices WHERE id = $1`
	if lock {
		// The lock first, then the question: two corrections of one invoice
		// are done one after the other, each reading what the last left.
		q += ` FOR UPDATE`
	}
	var isCredit bool
	err := tx.QueryRow(ctx, q, invoiceID).Scan(&p.original.ID, &p.original.Series, &p.original.Number, &p.original.WorkOrderID,
		&isCredit, &p.original.CustomerName, &p.address, &p.orgNumber, &p.vatNumber,
		&p.original.NetMinor, &p.original.VATMinor, &p.original.GrossMinor,
		&p.buyer.PostalCode, &p.buyer.City, &p.buyer.Country, &p.buyer.EndpointID, &p.buyer.Reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("read invoice: %w", err)
	}
	if isCredit {
		return p, fmt.Errorf("%w: a credit note cannot be credited", ErrInvalid)
	}

	var creditedGross int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(gross_minor), 0) FROM invoices WHERE credit_of_id = $1`, invoiceID).Scan(&creditedGross); err != nil {
		return p, fmt.Errorf("read credits: %w", err)
	}
	p.left = p.original.GrossMinor + creditedGross
	if p.left <= 0 {
		return p, ErrAlreadyCredited
	}
	var paid int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount_minor + rounding_minor), 0) FROM invoice_payments WHERE invoice_id = $1`, invoiceID).Scan(&paid); err != nil {
		return p, fmt.Errorf("read payments: %w", err)
	}
	p.owed = p.left - paid // owed now, with what has been credited already

	type orig struct {
		id                       string
		position                 int
		kind, description        string
		qty, unit                int64
		rate                     int
		net, vat                 int64
		creditedQty, creditedNet int64
		creditedVAT              int64
		unitCode                 string
	}
	rows, err := tx.Query(ctx, `
		SELECT l.id::text, l.position, l.kind, l.description, (l.quantity * 1000)::bigint, l.unit_price_minor, l.vat_rate_bp, l.net_minor, l.vat_minor,
		       coalesce((SELECT -sum((c.quantity * 1000)::bigint) FROM invoice_lines c WHERE c.credits_line_id = l.id), 0),
		       coalesce((SELECT -sum(c.net_minor) FROM invoice_lines c WHERE c.credits_line_id = l.id), 0),
		       coalesce((SELECT -sum(c.vat_minor) FROM invoice_lines c WHERE c.credits_line_id = l.id), 0),
		       coalesce(l.unit_code, '')
		FROM invoice_lines l WHERE l.invoice_id = $1 ORDER BY l.position`, invoiceID)
	if err != nil {
		return p, fmt.Errorf("read invoice lines: %w", err)
	}
	var origs []orig
	byPos := map[int]*orig{}
	for rows.Next() {
		var o orig
		if err := rows.Scan(&o.id, &o.position, &o.kind, &o.description, &o.qty, &o.unit, &o.rate, &o.net, &o.vat, &o.creditedQty, &o.creditedNet, &o.creditedVAT, &o.unitCode); err != nil {
			rows.Close()
			return p, fmt.Errorf("scan invoice line: %w", err)
		}
		origs = append(origs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}
	for i := range origs {
		byPos[origs[i].position] = &origs[i]
	}

	// Nothing named is everything that is left.
	lines := req.Lines
	if len(lines) == 0 && len(req.Adjustments) == 0 {
		for _, o := range origs {
			if o.net-o.creditedNet != 0 || o.vat-o.creditedVAT != 0 {
				lines = append(lines, CreditLine{Position: o.position})
			}
		}
	}
	seen := map[int]bool{}
	for _, cl := range lines {
		o, ok := byPos[cl.Position]
		if !ok {
			return p, fmt.Errorf("%w: line %d is not on that invoice", ErrInvalid, cl.Position)
		}
		if seen[cl.Position] {
			return p, fmt.Errorf("%w: line %d is named twice", ErrInvalid, cl.Position)
		}
		seen[cl.Position] = true
		remaining := o.qty - o.creditedQty
		qty := cl.QuantityMilli
		if qty == 0 {
			qty = remaining
		}
		if qty < 0 {
			return p, fmt.Errorf("%w: take back a quantity above nothing", ErrInvalid)
		}
		if qty == 0 || remaining <= 0 {
			return p, fmt.Errorf("%w: nothing is left of %q to credit", ErrInvalid, o.description)
		}
		if qty > remaining {
			return p, fmt.Errorf("%w: only %s of %q is left to credit", ErrInvalid, Line{QuantityMilli: remaining}.Quantity(), o.description)
		}
		var net, vat int64
		if qty == remaining {
			// What is left, exactly: the last part of a line takes the öre the
			// earlier parts rounded away, so the credits add up to the line.
			net, vat = o.net-o.creditedNet, o.vat-o.creditedVAT
		} else {
			net = money.LineNet(qty, o.unit)
			vat = money.VAT(net, o.rate)
		}
		p.lines = append(p.lines, invoiceLine{
			Kind: o.kind, Description: o.description, QuantityMilli: -qty, UnitPriceMinor: o.unit, VATRateBasis: o.rate,
			NetMinor: -net, VATMinor: -vat, CreditsLineID: o.id, UnitCode: o.unitCode,
		})
	}
	for _, a := range req.Adjustments {
		desc := strings.TrimSpace(a.Description)
		if desc == "" || len([]rune(desc)) > 200 {
			return p, fmt.Errorf("%w: say what the adjustment is for", ErrInvalid)
		}
		if a.NetMinor <= 0 {
			return p, fmt.Errorf("%w: an adjustment is an amount above nothing", ErrInvalid)
		}
		if a.VATRateBasis < 0 || a.VATRateBasis > 10000 {
			return p, fmt.Errorf("%w: that is not a VAT rate", ErrInvalid)
		}
		vat := money.VAT(a.NetMinor, a.VATRateBasis)
		p.lines = append(p.lines, invoiceLine{
			Kind: "fee", Description: desc, QuantityMilli: -1000, UnitPriceMinor: a.NetMinor, VATRateBasis: a.VATRateBasis,
			NetMinor: -a.NetMinor, VATMinor: -vat, UnitCode: "C62",
		})
	}
	if len(p.lines) == 0 {
		return p, ErrAlreadyCredited
	}
	for _, l := range p.lines {
		p.grossBack -= l.NetMinor + l.VATMinor
	}
	if p.grossBack > p.left {
		return p, fmt.Errorf("%w: that credits more than is left of the invoice", ErrInvalid)
	}
	if p.grossBack <= 0 {
		return p, fmt.Errorf("%w: a credit takes something back", ErrInvalid)
	}
	p.everything = p.grossBack == p.left
	return p, nil
}

func (p creditPlan) preview() CreditPreview {
	var out CreditPreview
	for i, l := range p.lines {
		out.Lines = append(out.Lines, DocumentLine{
			Position: i + 1, Kind: l.Kind, Description: l.Description, QuantityMilli: l.QuantityMilli,
			UnitPriceMinor: l.UnitPriceMinor, VATRateBasis: l.VATRateBasis, NetMinor: l.NetMinor, VATMinor: l.VATMinor,
		})
		out.NetMinor += l.NetMinor
		out.VATMinor += l.VATMinor
	}
	out.GrossMinor = out.NetMinor + out.VATMinor
	out.Bands = bandsFrom(out.Lines)
	out.LeftBefore, out.LeftAfter = p.left, p.left-p.grossBack
	out.OwedBefore, out.OwedAfter = p.owed, p.owed-p.grossBack
	out.Everything = p.everything
	return out
}

// PreviewCredit shows what a credit would come to and leave, and writes
// nothing.
func PreviewCredit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, req CreditRequest) (CreditPreview, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return CreditPreview{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return CreditPreview{}, ErrNotFound
	}
	var out CreditPreview
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		p, err := planCreditTx(ctx, tx, invoiceID, req, false)
		if err != nil {
			return err
		}
		out = p.preview()
		return nil
	})
	return out, err
}

// EligibleLines lists an invoice's lines and how much of each can still be
// credited.
func EligibleLines(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) ([]Eligible, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return nil, ErrNotFound
	}
	var out []Eligible
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM invoices WHERE id = $1 AND credit_of_id IS NULL)`, invoiceID).Scan(&exists); err != nil {
			return fmt.Errorf("read invoice: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, `
			SELECT l.position, l.description, (l.quantity * 1000)::bigint,
			       coalesce((SELECT -sum((c.quantity * 1000)::bigint) FROM invoice_lines c WHERE c.credits_line_id = l.id), 0),
			       l.unit_price_minor, l.vat_rate_bp, l.net_minor
			FROM invoice_lines l WHERE l.invoice_id = $1 ORDER BY l.position`, invoiceID)
		if err != nil {
			return fmt.Errorf("read lines: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e Eligible
			if err := rows.Scan(&e.Position, &e.Description, &e.QuantityMilli, &e.CreditedMilli, &e.UnitPriceMinor, &e.VATRateBasis, &e.NetMinor); err != nil {
				return fmt.Errorf("scan line: %w", err)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// IssueCredit issues a credit note for what the request names, or for
// everything that is left when it names nothing. The reason is kept with who
// asked and when; the original is not touched.
func IssueCredit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, req CreditRequest) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return Invoice{}, ErrNotFound
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len([]rune(req.Reason)) > 300 {
		return Invoice{}, fmt.Errorf("%w: say why, in 300 characters or fewer", ErrInvalid)
	}
	if req.Key == "" {
		req.Key = newKey()
	}
	if len(req.Key) > 100 {
		return Invoice{}, fmt.Errorf("%w: that key is too long", ErrInvalid)
	}
	var note Invoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// A form sent twice finds the credit note it already made.
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock the invoice: %w", err)
		}
		var existing *string
		if err := tx.QueryRow(ctx, `SELECT credit_invoice_id::text FROM invoice_corrections WHERE original_invoice_id = $1 AND request_key = $2`,
			invoiceID, req.Key).Scan(&existing); err == nil && existing != nil {
			return tx.QueryRow(ctx, `SELECT id, series, number, work_order_id, customer_name, issued_at, net_minor, vat_minor, gross_minor, credit_of_id
				FROM invoices WHERE id = $1`, *existing).Scan(&note.ID, &note.Series, &note.Number, &note.WorkOrderID, &note.CustomerName,
				&note.IssuedAt, &note.NetMinor, &note.VATMinor, &note.GrossMinor, &note.CreditOfID)
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check for the same request: %w", err)
		}

		p, err := planCreditTx(ctx, tx, invoiceID, req, true)
		if err != nil {
			return err
		}
		number, err := nextNumber(ctx, tx, scope.ShopID, p.original.Series)
		if err != nil {
			return err
		}
		// The seller as it is when the note is issued: it is its own document,
		// issued today by whoever the shop is today.
		sold, err := readSeller(ctx, tx, scope.ShopID)
		if err != nil {
			return err
		}
		var net, vat int64
		for _, l := range p.lines {
			net += l.NetMinor
			vat += l.VATMinor
		}
		const insert = `
			INSERT INTO invoices
			  (shop_id, series, number, work_order_id, credit_of_id, issued_by,
			   customer_name, customer_address, customer_org_number, customer_vat_number,
			   seller_name, seller_address, seller_org_number, seller_vat_number,
			   seller_phone, seller_email, seller_payment_reference,
			   seller_payment_terms_days, seller_f_tax,
			   net_minor, vat_minor, gross_minor,
			   ` + electronicColumns + `)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
			        $23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33)
			RETURNING id, issued_at`
		args := append([]any{
			scope.ShopID, p.original.Series, number, p.original.WorkOrderID, p.original.ID, scope.UserID,
			p.original.CustomerName, p.address, p.orgNumber, p.vatNumber,
			sold.Name, sold.Address, sold.OrgNumber, sold.VATNumber,
			sold.Phone, sold.Email, sold.PaymentReference,
			sold.PaymentTermsDays, sold.FTax,
			net, vat, net + vat,
		}, electronicArgs(sold, p.buyer)...)
		if err := tx.QueryRow(ctx, insert, args...).Scan(&note.ID, &note.IssuedAt); err != nil {
			return fmt.Errorf("write credit note: %w", err)
		}
		for i := range p.lines {
			p.lines[i].Position = i + 1
		}
		if err := writeInvoiceLines(ctx, tx, scope.ShopID, note.ID, p.lines); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_corrections (shop_id, original_invoice_id, kind, reason, request_key, credit_invoice_id, by_user)
			VALUES ($1, $2, 'credit', $3, $4, $5, $6)`, scope.ShopID, p.original.ID, req.Reason, req.Key, note.ID, scope.UserID); err != nil {
			return fmt.Errorf("record the correction: %w", err)
		}
		note.Series, note.Number = p.original.Series, number
		note.WorkOrderID = p.original.WorkOrderID
		note.CreditOfID = &p.original.ID
		note.CustomerName = p.original.CustomerName
		note.NetMinor, note.VATMinor, note.GrossMinor = net, vat, net+vat
		return nil
	})
	if err != nil {
		return Invoice{}, err
	}
	return note, nil
}

// RebillRequest is a correction of the buyer.
type RebillRequest struct {
	// The customer the invoice should have gone to.
	CustomerID string
	Reason     string
	Key        string
}

// RebillTo replaces an invoice that went to the wrong buyer. The original must
// have been credited in full already; the replacement is a new invoice for the
// same lines to the right customer, on a new work order that says it corrects
// the first, so that a work order still has one invoice and the protection
// against billing a job twice stays where it is.
//
// Nothing is consumed again and no work is counted twice: the replacement
// carries the invoice's lines and not the job's.
func RebillTo(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, req RebillRequest) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) || !looksLikeUUID(req.CustomerID) {
		return Invoice{}, ErrNotFound
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len([]rune(req.Reason)) > 300 {
		return Invoice{}, fmt.Errorf("%w: say why, in 300 characters or fewer", ErrInvalid)
	}
	if req.Key == "" {
		req.Key = newKey()
	}
	var out Invoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var orig Invoice
		var isCredit bool
		var vehicleID string
		if err := tx.QueryRow(ctx, `
			SELECT i.id, i.series, i.number, i.work_order_id, i.credit_of_id IS NOT NULL, i.net_minor, i.vat_minor, i.gross_minor, w.vehicle_id::text
			FROM invoices i JOIN work_orders w ON w.id = i.work_order_id WHERE i.id = $1 FOR UPDATE OF i`, invoiceID).Scan(
			&orig.ID, &orig.Series, &orig.Number, &orig.WorkOrderID, &isCredit, &orig.NetMinor, &orig.VATMinor, &orig.GrossMinor, &vehicleID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock the invoice: %w", err)
		}
		if isCredit {
			return fmt.Errorf("%w: a credit note cannot be rebilled", ErrInvalid)
		}
		var existing *string
		if err := tx.QueryRow(ctx, `SELECT replacement_invoice_id::text FROM invoice_corrections WHERE original_invoice_id = $1 AND kind = 'rebill'`, invoiceID).Scan(&existing); err == nil && existing != nil {
			if err := tx.QueryRow(ctx, `SELECT id, series, number, work_order_id, customer_name, issued_at, net_minor, vat_minor, gross_minor
				FROM invoices WHERE id = $1`, *existing).Scan(&out.ID, &out.Series, &out.Number, &out.WorkOrderID, &out.CustomerName, &out.IssuedAt, &out.NetMinor, &out.VATMinor, &out.GrossMinor); err != nil {
				return fmt.Errorf("read replacement: %w", err)
			}
			var sameKey bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM invoice_corrections WHERE original_invoice_id = $1 AND request_key = $2)`, invoiceID, req.Key).Scan(&sameKey); err != nil {
				return err
			}
			if sameKey {
				return nil
			}
			return fmt.Errorf("%w: this invoice has already been replaced", ErrInvalid)
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check replacement: %w", err)
		}
		var credited int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(gross_minor), 0) FROM invoices WHERE credit_of_id = $1`, invoiceID).Scan(&credited); err != nil {
			return fmt.Errorf("read credits: %w", err)
		}
		if orig.GrossMinor+credited != 0 {
			return fmt.Errorf("%w: credit the whole invoice first; a replacement is for an invoice that has been taken back", ErrInvalid)
		}

		var name, address, org, vat *string
		var buyer Buyer
		var customerID string
		if err := tx.QueryRow(ctx, `
			SELECT c.id::text, coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name),
			       nullif(concat_ws(', ', c.address_line1, c.address_line2, concat_ws(' ', c.postal_code, c.city)), ''),
			       c.org_number, c.vat_number,
			       c.postal_code, c.city, c.country, c.endpoint_id, c.buyer_reference
			FROM customers c LEFT JOIN people p ON p.id = c.person_id
			WHERE c.id = $1 AND c.merged_into IS NULL`, req.CustomerID).Scan(&customerID, &name, &address, &org, &vat,
			&buyer.PostalCode, &buyer.City, &buyer.Country, &buyer.EndpointID, &buyer.Reference); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read buyer: %w", err)
		}
		if name == nil || *name == "" {
			return ErrNoCustomer
		}

		var number int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(number), 0) + 1 FROM work_orders`).Scan(&number); err != nil {
			return fmt.Errorf("next order number: %w", err)
		}
		var jobID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO work_orders (shop_id, number, vehicle_id, customer_id, complaint, state, created_by, corrects_work_order_id)
			VALUES ($1, $2, $3, $4, $5, 'invoiced', $6, $7) RETURNING id`,
			scope.ShopID, number, vehicleID, customerID, fmt.Sprintf("Correction of invoice %s-%d", orig.Series, orig.Number), scope.UserID, orig.WorkOrderID).Scan(&jobID); err != nil {
			return fmt.Errorf("open the correction: %w", err)
		}

		rows, err := tx.Query(ctx, `
			SELECT position, kind, description, (quantity * 1000)::bigint, unit_price_minor, vat_rate_bp, net_minor, vat_minor,
			       coalesce(unit_code, '')
			FROM invoice_lines WHERE invoice_id = $1 AND credits_line_id IS NULL ORDER BY position`, invoiceID)
		if err != nil {
			return fmt.Errorf("read lines: %w", err)
		}
		var lines []invoiceLine
		for rows.Next() {
			var l invoiceLine
			if err := rows.Scan(&l.Position, &l.Kind, &l.Description, &l.QuantityMilli, &l.UnitPriceMinor, &l.VATRateBasis, &l.NetMinor, &l.VATMinor, &l.UnitCode); err != nil {
				rows.Close()
				return fmt.Errorf("scan line: %w", err)
			}
			lines = append(lines, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		newNumber, err := nextNumber(ctx, tx, scope.ShopID, orig.Series)
		if err != nil {
			return err
		}
		sold, err := readSeller(ctx, tx, scope.ShopID)
		if err != nil {
			return err
		}
		const insert = `
			INSERT INTO invoices
			  (shop_id, series, number, work_order_id, issued_by,
			   customer_name, customer_address, customer_org_number, customer_vat_number,
			   seller_name, seller_address, seller_org_number, seller_vat_number,
			   seller_phone, seller_email, seller_payment_reference,
			   seller_payment_terms_days, seller_f_tax,
			   net_minor, vat_minor, gross_minor,
			   ` + electronicColumns + `)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,
			        $22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)
			RETURNING id, issued_at`
		args := append([]any{
			scope.ShopID, orig.Series, newNumber, jobID, scope.UserID,
			*name, address, org, vat,
			sold.Name, sold.Address, sold.OrgNumber, sold.VATNumber,
			sold.Phone, sold.Email, sold.PaymentReference,
			sold.PaymentTermsDays, sold.FTax,
			orig.NetMinor, orig.VATMinor, orig.GrossMinor,
		}, electronicArgs(sold, buyer)...)
		if err := tx.QueryRow(ctx, insert, args...).Scan(&out.ID, &out.IssuedAt); err != nil {
			return fmt.Errorf("write replacement: %w", err)
		}
		if err := writeInvoiceLines(ctx, tx, scope.ShopID, out.ID, lines); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_corrections (shop_id, original_invoice_id, kind, reason, request_key, replacement_invoice_id, by_user)
			VALUES ($1, $2, 'rebill', $3, $4, $5, $6)`, scope.ShopID, invoiceID, req.Reason, req.Key, out.ID, scope.UserID); err != nil {
			return fmt.Errorf("record the correction: %w", err)
		}
		out.Series, out.Number, out.WorkOrderID, out.CustomerName = orig.Series, newNumber, jobID, *name
		out.NetMinor, out.VATMinor, out.GrossMinor = orig.NetMinor, orig.VATMinor, orig.GrossMinor
		return nil
	})
	if err != nil {
		return Invoice{}, err
	}
	return out, nil
}

// Correction is a step in correcting a document.
type Correction struct {
	Kind      string // credit or rebill
	Reference string // the credit note or the replacement
	InvoiceID string
	Reason    string
	By        string
	At        time.Time
}

// CorrectionsFor lists what has been done to correct an invoice, and, for a
// replacement, which invoice it replaced.
func CorrectionsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) ([]Correction, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return nil, ErrNotFound
	}
	var out []Correction
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.kind, x.series || '-' || x.number, x.id::text, c.reason, coalesce(p.display_name, ''), c.at
			FROM invoice_corrections c
			JOIN invoices x ON x.id = coalesce(c.credit_invoice_id, c.replacement_invoice_id)
			LEFT JOIN users u ON u.id = c.by_user LEFT JOIN people p ON p.id = u.person_id
			WHERE c.original_invoice_id = $1 ORDER BY c.at`, invoiceID)
		if err != nil {
			return fmt.Errorf("list corrections: %w", err)
		}
		for rows.Next() {
			var c Correction
			if err := rows.Scan(&c.Kind, &c.Reference, &c.InvoiceID, &c.Reason, &c.By, &c.At); err != nil {
				rows.Close()
				return fmt.Errorf("scan correction: %w", err)
			}
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Or this is the one that corrects another, or is the credit note.
		more, err := tx.Query(ctx, `
			SELECT 'replaces', x.series || '-' || x.number, x.id::text, c.reason, coalesce(p.display_name, ''), c.at
			FROM invoice_corrections c JOIN invoices x ON x.id = c.original_invoice_id
			LEFT JOIN users u ON u.id = c.by_user LEFT JOIN people p ON p.id = u.person_id
			WHERE c.replacement_invoice_id = $1
			UNION ALL
			SELECT 'credits', x.series || '-' || x.number, x.id::text, c.reason, coalesce(p.display_name, ''), c.at
			FROM invoice_corrections c JOIN invoices x ON x.id = c.original_invoice_id
			LEFT JOIN users u ON u.id = c.by_user LEFT JOIN people p ON p.id = u.person_id
			WHERE c.credit_invoice_id = $1`, invoiceID)
		if err != nil {
			return fmt.Errorf("list what this corrects: %w", err)
		}
		defer more.Close()
		for more.Next() {
			var c Correction
			if err := more.Scan(&c.Kind, &c.Reference, &c.InvoiceID, &c.Reason, &c.By, &c.At); err != nil {
				return fmt.Errorf("scan correction: %w", err)
			}
			out = append(out, c)
		}
		return more.Err()
	})
	return out, err
}
