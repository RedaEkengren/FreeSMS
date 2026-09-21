package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalid reports input a person can fix.
var ErrInvalid = errors.New("workshop: invalid")

// NormaliseRegistration strips what people vary and machines should not care
// about, so that "abc 12 d", "ABC12D" and "ABC-12D" find the same vehicle.
//
// The original is kept for display: a plate is shown the way it is written.
func NormaliseRegistration(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TakeIn opens a job for a vehicle arriving at the counter.
//
// The vehicle is created if it is not known. The customer is whoever owns it
// now, and is left empty when nobody does -- see the migration for why a job
// without a customer is allowed and an invoice without one is not.
func TakeIn(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, registration, complaint string, odometerKm *int64) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if strings.TrimSpace(complaint) == "" {
		return "", fmt.Errorf("%w: say what is wrong with it", ErrInvalid)
	}

	normalised := NormaliseRegistration(registration)
	var jobID string

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var vehicleID string

		if normalised != "" {
			err := tx.QueryRow(ctx, `
				SELECT vehicle_id FROM vehicle_registrations
				WHERE normalised = $1 AND valid_to IS NULL`, normalised).Scan(&vehicleID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("look up registration: %w", err)
			}
		}

		if vehicleID == "" {
			// Unknown vehicle, or none given. Either way a row is created: the
			// alternative is refusing to take the car in, and the car is
			// already here.
			if err := tx.QueryRow(ctx,
				`INSERT INTO vehicles (shop_id) VALUES ($1) RETURNING id`,
				scope.ShopID).Scan(&vehicleID); err != nil {
				return fmt.Errorf("create vehicle: %w", err)
			}
			if normalised != "" {
				if _, err := tx.Exec(ctx, `
					INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised)
					VALUES ($1, $2, $3, $4)`,
					scope.ShopID, vehicleID, strings.TrimSpace(registration), normalised); err != nil {
					return fmt.Errorf("record registration: %w", err)
				}
			}
		}

		var customerID *string
		var owner string
		err := tx.QueryRow(ctx, `
			SELECT customer_id FROM vehicle_ownership
			WHERE vehicle_id = $1 AND owned_to IS NULL`, vehicleID).Scan(&owner)
		if err == nil {
			customerID = &owner
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look up owner: %w", err)
		}

		// Order numbers are per shop and taken as the next one up. Proper
		// gap-free numbering under concurrency is the invoice numbering work;
		// a work order number is a label, not an accounting record, and two
		// people taking in cars at the same instant will collide on the unique
		// index and the second one retries. Do not copy this for invoices.
		var number int64
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(max(number), 0) + 1 FROM work_orders`).Scan(&number); err != nil {
			return fmt.Errorf("next number: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO work_orders (shop_id, number, vehicle_id, customer_id, complaint, state, created_by)
			VALUES ($1, $2, $3, $4, $5, 'draft', $6)
			RETURNING id`,
			scope.ShopID, number, vehicleID, customerID, strings.TrimSpace(complaint), scope.UserID,
		).Scan(&jobID); err != nil {
			return fmt.Errorf("open job: %w", err)
		}

		if odometerKm != nil {
			// Recorded even when it is lower than the last reading, and
			// flagged rather than refused: a replaced cluster is legitimate,
			// and a rolled-back odometer is something the shop wants on
			// record.
			var decreasing bool
			if err := tx.QueryRow(ctx, `
				SELECT exists(
				    SELECT 1 FROM odometer_readings
				    WHERE vehicle_id = $1 AND km > $2
				    ORDER BY read_at DESC LIMIT 1)`,
				vehicleID, *odometerKm).Scan(&decreasing); err != nil {
				return fmt.Errorf("compare odometer: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, decreasing)
				VALUES ($1, $2, $3, 'drop_off', $4)`,
				scope.ShopID, vehicleID, *odometerKm, decreasing); err != nil {
				return fmt.Errorf("record odometer: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return jobID, nil
}
