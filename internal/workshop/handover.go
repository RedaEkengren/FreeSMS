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

// PartRequest is a technician asking for something so they can carry on.
type PartRequest struct {
	ID          string
	WorkOrderID string
	Description string
	RequestedBy string
	RequestedAt time.Time
	ArrivedAt   *time.Time
	CancelledAt *time.Time
	// How many were asked for, and how many have come so far, in deliveries
	// or off the shelf.
	Quantity float64
	Received float64
	// Whether anybody has ordered it, and when the latest word is that it is
	// expected. No supplier and no cost: the shop floor needs to know when.
	Ordered      bool
	ETA          *time.Time
	Registration string
	Make         string
	Model        string
}

// Stage is where the request stands, as a catalogue key.
func (p PartRequest) Stage() string {
	switch {
	case p.CancelledAt != nil:
		return "cancelled"
	case p.ArrivedAt != nil:
		return "arrived"
	case p.Received > 0:
		return "partly here"
	case p.Ordered:
		return "ordered"
	}
	return "not ordered yet"
}

// Open reports whether this request is still waiting on somebody.
func (p PartRequest) Open() bool { return p.ArrivedAt == nil && p.CancelledAt == nil }

// Rest is what is still owed. Never negative: five of four is done.
func (p PartRequest) Rest() float64 { return round3(max(p.Quantity-p.Received, 0)) }

// Partly reports some of it here and some still owed -- the normal case, not
// an error.
func (p PartRequest) Partly() bool { return p.Open() && p.Received > 0 }

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
	return RequestParts(ctx, pool, scope, jobID, description, 1)
}

// RequestParts asks for a quantity: four brake discs, of which two may come
// on Tuesday and the rest on Friday.
func RequestParts(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, description string, quantity float64) error {
	if strings.TrimSpace(description) == "" {
		return fmt.Errorf("%w: say what is needed", ErrInvalid)
	}
	if quantity <= 0 {
		return fmt.Errorf("%w: ask for at least one", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := requestPartsTx(ctx, tx, scope, jobID, description, quantity)
		return err
	})
}

// requestPartsTx makes the request and puts the job to waiting, inside the
// caller's transaction, and says which request it made.
func requestPartsTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, description string, quantity float64) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO part_requests (shop_id, work_order_id, description, requested_by, quantity)
		SELECT $1, w.id, $2, $3, $5 FROM work_orders w WHERE w.id = $4
		RETURNING id`,
		scope.ShopID, strings.TrimSpace(description), scope.UserID, jobID, quantity).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("record the request: %w", err)
	}
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, jobID).Scan(&state); err != nil {
		return "", fmt.Errorf("read state: %w", err)
	}
	if CanTransition(state, StateAwaitingParts) {
		return id, setStateTx(ctx, tx, jobID, StateAwaitingParts)
	}
	// Already waiting, or somewhere the move does not apply. The request
	// still stands.
	return id, nil
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
			       coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''),
			       pr.quantity::float8, coalesce((SELECT sum(rc.quantity) FROM part_request_receipts rc WHERE rc.request_id = pr.id), 0)::float8,
			       EXISTS (SELECT 1 FROM supplier_order_lines sl WHERE sl.request_id = pr.id AND sl.cancelled_at IS NULL),
			       (SELECT e.eta::timestamptz FROM supplier_order_lines sl JOIN supplier_line_etas e ON e.line_id = sl.id
			         WHERE sl.request_id = pr.id AND sl.cancelled_at IS NULL ORDER BY e.at DESC, e.id DESC LIMIT 1)
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
				&p.RequestedAt, &p.Registration, &p.Make, &p.Model, &p.Quantity, &p.Received, &p.Ordered, &p.ETA); err != nil {
				return fmt.Errorf("scan part request: %w", err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// MarkPartArrived records everything still owed on a request as delivered.
func MarkPartArrived(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, requestID string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return ReceivePart(ctx, pool, scope, requestID, 0, "delivery")
}

// ReceivePart records some or all of a request arriving: in a delivery, or
// taken off the shelf instead. Zero is whatever is still owed. More than is
// owed is recorded as it came.
//
// The request is finished when everything asked for has come, and the job
// goes back to work when no request on it is still waiting. A gearbox job
// waiting on three parts, or on the second half of one, does not look ready
// to work on because something turned up.
func ReceivePart(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, requestID string, quantity float64, source string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(requestID) {
		return ErrNotFound
	}
	if source != "delivery" && source != "shelf" && source != "customer" {
		return fmt.Errorf("%w: %q is not where a part comes from", ErrInvalid, source)
	}
	if quantity < 0 {
		return fmt.Errorf("%w: a negative arrival is a return to the supplier", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := receiveRequestTx(ctx, tx, scope, requestID, quantity, source)
		return err
	})
}

// receiveRequestTx books an arrival against a request: the receipt, the
// request finished if everything has come, the job back to work if nothing else
// is owed. It says which receipt it wrote and whether the request is now done.
func receiveRequestTx(ctx context.Context, tx pgx.Tx, scope access.Scope, requestID string, quantity float64, source string) (string, bool, error) {
	var jobID string
	var asked, received float64
	err := tx.QueryRow(ctx, `
		SELECT pr.work_order_id, pr.quantity::float8,
		       coalesce((SELECT sum(rc.quantity) FROM part_request_receipts rc WHERE rc.request_id = pr.id), 0)::float8
		FROM part_requests pr
		WHERE pr.id = $1 AND pr.arrived_at IS NULL AND pr.cancelled_at IS NULL
		FOR UPDATE`, requestID).Scan(&jobID, &asked, &received)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("read request: %w", err)
	}
	if quantity == 0 {
		quantity = asked - received
	}
	var receiptID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO part_request_receipts (shop_id, request_id, quantity, source, received_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, scope.ShopID, requestID, quantity, source, scope.UserID).Scan(&receiptID); err != nil {
		return "", false, fmt.Errorf("record the arrival: %w", err)
	}
	if received+quantity+1e-9 < asked {
		// Part of it. The job keeps waiting for the rest.
		return receiptID, false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE part_requests SET arrived_at = now(), arrived_by = $1 WHERE id = $2`,
		scope.UserID, requestID); err != nil {
		return "", false, fmt.Errorf("mark arrived: %w", err)
	}
	return receiptID, true, resumeIfNothingOwedTx(ctx, tx, jobID)
}

// CancelPartRequest drops a request the supplier will not fill, or that is
// no longer needed. A job waiting on a part that is never coming has to stop
// waiting; what did arrive stays recorded.
func CancelPartRequest(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, requestID, reason string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(requestID) {
		return ErrNotFound
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: say why it is not coming", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var jobID string
		err := tx.QueryRow(ctx, `
			UPDATE part_requests SET cancelled_at = now(), note = $2
			WHERE id = $1 AND arrived_at IS NULL AND cancelled_at IS NULL
			RETURNING work_order_id`, requestID, reason).Scan(&jobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("cancel request: %w", err)
		}
		return resumeIfNothingOwedTx(ctx, tx, jobID)
	})
}

// resumeIfNothingOwedTx sends a job waiting for parts back to work once no
// request on it is still waiting.
func resumeIfNothingOwedTx(ctx context.Context, tx pgx.Tx, jobID string) error {
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
	// Declined or invoiced while the part was on its way. The part is bought
	// and the work is not happening; that is a return, and the request is
	// closed either way.
	return nil
}

// PartRequestsFor lists a job's requests, arrived and outstanding.
func PartRequestsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]PartRequest, error) {
	var out []PartRequest
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT pr.id, pr.work_order_id, pr.description,
			       coalesce(p.display_name, ''), pr.requested_at, pr.arrived_at, pr.cancelled_at,
			       pr.quantity::float8, coalesce((SELECT sum(rc.quantity) FROM part_request_receipts rc WHERE rc.request_id = pr.id), 0)::float8,
			       EXISTS (SELECT 1 FROM supplier_order_lines sl WHERE sl.request_id = pr.id AND sl.cancelled_at IS NULL),
			       (SELECT e.eta::timestamptz FROM supplier_order_lines sl JOIN supplier_line_etas e ON e.line_id = sl.id
			         WHERE sl.request_id = pr.id AND sl.cancelled_at IS NULL ORDER BY e.at DESC, e.id DESC LIMIT 1)
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
				&p.RequestedAt, &p.ArrivedAt, &p.CancelledAt, &p.Quantity, &p.Received, &p.Ordered, &p.ETA); err != nil {
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
