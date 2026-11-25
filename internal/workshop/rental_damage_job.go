package workshop

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

var errDamageNoCar = fmt.Errorf("%w: the case has no car with a registration to open a job on", ErrInvalid)

// DamageJob is a job opened for a damage case.
type DamageJob struct {
	WorkOrderID string
	Number      int64
	State       string
}

// CreateDamageJob opens a workshop job on the car a damage case is about, one
// step from the case: the shop's own customer is the job's, the description is
// its complaint, and the case and the job point at each other. The job blocks the
// car on the availability ledger as any job does, and is refused with the
// ledger's sentence when a rental is booked then, to be replanned by whoever
// runs the shop. It does not make the repair the renter's bill -- that is the
// case's steps and a return charge -- and it asks nothing of the renter. For the
// rental desk, in a shop that runs both the rental and the workshop.
func CreateDamageJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, caseID string) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Rental, access.Workshop); err != nil {
		return "", err
	}
	if !looksLikeUUID(caseID) {
		return "", ErrNotFound
	}
	var job string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The unit is the lock, as for everything done to it.
		c, err := damageCaseTx(ctx, tx, caseID, true)
		if err != nil {
			return err
		}
		var registration string
		err = tx.QueryRow(ctx, `
			SELECT coalesce(r.registration, '') FROM damage_cases d
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = d.vehicle_id AND r.valid_to IS NULL
			WHERE d.id = $1`, caseID).Scan(&registration)
		if err != nil {
			return err
		}
		if registration == "" {
			return errDamageNoCar
		}
		own, err := ownCustomerTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		job, err = takeInForTx(ctx, tx, scope, registration, c.Description, nil, Details{}, own)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO damage_case_jobs (shop_id, case_id, work_order_id, created_by) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`,
			scope.ShopID, caseID, job, scope.UserID)
		return err
	})
	return job, err
}

// DamageJobsOf lists the jobs opened for a case. For the rental desk. Reading
// is Core's: a shop that has switched the workshop off keeps the link as
// history.
func DamageJobsOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, caseID string) ([]DamageJob, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(caseID) {
		return nil, ErrNotFound
	}
	var out []DamageJob
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT w.id::text, w.number, w.state FROM damage_case_jobs j JOIN work_orders w ON w.id = j.work_order_id
			WHERE j.case_id = $1 ORDER BY j.created_at, j.id`, caseID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j DamageJob
			if err := rows.Scan(&j.WorkOrderID, &j.Number, &j.State); err != nil {
				return err
			}
			out = append(out, j)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}
