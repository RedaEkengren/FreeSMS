package workshop

import (
	"context"
	"errors"
	"fmt"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State is where a work order has got to.
//
// The same values are a CHECK constraint on the column. Both are deliberate:
// the constraint stops a value nobody planned for ever being stored, and this
// stops a value that is legal in isolation being reached from somewhere it
// makes no sense.
type State string

const (
	StateDraft            State = "draft"
	StateEstimated        State = "estimated"
	StateAwaitingApproval State = "awaiting_approval"
	StateApproved         State = "approved"
	StateInProgress       State = "in_progress"
	StateAwaitingParts    State = "awaiting_parts"
	StateReady            State = "ready"
	StateInvoiced         State = "invoiced"
	StateClosed           State = "closed"
	StateDeclined         State = "declined"
	StateCancelled        State = "cancelled"
)

// transitions is the whole state machine, in one place.
//
// Scattering these across the handlers that happen to need them is how a
// system ends up with two answers to "can this be approved twice", and the
// answer that wins is whichever code path the user found first.
var transitions = map[State][]State{
	StateDraft:     {StateEstimated, StateCancelled},
	StateEstimated: {StateAwaitingApproval, StateDraft, StateCancelled},

	// A customer may say no before anything is touched, or say yes.
	StateAwaitingApproval: {StateApproved, StateDeclined, StateEstimated, StateCancelled},

	StateApproved: {StateInProgress, StateAwaitingParts, StateDeclined, StateCancelled},

	// Declined from in_progress is the case worth naming: the car is in
	// pieces, the customer has changed their mind, and the diagnostic time and
	// the reassembly are still owed. That is why declined leads to invoiced
	// below rather than to closed.
	StateInProgress:    {StateAwaitingParts, StateReady, StateDeclined},
	StateAwaitingParts: {StateInProgress, StateReady, StateDeclined},

	// A car that has been handed back for more work is on a lift again.
	StateReady: {StateInvoiced, StateInProgress},

	StateInvoiced: {StateClosed},

	// Declined still has to be billed.
	StateDeclined: {StateInvoiced, StateCancelled},

	// Terminal. Reopening a closed order is a new order, never an edit of a
	// document that has already been issued.
	StateClosed:    {},
	StateCancelled: {},
}

// ErrIllegalTransition is returned for a move the state machine does not
// allow.
type ErrIllegalTransition struct {
	From, To State
}

func (e ErrIllegalTransition) Error() string {
	allowed := transitions[e.From]
	if len(allowed) == 0 {
		return fmt.Sprintf("a %s order cannot be changed; reopening one means a new order", e.From)
	}
	return fmt.Sprintf("a %s order cannot become %s; it can only become %v", e.From, e.To, allowed)
}

// ErrWouldLoseWork is returned when cancelling an order that has work on it.
//
// Cancelled means it never happened, and nothing is owed. An order with
// clocked hours or lines on it did happen, and cancelling it quietly writes
// off work somebody did. Declined is the honest state, and it leads to an
// invoice.
var ErrWouldLoseWork = errors.New(
	"this order has work recorded on it, so it cannot be cancelled; decline it instead, which still produces an invoice")

// ErrNoCustomer is returned when an order with nobody to bill reaches
// invoicing.
//
// The database refuses it too -- a constraint added when customer_id became
// nullable -- but a constraint violation reaching a handler is a 500 and a log
// line. A car taken in overnight with nobody identified is an ordinary
// situation with an obvious fix, and deserves a sentence saying so.
var ErrNoCustomer = errors.New(
	"this order has no customer yet, and an invoice needs somebody to address it to")

// CanTransition reports whether a move is legal.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// NextStates lists where an order can go from here.
func NextStates(from State) []State { return transitions[from] }

// SetState moves an order, or explains why it cannot move.
//
// Everything that changes a state goes through here. The clock does too: it
// advances an approved job to in_progress, and doing that with a bare UPDATE
// is how the second answer to "what is legal" gets written.
func SetState(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, to State) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return setStateTx(ctx, tx, jobID, to)
	})
}

func setStateTx(ctx context.Context, tx pgx.Tx, jobID string, to State) error {
	var from State
	// FOR UPDATE, because two people reading the same state and both deciding
	// their move is legal is how an order gets approved twice.
	err := tx.QueryRow(ctx,
		`SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}

	if from == to {
		// Not an error. Two taps on a slow connection should not produce one.
		return nil
	}
	if !CanTransition(from, to) {
		return ErrIllegalTransition{From: from, To: to}
	}

	if to == StateInvoiced {
		var hasCustomer bool
		if err := tx.QueryRow(ctx,
			`SELECT customer_id IS NOT NULL FROM work_orders WHERE id = $1`,
			jobID).Scan(&hasCustomer); err != nil {
			return fmt.Errorf("check for a customer: %w", err)
		}
		if !hasCustomer {
			return ErrNoCustomer
		}
	}

	if to == StateCancelled {
		var hasWork bool
		if err := tx.QueryRow(ctx, `
			SELECT exists(SELECT 1 FROM time_entries   WHERE work_order_id = $1)
			    OR exists(SELECT 1 FROM work_order_lines WHERE work_order_id = $1)`,
			jobID).Scan(&hasWork); err != nil {
			return fmt.Errorf("check for work: %w", err)
		}
		if hasWork {
			return ErrWouldLoseWork
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE work_orders SET state = $1, updated_at = now(),
		        closed_at = CASE WHEN $1 IN ('closed', 'cancelled') THEN now() ELSE NULL END
		 WHERE id = $2`, to, jobID); err != nil {
		return fmt.Errorf("set state: %w", err)
	}
	return nil
}
