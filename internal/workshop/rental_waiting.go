package workshop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// What the rental desk has to do now (#192, #157, #158): today's handovers
// and returns, a unit out past its return, and the booking after it that
// may not get it. Read from the bookings each time, never kept.

// rentalToday is the end of the shop's today, as t.tomorrow, to join
// beside a query: the shop's zone read in the same statement.
const rentalToday = `
	(SELECT (date_trunc('day', clock_timestamp() AT TIME ZONE timezone) + interval '1 day') AT TIME ZONE timezone AS tomorrow
	   FROM shops) t`

// The rental desk's kinds of thing to do, as SQL conditions on a booking b
// with its block k and the shop's today t (rentalToday).
const (
	rentalHandoverToday = rentalState + ` = 'confirmed' AND b.starts_at < t.tomorrow`
	rentalReturnToday   = rentalState + ` = 'checked_out' AND b.ends_at >= clock_timestamp() AND b.ends_at < t.tomorrow`
	rentalOverdue       = rentalState + ` = 'checked_out' AND b.ends_at < clock_timestamp()`
	rentalOutNow        = rentalState + ` = 'checked_out'`
	// A rental with a damage case found on it that nobody has taken a first
	// step on (#157): the cases the queue lists two days after, counted from
	// the first day, as bookings so it is one more column of the same count.
	rentalDamageToAssess = `EXISTS (SELECT 1 FROM damage_cases c WHERE c.booking_id = b.id
	                                AND NOT EXISTS (SELECT 1 FROM damage_case_steps s WHERE s.case_id = c.id))`
)

// rentalInvoicedThisMonth is what rental invoices came to before VAT in the
// shop's calendar month so far, credit notes counted as the negatives they
// are (the dashboard does the same). Only for whoever runs the shop.
func rentalInvoicedThisMonth(ctx context.Context, tx pgx.Tx, _ access.Scope) (int, int64, error) {
	var n int
	var net int64
	err := tx.QueryRow(ctx, `
		SELECT count(i.id), coalesce(sum(i.net_minor), 0)
		FROM invoices i CROSS JOIN shops s
		WHERE i.source = 'rental'
		  AND i.issued_at >= date_trunc('month', clock_timestamp() AT TIME ZONE s.timezone) AT TIME ZONE s.timezone`).Scan(&n, &net)
	return n, net, err
}

// rentalWaiting is the rental desk's part of the action queue.
func rentalWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, nil
	}
	var out []ActionItem
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT CASE WHEN `+rentalOverdue+` THEN 'rental_overdue'
			            WHEN `+rentalReturnToday+` THEN 'rental_return'
			            WHEN `+rentalState+` = 'confirmed' AND x.at_risk THEN 'rental_at_risk'
			            ELSE 'rental_handover' END,
			       b.id::text, b.number, u.fleet_number || coalesce(' · ' || reg.registration, ' · ' || r.name, ''),
			       CASE WHEN `+rentalState+` = 'checked_out' THEN b.ends_at ELSE b.starts_at END
			FROM rental_bookings b
			JOIN resource_blocks k ON k.id = b.block_id
			JOIN rental_units u ON u.id = b.unit_id
			JOIN resources r ON r.id = u.resource_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
			CROSS JOIN `+rentalToday+`
			CROSS JOIN LATERAL (SELECT EXISTS (
				SELECT 1 FROM rental_bookings o JOIN resource_blocks ok ON ok.id = o.block_id
				 WHERE o.unit_id = b.unit_id AND o.id <> b.id AND o.ends_at < clock_timestamp() AND o.starts_at < b.starts_at
				   AND (SELECT e.kind FROM rental_booking_events e WHERE e.booking_id = o.id
				          AND e.kind IN ('held', 'confirmed', 'checked_out', 'returned', 'cancelled', 'no_show')
				        ORDER BY e.seq DESC LIMIT 1) = 'checked_out') AS at_risk) x
			WHERE (`+rentalOverdue+`) OR (`+rentalReturnToday+`) OR (`+rentalHandoverToday+`)
			   OR (`+rentalState+` = 'confirmed' AND x.at_risk)
			ORDER BY 5, b.number`)
		if err != nil {
			return fmt.Errorf("read the rental desk's day: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			it := ActionItem{Module: access.Rental, SubjectKind: "rental_booking"}
			if err := rows.Scan(&it.Kind, &it.SubjectID, &it.Number, &it.Text, &it.Due); err != nil {
				return err
			}
			it.Urgency = UrgencyNow
			if it.Kind == "rental_return" {
				it.Urgency = UrgencySoon
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// countRentals counts the rental bookings for every card that counts them,
// in one statement: the counts in the cards' order.
func countRentals(ctx context.Context, tx pgx.Tx, cards []CardSource) ([]int, error) {
	if len(cards) == 0 {
		return nil, nil
	}
	cols := make([]string, len(cards))
	for i, c := range cards {
		cols[i] = `count(*) FILTER (WHERE ` + c.RentalWhere + `)`
	}
	counts := make([]int, len(cards))
	dest := make([]any, len(cards))
	for i := range counts {
		dest[i] = &counts[i]
	}
	err := tx.QueryRow(ctx, `SELECT `+strings.Join(cols, ", ")+`
		FROM rental_bookings b JOIN resource_blocks k ON k.id = b.block_id CROSS JOIN `+rentalToday).Scan(dest...)
	return counts, err
}

// rentalOpenBusinessTx is what is still open in Rental when somebody
// switches it off: every booking held, confirmed or out.
func rentalOpenBusinessTx(ctx context.Context, tx pgx.Tx, open *OpenBusiness) (*OpenBusiness, error) {
	rows, err := tx.Query(ctx, `
		SELECT b.number, coalesce(reg.registration, ''), u.fleet_number, b.starts_at, b.id::text
		FROM rental_bookings b
		JOIN resource_blocks k ON k.id = b.block_id
		JOIN rental_units u ON u.id = b.unit_id
		JOIN resources r ON r.id = u.resource_id
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
		WHERE `+rentalState+` IN ('held', 'confirmed', 'checked_out')
		ORDER BY b.starts_at, b.number`)
	if err != nil {
		return nil, fmt.Errorf("read open rentals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		it := OpenItem{Kind: "booking"}
		var at time.Time
		var id string
		if err := rows.Scan(&it.Number, &it.Registration, &it.What, &at, &id); err != nil {
			return nil, err
		}
		it.At, it.Link = &at, "/rental/bookings/"+id
		open.Items = append(open.Items, it)
	}
	return open, rows.Err()
}

// RentalDay is the rental desk's day: what goes out, what comes back, what
// is late. The rental desk and whoever runs the shop.
func RentalDay(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Rental); err != nil {
		return nil, err
	}
	return rentalWaiting(ctx, pool, scope)
}
