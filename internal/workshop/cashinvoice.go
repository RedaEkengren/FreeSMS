package workshop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Cash invoices (#156): an invoice issued at the moment it is paid, in one
// step and one transaction -- the invoice, its payments, and the record
// that it was paid on issue, frozen as the invoice is. Either all of it is
// written or none of it: a payment that failed at the terminal leaves no
// invoice and no gap in the series.
//
// FreeSMS is not a cash register and does not say a shop needs none.
// Whether a cash invoice keeps a sale paid in person out of the cash
// register rules is a question for the shop's adviser, per way of taking
// money (gate G2, #221): the shop records its payment flows and whether
// each has been checked, and the owner sees what has been taken in person
// this financial year against the threshold the shop sets.

// CashMethods are the ways a customer pays in person at the counter.
var CashMethods = []string{"cash", "card", "swish"}

// CashPayment is one way a cash invoice is paid: cash and card on one
// invoice is two. An amount of zero is the rest -- in cash rounded to the
// krona, the difference kept as rounding.
type CashPayment struct {
	Method      string
	AmountMinor int64
	Reference   string
}

// CashSale is how an invoice issued at payment is paid, and through which
// of the shop's payment flows. Named, a flow's way of paying fills in a
// payment that names none, and a payment that names another is refused. Not named, the sale is put through the shop's
// one flow for the way it was paid, when there is exactly one: the
// register says that is how the shop takes money that way. With none, or
// several and none chosen, it is through no flow.
type CashSale struct {
	Payments []CashPayment
	FlowID   string
}

// PaidOnIssue is what a cash invoice says about its payment.
type PaidOnIssue struct {
	At   time.Time
	With []PaidWith
	Flow string
}

// PaidWith is one way it was paid.
type PaidWith struct {
	PaymentID     string `json:"payment_id"`
	Method        string `json:"method"`
	AmountMinor   int64  `json:"amount_minor"`
	RoundingMinor int64  `json:"rounding_minor"`
	Reference     string `json:"reference,omitempty"`
}

var (
	errCashMethod = fmt.Errorf("%w: a cash invoice is paid in cash, by card or by Swish at the counter", ErrInvalid)
	// errCashFlowMethod is a payment through a flow paid another way than
	// the flow takes money: recorded, it would put the sale through a flow
	// it did not go through.
	errCashFlowMethod = fmt.Errorf("%w: a payment through a flow is paid the way the flow takes money", ErrInvalid)
	errCashSplit      = fmt.Errorf("%w: the payments add up to more or less than the invoice; only the last may be the rest", ErrInvalid)
	// errCashInvoicePayment is a cash invoice's payment reversed.
	errCashInvoicePayment = fmt.Errorf("%w: that payment is how a cash invoice was paid on issue, and stands; correct the invoice with a credit note", ErrInvalid)
)

func checkCashSale(s CashSale) error {
	if len(s.Payments) == 0 || len(s.Payments) > 5 {
		return fmt.Errorf("%w: say how it is paid, in up to five parts", ErrInvalid)
	}
	if s.FlowID != "" && !looksLikeUUID(s.FlowID) {
		return ErrNotFound
	}
	for i, p := range s.Payments {
		known := p.Method == "" && s.FlowID != ""
		for _, m := range CashMethods {
			known = known || p.Method == m
		}
		switch {
		case !known:
			return errCashMethod
		case p.AmountMinor < 0, p.AmountMinor == 0 && i != len(s.Payments)-1:
			return errCashSplit
		case utf8.RuneCountInString(strings.TrimSpace(p.Reference)) > 100:
			return fmt.Errorf("%w: a reference is longer than it can be", ErrInvalid)
		}
	}
	return nil
}

// IssueAndPay issues a job's invoice and records it paid at the counter, in
// one transaction: the front desk's, as invoicing and payments are.
func IssueAndPay(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, workOrderID string, sale CashSale, opts ...IssueOption) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if err := checkCashSale(sale); err != nil {
		return Invoice{}, err
	}
	var o issueOptions
	for _, opt := range opts {
		opt(&o)
	}
	var inv Invoice
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if inv, err = issueTx(ctx, tx, scope, workOrderID, o); err != nil {
			return err
		}
		if err := payOnIssueTx(ctx, tx, scope, inv, sale); err != nil {
			return err
		}
		return closeIfFinishedTx(ctx, tx, workOrderID)
	})
	if err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

// IssueManualAndPay issues an invoice entered by hand and records it paid
// at the counter, in one transaction: a rental's extras at the return, a
// deposit's balance. Core's, as a manual invoice is.
func IssueManualAndPay(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, m ManualInvoice, sale CashSale) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if !looksLikeUUID(m.CustomerID) {
		return Invoice{}, ErrNotFound
	}
	if err := checkCashSale(sale); err != nil {
		return Invoice{}, err
	}
	var inv Invoice
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if inv, err = issueSourcedTx(ctx, tx, scope, sourced{source: SourceManual, customerID: m.CustomerID}, m.Lines); err != nil {
			return err
		}
		return payOnIssueTx(ctx, tx, scope, inv, sale)
	})
	if err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

// payOnIssueTx records the payments of an invoice just issued, which settle
// it exactly, and that it was paid on issue.
func payOnIssueTx(ctx context.Context, tx pgx.Tx, scope access.Scope, inv Invoice, sale CashSale) error {
	payments := append([]CashPayment(nil), sale.Payments...)
	if sale.FlowID != "" {
		var method string
		err := tx.QueryRow(ctx, `SELECT method FROM payment_flows WHERE id = $1`, sale.FlowID).Scan(&method)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the payment flow: %w", err)
		}
		for i := range payments {
			switch {
			case payments[i].Method == "" && method == "other":
				return errCashMethod
			case payments[i].Method == "":
				payments[i].Method = method
			// A flow for something else names no way of paying, so says
			// nothing a payment's could contradict.
			case method != "other" && payments[i].Method != method:
				return errCashFlowMethod
			}
		}
	} else if one := payments[0].Method; func() bool {
		for _, p := range payments {
			if p.Method != one {
				return false
			}
		}
		return true
	}() {
		var ids []string
		rows, err := tx.Query(ctx, `SELECT id::text FROM payment_flows WHERE method = $1 LIMIT 2`, one)
		if err == nil {
			ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil {
			return fmt.Errorf("read the payment flows: %w", err)
		}
		if len(ids) == 1 {
			sale.FlowID = ids[0]
		}
	}
	left := inv.GrossMinor
	now := time.Now()
	var with []PaidWith
	for i, c := range payments {
		p := Payment{InvoiceID: inv.ID, Method: c.Method, PaidOn: now, Reference: strings.TrimSpace(c.Reference), AmountMinor: c.AmountMinor}
		if c.AmountMinor == 0 {
			p.AmountMinor = left
			if c.Method == "cash" {
				p.AmountMinor = money.CashRound(left)
				p.RoundingMinor = left - p.AmountMinor
			}
		}
		if p.AmountMinor <= 0 || p.Settles() > left || (i == len(payments)-1 && p.Settles() != left) {
			return errCashSplit
		}
		if err := insertPayment(ctx, tx, scope, &p); err != nil {
			return err
		}
		left -= p.Settles()
		with = append(with, PaidWith{PaymentID: p.ID, Method: p.Method, AmountMinor: p.AmountMinor,
			RoundingMinor: p.RoundingMinor, Reference: p.Reference})
	}
	raw, err := json.Marshal(with)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cash_invoices (invoice_id, shop_id, paid_with, flow_id) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`,
		inv.ID, scope.ShopID, raw, sale.FlowID); err != nil {
		return fmt.Errorf("record it paid on issue: %w", err)
	}
	return nil
}

// paidOnIssueTx reads what a cash invoice says about its payment; nil for
// any other invoice.
func paidOnIssueTx(ctx context.Context, tx pgx.Tx, invoiceID string) (*PaidOnIssue, error) {
	var p PaidOnIssue
	var raw []byte
	err := tx.QueryRow(ctx, `
		SELECT c.paid_at, c.paid_with, coalesce(f.name, '') FROM cash_invoices c
		LEFT JOIN payment_flows f ON f.id = c.flow_id WHERE c.invoice_id = $1`, invoiceID).Scan(&p.At, &raw, &p.Flow)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the cash invoice: %w", err)
	}
	if err := json.Unmarshal(raw, &p.With); err != nil {
		return nil, err
	}
	return &p, nil
}

// PaymentFlow is one way the shop takes money in person, and where its
// check against the cash register rules stands.
type PaymentFlow struct {
	ID        string
	Name      string
	Method    string
	Checked   bool
	Adviser   string
	CheckedOn time.Time
	Note      string
	// Cash invoices issued through it.
	Used int
}

// PaymentFlows lists the shop's payment flows: whoever runs the shop.
func PaymentFlows(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]PaymentFlow, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []PaymentFlow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = paymentFlowsTx(ctx, tx)
		return err
	})
	return out, err
}

func paymentFlowsTx(ctx context.Context, tx pgx.Tx) ([]PaymentFlow, error) {
	rows, err := tx.Query(ctx, `
		SELECT f.id::text, f.name, f.method, coalesce(c.status = 'checked', false), coalesce(c.adviser, ''),
		       coalesce(c.checked_on, '0001-01-01'::date), coalesce(c.note, ''),
		       (SELECT count(*) FROM cash_invoices i WHERE i.flow_id = f.id)::int
		FROM payment_flows f
		LEFT JOIN LATERAL (SELECT * FROM payment_flow_checks k WHERE k.flow_id = f.id ORDER BY k.recorded_at DESC, k.id DESC LIMIT 1) c ON true
		ORDER BY lower(f.name)`)
	if err != nil {
		return nil, fmt.Errorf("read payment flows: %w", err)
	}
	defer rows.Close()
	var out []PaymentFlow
	for rows.Next() {
		var f PaymentFlow
		if err := rows.Scan(&f.ID, &f.Name, &f.Method, &f.Checked, &f.Adviser, &f.CheckedOn, &f.Note, &f.Used); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CounterFlow is a payment flow as the counter chooses it for a cash
// invoice: its name and its way of paying, and nothing of its check.
type CounterFlow struct {
	ID     string
	Name   string
	Method string
}

// CounterPaymentFlows lists the flows a cash invoice can be paid through
// -- cash, card or Swish -- for the front desk and the rental desk, who
// issue one: so the invoice says which, and the owner sees the flows in
// use.
func CounterPaymentFlows(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]CounterFlow, error) {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []CounterFlow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, name, method FROM payment_flows WHERE method IN ('cash', 'card', 'swish') ORDER BY lower(name)`)
		if err != nil {
			return fmt.Errorf("read payment flows: %w", err)
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[CounterFlow])
		return err
	})
	return out, err
}

// AddPaymentFlow records a way the shop takes money in person, not yet
// checked. Whoever runs the shop.
func AddPaymentFlow(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, name, method string) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return "", fmt.Errorf("%w: name the flow, in 100 characters or fewer", ErrInvalid)
	}
	switch method {
	case "cash", "card", "swish", "other":
	default:
		return "", fmt.Errorf("%w: a flow takes cash, card, Swish or something else", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO payment_flows (shop_id, name, method, created_by) VALUES ($1, $2, $3, $4) RETURNING id`,
			scope.ShopID, name, method, scope.UserID).Scan(&id)
		if isUniqueViolation(err, "payment_flows_name_idx") {
			return fmt.Errorf("%w: there is a flow by that name already", ErrInvalid)
		}
		return err
	})
	return id, err
}

// CheckPaymentFlow records that the shop's adviser has checked a flow
// against the cash register rules, when and what they said; with checked
// false, the check withdrawn. A row each time. Whoever runs the shop.
func CheckPaymentFlow(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, flowID string, checked bool, adviser string, on time.Time, note string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(flowID) {
		return ErrNotFound
	}
	adviser, note = strings.TrimSpace(adviser), strings.TrimSpace(note)
	status := "withdrawn"
	var day any
	if checked {
		status = "checked"
		switch {
		case adviser == "" || utf8.RuneCountInString(adviser) > 200:
			return fmt.Errorf("%w: say who checked it", ErrInvalid)
		case on.IsZero() || on.After(time.Now().Add(24*time.Hour)):
			return fmt.Errorf("%w: say when it was checked", ErrInvalid)
		}
		day = on.Format("2006-01-02")
	}
	if utf8.RuneCountInString(note) > 1000 {
		return fmt.Errorf("%w: keep the note to 1000 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_flows WHERE id = $1)`, flowID).Scan(&found); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO payment_flow_checks (shop_id, flow_id, status, adviser, checked_on, note, recorded_by)
			VALUES ($1, $2, $3, nullif($4, ''), $5::date, nullif($6, ''), $7)`,
			scope.ShopID, flowID, status, adviser, day, note, scope.UserID)
		return err
	})
}

// InPersonTurnover is what was taken in person -- cash, card and Swish --
// so far this financial year, net of what was given back (inPersonSales
// says what counts), against the
// threshold the shop has set for the cash register rules; zero threshold
// when none is set.
type InPersonTurnover struct {
	Since          time.Time
	TakenMinor     int64
	ThresholdMinor int64
	YearStarts     int
}

// Over reports whether the threshold is set and reached.
func (t InPersonTurnover) Over() bool {
	return t.ThresholdMinor > 0 && t.TakenMinor >= t.ThresholdMinor
}

// InPersonTurnoverFor is the figure, for the owner alone.
func InPersonTurnoverFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (InPersonTurnover, error) {
	if !scope.Role.RunsTheShop() {
		return InPersonTurnover{}, access.ErrForbidden
	}
	var t InPersonTurnover
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		t.YearStarts = 1
		err := tx.QueryRow(ctx, `SELECT threshold_minor, year_starts FROM cash_register_settings`).Scan(&t.ThresholdMinor, &t.YearStarts)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read the setting: %w", err)
		}
		var zone string
		if err := tx.QueryRow(ctx, `SELECT timezone FROM shops LIMIT 1`).Scan(&zone); err != nil {
			return fmt.Errorf("read timezone: %w", err)
		}
		loc, err := time.LoadLocation(zone)
		if err != nil {
			loc = time.UTC
		}
		now := time.Now().In(loc)
		year := now.Year()
		if int(now.Month()) < t.YearStarts {
			year--
		}
		t.Since = time.Date(year, time.Month(t.YearStarts), 1, 0, 0, 0, 0, loc)
		return tx.QueryRow(ctx, inPersonSales, t.Since.Format("2006-01-02")).Scan(&t.TakenMinor)
	})
	return t, err
}

// inPersonSales is the figure the cash register threshold is compared with:
// sales taken in person since $1, and nothing else. The rule:
//
//   - An invoice payment in cash, by card or by Swish counts, on the day it
//     was paid. A payment by bank or any other way does not.
//   - A refund of such a payment counts against it, on the day it went
//     back, whichever way it went back.
//   - A payment reversed never happened: neither it nor its reversal
//     counts. The same for a refund reversed.
//   - A payout never counts: it is money out, not a sale.
//   - A deposit never counts while it is held, nor when it is refunded. It
//     counts as it is applied to an invoice, on the day it was applied, for
//     the part of the application its in-person receipts -- cash, card or
//     Swish, net of their refunds -- can cover, the deposit's earlier
//     applications taking theirs first. A deposit taken by bank and
//     applied is not a sale taken in person.
//   - Rounding is not money taken: the amounts, not what they settled.
//
// An application reversed, either half, does not count. In-person first
// for a deposit taken partly in person is the cautious reading: the figure
// is a warning against a threshold, and is better a little high than low.
const inPersonSales = `
	WITH live AS (
	  SELECT p.* FROM invoice_payments p
	  WHERE p.reverses_id IS NULL AND NOT EXISTS (SELECT 1 FROM invoice_payments x WHERE x.reverses_id = p.id)),
	sold AS (
	  SELECT id FROM live
	  WHERE invoice_id IS NOT NULL AND method IN ('cash', 'card', 'swish') AND refunds_id IS NULL AND amount_minor > 0),
	held AS (
	  SELECT x.deposit_id, sum(x.amount_minor) AS minor FROM live x LEFT JOIN invoice_payments r ON r.id = x.refunds_id
	  WHERE x.deposit_id IS NOT NULL
	    AND ((x.refunds_id IS NULL AND x.amount_minor > 0 AND x.method IN ('cash', 'card', 'swish'))
	         OR (x.refunds_id IS NOT NULL AND r.method IN ('cash', 'card', 'swish')))
	  GROUP BY x.deposit_id),
	applied AS (
	  SELECT i.amount_minor, i.paid_on, d.deposit_id,
	         coalesce(sum(i.amount_minor) OVER (PARTITION BY d.deposit_id ORDER BY i.recorded_at, i.id
	                                           ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0) AS before
	  FROM deposit_applications a JOIN live i ON i.id = a.invoice_payment_id JOIN live d ON d.id = a.deposit_payment_id)
	SELECT coalesce((SELECT sum(amount_minor) FROM live WHERE id IN (SELECT id FROM sold) AND paid_on >= $1::date), 0)
	     + coalesce((SELECT sum(amount_minor) FROM live WHERE refunds_id IN (SELECT id FROM sold) AND paid_on >= $1::date), 0)
	     + coalesce((SELECT sum(least(a.amount_minor, greatest(0, coalesce(h.minor, 0) - a.before)))
	                 FROM applied a LEFT JOIN held h ON h.deposit_id = a.deposit_id WHERE a.paid_on >= $1::date), 0)`

// SetCashRegisterThreshold sets the threshold and the financial year's
// first month. Whoever runs the shop.
func SetCashRegisterThreshold(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, thresholdMinor int64, yearStarts int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if thresholdMinor <= 0 || yearStarts < 1 || yearStarts > 12 {
		return fmt.Errorf("%w: a threshold above nothing, and a month from 1 to 12", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO cash_register_settings (shop_id, threshold_minor, year_starts, updated_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (shop_id) DO UPDATE SET threshold_minor = excluded.threshold_minor, year_starts = excluded.year_starts,
			       updated_at = clock_timestamp(), updated_by = excluded.updated_by`,
			scope.ShopID, thresholdMinor, yearStarts, scope.UserID)
		return err
	})
}

// uncheckedFlowsWaiting is the payment flows in use that nobody has had
// checked, for the owner's list; nothing for anybody else.
func uncheckedFlowsWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.RunsTheShop() {
		return nil, nil
	}
	var out []ActionItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		flows, err := paymentFlowsTx(ctx, tx)
		for _, f := range flows {
			if f.Used > 0 && !f.Checked {
				out = append(out, ActionItem{Kind: "flow_unchecked", SubjectKind: "payment_flows", Text: f.Name, Urgency: UrgencySoon})
			}
		}
		if err != nil {
			return err
		}
		unmatched, err := unmatchedPaymentsTx(ctx, tx)
		out = append(out, unmatched...)
		return err
	})
	return out, err
}
