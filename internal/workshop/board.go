package workshop

import (
	"context"
	"fmt"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
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

	// Whether a customer is recorded at all, which is not the same as having
	// a name. A company with no contact person has a customer and an empty
	// display name; a vehicle taken in before anybody typed the details has
	// neither. The front desk needs to tell those apart, because the second
	// one cannot be invoiced until it is fixed.
	HasCustomer bool
	OpenedAt     time.Time
	PromisedAt   *time.Time
	ReadyAt      *time.Time

	// Who has a clock running on it, if anyone.
	WorkingNow string

	// Reported from under the car and not yet priced. The front desk's cue
	// that somebody is waiting on them.
	OpenFindings int
}

// Waiting says what the vehicle is waiting for, in words a person at a counter
// would use. It returns the catalogue key, not the finished sentence, so the
// board reads in the shop's language like every other screen; the page renders
// it through the printer with WaitingArg.
//
// The counter's words are not the state machine's. "Declined" is a state; what
// the front desk needs to see is that somebody still has to invoice it. Where
// there is nothing better to say, the state's own label is used rather than
// the bare identifier.
func (b BoardEntry) Waiting() string {
	switch b.State {
	case "draft", "estimated":
		return "Not sent to the customer yet"
	case "awaiting_approval":
		return "Waiting for the customer to approve"
	case "in_progress":
		if b.WorkingNow != "" {
			return "%s is on it"
		}
		return "Started, nobody on it right now"
	case "declined":
		return "Declined — still needs invoicing"
	}
	return State(b.State).Label()
}

// WaitingArg is the single value Waiting's key may interpolate, and empty
// where it takes none. The printer ignores arguments for a text without a verb,
// so the page passes it unconditionally.
func (b BoardEntry) WaitingArg() string {
	if b.State == "in_progress" {
		return b.WorkingNow
	}
	return ""
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
			       w.customer_id IS NOT NULL,
			       w.opened_at, w.promised_at, w.ready_at,
			       coalesce((
			           SELECT p.display_name
			           FROM time_entries t
			           JOIN users u  ON u.id = t.user_id
			           JOIN people p ON p.id = u.person_id
			           WHERE t.work_order_id = w.id AND t.ended_at IS NULL
			           LIMIT 1), ''),
			       (SELECT count(*) FROM findings f
			         WHERE f.work_order_id = w.id AND f.handled_at IS NULL)
			FROM work_orders w
			JOIN vehicles v  ON v.id = w.vehicle_id
			-- Left, not inner. A work order is allowed to have no customer --
			-- migration 0005 made the column nullable so that the front desk
			-- is never pushed into inventing one -- and an inner join here
			-- would hide exactly those vehicles from the board that exists to
			-- show what is in the shop. The technician's list does not touch
			-- this table, so the car would be worked on while the counter
			-- could not see it.
			LEFT JOIN customers c ON c.id = w.customer_id
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
				&b.CustomerName, &b.HasCustomer, &b.OpenedAt, &b.PromisedAt, &b.ReadyAt,
				&b.WorkingNow, &b.OpenFindings); err != nil {
				return fmt.Errorf("scan board entry: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}
