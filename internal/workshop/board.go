package workshop

import (
	"context"
	"fmt"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BoardEntry is one vehicle on the front desk board.
//
// Unlike Job, this carries the customer's name: a service advisor answering
// the telephone cannot do their work without it. That is the whole reason the
// two types are separate rather than one struct with a flag -- the technician's
// query has no column to leak, and this one has a reason for every column it
// does have. Neither is a template deciding what to hide.
type BoardEntry struct {
	ID           string
	Number       int64
	State        string
	Registration string
	Make         string
	Model        string
	Complaint    string

	CustomerName string
	OpenedAt     time.Time
	PromisedAt   *time.Time
	ReadyAt      *time.Time

	// Who has a clock running on it, if anyone.
	WorkingNow string
}

// Waiting says what the vehicle is waiting for, in words a person at a counter
// would use.
func (b BoardEntry) Waiting() string {
	switch b.State {
	case "draft", "estimated":
		return "Not sent to the customer yet"
	case "awaiting_approval":
		return "Waiting for the customer to approve"
	case "approved":
		return "Approved, not started"
	case "in_progress":
		if b.WorkingNow != "" {
			return b.WorkingNow + " is on it"
		}
		return "Started, nobody on it right now"
	case "awaiting_parts":
		return "Waiting for parts"
	case "ready":
		return "Ready for collection"
	case "declined":
		return "Declined — still needs invoicing"
	}
	return b.State
}

// Late reports whether the promised time has passed on a job that is not
// finished.
//
// Ready and invoiced are excluded on purpose. A car that is done is not late
// however long ago it was promised, and colouring those rows too would make
// the whole board red, at which point red stops meaning anything.
func (b BoardEntry) Late() bool {
	if b.PromisedAt == nil {
		return false
	}
	switch b.State {
	case "ready", "invoiced", "closed", "cancelled":
		return false
	}
	return time.Now().After(*b.PromisedAt)
}

// WaitingDays is how long a finished car has been standing uncollected.
func (b BoardEntry) WaitingDays() int {
	if b.State != "ready" || b.ReadyAt == nil {
		return 0
	}
	return int(time.Since(*b.ReadyAt).Hours() / 24)
}

// Board lists everything in the shop that is not finished.
//
// Requires a role that may see customer personal data. The check is here, at
// the query, rather than on the route: a second caller added later gets the
// same refusal without anyone remembering to guard it.
func Board(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]BoardEntry, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}

	var out []BoardEntry
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		const q = `
			SELECT w.id, w.number, w.state,
			       coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''),
			       coalesce(w.complaint, ''),
			       coalesce(cp.display_name, c.company_name, ''),
			       w.opened_at, w.promised_at, w.ready_at,
			       coalesce((
			           SELECT p.display_name
			           FROM time_entries t
			           JOIN users u  ON u.id = t.user_id
			           JOIN people p ON p.id = u.person_id
			           WHERE t.work_order_id = w.id AND t.ended_at IS NULL
			           LIMIT 1), '')
			FROM work_orders w
			JOIN vehicles v  ON v.id = w.vehicle_id
			JOIN customers c ON c.id = w.customer_id
			LEFT JOIN people cp ON cp.id = c.person_id
			LEFT JOIN vehicle_registrations r
			       ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE w.state NOT IN ('closed', 'cancelled')
			ORDER BY
			    -- Late first, then by what was promised. A board is read top
			    -- down under pressure, so the order has to carry the urgency.
			    (w.promised_at IS NOT NULL
			     AND w.promised_at < now()
			     AND w.state NOT IN ('ready', 'invoiced')) DESC,
			    w.promised_at NULLS LAST,
			    w.opened_at`
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return fmt.Errorf("board: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b BoardEntry
			if err := rows.Scan(&b.ID, &b.Number, &b.State,
				&b.Registration, &b.Make, &b.Model, &b.Complaint,
				&b.CustomerName, &b.OpenedAt, &b.PromisedAt, &b.ReadyAt,
				&b.WorkingNow); err != nil {
				return fmt.Errorf("scan board entry: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}
