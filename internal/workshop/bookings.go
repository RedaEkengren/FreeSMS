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

// A workshop books three weeks ahead and FreeSMS had no surface for it. A
// booking holds a slot for a car that is not here yet; it becomes a work
// order when the car arrives, and not before.

// Booking is a slot sold to a customer.
type Booking struct {
	ID              string
	Starts, Ends    time.Time
	TechnicianID    string
	Technician      string
	Registration    string
	CustomerName    string
	CustomerPhone   string
	What            string
	EstimateMinutes *int
	PartsNeeded     bool
	Status          string
	WorkOrderID     string
	CreatedAt       time.Time
	CreatedBy       string
	// The car is in and its job is not finished yet.
	JobOpen bool
}

// EstimateHours is the work expected, as a page writes hours.
func (b Booking) EstimateHours() string {
	if b.EstimateMinutes == nil {
		return ""
	}
	return Hours(*b.EstimateMinutes)
}

// SlotMinutes is the length of the slot that was sold.
func (b Booking) SlotMinutes() int { return int(b.Ends.Sub(b.Starts).Minutes()) }

// Overbooked reports work expected to take longer than the slot sold for
// it: the gap a planner is looking at.
func (b Booking) Overbooked() bool {
	return b.EstimateMinutes != nil && *b.EstimateMinutes > b.SlotMinutes()
}

// NewBooking is a booking as the counter takes it.
type NewBooking struct {
	Starts, Ends    time.Time
	TechnicianID    string
	Registration    string
	CustomerName    string
	CustomerPhone   string
	What            string
	EstimateMinutes *int
	PartsNeeded     bool
}

// BookingEvent is one thing that happened to a booking.
type BookingEvent struct {
	Event        string
	Starts, Ends *time.Time
	Note         string
	At           time.Time
	By           string
}

const bookingColumns = `
	b.id, b.starts_at, b.ends_at, coalesce(b.technician_id::text, ''), coalesce(tp.display_name, ''),
	coalesce(b.registration, ''), coalesce(b.customer_name, ''), coalesce(b.customer_phone, ''),
	b.what, b.estimate_minutes, b.parts_needed, b.status, coalesce(b.work_order_id::text, ''),
	b.created_at, coalesce(cp.display_name, ''),
	coalesce(w.state NOT IN ('ready', 'invoiced', 'closed', 'declined', 'cancelled'), false)`

const bookingFrom = `
	FROM bookings b
	LEFT JOIN users tu  ON tu.id = b.technician_id
	LEFT JOIN people tp ON tp.id = tu.person_id
	LEFT JOIN users cu  ON cu.id = b.created_by
	LEFT JOIN people cp ON cp.id = cu.person_id
	LEFT JOIN work_orders w ON w.id = b.work_order_id`

func scanBooking(row pgx.Row) (Booking, error) {
	var b Booking
	err := row.Scan(&b.ID, &b.Starts, &b.Ends, &b.TechnicianID, &b.Technician,
		&b.Registration, &b.CustomerName, &b.CustomerPhone, &b.What, &b.EstimateMinutes,
		&b.PartsNeeded, &b.Status, &b.WorkOrderID, &b.CreatedAt, &b.CreatedBy, &b.JobOpen)
	return b, err
}

func checkBooking(nb *NewBooking) error {
	nb.What = strings.TrimSpace(nb.What)
	nb.Registration = strings.ToUpper(strings.TrimSpace(nb.Registration))
	nb.CustomerName = strings.TrimSpace(nb.CustomerName)
	nb.CustomerPhone = strings.TrimSpace(nb.CustomerPhone)
	switch {
	case nb.What == "":
		return fmt.Errorf("%w: say what the booking is for", ErrInvalid)
	case !nb.Ends.After(nb.Starts):
		return fmt.Errorf("%w: a booking ends after it starts", ErrInvalid)
	case nb.Ends.Sub(nb.Starts) > 24*time.Hour:
		return fmt.Errorf("%w: a booking is at most a day; book the next day as well", ErrInvalid)
	case nb.EstimateMinutes != nil && *nb.EstimateMinutes <= 0:
		return fmt.Errorf("%w: an estimate is more than nothing", ErrInvalid)
	}
	if nb.TechnicianID != "" && !looksLikeUUID(nb.TechnicianID) {
		return fmt.Errorf("%w: no such technician", ErrInvalid)
	}
	return nil
}

// CreateBooking takes a booking. The counter's: it is a conversation with the
// customer, and it holds their name and number.
func CreateBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, nb NewBooking) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if err := checkBooking(&nb); err != nil {
		return "", err
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := closedTx(ctx, tx, nb.Starts); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO bookings (shop_id, starts_at, ends_at, technician_id, registration, customer_name,
			                      customer_phone, what, estimate_minutes, parts_needed, created_by)
			VALUES ($1, $2, $3, nullif($4, '')::uuid, nullif($5, ''), nullif($6, ''), nullif($7, ''), $8, $9, $10, $11)
			RETURNING id`,
			scope.ShopID, nb.Starts, nb.Ends, nb.TechnicianID, nb.Registration, nb.CustomerName,
			nb.CustomerPhone, nb.What, nb.EstimateMinutes, nb.PartsNeeded, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("create booking: %w", err)
		}
		return bookingEventTx(ctx, tx, scope, id, "booked", &nb.Starts, &nb.Ends, "")
	})
	return id, err
}

// MoveBooking gives a booking another time or another technician. A row of
// its own records the move.
func MoveBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, starts, ends time.Time, technicianID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	nb := NewBooking{Starts: starts, Ends: ends, TechnicianID: technicianID, What: "x"}
	if err := checkBooking(&nb); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := closedTx(ctx, tx, starts); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE bookings SET starts_at = $2, ends_at = $3, technician_id = nullif($4, '')::uuid
			WHERE id = $1 AND status = 'booked'`, id, starts, ends, technicianID)
		if err != nil {
			return fmt.Errorf("move booking: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return bookingEventTx(ctx, tx, scope, id, "moved", &starts, &ends, "")
	})
}

// CancelBooking cancels one, with the reason. It stays, as cancelled.
func CancelBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return closeBooking(ctx, pool, scope, id, "cancelled", reason)
}

// BookingNoShow records that the car did not come.
func BookingNoShow(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return closeBooking(ctx, pool, scope, id, "no_show", "")
}

func closeBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, status, note string) error {
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE bookings SET status = $2 WHERE id = $1 AND status = 'booked'`, id, status)
		if err != nil {
			return fmt.Errorf("close booking: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return bookingEventTx(ctx, tx, scope, id, status, nil, nil, strings.TrimSpace(note))
	})
}

// ArriveBooking records that the booked car came and was taken in as a work
// order. The booking is not the order: it ends here, pointing at it.
func ArriveBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, workOrderID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) || !looksLikeUUID(workOrderID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE bookings SET status = 'arrived', work_order_id = $2,
			       vehicle_id = (SELECT vehicle_id FROM work_orders WHERE id = $2)
			WHERE id = $1 AND status = 'booked'`, id, workOrderID)
		if err != nil {
			return fmt.Errorf("arrive booking: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return bookingEventTx(ctx, tx, scope, id, "arrived", nil, nil, "")
	})
}

func bookingEventTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id, event string, starts, ends *time.Time, note string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO booking_events (shop_id, booking_id, event, starts_at, ends_at, note, by_user)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7)`,
		scope.ShopID, id, event, starts, ends, note, scope.UserID)
	if err != nil {
		return fmt.Errorf("record booking event: %w", err)
	}
	return nil
}

// BookingsBetween lists the bookings in a span of time that are still held
// or have arrived, earliest first. Cancelled ones and no-shows are not on
// the planner; they are on the booking's own history.
func BookingsBetween(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) ([]Booking, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	return queryBookings(ctx, pool, scope, `
		WHERE b.starts_at < $2 AND b.ends_at > $1 AND b.status IN ('booked', 'arrived')
		ORDER BY b.starts_at`, from, to)
}

// SearchBookings finds bookings by registration, customer or what they are
// for, newest first. A calendar nobody can search is half a calendar.
func SearchBookings(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, q string) ([]Booking, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	compact := "%" + strings.ReplaceAll(strings.ToUpper(q), " ", "") + "%"
	return queryBookings(ctx, pool, scope, `
		WHERE replace(upper(b.registration), ' ', '') LIKE $1 OR b.customer_name ILIKE $2 OR b.what ILIKE $2
		   OR b.customer_phone ILIKE $2
		ORDER BY b.starts_at DESC LIMIT 50`, compact, like)
}

// BookingByID reads one booking.
func BookingByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Booking, []BookingEvent, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Booking{}, nil, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return Booking{}, nil, ErrNotFound
	}
	var b Booking
	var events []BookingEvent
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = scanBooking(tx.QueryRow(ctx, `SELECT`+bookingColumns+bookingFrom+` WHERE b.id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT e.event, e.starts_at, e.ends_at, coalesce(e.note, ''), e.at, coalesce(p.display_name, '')
			FROM booking_events e
			LEFT JOIN users u ON u.id = e.by_user
			LEFT JOIN people p ON p.id = u.person_id
			WHERE e.booking_id = $1 ORDER BY e.at`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e BookingEvent
			if err := rows.Scan(&e.Event, &e.Starts, &e.Ends, &e.Note, &e.At, &e.By); err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	return b, events, err
}

func queryBookings(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, where string, args ...any) ([]Booking, error) {
	var out []Booking
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT`+bookingColumns+bookingFrom+where, args...)
		if err != nil {
			return fmt.Errorf("list bookings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBooking(rows)
			if err != nil {
				return fmt.Errorf("scan booking: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// PlannerRow is somebody bookings can be given to.
type PlannerRow struct {
	ID   string
	Name string
}

// PlannerRows are the active technicians, one row each on the planner.
func PlannerRows(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]PlannerRow, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []PlannerRow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id, p.display_name FROM users u JOIN people p ON p.id = u.person_id
			WHERE u.active AND u.role = 'technician' ORDER BY p.display_name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r PlannerRow
			if err := rows.Scan(&r.ID, &r.Name); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
