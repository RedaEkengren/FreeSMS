package workshop

import (
	"context"
	"fmt"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dashboard is the owner's screen.
//
// Deliberately small. The complaint about the commercial systems is not that
// they have too few reports; it is that there is too much interface. Every
// number here is one somebody would act on.
type Dashboard struct {
	From, To time.Time

	Invoices    int
	NetMinor    int64
	MeanMinor   int64
	MedianMinor int64

	HoursClocked float64
	HoursBilled  float64

	FindingsDecided  int
	FindingsApproved int

	Technicians []TechnicianTime
}

// Money renderers.
func (d Dashboard) Net() string    { return money.Format(d.NetMinor) }
func (d Dashboard) Mean() string   { return money.Format(d.MeanMinor) }
func (d Dashboard) Median() string { return money.Format(d.MedianMinor) }

// ApprovalRate is the share of decided inspection findings the customer said
// yes to, as a whole percentage.
func (d Dashboard) ApprovalRate() int {
	if d.FindingsDecided == 0 {
		return 0
	}
	return d.FindingsApproved * 100 / d.FindingsDecided
}

// HasFindings reports whether any inspection findings were decided at all, so
// a shop that has not started using inspections is not shown a nought.
func (d Dashboard) HasFindings() bool { return d.FindingsDecided > 0 }

// TechnicianTime is one person's hours over the period.
//
// Clocked and billed, both shown, rather than one efficiency figure. A single
// score becomes a stick, and the number that makes it look bad is often the
// job nobody wanted to give anybody else. The technician sees the same figures
// on their own hours screen.
type TechnicianTime struct {
	Name    string
	Clocked float64
	Billed  float64
}

// Difference is billed minus clocked, in hours.
func (t TechnicianTime) Difference() float64 { return t.Billed - t.Clocked }

// Summary reads the dashboard for a period.
//
// Invoices are counted by the day they were issued. An order opened in March
// and invoiced in April belongs to April, because that is when the money
// existed -- and the page says so rather than leaving somebody to work it out
// from a total that does not match their bookkeeping.
func Summary(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) (Dashboard, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Dashboard{}, access.ErrForbidden
	}

	d := Dashboard{From: from, To: to}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// Credit notes are included as the negatives they are. Leaving them
		// out would make a month look better than the books do, which is the
		// one direction a figure must never be wrong in.
		const totals = `
			SELECT count(*),
			       coalesce(sum(net_minor), 0),
			       coalesce(round(avg(net_minor)), 0),
			       coalesce(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY net_minor)), 0)
			FROM invoices
			WHERE issued_at >= $1 AND issued_at < $2`
		if err := tx.QueryRow(ctx, totals, from, to).Scan(
			&d.Invoices, &d.NetMinor, &d.MeanMinor, &d.MedianMinor); err != nil {
			return fmt.Errorf("read invoice totals: %w", err)
		}

		// Hours clocked against jobs in the period, and hours sold on labour
		// lines of the same jobs. Two different questions, and the gap between
		// them is the one an owner actually acts on.
		// A running entry counts up to now. Only finished ones meant that a
		// day in progress read as nothing, which is defensible for a closed
		// month and wrong for today -- and today is the period somebody
		// actually looks at. The figure changes while you watch it, which is
		// correct.
		const hours = `
			SELECT coalesce(sum(extract(epoch from (coalesce(t.ended_at, now()) - t.started_at)) / 3600), 0)
			FROM time_entries t
			WHERE t.started_at >= $1 AND t.started_at < $2`
		if err := tx.QueryRow(ctx, hours, from, to).Scan(&d.HoursClocked); err != nil {
			return fmt.Errorf("read clocked hours: %w", err)
		}

		const billed = `
			SELECT coalesce(sum(l.quantity), 0)
			FROM work_order_lines l
			JOIN invoices i ON i.work_order_id = l.work_order_id AND i.credit_of_id IS NULL
			WHERE l.kind = 'labour' AND l.cost_bearer = 'customer'
			  AND i.issued_at >= $1 AND i.issued_at < $2`
		if err := tx.QueryRow(ctx, billed, from, to).Scan(&d.HoursBilled); err != nil {
			return fmt.Errorf("read billed hours: %w", err)
		}

		// Only the latest answer per item counts: somebody who approved and
		// then declined has declined.
		const findings = `
			SELECT count(*) FILTER (WHERE d.decision IS NOT NULL),
			       count(*) FILTER (WHERE d.decision = 'approved')
			FROM inspection_items it
			JOIN LATERAL (
			    SELECT decision, decided_at FROM inspection_decisions
			    WHERE item_id = it.id ORDER BY decided_at DESC, id DESC LIMIT 1
			) d ON true
			WHERE d.decided_at >= $1 AND d.decided_at < $2`
		if err := tx.QueryRow(ctx, findings, from, to).Scan(
			&d.FindingsDecided, &d.FindingsApproved); err != nil {
			return fmt.Errorf("read findings: %w", err)
		}

		// Per person. A part-time technician and a full-time one are not
		// comparable on absolutes, so these are hours and not a ranking.
		const perTech = `
			SELECT coalesce(p.display_name, 'unknown'),
			       coalesce(sum(extract(epoch from (coalesce(t.ended_at, now()) - t.started_at)) / 3600), 0),
			       coalesce((
			           SELECT sum(l.quantity)
			           FROM work_order_lines l
			           JOIN time_entries t2 ON t2.line_id = l.id AND t2.user_id = t.user_id
			           WHERE l.kind = 'labour'
			             AND t2.started_at >= $1 AND t2.started_at < $2
			       ), 0)
			FROM time_entries t
			LEFT JOIN users u  ON u.id = t.user_id
			LEFT JOIN people p ON p.id = u.person_id
			WHERE t.started_at >= $1 AND t.started_at < $2
			GROUP BY t.user_id, p.display_name
			ORDER BY 2 DESC`
		rows, err := tx.Query(ctx, perTech, from, to)
		if err != nil {
			return fmt.Errorf("read per-technician hours: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var tt TechnicianTime
			if err := rows.Scan(&tt.Name, &tt.Clocked, &tt.Billed); err != nil {
				return fmt.Errorf("scan technician: %w", err)
			}
			d.Technicians = append(d.Technicians, tt)
		}
		return rows.Err()
	})
	return d, err
}
