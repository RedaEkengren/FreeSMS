package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Telling the customer from the system. A message is queued by the front
// desk, sent by the server when it may be, and every thing that happens to
// it -- queued, sent, retried, delivered, failed -- is a row. A sent message
// is a contact in the log (#92), so "customer told 14:20" stays one fact
// whether somebody rang or the system texted; a message the provider says
// never arrived stops counting as told.

// MessageAbouts are what a message can be about, as catalogue keys for the
// page. One of each that matters, not one per change of state.
var MessageAbouts = map[string]string{
	"ready":        "Your car is ready to collect",
	"answer":       "We need your answer",
	"waiting_part": "Waiting for a part",
}

// SendHours are when a message may go, as whole hours in the shop's zone:
// from eight, until eight in the evening, unless the workshop says otherwise.
// A message queued outside them waits.
type SendHours struct{ From, Until int }

// DefaultSendHours are eight to eight.
var DefaultSendHours = SendHours{8, 20}

// NotBefore is when a message queued at now may go, in the shop's zone.
func (h SendHours) NotBefore(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	y, m, d := t.Date()
	switch {
	case t.Hour() < h.From:
		return time.Date(y, m, d, h.From, 0, 0, 0, loc)
	case t.Hour() >= h.Until:
		return time.Date(y, m, d+1, h.From, 0, 0, 0, loc)
	}
	return now
}

// OutboundMessage is a message on a job's page.
type OutboundMessage struct {
	ID        string
	About     string
	Channel   string
	Recipient string
	NotBefore time.Time
	CreatedAt time.Time
	CreatedBy string
	// The latest thing that happened to it, and when.
	State  string
	At     time.Time
	Detail string
}

// Queued reports a message still waiting to go, as the page says it.
func (m OutboundMessage) Waiting() bool { return m.State == "queued" || m.State == "retry" }

// NewMessage is a message the front desk has decided to send.
type NewMessage struct {
	JobID     string
	About     string
	Channel   string
	Recipient string
	Subject   string
	Body      string
	NotBefore time.Time
}

// SentBefore reports whether this job's customer has been sent anything at
// this address already. The first message to a new one is confirmed first: a
// number one digit out is somebody else's, and the link shows what is owed.
func SentBefore(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, recipient string) (bool, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return false, access.ErrForbidden
	}
	var sent bool
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM outbound_messages m
			  JOIN work_orders mw ON mw.id = m.work_order_id
			  JOIN work_orders w  ON w.id = $1
			  WHERE m.recipient = $2 AND mw.customer_id = w.customer_id
			    AND EXISTS (SELECT 1 FROM message_events e WHERE e.message_id = m.id AND e.event IN ('sent', 'delivered')))`,
			jobID, recipient).Scan(&sent)
	})
	return sent, err
}

// AlreadySent is when a message about the same thing went to this job's
// customer -- since the car last became ready, for "ready" -- or nil.
func AlreadySent(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, about string) (*time.Time, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var at *time.Time
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT max(m.created_at) FROM outbound_messages m JOIN work_orders w ON w.id = m.work_order_id
			WHERE m.work_order_id = $1 AND m.about = $2
			  AND ($2 <> 'ready' OR w.ready_at IS NULL OR m.created_at >= w.ready_at)
			  AND NOT EXISTS (SELECT 1 FROM message_events e WHERE e.message_id = m.id AND e.event = 'failed')`,
			jobID, about).Scan(&at)
	})
	return at, err
}

// QueueMessage puts a message in the queue. The front desk's: it is a
// conversation with the customer.
func QueueMessage(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, m NewMessage) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if _, ok := MessageAbouts[m.About]; !ok {
		return "", fmt.Errorf("%w: %q is not something to tell a customer", ErrInvalid, m.About)
	}
	if m.Channel != "sms" && m.Channel != "email" {
		return "", fmt.Errorf("%w: a text or an email", ErrInvalid)
	}
	if m.Recipient == "" || m.Body == "" {
		return "", fmt.Errorf("%w: nobody to send it to, or nothing to say", ErrInvalid)
	}
	if !looksLikeUUID(m.JobID) {
		return "", ErrNotFound
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO outbound_messages (shop_id, work_order_id, about, channel, recipient, subject, body, not_before, created_by)
			SELECT $1, w.id, $3, $4, $5, $6, $7, $8, $9 FROM work_orders w WHERE w.id = $2
			RETURNING id`,
			scope.ShopID, m.JobID, m.About, m.Channel, m.Recipient, m.Subject, m.Body, m.NotBefore, scope.UserID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("queue message: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO message_events (shop_id, message_id, event) VALUES ($1, $2, 'queued')`, scope.ShopID, id)
		return err
	})
	return id, err
}

// MessagesFor lists a job's messages and what last happened to each.
func MessagesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]OutboundMessage, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []OutboundMessage
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.id, m.about, m.channel, m.recipient, m.not_before, m.created_at, coalesce(p.display_name, ''),
			       e.event, e.at, coalesce(e.detail, '')
			FROM outbound_messages m
			LEFT JOIN users u  ON u.id = m.created_by
			LEFT JOIN people p ON p.id = u.person_id
			JOIN LATERAL (SELECT event, at, detail FROM message_events
			              WHERE message_id = m.id ORDER BY at DESC LIMIT 1) e ON true
			WHERE m.work_order_id = $1
			ORDER BY m.created_at DESC`, jobID)
		if err != nil {
			return fmt.Errorf("messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m OutboundMessage
			if err := rows.Scan(&m.ID, &m.About, &m.Channel, &m.Recipient, &m.NotBefore, &m.CreatedAt, &m.CreatedBy,
				&m.State, &m.At, &m.Detail); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// MessagesThisMonth counts what was sent this month by channel: a text costs
// money each, and whoever pays should be able to see how many.
func MessagesThisMonth(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, now time.Time) (sms, email int, err error) {
	if !scope.Role.RunsTheShop() {
		return 0, 0, access.ErrForbidden
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		t := now.In(loc)
		from := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		return tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE m.channel = 'sms'), count(*) FILTER (WHERE m.channel = 'email')
			FROM outbound_messages m
			WHERE EXISTS (SELECT 1 FROM message_events e WHERE e.message_id = m.id AND e.event = 'sent' AND e.at >= $1)`,
			from).Scan(&sms, &email)
	})
	return sms, email, err
}

// DueMessage is a message the sender may try now.
type DueMessage struct {
	ID, JobID, About, Channel, Recipient, Subject, Body string
	Tries                                               int
}

// maxTries is how often a message that cannot be handed over is tried
// before it is given up as failed -- about half an hour, at growing gaps.
const maxTries = 6

// DueMessages are the queued messages whose time has come, the oldest first.
// The sender runs as the shop, with no person behind it, so this takes the
// shop rather than a scope.
func DueMessages(ctx context.Context, pool *pgxpool.Pool, shopID string, now time.Time, limit int) ([]DueMessage, error) {
	var out []DueMessage
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.id, m.work_order_id, m.about, m.channel, m.recipient, m.subject, m.body,
			       (SELECT count(*) FROM message_events r WHERE r.message_id = m.id AND r.event = 'retry')::int AS tries
			FROM outbound_messages m
			JOIN LATERAL (SELECT event, at FROM message_events WHERE message_id = m.id ORDER BY at DESC LIMIT 1) e ON true
			WHERE m.not_before <= $1 AND m.recipient <> ''
			  AND (e.event = 'queued'
			       OR (e.event = 'retry' AND e.at + interval '1 minute' * power(2, (SELECT count(*) FROM message_events r
			            WHERE r.message_id = m.id AND r.event = 'retry') - 1) <= $1))
			ORDER BY m.created_at LIMIT $2`, now, limit)
		if err != nil {
			return fmt.Errorf("due messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m DueMessage
			if err := rows.Scan(&m.ID, &m.JobID, &m.About, &m.Channel, &m.Recipient, &m.Subject, &m.Body, &m.Tries); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// MessageSent records a message handed to its provider, and that the
// customer was told by it.
func MessageSent(ctx context.Context, pool *pgxpool.Pool, shopID, messageID, provider, providerID string) error {
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO message_events (shop_id, message_id, event, provider, provider_id) VALUES ($1, $2, 'sent', $3, nullif($4, ''))`,
			shopID, messageID, provider, providerID); err != nil {
			return fmt.Errorf("record sent: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO customer_contacts (shop_id, work_order_id, about, how, recorded_by, message_id)
			SELECT shop_id, work_order_id, about, channel, created_by, id FROM outbound_messages WHERE id = $1`, messageID)
		if err != nil {
			return fmt.Errorf("record the contact: %w", err)
		}
		return nil
	})
}

// MessageNotSent records a try that did not reach the provider. It is tried
// again later, and given up as failed after maxTries.
func MessageNotSent(ctx context.Context, pool *pgxpool.Pool, shopID, messageID string, tries int, detail string) error {
	event := "retry"
	if tries+1 >= maxTries {
		event = "failed"
	}
	if len(detail) > 500 {
		detail = detail[:500]
	}
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO message_events (shop_id, message_id, event, detail) VALUES ($1, $2, $3, $4)`,
			shopID, messageID, event, detail)
		return err
	})
}

// MessageDelivery records what a provider reported about a message it was
// handed: delivered, or failed. A failed one stops counting as told.
func MessageDelivery(ctx context.Context, pool *pgxpool.Pool, shopID, provider, providerID string, delivered bool) error {
	event := "failed"
	if delivered {
		event = "delivered"
	}
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO message_events (shop_id, message_id, event, provider, provider_id)
			SELECT shop_id, message_id, $3, provider, provider_id FROM message_events
			WHERE provider = $1 AND provider_id = $2 AND event = 'sent'
			LIMIT 1`, provider, providerID, event)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Signature is what a message is signed with: the workshop's name and the
// number to ring, and the language it writes to customers in.
type Signature struct {
	Name, Phone, Locale string
}

// ShopSignature reads it, for whoever sends the message.
func ShopSignature(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (Signature, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Signature{}, access.ErrForbidden
	}
	var s Signature
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT name, coalesce(phone, ''), locale FROM shops WHERE id = $1`, scope.ShopID).
			Scan(&s.Name, &s.Phone, &s.Locale)
	})
	return s, err
}
