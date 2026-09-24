// Package workshop reads and writes the shop's working data.
//
// Every function takes a scope and goes through database.InScope, so there is
// no way to reach a row without saying who is asking. Queries are plain SQL
// with the columns written out: a SELECT * would quietly start returning
// personal data the day somebody adds a column.
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
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned for a row that does not exist and for one that
// exists in another shop. The two must be indistinguishable, or the error
// itself tells the caller what is out there.
var ErrNotFound = errors.New("workshop: not found")

// Job is a work order as a technician needs to see it.
//
// There is no customer name, address, telephone number or email here. A
// technician needs the vehicle and the work; they do not need the customer's
// home address to change a clutch. The field is not hidden in a template -- it
// is not fetched, and a column nobody selects cannot leak.
type Job struct {
	ID           string
	VehicleID    string
	Number       int64
	State        string
	Complaint    string
	Registration string
	Make         string
	Model        string
	ModelYear    *int16
	OpenedAt     time.Time
	PromisedAt   *time.Time
	OdometerKm   *int64

	// Set when this technician has a running clock on the job.
	ClockRunning   bool
	ClockStartedAt *time.Time

	// True when anything has been clocked or priced on the order. Cancelling
	// such an order is refused, so the button is not offered.
	HasWork bool

	// Who the job belongs to today, and whether that is the caller. Not a
	// lock: whoever else has clocked time on it is just as real.
	AssignedTo   string
	AssignedToMe bool

	// Counts the front desk and the technician both need at a glance.
	OpenRequests int
	OpenFindings int
}

// Line is one line of a work order.
type Line struct {
	Position       int
	Kind           string
	Description    string
	QuantityMilli  int64
	CostBearer     string
	ApprovedAt     *time.Time
	UnitPriceMinor int64
	EstimatedMinor *int64
	VATRateBasis   int
}

// Quantity renders thousandths as a decimal, trimming the noise: 1000 is "1",
// 4500 is "4.5", 250 is "0.25".
func (l Line) Quantity() string {
	whole := l.QuantityMilli / 1000
	frac := l.QuantityMilli % 1000
	if frac < 0 {
		frac = -frac
	}
	if frac == 0 {
		return fmt.Sprintf("%d", whole)
	}
	out := fmt.Sprintf("%d.%03d", whole, frac)
	return strings.TrimRight(out, "0")
}

// TotalsFor prices a set of lines.
//
// The money lives in its own package so that the rounding rule has one home
// and one set of tests. This is the only adapter between a stored line and
// that calculation.
func TotalsFor(lines []Line) money.Totals {
	priced := make([]money.Line, 0, len(lines))
	for _, l := range lines {
		priced = append(priced, money.Line{
			QuantityMilli:     l.QuantityMilli,
			UnitPriceMinor:    l.UnitPriceMinor,
			VATRateBasis:      l.VATRateBasis,
			ChargedToCustomer: l.CostBearer == "customer",
		})
	}
	return money.Compute(priced)
}

// Net is what this line comes to before VAT.
func (l Line) Net() string { return money.Format(money.LineNet(l.QuantityMilli, l.UnitPriceMinor)) }

// Approved reports whether the customer agreed to this line.
//
// A line added after approval is not hidden and is not quietly included: it
// shows as unapproved, which is the only honest thing to put in front of
// somebody being asked to pay for it.
func (l Line) Approved() bool { return l.ApprovedAt != nil }

// PriceChanged reports whether the line is being charged at something other
// than what was quoted.
func (l Line) PriceChanged() bool {
	return l.EstimatedMinor != nil && *l.EstimatedMinor != l.UnitPriceMinor
}

// EstimatedPrice renders the quoted price, for a line whose price has moved.
func (l Line) EstimatedPrice() string {
	if l.EstimatedMinor == nil {
		return ""
	}
	return money.Format(*l.EstimatedMinor)
}

// Price renders what is being charged.
func (l Line) Price() string { return money.Format(l.UnitPriceMinor) }

const jobColumns = `
	w.id, v.id, w.number, w.state, coalesce(w.complaint, ''),
	coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''), v.model_year,
	w.opened_at, w.promised_at,
	(SELECT o.km FROM odometer_readings o
	  WHERE o.vehicle_id = v.id ORDER BY o.read_at DESC LIMIT 1) AS odometer_km,
	(SELECT t.started_at FROM time_entries t
	  WHERE t.work_order_id = w.id AND t.user_id = $1 AND t.ended_at IS NULL
	  LIMIT 1) AS clock_started_at,
	(EXISTS (SELECT 1 FROM time_entries t2      WHERE t2.work_order_id = w.id)
	      OR EXISTS (SELECT 1 FROM work_order_lines l WHERE l.work_order_id = w.id)) AS has_work,
	coalesce((SELECT p.display_name FROM users au
	           JOIN people p ON p.id = au.person_id
	          WHERE au.id = w.assigned_to), '') AS assigned_to,
	(w.assigned_to = $1) IS TRUE AS assigned_to_me,
	(SELECT count(*) FROM part_requests pr
	  WHERE pr.work_order_id = w.id AND pr.arrived_at IS NULL AND pr.cancelled_at IS NULL) AS open_requests,
	(SELECT count(*) FROM findings f
	  WHERE f.work_order_id = w.id AND f.handled_at IS NULL) AS open_findings`

const jobFrom = `
	FROM work_orders w
	JOIN vehicles v ON v.id = w.vehicle_id
	LEFT JOIN vehicle_registrations r
	       ON r.vehicle_id = v.id AND r.valid_to IS NULL`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.VehicleID, &j.Number, &j.State, &j.Complaint,
		&j.Registration, &j.Make, &j.Model, &j.ModelYear,
		&j.OpenedAt, &j.PromisedAt, &j.OdometerKm, &j.ClockStartedAt, &j.HasWork,
		&j.AssignedTo, &j.AssignedToMe, &j.OpenRequests, &j.OpenFindings)
	j.ClockRunning = j.ClockStartedAt != nil
	return j, err
}

// OpenJobs lists what is in the shop and not finished, newest first.
func OpenJobs(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Job, error) {
	var jobs []Job
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT`+jobColumns+jobFrom+`
			WHERE w.state NOT IN ('closed', 'cancelled')
			-- Mine first. "What am I doing today" is the question a
			-- technician opens this screen to answer, and a list of every job
			-- in the shop does not answer it.
			ORDER BY (w.assigned_to = $1) IS NOT TRUE,
			         w.promised_at NULLS LAST, w.opened_at`, scope.UserID)
		if err != nil {
			return fmt.Errorf("list jobs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				return fmt.Errorf("scan job: %w", err)
			}
			jobs = append(jobs, j)
		}
		return rows.Err()
	})
	return jobs, err
}

// JobByID returns one job, or ErrNotFound.
//
// Row level security means a work order in another shop simply is not there,
// so the caller gets the same answer for "does not exist" and "not yours"
// without having to remember to check.
func JobByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Job, []Line, error) {
	var job Job
	var lines []Line

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT`+jobColumns+jobFrom+` WHERE w.id = $2`, scope.UserID, id)
		j, err := scanJob(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read job: %w", err)
		}
		job = j

		rows, err := tx.Query(ctx, `
			SELECT position, kind, description,
			       -- Thousandths, taken as an integer. quantity is
			       -- numeric(12,3), so this is exact; reading it as a float
			       -- would put a floating point value in the middle of a
			       -- calculation the rest of this system avoids them in.
			       (quantity * 1000)::bigint,
			       cost_bearer, approved_at,
			       unit_price_minor, estimated_unit_price_minor, vat_rate_bp
			FROM work_order_lines WHERE work_order_id = $1 ORDER BY position`, id)
		if err != nil {
			return fmt.Errorf("read lines: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l Line
			if err := rows.Scan(&l.Position, &l.Kind, &l.Description, &l.QuantityMilli,
				&l.CostBearer, &l.ApprovedAt, &l.UnitPriceMinor, &l.EstimatedMinor,
				&l.VATRateBasis); err != nil {
				return fmt.Errorf("scan line: %w", err)
			}
			lines = append(lines, l)
		}
		return rows.Err()
	})
	if err != nil {
		return Job{}, nil, err
	}
	return job, lines, nil
}

// ClockIn starts the caller's clock on a job.
//
// A unique index allows one running entry per user, so a second clock-in is
// refused by the database rather than by a check that can be raced. Stopping
// whatever was running first is deliberate: a technician who moves to another
// car has stopped working on the first one, and asking them to remember two
// taps in a workshop is how hours go missing.
func ClockIn(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// Closes whatever was running, and flags it when the result is longer
		// than anybody works in one go. Silently closing a sixteen-hour entry
		// hides a payroll dispute rather than settling one.
		if err := closeRunning(ctx, tx, scope.UserID); err != nil {
			return err
		}

		// Picking a job up is what assigns it, when nobody has it. Asking
		// somebody to assign themselves before starting is a step that gets
		// skipped, and then the column is empty and useless.
		if _, err := tx.Exec(ctx,
			`UPDATE work_orders SET assigned_to = $1 WHERE id = $2 AND assigned_to IS NULL`,
			scope.UserID, jobID); err != nil {
			return fmt.Errorf("assign job: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO time_entries (shop_id, work_order_id, user_id, started_at)
			SELECT $1, w.id, $2, now() FROM work_orders w WHERE w.id = $3`,
			scope.ShopID, scope.UserID, jobID)
		if err != nil {
			return fmt.Errorf("clock in: %w", err)
		}
		// No row inserted means the work order was not visible, which is the
		// same answer as not existing.
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}

		// Moving to a job implies work has started on it -- but through the
		// state machine, not a bare UPDATE. A second place deciding what is
		// legal is how a system ends up with two answers.
		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, jobID).Scan(&state); err != nil {
			return fmt.Errorf("read state: %w", err)
		}
		if state == StateApproved || state == StateAwaitingParts {
			return setStateTx(ctx, tx, jobID, StateInProgress)
		}
		return nil
	})
}

// ClockOut stops the caller's running clock.
func ClockOut(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE time_entries SET ended_at = now()
			 WHERE user_id = $1 AND work_order_id = $2 AND ended_at IS NULL`,
			scope.UserID, jobID)
		return err
	})
}

// ResolveShop decides which shop this installation serves.
//
// Multi-tenancy is in the schema, but the normal deployment is one workshop
// running its own copy. When configured is empty and exactly one shop exists,
// that is the answer. More than one and the installation has to say which,
// because guessing would silently serve the wrong shop's data.
//
// This runs before any scope exists -- it is what establishes one -- which is
// why shops is the one table the schema owner may read unscoped.
func ResolveShop(ctx context.Context, pool *pgxpool.Pool, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	rows, err := pool.Query(ctx, `SELECT id FROM shops ORDER BY created_at LIMIT 2`)
	if err != nil {
		return "", fmt.Errorf("list shops: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("scan shop: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(ids) {
	case 0:
		return "", errors.New("workshop: no shop exists yet; the setup page creates one")
	case 1:
		return ids[0], nil
	default:
		return "", errors.New("workshop: more than one shop exists; set SHOP_ID to say which this installation serves")
	}
}
