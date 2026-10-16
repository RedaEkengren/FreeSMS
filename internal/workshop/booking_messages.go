package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Telling a customer about a booking.
//
// A booking comes before a work order, so these messages hang on the booking
// and no job is made to hold them. They go through the same queue, providers,
// quiet hours, retries and delivery reports as every other message, and they
// are not sent for a booking that is no longer what they say: moved, cancelled
// or already arrived, the ones waiting are stopped when that happens, and the
// sender asks once more as each is about to go.

// BookingKinds are the messages a booking can have, by the word used in forms.
var BookingKinds = map[string]string{
	"confirm":   "booking_confirm",
	"reminder":  "booking_reminder",
	"moved":     "booking_moved",
	"cancelled": "booking_cancelled",
}

// BookingMessageInput is a message about a booking to be queued.
type BookingMessageInput struct {
	// confirm, reminder, moved or cancelled.
	Kind string
	// sms or email.
	Channel   string
	Address   string
	Recipient string // who it is for, as the counter confirmed
	Confirmed bool
	Subject   string
	Body      string
	// When it may go; a reminder is queued for before the booking.
	NotBefore time.Time
	// Send it again although the same message already went for this time.
	Again bool
}

// QueueBookingMessage queues a message about a booking. The counter confirms
// who it is for and where it goes; the first message of a kind for a time is
// not sent twice unless somebody says so.
func QueueBookingMessage(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string, in BookingMessageInput) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return "", ErrNotFound
	}
	about, ok := BookingKinds[in.Kind]
	if !ok {
		return "", fmt.Errorf("%w: a confirmation, a reminder, a move or a cancellation", ErrInvalid)
	}
	in.Address, in.Recipient = strings.TrimSpace(in.Address), strings.TrimSpace(in.Recipient)
	if !in.Confirmed || in.Recipient == "" {
		return "", fmt.Errorf("%w: confirm who it is for and where it goes", ErrInvalid)
	}
	if in.Channel != "sms" && in.Channel != "email" {
		return "", fmt.Errorf("%w: a text or an email", ErrInvalid)
	}
	if in.Address == "" || strings.TrimSpace(in.Body) == "" {
		return "", fmt.Errorf("%w: nowhere to send it, or nothing to say", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		var starts time.Time
		if err := tx.QueryRow(ctx, `SELECT status, starts_at FROM bookings WHERE id = $1 FOR UPDATE`, bookingID).Scan(&status, &starts); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock booking: %w", err)
		}
		if in.Kind == "cancelled" {
			if status != "cancelled" {
				return fmt.Errorf("%w: the booking is not cancelled", ErrInvalid)
			}
		} else {
			if status != "booked" {
				return fmt.Errorf("%w: the booking is %s, so there is nothing to confirm", ErrInvalid, status)
			}
			if !starts.After(time.Now()) {
				return fmt.Errorf("%w: the booking's time has gone", ErrInvalid)
			}
		}
		not := in.NotBefore
		if not.IsZero() {
			not = time.Now()
		}
		if in.Kind == "reminder" && !not.Before(starts) {
			return fmt.Errorf("%w: a reminder has to go before the booking", ErrInvalid)
		}
		if !in.Again {
			var sent bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM outbound_messages m
				                WHERE m.booking_id = $1 AND m.about = $2 AND m.valid_for_starts = $3
				                  AND NOT EXISTS (SELECT 1 FROM message_events e WHERE e.message_id = m.id AND e.event IN ('failed', 'cancelled')))`,
				bookingID, about, starts).Scan(&sent); err != nil {
				return fmt.Errorf("check for the same message: %w", err)
			}
			if sent {
				return fmt.Errorf("%w: that was already sent for this time; send it again if it should go twice", ErrInvalid)
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO outbound_messages (shop_id, booking_id, valid_for_starts, about, channel, recipient, subject, body, not_before, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			scope.ShopID, bookingID, starts, about, in.Channel, in.Address, in.Subject, in.Body, not, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("queue message: %w", err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO message_events (shop_id, message_id, event) VALUES ($1, $2, 'queued')`, scope.ShopID, id)
		return err
	})
	return id, err
}

// stopPendingBookingMessagesTx cancels the messages about a booking that have
// not gone yet and would now say the wrong thing: a confirmation or a reminder
// for a time that has changed or a booking that is gone. A move's or a
// cancellation's own message is for the new state and is left.
func stopPendingBookingMessagesTx(ctx context.Context, tx pgx.Tx, shopID, bookingID, why string, keep ...string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO message_events (shop_id, message_id, event, detail)
		SELECT m.shop_id, m.id, 'cancelled', $3
		FROM outbound_messages m
		JOIN LATERAL (SELECT event FROM message_events WHERE message_id = m.id ORDER BY at DESC, id DESC LIMIT 1) l ON true
		WHERE m.booking_id = $2 AND l.event IN ('queued', 'retry') AND m.shop_id = $1
		  AND NOT (m.about = ANY($4::text[]))`, shopID, bookingID, why, keep)
	if err != nil {
		return fmt.Errorf("stop pending messages: %w", err)
	}
	return nil
}

// BookingMessageStillWanted is asked as a booking message is about to go:
// whether the booking is still what it was written about.
func BookingMessageStillWanted(ctx context.Context, pool *pgxpool.Pool, shopID, bookingID, about string, validFor time.Time) (bool, string, error) {
	var status string
	var starts time.Time
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, starts_at FROM bookings WHERE id = $1`, bookingID).Scan(&status, &starts)
	})
	if err != nil {
		return false, "", err
	}
	switch {
	case about == "booking_cancelled":
		return status == "cancelled", "The booking is no longer cancelled; nothing was sent.", nil
	case status != "booked":
		return false, "The booking was " + status + " before this went; nothing was sent.", nil
	case !starts.Equal(validFor):
		return false, "The booking was moved before this went; nothing was sent.", nil
	case !starts.After(time.Now()):
		return false, "The booking's time had gone before this went; nothing was sent.", nil
	}
	return true, "", nil
}

// BookingMessagesFor lists the messages about a booking and where each got to.
func BookingMessagesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) ([]OutboundMessage, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return nil, ErrNotFound
	}
	var out []OutboundMessage
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.id, m.about, m.channel, m.recipient, m.not_before, m.created_at, coalesce(p.display_name, ''),
			       e.event, e.at, coalesce(e.detail, '')
			FROM outbound_messages m
			JOIN LATERAL (SELECT event, at, detail FROM message_events WHERE message_id = m.id ORDER BY at DESC, id DESC LIMIT 1) e ON true
			LEFT JOIN users u ON u.id = m.created_by LEFT JOIN people p ON p.id = u.person_id
			WHERE m.booking_id = $1 ORDER BY m.created_at DESC`, bookingID)
		if err != nil {
			return fmt.Errorf("list booking messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m OutboundMessage
			if err := rows.Scan(&m.ID, &m.About, &m.Channel, &m.Recipient, &m.NotBefore, &m.CreatedAt, &m.CreatedBy, &m.State, &m.At, &m.Detail); err != nil {
				return fmt.Errorf("scan booking message: %w", err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// CorrectBookingContact puts right who to ring for a booking. Messages still
// waiting to go to the old number are stopped; a new one is the counter's to
// queue.
func CorrectBookingContact(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, name, phone string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return ErrNotFound
	}
	name, phone = strings.TrimSpace(name), strings.TrimSpace(phone)
	if len([]rune(name)) > 200 || len([]rune(phone)) > 40 {
		return fmt.Errorf("%w: the name or the number is too long", ErrInvalid)
	}
	phone, err := cleanPhone(phone)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE bookings SET customer_name = nullif($2, ''), customer_phone = nullif($3, '') WHERE id = $1`, bookingID, name, phone)
		if err != nil {
			return fmt.Errorf("correct contact: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return bookingEventTx(ctx, tx, scope, bookingID, "contact_corrected", nil, nil, "")
	})
}

// ConfirmBookingByHand writes down that the customer was told another way: on
// the telephone, in writing, at the counter. The path that works when nothing
// can be sent.
func ConfirmBookingByHand(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, how string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return ErrNotFound
	}
	how = strings.TrimSpace(how)
	if how == "" || len([]rune(how)) > 200 {
		return fmt.Errorf("%w: say how, in 200 characters or fewer", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM bookings WHERE id = $1)`, bookingID).Scan(&exists); err != nil {
			return fmt.Errorf("read booking: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		return bookingEventTx(ctx, tx, scope, bookingID, "confirmed_by_hand", nil, nil, how)
	})
}

// BookingNeedingAttention is an upcoming booking whose message did not get to
// the customer, for the counter's queue.
type BookingNeedingAttention struct {
	BookingID string
	Starts    time.Time
	What      string
}

// BookingsNeedingAttention lists the bookings still to come whose latest
// message about them failed and that nobody has since told another way or sent
// again.
func BookingsNeedingAttention(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]BookingNeedingAttention, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []BookingNeedingAttention
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT b.id::text, b.starts_at, b.what FROM bookings b
			WHERE b.status = 'booked' AND b.starts_at > now()
			  AND EXISTS (
			    SELECT 1 FROM outbound_messages m
			    JOIN LATERAL (SELECT event FROM message_events WHERE message_id = m.id ORDER BY at DESC, id DESC LIMIT 1) l ON true
			    WHERE m.booking_id = b.id AND l.event = 'failed'
			      AND NOT EXISTS (SELECT 1 FROM outbound_messages m2
			                      JOIN LATERAL (SELECT event FROM message_events WHERE message_id = m2.id ORDER BY at DESC, id DESC LIMIT 1) l2 ON true
			                      WHERE m2.booking_id = b.id AND m2.created_at > m.created_at AND l2.event IN ('queued', 'retry', 'sent', 'delivered'))
			      AND NOT EXISTS (SELECT 1 FROM booking_events be WHERE be.booking_id = b.id AND be.event = 'confirmed_by_hand' AND be.at > m.created_at))
			ORDER BY b.starts_at`)
		if err != nil {
			return fmt.Errorf("list bookings needing attention: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b BookingNeedingAttention
			if err := rows.Scan(&b.BookingID, &b.Starts, &b.What); err != nil {
				return fmt.Errorf("scan booking: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}
