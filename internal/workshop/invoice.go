package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultSeries is the invoice series a shop uses unless it says otherwise.
//
// Separate series exist for separate books -- a second workshop, a parts
// counter -- and each is numbered independently, so the concept has to be in
// the schema from the start even while there is only one.
const DefaultSeries = "A"

// ErrAlreadyInvoiced is returned when a work order already has an invoice.
var ErrAlreadyInvoiced = errors.New("workshop: this order has already been invoiced")

// ErrNothingToInvoice is returned for an order with no chargeable lines.
var ErrNothingToInvoice = errors.New("workshop: there is nothing on this order to charge for")

// Invoice is an issued document.
type Invoice struct {
	ID           string
	Series       string
	Number       int64
	WorkOrderID  string
	CreditOfID   *string
	IssuedAt     time.Time
	CustomerName string
	NetMinor     int64
	VATMinor     int64
	GrossMinor   int64
}

// Reference is how the invoice is referred to on paper: A-1001.
func (i Invoice) Reference() string { return fmt.Sprintf("%s-%d", i.Series, i.Number) }

// Net, VAT and Gross render the totals.
func (i Invoice) Net() string   { return money.Format(i.NetMinor) }
func (i Invoice) VAT() string   { return money.Format(i.VATMinor) }
func (i Invoice) Gross() string { return money.Format(i.GrossMinor) }

// Issue turns a work order into an invoice and moves it to invoiced.
//
// One transaction, from allocating the number to setting the state. That is
// what makes the series gap-free: a failure anywhere un-allocates the number
// along with everything else, which a sequence could not do because sequences
// do not roll back.
func Issue(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, workOrderID string) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}

	var inv Invoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM invoices WHERE work_order_id = $1 AND credit_of_id IS NULL)`,
			workOrderID).Scan(&exists); err != nil {
			return fmt.Errorf("check for an existing invoice: %w", err)
		}
		if exists {
			return ErrAlreadyInvoiced
		}

		// The state machine decides whether this order may be invoiced at all,
		// and refuses an order with nobody to bill. Doing it first means the
		// number is not allocated for an invoice that cannot be issued.
		if err := setStateTx(ctx, tx, workOrderID, StateInvoiced); err != nil {
			return err
		}

		// The customer as they are right now, copied. Correcting an address
		// next year must not reprint this document to a place they did not
		// live.
		var name, address, orgNumber, vatNumber *string
		const customer = `
			SELECT coalesce(p.display_name, c.company_name),
			       nullif(concat_ws(', ', c.address_line1, c.address_line2,
			                        concat_ws(' ', c.postal_code, c.city)), ''),
			       c.org_number, c.vat_number
			FROM work_orders w
			JOIN customers c ON c.id = w.customer_id
			LEFT JOIN people p ON p.id = c.person_id
			WHERE w.id = $1`
		if err := tx.QueryRow(ctx, customer, workOrderID).Scan(&name, &address, &orgNumber, &vatNumber); err != nil {
			return fmt.Errorf("read customer: %w", err)
		}
		if name == nil || *name == "" {
			return ErrNoCustomer
		}

		lines, err := chargeableLines(ctx, tx, workOrderID)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return ErrNothingToInvoice
		}
		totals := money.Compute(toMoneyLines(lines))

		number, err := nextNumber(ctx, tx, scope.ShopID, DefaultSeries)
		if err != nil {
			return err
		}

		const insert = `
			INSERT INTO invoices
			  (shop_id, series, number, work_order_id, issued_by,
			   customer_name, customer_address, customer_org_number, customer_vat_number,
			   net_minor, vat_minor, gross_minor)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			RETURNING id, issued_at`
		if err := tx.QueryRow(ctx, insert,
			scope.ShopID, DefaultSeries, number, workOrderID, scope.UserID,
			*name, address, orgNumber, vatNumber,
			totals.NetMinor, totals.VATMinor, totals.GrossMinor,
		).Scan(&inv.ID, &inv.IssuedAt); err != nil {
			return fmt.Errorf("write invoice: %w", err)
		}

		if err := copyLines(ctx, tx, scope.ShopID, inv.ID, lines, 1); err != nil {
			return err
		}

		// The parts leave the shelf here, in the transaction that issues the
		// document. Anywhere else and the two can disagree.
		if err := consumeForInvoice(ctx, tx, scope, workOrderID); err != nil {
			return err
		}

		inv.Series, inv.Number = DefaultSeries, number
		inv.WorkOrderID = workOrderID
		inv.CustomerName = *name
		inv.NetMinor, inv.VATMinor, inv.GrossMinor = totals.NetMinor, totals.VATMinor, totals.GrossMinor
		return nil
	})
	if err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

// CreditNote reverses an invoice exactly.
//
// Each line is negated rather than the totals recomputed from a negative
// quantity, so the rounding that was applied to the original is reversed
// rather than recalculated. Recalculating can round the other way and leave an
// öre behind, which is precisely the kind of residue nobody can account for a
// year later.
func CreditNote(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}

	var note Invoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var original Invoice
		var address, orgNumber, vatNumber *string
		const read = `
			SELECT id, series, number, work_order_id, customer_name,
			       customer_address, customer_org_number, customer_vat_number,
			       net_minor, vat_minor, gross_minor
			FROM invoices WHERE id = $1`
		err := tx.QueryRow(ctx, read, invoiceID).Scan(
			&original.ID, &original.Series, &original.Number, &original.WorkOrderID,
			&original.CustomerName, &address, &orgNumber, &vatNumber,
			&original.NetMinor, &original.VATMinor, &original.GrossMinor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read invoice: %w", err)
		}

		var credited bool
		if err := tx.QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM invoices WHERE credit_of_id = $1)`, invoiceID).Scan(&credited); err != nil {
			return fmt.Errorf("check for an existing credit note: %w", err)
		}
		if credited {
			return fmt.Errorf("%w: this invoice has already been credited", ErrInvalid)
		}

		rows, err := tx.Query(ctx, `
			SELECT position, kind, description, (quantity * 1000)::bigint,
			       unit_price_minor, vat_rate_bp, net_minor, vat_minor
			FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, invoiceID)
		if err != nil {
			return fmt.Errorf("read invoice lines: %w", err)
		}
		var lines []invoiceLine
		for rows.Next() {
			var l invoiceLine
			if err := rows.Scan(&l.Position, &l.Kind, &l.Description, &l.QuantityMilli,
				&l.UnitPriceMinor, &l.VATRateBasis, &l.NetMinor, &l.VATMinor); err != nil {
				rows.Close()
				return fmt.Errorf("scan invoice line: %w", err)
			}
			// Negate what was stored. Not recomputed.
			l.QuantityMilli = -l.QuantityMilli
			l.NetMinor = -l.NetMinor
			l.VATMinor = -l.VATMinor
			lines = append(lines, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		number, err := nextNumber(ctx, tx, scope.ShopID, original.Series)
		if err != nil {
			return err
		}

		const insert = `
			INSERT INTO invoices
			  (shop_id, series, number, work_order_id, credit_of_id, issued_by,
			   customer_name, customer_address, customer_org_number, customer_vat_number,
			   net_minor, vat_minor, gross_minor)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			RETURNING id, issued_at`
		if err := tx.QueryRow(ctx, insert,
			scope.ShopID, original.Series, number, original.WorkOrderID, original.ID, scope.UserID,
			original.CustomerName, address, orgNumber, vatNumber,
			-original.NetMinor, -original.VATMinor, -original.GrossMinor,
		).Scan(&note.ID, &note.IssuedAt); err != nil {
			return fmt.Errorf("write credit note: %w", err)
		}

		for i := range lines {
			lines[i].Position = i + 1
		}
		if err := writeInvoiceLines(ctx, tx, scope.ShopID, note.ID, lines); err != nil {
			return err
		}

		note.Series, note.Number = original.Series, number
		note.WorkOrderID = original.WorkOrderID
		note.CreditOfID = &original.ID
		note.CustomerName = original.CustomerName
		note.NetMinor, note.VATMinor, note.GrossMinor =
			-original.NetMinor, -original.VATMinor, -original.GrossMinor
		return nil
	})
	if err != nil {
		return Invoice{}, err
	}
	return note, nil
}

// InvoicesFor returns every document issued against a work order, oldest
// first: the invoice, then any credit note.
func InvoicesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, workOrderID string) ([]Invoice, error) {
	var out []Invoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, series, number, work_order_id, credit_of_id, issued_at,
			       customer_name, net_minor, vat_minor, gross_minor
			FROM invoices WHERE work_order_id = $1 ORDER BY issued_at, number`, workOrderID)
		if err != nil {
			return fmt.Errorf("read invoices: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var i Invoice
			if err := rows.Scan(&i.ID, &i.Series, &i.Number, &i.WorkOrderID, &i.CreditOfID,
				&i.IssuedAt, &i.CustomerName, &i.NetMinor, &i.VATMinor, &i.GrossMinor); err != nil {
				return fmt.Errorf("scan invoice: %w", err)
			}
			out = append(out, i)
		}
		return rows.Err()
	})
	return out, err
}

// IsCreditNote reports whether this document reverses another.
func (i Invoice) IsCreditNote() bool { return i.CreditOfID != nil }

// consumeForInvoice takes the job's parts off the shelf.
//
// Every line with a part, whoever is paying for it: a warranty replacement
// costs the customer nothing and the part left the shelf all the same.
// Consuming releases whatever was reserved for the job first, so nothing is
// counted as both put aside and used.
func consumeForInvoice(ctx context.Context, tx pgx.Tx, scope access.Scope, workOrderID string) error {
	rows, err := tx.Query(ctx, `
		SELECT part_id, sum(quantity)
		FROM work_order_lines
		WHERE work_order_id = $1 AND part_id IS NOT NULL AND quantity > 0
		GROUP BY part_id`, workOrderID)
	if err != nil {
		return fmt.Errorf("read the job's parts: %w", err)
	}
	type use struct {
		part     string
		quantity float64
	}
	var used []use
	for rows.Next() {
		var u use
		if err := rows.Scan(&u.part, &u.quantity); err != nil {
			rows.Close()
			return fmt.Errorf("scan part: %w", err)
		}
		used = append(used, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(used) == 0 {
		return nil
	}

	if err := releaseReservationsForOrder(ctx, tx, workOrderID); err != nil {
		return err
	}
	for _, u := range used {
		// Nothing here refuses because the shelf would go negative. The car
		// has left; a negative figure is a symptom to be seen, not a reason to
		// stop an invoice.
		if err := moveTx(ctx, tx, scope,
			Movement{Kind: "consumed", Quantity: -u.quantity}, u.part, workOrderID, ""); err != nil {
			return err
		}
	}
	return nil
}

// nextNumber allocates the next number in a series.
//
// The UPDATE takes a row lock, so two transactions issuing at the same instant
// serialise here rather than both reading the same value. That is the whole
// mechanism, and it is why the counter is a table and not a sequence: this
// allocation rolls back with the transaction, and a sequence would not.
func nextNumber(ctx context.Context, tx pgx.Tx, shopID, series string) (int64, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO invoice_series (shop_id, series) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		shopID, series); err != nil {
		return 0, fmt.Errorf("create series: %w", err)
	}
	var number int64
	if err := tx.QueryRow(ctx,
		`UPDATE invoice_series SET next_number = next_number + 1
		 WHERE shop_id = $1 AND series = $2
		 RETURNING next_number - 1`, shopID, series).Scan(&number); err != nil {
		return 0, fmt.Errorf("allocate number: %w", err)
	}
	return number, nil
}

type invoiceLine struct {
	Position       int
	Kind           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
	NetMinor       int64
	VATMinor       int64
}

func chargeableLines(ctx context.Context, tx pgx.Tx, workOrderID string) ([]invoiceLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT position, kind, description, (quantity * 1000)::bigint,
		       unit_price_minor, vat_rate_bp
		FROM work_order_lines
		WHERE work_order_id = $1 AND cost_bearer = 'customer'
		ORDER BY position`, workOrderID)
	if err != nil {
		return nil, fmt.Errorf("read lines: %w", err)
	}
	defer rows.Close()

	var out []invoiceLine
	for rows.Next() {
		var l invoiceLine
		if err := rows.Scan(&l.Position, &l.Kind, &l.Description, &l.QuantityMilli,
			&l.UnitPriceMinor, &l.VATRateBasis); err != nil {
			return nil, fmt.Errorf("scan line: %w", err)
		}
		l.NetMinor = money.LineNet(l.QuantityMilli, l.UnitPriceMinor)
		l.VATMinor = money.VAT(l.NetMinor, l.VATRateBasis)
		out = append(out, l)
	}
	return out, rows.Err()
}

func toMoneyLines(lines []invoiceLine) []money.Line {
	out := make([]money.Line, 0, len(lines))
	for _, l := range lines {
		out = append(out, money.Line{
			QuantityMilli:     l.QuantityMilli,
			UnitPriceMinor:    l.UnitPriceMinor,
			VATRateBasis:      l.VATRateBasis,
			ChargedToCustomer: true,
		})
	}
	return out
}

func copyLines(ctx context.Context, tx pgx.Tx, shopID, invoiceID string, lines []invoiceLine, from int) error {
	for i := range lines {
		lines[i].Position = from + i
	}
	return writeInvoiceLines(ctx, tx, shopID, invoiceID, lines)
}

func writeInvoiceLines(ctx context.Context, tx pgx.Tx, shopID, invoiceID string, lines []invoiceLine) error {
	const insert = `
		INSERT INTO invoice_lines
		  (shop_id, invoice_id, position, kind, description, quantity,
		   unit_price_minor, vat_rate_bp, net_minor, vat_minor)
		VALUES ($1,$2,$3,$4,$5,($6::bigint)::numeric / 1000,$7,$8,$9,$10)`
	for _, l := range lines {
		if _, err := tx.Exec(ctx, insert, shopID, invoiceID, l.Position, l.Kind, l.Description,
			l.QuantityMilli, l.UnitPriceMinor, l.VATRateBasis, l.NetMinor, l.VATMinor); err != nil {
			return fmt.Errorf("write invoice line %d: %w", l.Position, err)
		}
	}
	return nil
}
