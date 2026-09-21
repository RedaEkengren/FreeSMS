package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PartRequest is a technician asking for something so they can carry on.
type PartRequest struct {
	ID           string
	WorkOrderID  string
	Description  string
	RequestedBy  string
	RequestedAt  time.Time
	ArrivedAt    *time.Time
	CancelledAt  *time.Time
	Registration string
	Make         string
	Model        string
}

// Open reports whether this request is still waiting on somebody.
func (p PartRequest) Open() bool { return p.ArrivedAt == nil && p.CancelledAt == nil }

// Finding is something noticed under the car that somebody else should price.
type Finding struct {
	ID          string
	Note        string
	ReportedBy  string
	ReportedAt  time.Time
	HandledAt   *time.Time
	WorkOrderID string
}

// Handled reports whether the front desk has dealt with it.
func (f Finding) Handled() bool { return f.HandledAt != nil }

// RequestPart records what a technician needs and puts the job in
// awaiting_parts.
//
// Both together. Asking for a part and separately remembering to change the
// state is how a job sits in_progress with nobody on it, looking active.
func RequestPart(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, description string) error {
	if strings.TrimSpace(description) == "" {
		return fmt.Errorf("%w: say what is needed", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO part_requests (shop_id, work_order_id, description, requested_by)
			SELECT $1, w.id, $2, $3 FROM work_orders w WHERE w.id = $4`,
			scope.ShopID, strings.TrimSpace(description), scope.UserID, jobID)
		if err != nil {
			return fmt.Errorf("record the request: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}

		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, jobID).Scan(&state); err != nil {
			return fmt.Errorf("read state: %w", err)
		}
		if CanTransition(state, StateAwaitingParts) {
			return setStateTx(ctx, tx, jobID, StateAwaitingParts)
		}
		// Already waiting, or somewhere the move does not apply. The request
		// still stands.
		return nil
	})
}

// OpenPartRequests is the parts desk's list: what is being waited for.
func OpenPartRequests(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]PartRequest, error) {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []PartRequest
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT pr.id, pr.work_order_id, pr.description,
			       coalesce(p.display_name, ''), pr.requested_at,
			       coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, '')
			FROM part_requests pr
			JOIN work_orders w ON w.id = pr.work_order_id
			JOIN vehicles v    ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r
			       ON r.vehicle_id = v.id AND r.valid_to IS NULL
			LEFT JOIN users u  ON u.id = pr.requested_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE pr.arrived_at IS NULL AND pr.cancelled_at IS NULL
			  AND w.state NOT IN ('closed', 'cancelled')
			ORDER BY pr.requested_at`)
		if err != nil {
			return fmt.Errorf("list part requests: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p PartRequest
			if err := rows.Scan(&p.ID, &p.WorkOrderID, &p.Description, &p.RequestedBy,
				&p.RequestedAt, &p.Registration, &p.Make, &p.Model); err != nil {
				return fmt.Errorf("scan part request: %w", err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// MarkPartArrived closes a request and sends the job back to the technician.
//
// The state moves only when nothing else is still outstanding. A gearbox job
// waiting on three parts should not look ready to work on because one of them
// turned up.
func MarkPartArrived(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, requestID string) error {
	if !looksLikeUUID(requestID) {
		return ErrNotFound
	}
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var jobID string
		err := tx.QueryRow(ctx, `
			UPDATE part_requests SET arrived_at = now(), arrived_by = $1
			WHERE id = $2 AND arrived_at IS NULL AND cancelled_at IS NULL
			RETURNING work_order_id`, scope.UserID, requestID).Scan(&jobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("mark arrived: %w", err)
		}

		var outstanding bool
		if err := tx.QueryRow(ctx, `
			SELECT exists(SELECT 1 FROM part_requests
			              WHERE work_order_id = $1
			                AND arrived_at IS NULL AND cancelled_at IS NULL)`,
			jobID).Scan(&outstanding); err != nil {
			return fmt.Errorf("check remaining requests: %w", err)
		}
		if outstanding {
			return nil
		}

		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, jobID).Scan(&state); err != nil {
			return fmt.Errorf("read state: %w", err)
		}
		if state == StateAwaitingParts {
			return setStateTx(ctx, tx, jobID, StateInProgress)
		}
		// Declined or invoiced while the part was on its way. The part is
		// bought and the work is not happening; that is a return, and the
		// request is closed either way.
		return nil
	})
}

// PartRequestsFor lists a job's requests, arrived and outstanding.
func PartRequestsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]PartRequest, error) {
	var out []PartRequest
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT pr.id, pr.work_order_id, pr.description,
			       coalesce(p.display_name, ''), pr.requested_at, pr.arrived_at, pr.cancelled_at
			FROM part_requests pr
			LEFT JOIN users u  ON u.id = pr.requested_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE pr.work_order_id = $1
			ORDER BY pr.requested_at`, jobID)
		if err != nil {
			return fmt.Errorf("list requests: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p PartRequest
			if err := rows.Scan(&p.ID, &p.WorkOrderID, &p.Description, &p.RequestedBy,
				&p.RequestedAt, &p.ArrivedAt, &p.CancelledAt); err != nil {
				return fmt.Errorf("scan request: %w", err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// ReportFinding records something noticed under the car.
//
// Deliberately not a priced line. "The other track rod end is going too" is a
// fact for the front desk to turn into a quote; handing the technician the
// pricing form asks the wrong person the wrong question.
func ReportFinding(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, note string) error {
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("%w: say what you found", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO findings (shop_id, work_order_id, note, reported_by)
			SELECT $1, w.id, $2, $3 FROM work_orders w WHERE w.id = $4`,
			scope.ShopID, strings.TrimSpace(note), scope.UserID, jobID)
		if err != nil {
			return fmt.Errorf("record the finding: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// FindingsFor lists what has been reported on a job.
func FindingsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]Finding, error) {
	var out []Finding
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT f.id, f.note, coalesce(p.display_name, ''), f.reported_at, f.handled_at, f.work_order_id
			FROM findings f
			LEFT JOIN users u  ON u.id = f.reported_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE f.work_order_id = $1
			ORDER BY f.reported_at`, jobID)
		if err != nil {
			return fmt.Errorf("list findings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var f Finding
			if err := rows.Scan(&f.ID, &f.Note, &f.ReportedBy, &f.ReportedAt, &f.HandledAt, &f.WorkOrderID); err != nil {
				return fmt.Errorf("scan finding: %w", err)
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}

// HandleFinding marks a finding as dealt with, whether it was priced or
// declined. Either way it stops being an open question.
func HandleFinding(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, findingID string) error {
	if !looksLikeUUID(findingID) {
		return ErrNotFound
	}
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE findings SET handled_at = now(), handled_by = $1
			 WHERE id = $2 AND handled_at IS NULL`, scope.UserID, findingID)
		if err != nil {
			return fmt.Errorf("handle finding: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
