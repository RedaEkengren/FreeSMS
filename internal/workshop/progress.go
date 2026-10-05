package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Progress answers the question a customer asks on the telephone: when is it
// ready? It puts together what was promised, the hours sold on the order and
// the time clocked against it -- three things the system kept and never
// related.
type Progress struct {
	// The labour lines' hours, whoever pays: warranty work takes as long as
	// work the customer pays for. Zero is no estimate, not an estimate of
	// nothing.
	EstimateMinutes int
	// Every technician's time on the order. A clock still running counts up
	// to AsOf.
	ClockedMinutes int
	Running        bool
	AsOf           time.Time

	PromisedAt *time.Time
	// Ready, invoiced, closed or cancelled: the work is over, and a car that
	// is done is not late however long ago it was promised.
	Finished bool
}

// Known reports whether there is an estimate to measure against.
func (p Progress) Known() bool { return p.EstimateMinutes > 0 }

// Percent of the estimate clocked. Not clamped: an order at 140 per cent is
// the one somebody needs to look at, and a full bar would hide it.
func (p Progress) Percent() int {
	if !p.Known() {
		return 0
	}
	return p.ClockedMinutes * 100 / p.EstimateMinutes
}

// Over reports whether more has been clocked than was sold.
func (p Progress) Over() bool { return p.Known() && p.ClockedMinutes > p.EstimateMinutes }

// LeftMinutes is what the estimate says is still to do, never below nothing.
func (p Progress) LeftMinutes() int {
	if left := p.EstimateMinutes - p.ClockedMinutes; left > 0 {
		return left
	}
	return 0
}

// Verdict compares the promise with the work: "late" when the promise has
// passed on an unfinished order, "at risk" when what is left will not fit
// before it -- or the estimate is already used up -- and "on time" otherwise.
// Empty when there is nothing to judge: nothing promised, the work finished,
// or no estimate to measure the rest by.
//
// A promise is not an estimate of completion. A car promised at 16:00 with
// two hours of work left at 15:00 is on time on the clock and late in fact;
// that is the case worth colouring.
func (p Progress) Verdict() string {
	if p.PromisedAt == nil || p.Finished {
		return ""
	}
	if p.AsOf.After(*p.PromisedAt) {
		return "late"
	}
	if !p.Known() {
		return ""
	}
	if p.Over() || p.AsOf.Add(time.Duration(p.LeftMinutes())*time.Minute).After(*p.PromisedAt) {
		return "at risk"
	}
	return "on time"
}

// Bar is the clocked share drawn against a scale that grows past the
// estimate: Fill is how much of the bar is clocked, Mark where the estimate
// sits, both in per cent of the bar's width. Under the estimate the mark is
// the end of the bar; over it, the mark moves left and the bar is full.
func (p Progress) Bar() (fill, mark int) {
	pct := p.Percent()
	scale := 100
	if pct > scale {
		scale = pct
	}
	return pct * 100 / scale, 100 * 100 / scale
}

// BarFill and BarMark are Bar for a template.
func (p Progress) BarFill() int { f, _ := p.Bar(); return f }
func (p Progress) BarMark() int { _, m := p.Bar(); return m }

// Hours renders minutes as hours with one decimal, for the page.
func Hours(minutes int) string { return fmt.Sprintf("%.1f", float64(minutes)/60) }

// progressColumns reads a work order's progress, aliased w, with the
// database's clock as AsOf so a running entry and the comparison agree.
const progressColumns = `
	coalesce((SELECT round(sum(l.quantity) * 60) FROM work_order_lines l
	          WHERE l.work_order_id = w.id AND l.kind = 'labour'), 0)::int,
	coalesce((SELECT round(sum(extract(epoch from coalesce(t.ended_at, now()) - t.started_at)) / 60)
	          FROM time_entries t WHERE t.work_order_id = w.id), 0)::int,
	EXISTS (SELECT 1 FROM time_entries t WHERE t.work_order_id = w.id AND t.ended_at IS NULL),
	now(), w.promised_at, w.state IN ('ready', 'invoiced', 'closed', 'cancelled')`

func (p *Progress) scanTargets() []any {
	return []any{&p.EstimateMinutes, &p.ClockedMinutes, &p.Running, &p.AsOf, &p.PromisedAt, &p.Finished}
}

// ProgressFor reads one order's progress. Any role: it is hours and a time,
// and a technician asked "when will it be done" needs it as much as anybody.
func ProgressFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (Progress, error) {
	var p Progress
	if !looksLikeUUID(jobID) {
		return p, ErrNotFound
	}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT`+progressColumns+` FROM work_orders w WHERE w.id = $1`, jobID).
			Scan(p.scanTargets()...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return p, err
}

// SetPromise records when the customer was told the car will be ready, or
// clears it with nil. The front desk's: it is a conversation with the
// customer. promised_at was read by the board and written by nothing.
func SetPromise(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, at *time.Time) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE work_orders SET promised_at = $2
			WHERE id = $1 AND state NOT IN ('invoiced', 'closed', 'cancelled')`, jobID, at)
		if err != nil {
			return fmt.Errorf("set promise: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// MeterMax is the scale of the page's meter: the estimate, or what was
// clocked when that is more, so 140 per cent shows as a full meter past the
// estimate's mark rather than as a full meter and nothing else.
func (p Progress) MeterMax() int {
	if p.ClockedMinutes > p.EstimateMinutes {
		return p.ClockedMinutes
	}
	return p.EstimateMinutes
}

// ClockedHours, EstimateHours and LeftHours are the minutes as a page writes
// them.
func (p Progress) ClockedHours() string  { return Hours(p.ClockedMinutes) }
func (p Progress) EstimateHours() string { return Hours(p.EstimateMinutes) }
func (p Progress) LeftHours() string     { return Hours(p.LeftMinutes()) }
