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
	"github.com/RedaEkengren/FreeSMS/internal/payref"
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
	// Listed on a cash invoice as how it was paid on issue: it stands as
	// the document does, and is corrected with a credit note (0087).
	PaidOnIssue bool
	// What it is against: one of an invoice, a deposit and a payout (#152).
	InvoiceID string
	DepositID string
	PayoutID  string
	// The payment it gives back part or all of, for a refund.
	RefundsID string
	// Who carried it, and their id for it.
	Provider   string
	ExternalID string
	// Why it was reversed or refunded.
	Reason string
	// The request's own key: repeated, it finds this row.
	Key string
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

	// Set when the person has been told the amount is more than is owed and
	// says it is meant, and why. Without it an extra zero is recorded as
	// money that arrived; with it, the decision is its own event (#151).
	ConfirmedOverpayment bool
	OverpaymentReason    string

	// Who carried it and their own id for it -- the bank's line, the Swish
	// payment -- so the same one is recorded once.
	Provider   string
	ExternalID string
	// The request's own key: repeated, it records nothing more and answers
	// with the payment it made.
	Key string
}

// overpaymentMargin is how far above what is owed a payment may go before
// somebody is asked: a krona covers cash rounding and an öre typed wrong.
const overpaymentMargin = 100

// maxPaidBeforeInvoiceDays is how long before its invoice a payment may be
// dated. It is not zero, which is what #133 first asked for ("not before the
// invoice date"), because money is taken before the document is written as a
// matter of course: a deposit when a part is ordered, the counter paid on
// Friday and the invoice written up on Monday, a prepaid booking. A month
// holds all of those and still refuses a year typed wrong (1999 for 2026),
// which is the mistake that moves money into a day with nothing to do with the
// job. A payment may not be dated more than a day ahead either: money that has
// not arrived has not arrived.
//
// A day that has been signed off (day.go) is not protected by refusing a
// payment dated into it. The payment is recorded and the sign-off says the
// day has changed since, the way it does for anything recorded late: refusing
// would leave money that did arrive with nowhere to be written down.
const maxPaidBeforeInvoiceDays = 31

// ErrOverpayment is refusing a payment far above what is owed until the person
// says that is what they mean.
var ErrOverpayment = fmt.Errorf("%w: that is more than is owed on the invoice; if the customer really paid more, tick that and record it again", ErrInvalid)

// ErrOverpaymentNoReason is recording far more than is owed without saying
// why.
var ErrOverpaymentNoReason = fmt.Errorf("%w: say why more than is owed is recorded, in 300 characters or fewer", ErrInvalid)

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
	q := `SELECT id, series || '-' || number, coalesce(work_order_id::text, ''), customer_name, issued_at, gross_minor,
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
		       EXISTS (SELECT 1 FROM invoice_payments r WHERE r.reverses_id = p.id),
		       EXISTS (SELECT 1 FROM cash_invoices c WHERE c.invoice_id = p.invoice_id
		               AND c.paid_with @> jsonb_build_array(jsonb_build_object('payment_id', p.id::text)))
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
			&p.PaidOn, &p.RecordedAt, &p.RecordedBy, &p.Reference, &p.Reverses, &p.Reversed, &p.PaidOnIssue); err != nil {
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
	if (p.Provider == "") != (p.ExternalID == "") {
		return Payment{}, fmt.Errorf("%w: a provider's id needs the provider, and the other way round", ErrInvalid)
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
		// The same request again is answered with what it recorded, before
		// anything is asked of a balance it has itself changed.
		if strings.TrimSpace(p.Key) != "" {
			out = Payment{InvoiceID: b.InvoiceID, Method: p.Method}
			if p.AmountMinor != nil {
				out.AmountMinor = *p.AmountMinor
			}
			if found, err := replayedPaymentTx(ctx, tx, strings.TrimSpace(p.Key), &out); found || err != nil {
				return err
			}
		}
		// Money can arrive before the invoice is written -- paid at the
		// counter on Friday, written up on Monday -- but not a month before.
		// A year typed wrong moves it into a day that had nothing to do
		// with this job, and the day's figures read these dates.
		if issued := b.IssuedAt.In(b.Today.Location()); p.PaidOn.Format("2006-01-02") < issued.AddDate(0, 0, -maxPaidBeforeInvoiceDays).Format("2006-01-02") {
			return fmt.Errorf("%w: that is more than %d days before the invoice was issued, on %s", ErrInvalid, maxPaidBeforeInvoiceDays, issued.Format("2006-01-02"))
		}
		// Money given back here is an overpayment returned, and is at most
		// what is owed back; a refund of a payment is RefundPayment. The
		// database holds it too (0083).
		if p.AmountMinor != nil && *p.AmountMinor < 0 && b.OutstandingMinor()-*p.AmountMinor > 0 {
			return errGivenBackTooMuch
		}
		over := p.AmountMinor != nil && *p.AmountMinor > b.OutstandingMinor()+overpaymentMargin
		var why string
		switch {
		case over && !p.ConfirmedOverpayment:
			return ErrOverpayment
		case over:
			if why, err = checkReason(p.OverpaymentReason, ErrOverpaymentNoReason); err != nil {
				return err
			}
		}
		out = Payment{InvoiceID: b.InvoiceID, Method: p.Method, PaidOn: p.PaidOn, Reference: strings.TrimSpace(p.Reference),
			Provider: p.Provider, ExternalID: p.ExternalID, Key: p.Key}
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
		if replayed, err := insertPaymentOnce(ctx, tx, scope, &out); err != nil || replayed {
			return err
		}
		if over {
			if err := auditTx(ctx, tx, scope, out.ID, "payment.overpayment_confirmed", "", "", why,
				map[string]any{"invoice_id": b.InvoiceID, "amount_minor": out.AmountMinor, "owed_minor": b.OutstandingMinor()}); err != nil {
				return err
			}
		}
		// The money that settles it may be what finishes the job.
		return closeIfFinishedTx(ctx, tx, b.WorkOrderID)
	})
	return out, err
}

// ReversePayment corrects a payment recorded in error with one that undoes
// it, dated today, and says why, which is required. The first stays, with
// the reversal beside it: the ledger is no easier to quietly rewrite than
// the invoice. Any payment -- into an invoice or a deposit, a payout, a
// refund -- by whoever may move money on what it is against.
//
// A reversal undoes the whole payment, so one with refunds standing is
// refused until they are reversed, and a deposit's receipt only while the
// deposit still holds it: what was given back or applied did move. The
// database holds both (0081).
//
// A deposit applied to an invoice is two rows and one fact: reversed from
// either, both are, so the money is neither held and paid at once nor
// gone (0082).
//
// A payment a cash invoice lists as how it was paid on issue is not
// reversed: it is part of a document frozen as paid, and a mistake is a
// credit note, with a new invoice if one is needed (0087).
func ReversePayment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, paymentID, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.SellsVehicles() && !scope.Role.HandsOverRentals() && !scope.Role.SeesPurchasePrice() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(paymentID) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, ErrReversalNoReason)
	if err != nil {
		return err
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// Which it is decides what is locked, and in which order: an
		// application is the deposit's and the invoice's, as ApplyDeposit
		// locks them.
		var method, depositHalf, invoiceHalf string
		err := tx.QueryRow(ctx, `
			SELECT p.method, coalesce(a.deposit_payment_id::text, ''), coalesce(a.invoice_payment_id::text, '')
			FROM invoice_payments p
			LEFT JOIN deposit_applications a ON p.id IN (a.deposit_payment_id, a.invoice_payment_id)
			WHERE p.id = $1`, paymentID).Scan(&method, &depositHalf, &invoiceHalf)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read payment: %w", err)
		}
		if method == "deposit" {
			return reverseApplicationTx(ctx, tx, scope, depositHalf, invoiceHalf, reason)
		}
		orig, err := paymentTargetTx(ctx, tx, scope, paymentID)
		if err != nil {
			return err
		}
		if orig.Reverses != "" {
			return fmt.Errorf("%w: that is itself a reversal; record the payment again instead", ErrInvalid)
		}
		if orig.InvoiceID != "" {
			var onIssue bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM cash_invoices c WHERE c.invoice_id = $1
				               AND c.paid_with @> jsonb_build_array(jsonb_build_object('payment_id', $2::text)))`,
				orig.InvoiceID, orig.ID).Scan(&onIssue); err != nil {
				return fmt.Errorf("read the cash invoice: %w", err)
			}
			if onIssue {
				return errCashInvoicePayment
			}
		}
		if orig.RefundsID == "" {
			var standing bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM invoice_payments r WHERE r.refunds_id = $1
				               AND NOT EXISTS (SELECT 1 FROM invoice_payments x WHERE x.reverses_id = r.id))`,
				paymentID).Scan(&standing); err != nil {
				return fmt.Errorf("read refunds: %w", err)
			}
			if standing {
				return errRefundsStand
			}
		}
		undo := Payment{InvoiceID: orig.InvoiceID, DepositID: orig.DepositID, PayoutID: orig.PayoutID,
			AmountMinor: -orig.AmountMinor, RoundingMinor: -orig.RoundingMinor, Method: orig.Method, PaidOn: time.Now(),
			Reverses: paymentID, Reason: reason}
		return insertPayment(ctx, tx, scope, &undo)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "invoice_payments_reverses_id_key" {
		return fmt.Errorf("%w: that payment has already been reversed", ErrInvalid)
	}
	return err
}

// reverseApplicationTx reverses both rows of a deposit's application to an
// invoice: the front desk's, who applies one. The deposit is locked first,
// then the invoice, as ApplyDeposit does.
func reverseApplicationTx(ctx context.Context, tx pgx.Tx, scope access.Scope, depositHalf, invoiceHalf, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if depositHalf == "" {
		return fmt.Errorf("%w: that is a reversal of a deposit's application, or its other half cannot be found", ErrInvalid)
	}
	var halves [2]Payment
	for i, id := range []string{depositHalf, invoiceHalf} {
		p := &halves[i]
		if err := tx.QueryRow(ctx, `
			SELECT id, coalesce(invoice_id::text, ''), coalesce(deposit_id::text, ''), amount_minor, rounding_minor,
			       EXISTS (SELECT 1 FROM invoice_payments r WHERE r.reverses_id = p.id)
			FROM invoice_payments p WHERE id = $1`, id).Scan(&p.ID, &p.InvoiceID, &p.DepositID, &p.AmountMinor, &p.RoundingMinor, &p.Reversed); err != nil {
			return fmt.Errorf("read the application: %w", err)
		}
	}
	if _, err := depositTx(ctx, tx, halves[0].DepositID, true); err != nil {
		return err
	}
	if _, err := balanceTx(ctx, tx, halves[1].InvoiceID, true); err != nil {
		return err
	}
	if halves[0].Reversed && halves[1].Reversed {
		return fmt.Errorf("%w: that payment has already been reversed", ErrInvalid)
	}
	// Before 0082 either half could be reversed alone, and 0082 linked
	// such pairs as they were: the half left is reversed alone now, which
	// makes the application reversed whole.
	for _, p := range halves {
		if p.Reversed {
			continue
		}
		undo := Payment{InvoiceID: p.InvoiceID, DepositID: p.DepositID, AmountMinor: -p.AmountMinor, RoundingMinor: -p.RoundingMinor,
			Method: "deposit", PaidOn: time.Now(), Reverses: p.ID, Reason: reason}
		if err := insertPayment(ctx, tx, scope, &undo); err != nil {
			return err
		}
	}
	return nil
}

// insertPaymentOnce is insertPayment for a request that may be repeated:
// with a key already used, it reads that payment into p instead, and says
// it did. The same key for a payment against something else is refused.
// Repeated while the first is still in flight, it waits for the first, as
// BookResource does.
func insertPaymentOnce(ctx context.Context, tx pgx.Tx, scope access.Scope, p *Payment) (replayed bool, err error) {
	key := strings.TrimSpace(p.Key)
	if key == "" {
		return false, insertPayment(ctx, tx, scope, p)
	}
	if found, err := replayedPaymentTx(ctx, tx, key, p); found || err != nil {
		return found, err
	}
	if err := insertKeyedTx(ctx, tx, scope, p); err != nil {
		if errors.Is(err, errKeyTaken) {
			return replayedPaymentTx(ctx, tx, key, p)
		}
		return false, err
	}
	return false, nil
}

// errKeyTaken is a request key another request has used since it was
// looked for.
var errKeyTaken = errors.New("workshop: the request key was taken meanwhile")

// insertKeyedTx inserts p, whose key may have been taken by another request
// since it was looked for -- one not serialised by the same lock, which
// waits on the key's unique index for the other to finish -- and says so
// with errKeyTaken, leaving the transaction usable.
func insertKeyedTx(ctx context.Context, tx pgx.Tx, scope access.Scope, p *Payment) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := insertPayment(ctx, sp, scope, p); err != nil {
		if !isUniqueViolation(err, "invoice_payments_request_idx") {
			return err
		}
		if rerr := sp.Rollback(ctx); rerr != nil {
			return rerr
		}
		return errKeyTaken
	}
	return sp.Commit(ctx)
}

// replayedPaymentTx finds the payment a key recorded and reads it into p,
// when it is the same request: against the same thing, the same kind of
// movement -- a payment, a refund of the same one -- by the same way of
// paying and, where p says, for the same amount. A key used for anything
// else is refused.
func replayedPaymentTx(ctx context.Context, tx pgx.Tx, key string, p *Payment) (bool, error) {
	var target, refunds, reverses, method string
	var got Payment
	err := tx.QueryRow(ctx, `
		SELECT id, series, number, amount_minor, rounding_minor, recorded_at, coalesce(invoice_id, deposit_id, payout_id)::text,
		       coalesce(refunds_id::text, ''), coalesce(reverses_id::text, ''), method
		FROM invoice_payments WHERE request_key = $1`, key).Scan(&got.ID, &got.Series, &got.Number, &got.AmountMinor,
		&got.RoundingMinor, &got.RecordedAt, &target, &refunds, &reverses, &method)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the request: %w", err)
	}
	switch {
	case !strings.EqualFold(target, p.InvoiceID+p.DepositID+p.PayoutID),
		!strings.EqualFold(refunds, p.RefundsID), reverses != "",
		p.Method != "" && method != p.Method,
		p.AmountMinor != 0 && got.AmountMinor != p.AmountMinor:
		return false, errKeyOtherPayment
	}
	p.ID, p.Series, p.Number, p.AmountMinor, p.RoundingMinor, p.RecordedAt = got.ID, got.Series, got.Number, got.AmountMinor, got.RoundingMinor, got.RecordedAt
	return true, nil
}

// insertPayment writes p as the next payment in the series, against
// whichever one of an invoice, a deposit and a payout it names.
func insertPayment(ctx context.Context, tx pgx.Tx, scope access.Scope, p *Payment) error {
	number, err := nextNumber(ctx, tx, scope.ShopID, PaymentSeries)
	if err != nil {
		return err
	}
	p.Series, p.Number = PaymentSeries, number
	err = tx.QueryRow(ctx, `
		INSERT INTO invoice_payments
		  (shop_id, invoice_id, deposit_id, payout_id, series, number, amount_minor, rounding_minor, method, paid_on,
		   recorded_by, reference, reverses_id, refunds_id, provider, external_id, reason, request_key)
		VALUES ($1, NULLIF($2, '')::uuid, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $10::date,
		        $11, nullif($12, ''), nullif($13, '')::uuid, nullif($14, '')::uuid, nullif($15, ''), nullif($16, ''),
		        nullif($17, ''), nullif($18, ''))
		RETURNING id, recorded_at`,
		scope.ShopID, p.InvoiceID, p.DepositID, p.PayoutID, p.Series, p.Number, p.AmountMinor, p.RoundingMinor, p.Method,
		p.PaidOn.Format("2006-01-02"), scope.UserID, p.Reference, p.Reverses, p.RefundsID, p.Provider,
		strings.TrimSpace(p.ExternalID), p.Reason, strings.TrimSpace(p.Key)).Scan(&p.ID, &p.RecordedAt)
	if isUniqueViolation(err, "invoice_payments_external_idx") {
		return errExternalTwice
	}
	// The database holds what is handed back within what came in (0081);
	// the checks before this say so first, and these are for the request
	// that lost a race with another.
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23514" {
		switch {
		case strings.Contains(pg.Message, "refund of more"):
			return errRefundTooMuch
		case strings.Contains(pg.Message, "reversed payment is not refunded"):
			return errReversedRefund
		case strings.Contains(pg.Message, "reversed only once they are"):
			return errRefundsStand
		case strings.Contains(pg.Message, "more taken from a deposit"):
			return errDepositShort
		case strings.Contains(pg.Message, "more given back on an invoice"):
			return errGivenBackTooMuch
		case strings.Contains(pg.Message, "cash invoice's payment is not reversed"):
			return errCashInvoicePayment
		}
	}
	return err
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

var (
	errReferenceCheck   = fmt.Errorf("%w: that reference's check digit is wrong; check it against the invoice", ErrInvalid)
	errReferenceShape   = fmt.Errorf("%w: a payment reference is 2 to 25 digits", ErrInvalid)
	errReferenceUnknown = fmt.Errorf("%w: no invoice has that payment reference", ErrInvalid)
)

// InvoiceByPaymentReference is the invoice a payment reference is for, as
// it was typed or as a bank sent it (#234): its check digit is checked
// before anything is looked up, so a mistyped digit is refused for what it
// is rather than paying somebody else's invoice. For whoever records
// payments.
func InvoiceByPaymentReference(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, typed string) (id, reference string, err error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", "", access.ErrForbidden
	}
	reference, err = payref.CheckOCR(typed)
	switch {
	case errors.Is(err, payref.ErrCheckDigit):
		return "", "", errReferenceCheck
	case err != nil:
		return "", "", errReferenceShape
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text FROM invoices WHERE payment_ocr = $1`, reference).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errReferenceUnknown
	}
	return id, reference, err
}
