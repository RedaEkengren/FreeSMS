package workshop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// That the customer was told their car is ready, as rows: who, how, when.
// Nothing used to record it, so "did anybody ring them?" had no answer.

// ContactWays are how the front desk can have told a customer, as catalogue
// keys. "No answer" is a try that counts as one, and not as told.
var ContactWays = []string{"phoned", "sms", "email", "link", "counter", "no_answer"}

// CustomerContact is one time somebody told, or tried to tell, the customer.
type CustomerContact struct {
	How  string
	Note string
	At   time.Time
	By   string
}

// Reached reports whether this told the customer, rather than tried to.
func (c CustomerContact) Reached() bool { return c.How != "no_answer" }

// HowLabel is the way, as a person reads it.
func (c CustomerContact) HowLabel() string { return contactLabels[c.How] }

var contactLabels = map[string]string{
	"phoned":    "phoned",
	"sms":       "texted from our own phone",
	"email":     "emailed",
	"link":      "sent the link",
	"counter":   "told at the counter",
	"no_answer": "no answer",
}

// ContactLabel is a way's label, for a form's choices.
func ContactLabel(how string) string { return contactLabels[how] }

// toldColumns is when the customer was last reached about a ready car w,
// since it last became ready: telling them about the previous time it was
// ready does not count once it has been back on a lift.
const toldColumns = `
	(SELECT max(cc.at) FROM customer_contacts cc
	  WHERE cc.work_order_id = w.id AND cc.about = 'ready' AND cc.how <> 'no_answer'
	    AND w.ready_at IS NOT NULL AND cc.at >= w.ready_at
	    -- Sent and then reported as never arriving is not told.
	    AND NOT EXISTS (SELECT 1 FROM message_events me WHERE me.message_id = cc.message_id AND me.event = 'failed')) AS told_at`

// RecordContact records that the front desk told, or tried to tell, the
// customer their car is ready. The counter's: it is a conversation with the
// customer.
func RecordContact(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, how, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if _, ok := contactLabels[how]; !ok {
		return fmt.Errorf("%w: %q is not a way of telling a customer", ErrInvalid, how)
	}
	note = strings.TrimSpace(note)
	if len(note) > 500 {
		return fmt.Errorf("%w: keep the note under 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, jobID).Scan(&state); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("read order: %w", err)
		}
		if state != StateReady {
			return fmt.Errorf("%w: the car is not ready, so there is nothing to tell them yet", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_contacts (shop_id, work_order_id, about, how, note, recorded_by)
			VALUES ($1, $2, 'ready', $3, nullif($4, ''), $5)`,
			scope.ShopID, jobID, how, note, scope.UserID); err != nil {
			return fmt.Errorf("record contact: %w", err)
		}
		return nil
	})
}

// ContactsFor lists the times the customer was told, or tried, newest first.
func ContactsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]CustomerContact, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []CustomerContact
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT cc.how, coalesce(cc.note, ''), cc.at, coalesce(p.display_name, '')
			FROM customer_contacts cc
			JOIN users u ON u.id = cc.recorded_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE cc.work_order_id = $1
			ORDER BY cc.at DESC`, jobID)
		if err != nil {
			return fmt.Errorf("read contacts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c CustomerContact
			if err := rows.Scan(&c.How, &c.Note, &c.At, &c.By); err != nil {
				return fmt.Errorf("scan contact: %w", err)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}
