package workshop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Closing the day.
//
// The day's figures are read from the documents and entries that already exist
// -- nothing is stored but the comparisons a person made and the sign-off they
// gave -- and every figure can be followed to the document or entry it is made
// of. Sales (what was invoiced and credited) are kept apart from cash movement
// (what was recorded as received and paid out), and apart from what is still
// owed. A payment recorded in FreeSMS is a statement by whoever recorded it; it
// is not the bank saying so, and nothing here infers settlement from it.
//
// A day is the shop's local day. A document belongs to the day it was issued,
// an entry to the day it says the money moved, so a payment recorded a week late
// changes that earlier day, and a sign-off given before then says it is stale.

// DayDocument is an invoice or credit note issued on the day.
type DayDocument struct {
	ID        string
	Reference string
	Customer  string
	Credit    bool
	NetMinor  int64
	VATMinor  int64
	Gross     int64
	Exported  bool
}

// DayEntry is a payment, a payout or a reversal on the day.
type DayEntry struct {
	ID        string
	Reference string // P-12
	Invoice   string // the invoice's reference
	InvoiceID string
	Kind      string // "payment", "refund" or "reversal"
	Amount    int64
	Rounding  int64
	Recorded  time.Time
	Exported  bool
}

// MethodFigures is what came and went by one way of paying.
type MethodFigures struct {
	Method   string
	Entries  []DayEntry
	Received int64
	// Money paid out as refunds, as a positive sum.
	PaidOut int64
	// Entries that undo an earlier entry, signed: a mistaken payment reversed
	// is negative.
	Reversed int64
	Rounding int64
	// What the other side said, if somebody compared.
	Compared    bool
	External    int64
	ComparedBy  string
	ComparedAt  time.Time
	Explanation string
}

// Movement is the net money recorded for the method that day: what came in,
// less what was paid out, less what was reversed.
func (m MethodFigures) Movement() int64 { return m.Received - m.PaidOut + m.Reversed }

// Difference is the other side's figure against ours.
func (m MethodFigures) Difference() int64 {
	if !m.Compared {
		return 0
	}
	return m.External - m.Movement()
}

// DayExceptions are the things that should be put right before the day is
// signed off.
type DayExceptions struct {
	RunningClocks    []string
	ReadyNotInvoiced []string
	UnpricedTakeOuts []string
	Undelivered      []Undelivered
	RefundTasks      int
}

// Count is how many there are.
func (e DayExceptions) Count() int {
	return len(e.RunningClocks) + len(e.ReadyNotInvoiced) + len(e.UnpricedTakeOuts) + len(e.Undelivered) + e.RefundTasks
}

// DayReview is a sign-off.
type DayReview struct {
	Version  int
	By       string
	At       time.Time
	Note     string
	Open     int
	Invoiced int64
	Credited int64
	Received int64
	PaidOut  int64
	// What the fingerprint was when it was given.
	Fingerprint string
}

// DayExportInfo says where the day's documents went.
type DayExportInfo struct {
	DocsExported, DocsNot       int
	EntriesExported, EntriesNot int
}

// DayFigures is the whole of one day.
type DayFigures struct {
	Day       time.Time
	Documents []DayDocument
	// Sales.
	InvoicedNet, InvoicedVAT, InvoicedGross int64
	CreditedNet, CreditedVAT, CreditedGross int64
	Methods                                 []MethodFigures
	// Still owed, now, by customers; and owed back to them.
	OwedToUs, OwedBack int64
	Exceptions         DayExceptions
	Exports            DayExportInfo
	Review             *DayReview
	// The sign-off was given and the day has changed since.
	Stale       bool
	Fingerprint string
}

// NetSales is invoiced less credited, excluding VAT.
func (d DayFigures) NetSales() int64 { return d.InvoicedNet + d.CreditedNet }

// Received is all money recorded as arrived, by every method.
func (d DayFigures) Received() (n int64) {
	for _, m := range d.Methods {
		n += m.Received
	}
	return
}

// PaidOut is all money recorded as paid out as refunds.
func (d DayFigures) PaidOut() (n int64) {
	for _, m := range d.Methods {
		n += m.PaidOut
	}
	return
}

func parseDay(day string) (string, error) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return "", fmt.Errorf("%w: that is not a day", ErrInvalid)
	}
	return t.Format("2006-01-02"), nil
}

// DayFor reads one day, given as 2006-01-02 in the shop's own calendar.
func DayFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, day string) (DayFigures, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return DayFigures{}, access.ErrForbidden
	}
	day, err := parseDay(day)
	if err != nil {
		return DayFigures{}, err
	}
	var out DayFigures
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = dayTx(ctx, tx, day)
		return err
	})
	return out, err
}

func dayTx(ctx context.Context, tx pgx.Tx, day string) (DayFigures, error) {
	var d DayFigures
	var zone string
	if err := tx.QueryRow(ctx, `SELECT timezone FROM shops LIMIT 1`).Scan(&zone); err != nil {
		return d, fmt.Errorf("read zone: %w", err)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	t, _ := time.ParseInLocation("2006-01-02", day, loc)
	d.Day = t

	// Sales: the documents issued on the day, by the shop's calendar.
	rows, err := tx.Query(ctx, `
		SELECT id::text, series || '-' || number, customer_name, credit_of_id IS NOT NULL, net_minor, vat_minor, gross_minor,
		       accounting_export_id IS NOT NULL
		FROM invoices WHERE (issued_at AT TIME ZONE $1)::date = $2::date ORDER BY series, number`, zone, day)
	if err != nil {
		return d, fmt.Errorf("read documents: %w", err)
	}
	for rows.Next() {
		var x DayDocument
		if err := rows.Scan(&x.ID, &x.Reference, &x.Customer, &x.Credit, &x.NetMinor, &x.VATMinor, &x.Gross, &x.Exported); err != nil {
			rows.Close()
			return d, fmt.Errorf("scan document: %w", err)
		}
		d.Documents = append(d.Documents, x)
		if x.Credit {
			d.CreditedNet += x.NetMinor
			d.CreditedVAT += x.VATMinor
			d.CreditedGross += x.Gross
		} else {
			d.InvoicedNet += x.NetMinor
			d.InvoicedVAT += x.VATMinor
			d.InvoicedGross += x.Gross
		}
		if x.Exported {
			d.Exports.DocsExported++
		} else {
			d.Exports.DocsNot++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}

	// Cash movement: entries dated the day, by the way of paying.
	pay, err := tx.Query(ctx, `
		SELECT p.id::text, p.series || '-' || p.number, i.series || '-' || i.number, i.id::text, p.method, p.amount_minor, p.rounding_minor,
		       p.recorded_at, p.accounting_export_id IS NOT NULL, p.reverses_id IS NOT NULL
		FROM invoice_payments p JOIN invoices i ON i.id = p.invoice_id
		WHERE p.paid_on = $1::date ORDER BY p.method, p.number`, day)
	if err != nil {
		return d, fmt.Errorf("read payments: %w", err)
	}
	byMethod := map[string]*MethodFigures{}
	for pay.Next() {
		var e DayEntry
		var method string
		var reversal bool
		if err := pay.Scan(&e.ID, &e.Reference, &e.Invoice, &e.InvoiceID, &method, &e.Amount, &e.Rounding, &e.Recorded, &e.Exported, &reversal); err != nil {
			pay.Close()
			return d, fmt.Errorf("scan payment: %w", err)
		}
		m := byMethod[method]
		if m == nil {
			m = &MethodFigures{Method: method}
			byMethod[method] = m
		}
		switch {
		case reversal:
			e.Kind = "reversal"
			m.Reversed += e.Amount
		case e.Amount < 0:
			e.Kind = "refund"
			m.PaidOut += -e.Amount
		default:
			e.Kind = "payment"
			m.Received += e.Amount
		}
		m.Rounding += e.Rounding
		m.Entries = append(m.Entries, e)
		if e.Exported {
			d.Exports.EntriesExported++
		} else {
			d.Exports.EntriesNot++
		}
	}
	pay.Close()
	if err := pay.Err(); err != nil {
		return d, err
	}

	// What people compared the methods with, latest per method.
	cmp, err := tx.Query(ctx, `
		SELECT DISTINCT ON (s.method) s.method, s.external_minor, coalesce(s.explanation, ''), coalesce(p.display_name, ''), s.at
		FROM day_settlements s LEFT JOIN users u ON u.id = s.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE s.day = $1::date ORDER BY s.method, s.at DESC, s.id`, day)
	if err != nil {
		return d, fmt.Errorf("read comparisons: %w", err)
	}
	for cmp.Next() {
		var method, expl, by string
		var ext int64
		var at time.Time
		if err := cmp.Scan(&method, &ext, &expl, &by, &at); err != nil {
			cmp.Close()
			return d, fmt.Errorf("scan comparison: %w", err)
		}
		m := byMethod[method]
		if m == nil {
			m = &MethodFigures{Method: method}
			byMethod[method] = m
		}
		m.Compared, m.External, m.Explanation, m.ComparedBy, m.ComparedAt = true, ext, expl, by, at
	}
	cmp.Close()
	if err := cmp.Err(); err != nil {
		return d, err
	}
	var methods []string
	for k := range byMethod {
		methods = append(methods, k)
	}
	sort.Strings(methods)
	for _, k := range methods {
		d.Methods = append(d.Methods, *byMethod[k])
	}

	// What is owed, now.
	if err := tx.QueryRow(ctx, `
		WITH open AS (
		  SELECT i.gross_minor
		         + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = i.id), 0)
		         - coalesce((SELECT sum(p.amount_minor + p.rounding_minor) FROM invoice_payments p WHERE p.invoice_id = i.id), 0) AS owed
		  FROM invoices i WHERE i.credit_of_id IS NULL)
		SELECT coalesce(sum(owed) FILTER (WHERE owed > 0), 0), coalesce(-sum(owed) FILTER (WHERE owed < 0), 0) FROM open`).Scan(&d.OwedToUs, &d.OwedBack); err != nil {
		return d, fmt.Errorf("read what is owed: %w", err)
	}

	if d.Exceptions, err = exceptionsTx(ctx, tx); err != nil {
		return d, err
	}
	if d.Exceptions.Undelivered, err = undeliveredTx(ctx, tx, time.Now().AddDate(0, 0, -60)); err != nil {
		return d, err
	}

	d.Fingerprint = fingerprint(d)
	var rev DayReview
	err = tx.QueryRow(ctx, `
		SELECT r.version, coalesce(p.display_name, ''), r.at, coalesce(r.note, ''), r.open_exceptions,
		       r.invoiced_gross_minor, r.credited_gross_minor, r.received_minor, r.paid_out_minor, r.fingerprint
		FROM day_reviews r LEFT JOIN users u ON u.id = r.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE r.day = $1::date ORDER BY r.version DESC LIMIT 1`, day).Scan(
		&rev.Version, &rev.By, &rev.At, &rev.Note, &rev.Open, &rev.Invoiced, &rev.Credited, &rev.Received, &rev.PaidOut, &rev.Fingerprint)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return d, fmt.Errorf("read review: %w", err)
	default:
		d.Review = &rev
		d.Stale = rev.Fingerprint != d.Fingerprint
	}
	return d, nil
}

// fingerprint is a hash of the documents and entries a day is made of. It
// changes when any of them is added or when an amount in one changes, which is
// what a sign-off must notice.
func fingerprint(d DayFigures) string {
	h := sha256.New()
	for _, x := range d.Documents {
		fmt.Fprintf(h, "d|%s|%d|%d\n", x.ID, x.Gross, x.NetMinor)
	}
	for _, m := range d.Methods {
		for _, e := range m.Entries {
			fmt.Fprintf(h, "e|%s|%s|%d|%d\n", e.ID, m.Method, e.Amount, e.Rounding)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func exceptionsTx(ctx context.Context, tx pgx.Tx) (DayExceptions, error) {
	var e DayExceptions
	strs := func(q string) ([]string, error) {
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("read exceptions: %w", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, fmt.Errorf("scan exception: %w", err)
			}
			out = append(out, s)
		}
		return out, rows.Err()
	}
	var err error
	if e.RunningClocks, err = strs(`
		SELECT coalesce(p.display_name, '') || ' on #' || w.number || ' since ' || to_char(t.started_at AT TIME ZONE (SELECT timezone FROM shops LIMIT 1), 'Mon DD HH24:MI')
		FROM time_entries t JOIN work_orders w ON w.id = t.work_order_id
		LEFT JOIN users u ON u.id = t.user_id LEFT JOIN people p ON p.id = u.person_id
		WHERE t.ended_at IS NULL ORDER BY t.started_at`); err != nil {
		return e, err
	}
	if e.ReadyNotInvoiced, err = strs(`
		SELECT '#' || w.number || ' ' || coalesce(r.registration, '') || ' (' || w.state || ')'
		FROM work_orders w LEFT JOIN vehicle_registrations r ON r.vehicle_id = w.vehicle_id AND r.valid_to IS NULL
		WHERE (w.state = 'ready'
		       OR (w.state = 'declined' AND EXISTS (SELECT 1 FROM work_order_lines l WHERE l.work_order_id = w.id AND l.performed_at IS NOT NULL AND l.withdrawn_at IS NULL)))
		  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.work_order_id = w.id)
		ORDER BY w.number`); err != nil {
		return e, err
	}
	if e.UnpricedTakeOuts, err = strs(`
		SELECT '#' || w.number || ' ' || p.name || ' x ' || trim(trailing '.' from trim(trailing '0' from (taken - priced)::text))
		FROM (
		  SELECT m.work_order_id, m.part_id, -sum(m.quantity) AS taken,
		         coalesce((SELECT sum(l.quantity) FROM work_order_lines l
		                    WHERE l.work_order_id = m.work_order_id AND l.part_id = m.part_id AND l.kind = 'part'
		                      AND l.declined_at IS NULL AND l.withdrawn_at IS NULL), 0) AS priced
		  FROM stock_movements m WHERE m.kind IN ('consumed', 'put_back') AND m.work_order_id IS NOT NULL
		  GROUP BY m.work_order_id, m.part_id) x
		JOIN work_orders w ON w.id = x.work_order_id JOIN parts p ON p.id = x.part_id
		WHERE taken > priced AND w.state NOT IN ('invoiced', 'closed', 'cancelled')
		ORDER BY w.number`); err != nil {
		return e, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*)::int FROM (
		  SELECT i.id FROM invoices i WHERE i.credit_of_id IS NULL
		    AND i.gross_minor
		        + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = i.id), 0)
		        - coalesce((SELECT sum(p.amount_minor + p.rounding_minor) FROM invoice_payments p WHERE p.invoice_id = i.id), 0) < 0) x`).Scan(&e.RefundTasks); err != nil {
		return e, fmt.Errorf("count refund tasks: %w", err)
	}
	return e, nil
}

// RecordSettlement writes down what the other side says a way of paying came
// to on a day -- the bank statement, the terminal report, the till count --
// next to what FreeSMS has recorded. When they differ, a reason is required.
// FreeSMS never assumes the bank agrees.
func RecordSettlement(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, day, method string, externalMinor int64, explanation string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	day, err := parseDay(day)
	if err != nil {
		return err
	}
	if !knownMethod(method) {
		return fmt.Errorf("%w: %q is not a way of paying", ErrInvalid, method)
	}
	explanation = strings.TrimSpace(explanation)
	if len([]rune(explanation)) > 500 {
		return fmt.Errorf("%w: keep the explanation to 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := dayTx(ctx, tx, day)
		if err != nil {
			return err
		}
		var ours int64
		for _, m := range d.Methods {
			if m.Method == method {
				ours = m.Movement()
			}
		}
		if externalMinor != ours && explanation == "" {
			return fmt.Errorf("%w: the figures differ; say why", ErrInvalid)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO day_settlements (shop_id, day, method, external_minor, recorded_minor, explanation, by_user)
			VALUES ($1, $2::date, $3, $4, $5, nullif($6, ''), $7)`, scope.ShopID, day, method, externalMinor, ours, explanation, scope.UserID)
		if err != nil {
			return fmt.Errorf("record the comparison: %w", err)
		}
		return nil
	})
}

// ReviewDay signs a day off: who looked, when, and how many things were still
// open. It is the owner's. It does not close the day for good; the ledgers can
// still be corrected, and a sign-off the day has outgrown says so.
func ReviewDay(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, day, note string) (DayReview, error) {
	if !scope.Role.RunsTheShop() {
		return DayReview{}, access.ErrForbidden
	}
	day, err := parseDay(day)
	if err != nil {
		return DayReview{}, err
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 500 {
		return DayReview{}, fmt.Errorf("%w: keep the note to 500 characters", ErrInvalid)
	}
	var out DayReview
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := dayTx(ctx, tx, day)
		if err != nil {
			return err
		}
		if d.Day.After(time.Now().Add(24 * time.Hour)) {
			return fmt.Errorf("%w: a day that has not happened cannot be reviewed", ErrInvalid)
		}
		// Reviewing the same day twice with nothing changed is the same sign-off.
		if d.Review != nil && !d.Stale {
			out = *d.Review
			return nil
		}
		version := 1
		if d.Review != nil {
			version = d.Review.Version + 1
		}
		open := d.Exceptions.Count()
		if _, err := tx.Exec(ctx, `
			INSERT INTO day_reviews (shop_id, day, version, fingerprint, invoiced_gross_minor, credited_gross_minor, received_minor, paid_out_minor, open_exceptions, note, by_user)
			VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), $11)`,
			scope.ShopID, day, version, d.Fingerprint, d.InvoicedGross, d.CreditedGross, d.Received(), d.PaidOut(), open, note, scope.UserID); err != nil {
			return fmt.Errorf("record the review: %w", err)
		}
		out = DayReview{Version: version, At: time.Now(), Note: note, Open: open, Invoiced: d.InvoicedGross, Credited: d.CreditedGross,
			Received: d.Received(), PaidOut: d.PaidOut(), Fingerprint: d.Fingerprint}
		return nil
	})
	return out, err
}
