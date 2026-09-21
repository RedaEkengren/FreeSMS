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
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
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
}

// Line is one line of a work order.
type Line struct {
	Position    int
	Kind        string
	Description string
	Quantity    float64
	CostBearer  string
	ApprovedAt  *time.Time
}

const jobColumns = `
	w.id, w.number, w.state, coalesce(w.complaint, ''),
	coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''), v.model_year,
	w.opened_at, w.promised_at,
	(SELECT o.km FROM odometer_readings o
	  WHERE o.vehicle_id = v.id ORDER BY o.read_at DESC LIMIT 1) AS odometer_km,
	(SELECT t.started_at FROM time_entries t
	  WHERE t.work_order_id = w.id AND t.user_id = $1 AND t.ended_at IS NULL
	  LIMIT 1) AS clock_started_at`

const jobFrom = `
	FROM work_orders w
	JOIN vehicles v ON v.id = w.vehicle_id
	LEFT JOIN vehicle_registrations r
	       ON r.vehicle_id = v.id AND r.valid_to IS NULL`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Number, &j.State, &j.Complaint,
		&j.Registration, &j.Make, &j.Model, &j.ModelYear,
		&j.OpenedAt, &j.PromisedAt, &j.OdometerKm, &j.ClockStartedAt)
	j.ClockRunning = j.ClockStartedAt != nil
	return j, err
}

// OpenJobs lists what is in the shop and not finished, newest first.
func OpenJobs(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Job, error) {
	var jobs []Job
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT`+jobColumns+jobFrom+`
			WHERE w.state NOT IN ('closed', 'cancelled')
			ORDER BY w.promised_at NULLS LAST, w.opened_at`, scope.UserID)
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
			SELECT position, kind, description, quantity, cost_bearer, approved_at
			FROM work_order_lines WHERE work_order_id = $1 ORDER BY position`, id)
		if err != nil {
			return fmt.Errorf("read lines: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l Line
			if err := rows.Scan(&l.Position, &l.Kind, &l.Description, &l.Quantity, &l.CostBearer, &l.ApprovedAt); err != nil {
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
		if _, err := tx.Exec(ctx,
			`UPDATE time_entries SET ended_at = now() WHERE user_id = $1 AND ended_at IS NULL`,
			scope.UserID); err != nil {
			return fmt.Errorf("stop running clock: %w", err)
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

		// Moving to a job implies work has started on it.
		_, err = tx.Exec(ctx,
			`UPDATE work_orders SET state = 'in_progress', updated_at = now()
			 WHERE id = $1 AND state IN ('approved', 'awaiting_parts')`, jobID)
		return err
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
		return "", errors.New("workshop: no shop exists yet; seed one with scripts/seed.sql")
	case 1:
		return ids[0], nil
	default:
		return "", errors.New("workshop: more than one shop exists; set SHOP_ID to say which this installation serves")
	}
}
