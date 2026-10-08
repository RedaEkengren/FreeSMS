package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Capacity is what limits a span of days: people and lifts. A technician
// away for the day takes their row's capacity with them; a closed day takes
// the shop's; the lifts limit how many cars are worked on at once, whoever
// is free.
type Capacity struct {
	// Absent: user ID, then the day as 2006-01-02, to the reason.
	Absent map[string]map[string]string
	// Closed: the day as 2006-01-02, to the reason.
	Closed map[string]string
	// Lifts, or zero for not limited.
	Lifts int
}

// AbsenceReasons are why somebody is away, as catalogue keys.
var AbsenceReasons = map[string]string{"sick": "off sick", "holiday": "on holiday", "other": "away"}

// CapacityBetween reads absences, closed days and lifts for a span.
func CapacityBetween(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) (Capacity, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Capacity{}, access.ErrForbidden
	}
	c := Capacity{Absent: map[string]map[string]string{}, Closed: map[string]string{}}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var lifts *int
		if err := tx.QueryRow(ctx, `SELECT lifts FROM shops WHERE id = $1`, scope.ShopID).Scan(&lifts); err != nil {
			return err
		}
		if lifts != nil {
			c.Lifts = *lifts
		}
		rows, err := tx.Query(ctx, `SELECT user_id, day, reason FROM staff_absences WHERE day >= $1::date AND day < $2::date`,
			from.Format("2006-01-02"), to.Format("2006-01-02"))
		if err != nil {
			return err
		}
		for rows.Next() {
			var user, reason string
			var day time.Time
			if err := rows.Scan(&user, &day, &reason); err != nil {
				rows.Close()
				return err
			}
			if c.Absent[user] == nil {
				c.Absent[user] = map[string]string{}
			}
			c.Absent[user][day.Format("2006-01-02")] = reason
		}
		rows.Close()
		rows, err = tx.Query(ctx, `SELECT day, reason FROM shop_closures WHERE day >= $1::date AND day < $2::date`,
			from.Format("2006-01-02"), to.Format("2006-01-02"))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var day time.Time
			var reason string
			if err := rows.Scan(&day, &reason); err != nil {
				return err
			}
			c.Closed[day.Format("2006-01-02")] = reason
		}
		return rows.Err()
	})
	return c, err
}

// SetAbsence records somebody away for a day. The bookings on their row stay,
// and show as needing somebody else.
func SetAbsence(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string, day time.Time, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if _, ok := AbsenceReasons[reason]; !ok {
		return fmt.Errorf("%w: %q is not a reason to be away", ErrInvalid, reason)
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO staff_absences (shop_id, user_id, day, reason, created_by)
			SELECT $1, u.id, $3::date, $4, $5 FROM users u WHERE u.id = $2
			ON CONFLICT (user_id, day) DO UPDATE SET reason = excluded.reason`,
			scope.ShopID, userID, day.Format("2006-01-02"), reason, scope.UserID)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
}

// ClearAbsence takes it back: they came in after all.
func ClearAbsence(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string, day time.Time) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM staff_absences WHERE user_id = $1 AND day = $2::date`, userID, day.Format("2006-01-02"))
		return err
	})
}

// CloseDay shuts the shop for a day. Whoever runs the shop decides that.
func CloseDay(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, day time.Time, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: say why the shop is shut", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO shop_closures (shop_id, day, reason, created_by) VALUES ($1, $2::date, $3, $4)`,
			scope.ShopID, day.Format("2006-01-02"), reason, scope.UserID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: the shop is already shut that day", ErrInvalid)
		}
		return err
	})
}

// OpenDay opens a day that was marked shut.
func OpenDay(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, day time.Time) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM shop_closures WHERE day = $1::date`, day.Format("2006-01-02"))
		return err
	})
}

// SetLifts says how many cars can be worked on at once; zero is not limited.
func SetLifts(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, lifts int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if lifts < 0 {
		return fmt.Errorf("%w: lifts are not negative", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE shops SET lifts = nullif($2, 0) WHERE id = $1`, scope.ShopID, lifts)
		return err
	})
}

// shopLocationTx is the shop's zone, for a question about its calendar.
func shopLocationTx(ctx context.Context, tx pgx.Tx) (*time.Location, error) {
	var zone string
	if err := tx.QueryRow(ctx, `SELECT timezone FROM shops LIMIT 1`).Scan(&zone); err != nil {
		return nil, fmt.Errorf("read timezone: %w", err)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.UTC, nil
	}
	return loc, nil
}

// closedTx refuses a booking on a day the shop is shut.
func closedTx(ctx context.Context, tx pgx.Tx, starts time.Time) error {
	loc, err := shopLocationTx(ctx, tx)
	if err != nil {
		return err
	}
	var reason string
	err = tx.QueryRow(ctx, `SELECT reason FROM shop_closures WHERE day = $1::date`, starts.In(loc).Format("2006-01-02")).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: the shop is shut that day (%s)", ErrInvalid, reason)
}
