package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PaymentSeries is the voucher series payments are numbered in, apart from
// the invoices' A: a series is numbered in order, and a payment is not an
// invoice.
const PaymentSeries = "B"

// PaymentMethods are the ways money arrives, for the books. Recording that it
// arrived is not taking it: there is no card terminal and no Swish here.
var PaymentMethods = []string{"bank", "cash", "card", "swish", "other"}

// Payment is one row of an invoice's ledger.
type Payment struct {
	ID            string
	Series        string
	Number        int64
	AmountMinor   int64
	RoundingMinor int64
	Method        string
	PaidOn        time.Time
	RecordedAt    time.Time
	RecordedBy    string
	Reference     string
	Reverses      string
	Reversed      bool
}

// Settles is what the payment took off the balance: the money and the
// rounding.
func (p Payment) Settles() int64 { return p.AmountMinor + p.RoundingMinor }

// Balance is what is owed on an invoice, derived from the invoice, its credit
// notes and its payments -- never a column written over.
type Balance struct {
	InvoiceID    string
	Reference    string
	WorkOrderID  string
	CustomerName string
	IssuedAt     time.Time
	GrossMinor   int64
	// The credit notes against it, as the negative amounts they are.
	CreditedMinor int64
	// Everything the payments settled, rounding included.
	PaidMinor int64
	Due       time.Time
	// Today, in the shop's zone: whether it is overdue is a question about a
	// date on the shop's calendar.
	Today    time.Time
	Payments []Payment
}

// OutstandingMinor is what is still owed. Negative is money the shop owes
// back -- somebody paid an old figure after a credit note -- and is shown, not
// clamped.
func (b Balance) OutstandingMinor() int64 { return b.GrossMinor + b.CreditedMinor - b.PaidMinor }

// Overdue reports whether something is still owed after the due date. The
// due date is the terms frozen on the document, so a shop that shortens its
// terms does not make last month's invoices late.
func (b Balance) Overdue() bool {
	return b.OutstandingMinor() > 0 && b.Today.After(b.Due)
}

// Settled reports whether nothing is owed either way.
func (b Balance) Settled() bool { return b.OutstandingMinor() == 0 }

// NewPayment is a payment as the front desk records it.
type NewPayment struct {
	// Nil settles whatever is owed. Cash is then rounded to the krona and the
	// difference recorded as rounding; any other way pays the öre too.
	AmountMinor *int64
	Method      string
	PaidOn      time.Time
	Reference   string
}

// ErrNothingOwed is refusing to settle a balance that is already settled.
var ErrNothingOwed = fmt.Errorf("%w: nothing is owed on that invoice", ErrInvalid)

// BalanceFor reads what is owed on an invoice, and its payments. Given a
// credit note, it answers for the invoice the credit note belongs to.
func BalanceFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) (Balance, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Balance{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return Balance{}, ErrNotFound
	}
	var b Balance
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = balanceTx(ctx, tx, invoiceID, false)
		return err
	})
	return b, err
}

func balanceTx(ctx context.Context, tx pgx.Tx, invoiceID string, lock bool) (Balance, error) {
	var b Balance
	var original *string
	if err := tx.QueryRow(ctx, `SELECT credit_of_id FROM invoices WHERE id = $1`, invoiceID).Scan(&original); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return b, ErrNotFound
		}
		return b, fmt.Errorf("read invoice: %w", err)
	}
	if original != nil {
		invoiceID = *original
	}
	q := `SELECT id, series || '-' || number, work_order_id, customer_name, issued_at, gross_minor,
	             coalesce(seller_payment_terms_days, 0)
	      FROM invoices WHERE id = $1`
	if lock {
		// The payment and the balance it was checked against are one
		// step: two people settling the same invoice at once do not both
		// settle it.
		q += ` FOR UPDATE`
	}
	var terms int
	if err := tx.QueryRow(ctx, q, invoiceID).Scan(&b.InvoiceID, &b.Reference, &b.WorkOrderID,
		&b.CustomerName, &b.IssuedAt, &b.GrossMinor, &terms); err != nil {
		return b, fmt.Errorf("read invoice: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(gross_minor), 0) FROM invoices WHERE credit_of_id = $1`,
		invoiceID).Scan(&b.CreditedMinor); err != nil {
		return b, fmt.Errorf("read credit notes: %w", err)
	}

	var zone string
	if err := tx.QueryRow(ctx, `SELECT timezone FROM shops LIMIT 1`).Scan(&zone); err != nil {
		return b, fmt.Errorf("read timezone: %w", err)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	issued := b.IssuedAt.In(loc)
	due := issued.AddDate(0, 0, terms)
	b.Due = time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, loc)
	now := time.Now().In(loc)
	b.Today = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	rows, err := tx.Query(ctx, `
		SELECT p.id, p.series, p.number, p.amount_minor, p.rounding_minor, p.method, p.paid_on,
		       p.recorded_at, coalesce(pe.display_name, ''), coalesce(p.reference, ''),
		       coalesce(p.reverses_id::text, ''),
		       EXISTS (SELECT 1 FROM invoice_payments r WHERE r.reverses_id = p.id)
		FROM invoice_payments p
		LEFT JOIN users u ON u.id = p.recorded_by
		LEFT JOIN people pe ON pe.id = u.person_id
		WHERE p.invoice_id = $1
		ORDER BY p.paid_on, p.recorded_at`, invoiceID)
	if err != nil {
		return b, fmt.Errorf("read payments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.Series, &p.Number, &p.AmountMinor, &p.RoundingMinor, &p.Method,
			&p.PaidOn, &p.RecordedAt, &p.RecordedBy, &p.Reference, &p.Reverses, &p.Reversed); err != nil {
			return b, fmt.Errorf("scan payment: %w", err)
		}
		b.PaidMinor += p.Settles()
		b.Payments = append(b.Payments, p)
	}
	return b, rows.Err()
}

// RecordPayment records money arriving against an invoice. The front desk's:
// money is a conversation with the customer.
func RecordPayment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, p NewPayment) (Payment, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Payment{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return Payment{}, ErrNotFound
	}
	if !knownMethod(p.Method) {
		return Payment{}, fmt.Errorf("%w: %q is not a way money arrives", ErrInvalid, p.Method)
	}
	if p.PaidOn.IsZero() {
		return Payment{}, fmt.Errorf("%w: when did it arrive?", ErrInvalid)
	}
	if p.PaidOn.After(time.Now().Add(24 * time.Hour)) {
		return Payment{}, fmt.Errorf("%w: money that has not arrived yet cannot be recorded as arrived", ErrInvalid)
	}
	if p.AmountMinor != nil && *p.AmountMinor == 0 {
		return Payment{}, fmt.Errorf("%w: a payment of nothing is not a payment", ErrInvalid)
	}
	var out Payment
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		b, err := balanceTx(ctx, tx, invoiceID, true)
		if err != nil {
			return err
		}
		if b.InvoiceID != invoiceID {
			return fmt.Errorf("%w: that is a credit note; record the payment against %s", ErrInvalid, b.Reference)
		}
		out = Payment{Method: p.Method, PaidOn: p.PaidOn, Reference: strings.TrimSpace(p.Reference)}
		if p.AmountMinor != nil {
			out.AmountMinor = *p.AmountMinor
		} else {
			owed := b.OutstandingMinor()
			if owed <= 0 {
				return ErrNothingOwed
			}
			out.AmountMinor = owed
			if p.Method == "cash" {
				// Rounded at the counter, and the difference booked as
				// rounding rather than left on the invoice.
				out.AmountMinor = money.CashRound(owed)
				out.RoundingMinor = owed - out.AmountMinor
			}
		}
		if err := insertPayment(ctx, tx, scope, b.InvoiceID, &out, ""); err != nil {
			return err
		}
		// The money that settles it may be what finishes the job.
		return closeIfFinishedTx(ctx, tx, b.WorkOrderID)
	})
	return out, err
}

// ReversePayment corrects a payment recorded in error with one that undoes
// it, dated today. The first stays, with the reversal beside it: the ledger is
// no easier to quietly rewrite than the invoice.
func ReversePayment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, paymentID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(paymentID) {
		return ErrNotFound
	}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var invoiceID, method string
		var amount, rounding int64
		var reverses *string
		err := tx.QueryRow(ctx, `
			SELECT invoice_id, amount_minor, rounding_minor, method, reverses_id
			FROM invoice_payments WHERE id = $1`, paymentID).Scan(&invoiceID, &amount, &rounding, &method, &reverses)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read payment: %w", err)
		}
		if reverses != nil {
			return fmt.Errorf("%w: that is itself a reversal; record the payment again instead", ErrInvalid)
		}
		if _, err := balanceTx(ctx, tx, invoiceID, true); err != nil {
			return err
		}
		undo := Payment{AmountMinor: -amount, RoundingMinor: -rounding, Method: method, PaidOn: time.Now()}
		return insertPayment(ctx, tx, scope, invoiceID, &undo, paymentID)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: that payment has already been reversed", ErrInvalid)
	}
	return err
}

func insertPayment(ctx context.Context, tx pgx.Tx, scope access.Scope, invoiceID string, p *Payment, reverses string) error {
	number, err := nextNumber(ctx, tx, scope.ShopID, PaymentSeries)
	if err != nil {
		return err
	}
	p.Series, p.Number = PaymentSeries, number
	return tx.QueryRow(ctx, `
		INSERT INTO invoice_payments
		  (shop_id, invoice_id, series, number, amount_minor, rounding_minor, method, paid_on,
		   recorded_by, reference, reverses_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9, nullif($10, ''), nullif($11, '')::uuid)
		RETURNING id, recorded_at`,
		scope.ShopID, invoiceID, p.Series, p.Number, p.AmountMinor, p.RoundingMinor, p.Method,
		p.PaidOn.Format("2006-01-02"), scope.UserID, p.Reference, reverses).Scan(&p.ID, &p.RecordedAt)
}

// Receivables lists the invoices with something owed either way, overdue
// first: the question a workshop asks every week, answerable at the counter.
func Receivables(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Balance, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []Balance
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT i.id FROM invoices i
			WHERE i.credit_of_id IS NULL
			  AND i.gross_minor
			      + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = i.id), 0)
			      - coalesce((SELECT sum(p.amount_minor + p.rounding_minor) FROM invoice_payments p
			                  WHERE p.invoice_id = i.id), 0) <> 0
			ORDER BY i.issued_at`)
		if err != nil {
			return fmt.Errorf("list receivables: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			b, err := balanceTx(ctx, tx, id, false)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return nil
	})
	// Overdue first, oldest due first; then the rest, and money owed back
	// last but not hidden.
	sortReceivables(out)
	return out, err
}

func sortReceivables(bs []Balance) {
	rank := func(b Balance) int {
		switch {
		case b.Overdue():
			return 0
		case b.OutstandingMinor() > 0:
			return 1
		}
		return 2
	}
	for i := 1; i < len(bs); i++ {
		for j := i; j > 0; j-- {
			a, b := bs[j-1], bs[j]
			if rank(a) < rank(b) || (rank(a) == rank(b) && !a.Due.After(b.Due)) {
				break
			}
			bs[j-1], bs[j] = b, a
		}
	}
}

func knownMethod(m string) bool {
	for _, k := range PaymentMethods {
		if m == k {
			return true
		}
	}
	return false
}
