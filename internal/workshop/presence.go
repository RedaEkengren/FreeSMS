package workshop

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Where the car is, as a ledger of what happened to it: left with us,
// collected, brought back, expected back on another day. A job waiting for a
// part said nothing about whether the car was on the lift or at home until
// Thursday.

// presenceColumns are the car's whereabouts for a work order w, for any query
// that lists jobs. Away is the latest row being a collection or a new date
// for one; the expected return is the latest date given since the car left.
const presenceColumns = `
	coalesce((SELECT p.event IN ('collected', 'rebooked') FROM vehicle_presence p
	           WHERE p.work_order_id = w.id ORDER BY p.at DESC, p.id DESC LIMIT 1), false) AS car_away,
	(SELECT p.expected_back FROM vehicle_presence p
	  WHERE p.work_order_id = w.id AND p.expected_back IS NOT NULL
	    AND p.at >= coalesce((SELECT max(c.at) FROM vehicle_presence c
	                           WHERE c.work_order_id = w.id AND c.event = 'collected'), '-infinity')
	  ORDER BY p.at DESC LIMIT 1) AS expected_back`

// PresenceEvent is one thing that happened to the car.
type PresenceEvent struct {
	Event        string // left, collected, returned, rebooked
	ExpectedBack *time.Time
	At           time.Time
	By           string
}

// Presence is where the car is and how it got there.
type Presence struct {
	Here         bool
	ExpectedBack *time.Time
	Events       []PresenceEvent
}

// PresenceFor reads a job's car's whereabouts.
//
// Any role: a technician needs to know whether the car is here before going
// to look for it, and nothing in it is about the customer.
func PresenceFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (Presence, error) {
	var p Presence
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = presenceTx(ctx, tx, jobID)
		return err
	})
	return p, err
}

func presenceTx(ctx context.Context, tx pgx.Tx, jobID string) (Presence, error) {
	p := Presence{Here: true}
	rows, err := tx.Query(ctx, `
		SELECT v.event, v.expected_back, v.at, coalesce(pe.display_name, '')
		FROM vehicle_presence v
		LEFT JOIN users u ON u.id = v.recorded_by
		LEFT JOIN people pe ON pe.id = u.person_id
		WHERE v.work_order_id = $1
		ORDER BY v.at, v.id`, jobID)
	if err != nil {
		return p, fmt.Errorf("read presence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e PresenceEvent
		if err := rows.Scan(&e.Event, &e.ExpectedBack, &e.At, &e.By); err != nil {
			return p, fmt.Errorf("scan presence: %w", err)
		}
		switch e.Event {
		case "collected":
			p.Here, p.ExpectedBack = false, e.ExpectedBack
		case "rebooked":
			p.ExpectedBack = e.ExpectedBack
		case "left", "returned":
			p.Here, p.ExpectedBack = true, nil
		}
		p.Events = append(p.Events, e)
	}
	return p, rows.Err()
}

// CarCollected records the customer taking the car, finished or not.
// expectedBack is when an unfinished job's car is due back, if anybody knows.
//
// The front desk's: handing a car back is a conversation at the counter.
func CarCollected(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, expectedBack *time.Time) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return presenceEvent(ctx, pool, scope, jobID, "collected", expectedBack)
}

// CarReturned records the car brought back for the same job.
func CarReturned(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return presenceEvent(ctx, pool, scope, jobID, "returned", nil)
}

// CarRebooked moves when a car that is away is expected back. A row of its
// own, so the shop can see the date moved, and when.
func CarRebooked(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, expectedBack time.Time) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return presenceEvent(ctx, pool, scope, jobID, "rebooked", &expectedBack)
}

func presenceEvent(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, event string, expectedBack *time.Time) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The order row serialises two people at the counter recording the
		// same car at once, so the second reads what the first wrote.
		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).
			Scan(&state); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}
		p, err := presenceTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		switch {
		case event == "collected" && !p.Here:
			return fmt.Errorf("%w: the car has already been collected", ErrInvalid)
		case event == "returned" && p.Here:
			return fmt.Errorf("%w: the car is here; it cannot come back without having left", ErrInvalid)
		case event == "rebooked" && p.Here:
			return fmt.Errorf("%w: the car is here; there is no return to move", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_presence (shop_id, work_order_id, event, expected_back, recorded_by)
			VALUES ($1, $2, $3, $4, $5)`, scope.ShopID, jobID, event, expectedBack, scope.UserID); err != nil {
			return fmt.Errorf("record presence: %w", err)
		}
		// Collecting a paid car is what finishes its job.
		if event == "collected" {
			return closeIfFinishedTx(ctx, tx, jobID)
		}
		return nil
	})
}
