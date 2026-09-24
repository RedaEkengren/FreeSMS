package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxPlausibleShift is how long a single clocked entry can be before it is
// treated as a mistake rather than a day's work.
//
// Ten hours is long for one job and short for a forgotten clock. The point is
// not to be exactly right: it is that a sixteen-hour entry must be noticed by
// the system rather than by whoever is doing the wages on Friday.
const maxPlausibleShift = 10 * time.Hour

// TimeEntry is one stretch of clocked work.
type TimeEntry struct {
	ID           string
	WorkOrderID  string
	Number       int64
	Registration string
	UserName     string
	StartedAt    time.Time
	EndedAt      *time.Time
	Flagged      bool
	Note         string

	CorrectedBy   string
	CorrectedAt   *time.Time
	OriginalStart *time.Time
	OriginalEnd   *time.Time
}

// Running reports whether the clock is still going.
func (t TimeEntry) Running() bool { return t.EndedAt == nil }

// Duration is how long it has run, so far or in total.
func (t TimeEntry) Duration() time.Duration {
	if t.EndedAt == nil {
		return time.Since(t.StartedAt)
	}
	return t.EndedAt.Sub(t.StartedAt)
}

// Hours renders the duration the way a workshop writes it: 1.5, not 1h30m.
func (t TimeEntry) Hours() string {
	return fmt.Sprintf("%.2f", t.Duration().Hours())
}

// Implausible reports whether this entry is longer than anybody works in one
// go, which almost always means a clock that was never stopped.
func (t TimeEntry) Implausible() bool { return t.Duration() > maxPlausibleShift }

// Corrected reports whether a supervisor has adjusted it.
func (t TimeEntry) Corrected() bool { return t.CorrectedAt != nil }

// closeRunning stops whatever clock a user has going, and flags it when the
// result is not a plausible stretch of work.
//
// Silently closing a sixteen-hour entry is the thing the flag exists to
// prevent: it hides a payroll dispute rather than settling one. Refusing to
// close it is no better -- the technician is standing at another car and needs
// to start. So it is closed, and it is marked, and somebody is asked.
func closeRunning(ctx context.Context, tx pgx.Tx, userID string) error {
	const q = `
		UPDATE time_entries
		   SET ended_at = now(),
		       flagged = (now() - started_at) > $2::interval,
		       note = CASE WHEN (now() - started_at) > $2::interval
		                   THEN concat_ws(' ', note,
		                        'Closed automatically when the next job was started; it had been running for '
		                        || round(extract(epoch from now() - started_at) / 3600)::text || ' hours.')
		                   ELSE note END
		 WHERE user_id = $1 AND ended_at IS NULL`
	_, err := tx.Exec(ctx, q, userID, fmt.Sprintf("%d seconds", int(maxPlausibleShift.Seconds())))
	if err != nil {
		return fmt.Errorf("stop running clock: %w", err)
	}
	return nil
}

// TimeFor lists the entries on one job.
func TimeFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]TimeEntry, error) {
	return timeEntries(ctx, pool, scope, `WHERE t.work_order_id = $1`, jobID)
}

// FlaggedTime lists entries somebody needs to look at.
//
// The front desk's list, because correcting somebody's hours is not the
// technician's own job -- the whole point of a correction is that a second
// person agreed to it.
func FlaggedTime(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]TimeEntry, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	return timeEntries(ctx, pool, scope,
		`WHERE t.flagged AND t.corrected_at IS NULL`)
}

// MyTime lists what the caller has clocked recently.
func MyTime(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, since time.Time) ([]TimeEntry, error) {
	return timeEntries(ctx, pool, scope,
		`WHERE t.user_id = $1 AND t.started_at >= $2`, scope.UserID, since)
}

func timeEntries(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, where string, args ...any) ([]TimeEntry, error) {
	var out []TimeEntry
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		q := `
			SELECT t.id, t.work_order_id, w.number, coalesce(r.registration, ''),
			       coalesce(p.display_name, ''), t.started_at, t.ended_at,
			       t.flagged, coalesce(t.note, ''),
			       coalesce(cp.display_name, ''), t.corrected_at,
			       t.corrected_from_started_at, t.corrected_from_ended_at
			FROM time_entries t
			JOIN work_orders w ON w.id = t.work_order_id
			JOIN vehicles v    ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r
			       ON r.vehicle_id = v.id AND r.valid_to IS NULL
			LEFT JOIN users u   ON u.id = t.user_id
			LEFT JOIN people p  ON p.id = u.person_id
			LEFT JOIN users cu  ON cu.id = t.corrected_by
			LEFT JOIN people cp ON cp.id = cu.person_id
			` + where + `
			ORDER BY t.started_at DESC
			LIMIT 200`
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("list time: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t TimeEntry
			if err := rows.Scan(&t.ID, &t.WorkOrderID, &t.Number, &t.Registration,
				&t.UserName, &t.StartedAt, &t.EndedAt, &t.Flagged, &t.Note,
				&t.CorrectedBy, &t.CorrectedAt, &t.OriginalStart, &t.OriginalEnd); err != nil {
				return fmt.Errorf("scan entry: %w", err)
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// CorrectTime adjusts an entry, keeping what it said before.
//
// The original is preserved rather than overwritten, because a correction
// somebody cannot see is indistinguishable from the hours having always been
// that. It is also the only defence the person who made the correction has.
func CorrectTime(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, entryID string, started, ended time.Time, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(entryID) {
		return ErrNotFound
	}
	if !ended.After(started) {
		return fmt.Errorf("%w: the end has to be after the start", ErrInvalid)
	}
	if ended.Sub(started) > maxPlausibleShift {
		return fmt.Errorf("%w: %.1f hours is longer than anybody works in one go",
			ErrInvalid, ended.Sub(started).Hours())
	}

	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// An entry already on an issued invoice must not move. The invoice is
		// immutable, so changing the hours underneath it would make the two
		// disagree with nothing to show which is right.
		var invoiced bool
		if err := tx.QueryRow(ctx, `
			SELECT exists(
			    SELECT 1 FROM time_entries t
			    JOIN invoices i ON i.work_order_id = t.work_order_id
			    WHERE t.id = $1)`, entryID).Scan(&invoiced); err != nil {
			return fmt.Errorf("check for an invoice: %w", err)
		}
		if invoiced {
			return fmt.Errorf("%w: this job has been invoiced, and the invoice cannot change", ErrInvalid)
		}

		const q = `
			UPDATE time_entries
			   SET corrected_from_started_at = coalesce(corrected_from_started_at, started_at),
			       corrected_from_ended_at   = coalesce(corrected_from_ended_at, ended_at),
			       started_at = $2,
			       ended_at   = $3,
			       corrected_by = $4,
			       corrected_at = now(),
			       flagged = false,
			       note = nullif(trim(concat_ws(' ', note, $5::text)), '')
			 WHERE id = $1`
		tag, err := tx.Exec(ctx, q, entryID, started, ended, scope.UserID, strings.TrimSpace(note))
		if err != nil {
			return fmt.Errorf("correct time: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ErrStillRunning is returned when a correction is attempted on a clock that
// has not been stopped.
var ErrStillRunning = errors.New("workshop: stop the clock before correcting it")

// ShopTimezone reads the shop's timezone.
//
// Stored times are UTC. This is for reading what a person typed and showing it
// back, which is the only place local time belongs.
func ShopTimezone(ctx context.Context, pool *pgxpool.Pool, shopID string) (string, error) {
	var tz string
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT timezone FROM shops WHERE id = $1`, shopID).Scan(&tz)
	})
	return tz, err
}
