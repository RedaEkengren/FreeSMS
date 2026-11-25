package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// A seasonal change as a job (#229): a set taken out of storage is held by the
// job until it is fitted or put back, so two jobs cannot fit the same set; the
// set that came off the car is checked in to a place, with every tyre measured,
// or the technician says why not.

var (
	errJobSettled      = fmt.Errorf("%w: that job is finished; its tyres cannot be moved on it", ErrInvalid)
	errSetHeld         = fmt.Errorf("%w: another job holds that set", ErrInvalid)
	errSetNotHeld      = fmt.Errorf("%w: that job does not hold the set", ErrInvalid)
	errSetNotOnThisCar = fmt.Errorf("%w: that set is on another car", ErrInvalid)
	errCheckIn         = fmt.Errorf("%w: a set checked in has a place, or is taken home, or the reason it has neither; and every tyre measured on this job, or the reason it was not", ErrInvalid)
	errJobTyreWhy      = fmt.Errorf("%w: a reason is at most 300 characters", ErrInvalid)
)

// JobTyreMove is one thing that happened to a set on a job.
type JobTyreMove struct {
	SetID    string
	Set      string
	Kind     string
	Location string
	Why      string
	At       time.Time
	By       string
}

// CheckIn says what became of the set that came off the car.
type CheckIn struct {
	// Where it was put; empty when the customer took it home or there is no place.
	LocationID string
	TakenHome  bool
	// Why there is no place, or why a tyre was not measured.
	Why string
}

type jobFacts struct {
	vehicle  string
	customer string
	opened   time.Time
}

// jobTx reads an open job and the car and customer it is for.
func jobTx(ctx context.Context, tx pgx.Tx, jobID string) (jobFacts, error) {
	var f jobFacts
	var state State
	if err := tx.QueryRow(ctx, `SELECT state, vehicle_id, customer_id, opened_at FROM work_orders WHERE id = $1`, jobID).Scan(&state, &f.vehicle, &f.customer, &f.opened); errors.Is(err, pgx.ErrNoRows) {
		return f, ErrNotFound
	} else if err != nil {
		return f, err
	}
	if settled(state) {
		return f, errJobSettled
	}
	return f, nil
}

// lastHoldTx says which job holds a set now: the newest job move of the set,
// when it is a check-out. The set is locked for the transaction, so two jobs
// asking at once are told apart.
func lastHoldTx(ctx context.Context, tx pgx.Tx, setID string) (job string, err error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "tyreset:"+setID); err != nil {
		return "", err
	}
	if err := setLiveTx(ctx, tx, setID); err != nil {
		return "", err
	}
	var kind, wo string
	err = tx.QueryRow(ctx, `SELECT kind, work_order_id FROM tyre_job_moves WHERE tyre_set_id = $1 ORDER BY seq DESC LIMIT 1`, setID).Scan(&kind, &wo)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != "checked_out") {
		return "", nil
	}
	return wo, err
}

func jobMoveTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, setID, kind, location, why string) error {
	_, err := tx.Exec(ctx, `INSERT INTO tyre_job_moves (shop_id, work_order_id, tyre_set_id, kind, location_id, why, by_user)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, nullif($6, ''), NULLIF($7, '')::uuid)`, scope.ShopID, jobID, setID, kind, location, why, scope.UserID)
	return err
}

func checkJobTyreIDs(jobID, setID string) error {
	if !looksLikeUUID(jobID) || !looksLikeUUID(setID) {
		return ErrNotFound
	}
	return nil
}

// CheckOutTyreSet takes a set out of its place for a job, which holds it until
// the set is fitted or put back. Whoever works in the workshop.
func CheckOutTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, setID string) error {
	if !scope.Role.WorksInTheWorkshop() {
		return access.ErrForbidden
	}
	if err := checkJobTyreIDs(jobID, setID); err != nil {
		return err
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := jobTx(ctx, tx, jobID); err != nil {
			return err
		}
		held, err := lastHoldTx(ctx, tx, setID)
		if err != nil {
			return err
		}
		if held != "" {
			return errSetHeld
		}
		if err := takeOutTx(ctx, tx, scope, setID); err != nil {
			return err
		}
		return jobMoveTx(ctx, tx, scope, jobID, setID, "checked_out", "", "")
	})
}

// FitTyreSet puts the set the job holds on the job's car, which its owner must
// have. Whoever works in the workshop.
func FitTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, setID string) error {
	if !scope.Role.WorksInTheWorkshop() {
		return access.ErrForbidden
	}
	if err := checkJobTyreIDs(jobID, setID); err != nil {
		return err
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		job, err := jobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		held, err := lastHoldTx(ctx, tx, setID)
		if err != nil {
			return err
		}
		if held != jobID {
			return errSetNotHeld
		}
		var owner string
		if err := tx.QueryRow(ctx, `SELECT customer_id FROM tyre_sets WHERE id = $1`, setID).Scan(&owner); err != nil {
			return err
		}
		ok, err := ownsVehicleTx(ctx, tx, owner, job.vehicle)
		if err != nil {
			return err
		}
		if !ok {
			return errTyreVehicle
		}
		if err := putTyreSetTx(ctx, tx, scope, setID, job.vehicle); err != nil {
			return err
		}
		return jobMoveTx(ctx, tx, scope, jobID, setID, "fitted", "", "")
	})
}

// ReturnTyreSet puts a set the job holds but did not fit back in a place.
// Whoever works in the workshop.
func ReturnTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, setID, locationID string) error {
	if !scope.Role.WorksInTheWorkshop() {
		return access.ErrForbidden
	}
	if err := checkJobTyreIDs(jobID, setID); err != nil {
		return err
	}
	if !looksLikeUUID(locationID) {
		return ErrNotFound
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := jobTx(ctx, tx, jobID); err != nil {
			return err
		}
		held, err := lastHoldTx(ctx, tx, setID)
		if err != nil {
			return err
		}
		if held != jobID {
			return errSetNotHeld
		}
		if err := placeTx(ctx, tx, scope, setID, locationID); err != nil {
			return err
		}
		return jobMoveTx(ctx, tx, scope, jobID, setID, "returned", locationID, "")
	})
}

// CheckInTyreSet takes in the set that came off the job's car: into a place or
// taken home, with every tyre measured on this job or the reason it was not.
// Whoever works in the workshop.
func CheckInTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, setID string, c CheckIn) error {
	if !scope.Role.WorksInTheWorkshop() {
		return access.ErrForbidden
	}
	if err := checkJobTyreIDs(jobID, setID); err != nil {
		return err
	}
	c.Why = strings.TrimSpace(c.Why)
	if utf8.RuneCountInString(c.Why) > 300 {
		return errJobTyreWhy
	}
	if c.LocationID != "" && !looksLikeUUID(c.LocationID) {
		return ErrNotFound
	}
	if c.LocationID != "" && c.TakenHome {
		return errCheckIn
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		job, err := jobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		held, err := lastHoldTx(ctx, tx, setID)
		if err != nil {
			return err
		}
		if held != "" {
			return errSetHeld
		}
		var current *string
		if err := tx.QueryRow(ctx, `SELECT vehicle_id::text FROM tyre_set_vehicle_events WHERE tyre_set_id = $1 ORDER BY seq DESC LIMIT 1`, setID).Scan(&current); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current != nil && *current != job.vehicle {
			return errSetNotOnThisCar
		}
		// A set that came off a car is measured on the way in, or the technician says why not.
		var unmeasured int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM tyres t
			WHERE t.tyre_set_id = $1 AND NOT EXISTS (SELECT 1 FROM tyres r WHERE r.replaces_id = t.id)
			  AND NOT EXISTS (SELECT 1 FROM tyre_measurements m WHERE m.tyre_id = t.id AND m.measured_at >= $2)`, setID, job.opened).Scan(&unmeasured); err != nil {
			return err
		}
		if (c.LocationID == "" && !c.TakenHome && c.Why == "") || (unmeasured > 0 && c.Why == "") {
			return errCheckIn
		}
		if c.LocationID != "" {
			if err := placeTx(ctx, tx, scope, setID, c.LocationID); err != nil {
				return err
			}
		} else if err := takeOutIfStoredTx(ctx, tx, scope, setID); err != nil {
			return err
		}
		if err := putTyreSetTx(ctx, tx, scope, setID, ""); err != nil {
			return err
		}
		kind := "checked_in"
		if c.TakenHome {
			kind = "taken_home"
		}
		return jobMoveTx(ctx, tx, scope, jobID, setID, kind, c.LocationID, c.Why)
	})
}

// takeOutIfStoredTx takes a set out of its place when it is in one.
func takeOutIfStoredTx(ctx context.Context, tx pgx.Tx, scope access.Scope, setID string) error {
	err := takeOutTx(ctx, tx, scope, setID)
	if errors.Is(err, errNotStored) {
		return nil
	}
	return err
}

// JobTyreMoves lists what has happened to sets on a job, oldest first. The
// owner of a set is not part of it.
func JobTyreMoves(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]JobTyreMove, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	var out []JobTyreMove
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_orders WHERE id = $1)`, jobID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, `
			SELECT m.tyre_set_id, trim(concat_ws(' ', s.brand, s.model)), m.kind, coalesce(l.name, ''), coalesce(m.why, ''), m.at, coalesce(pe.display_name, '')
			FROM tyre_job_moves m
			JOIN tyre_sets s ON s.id = m.tyre_set_id
			LEFT JOIN storage_locations l ON l.id = m.location_id
			LEFT JOIN users u ON u.id = m.by_user LEFT JOIN people pe ON pe.id = u.person_id
			WHERE m.work_order_id = $1 ORDER BY m.seq`, jobID)
		if err != nil {
			return fmt.Errorf("read the job's tyres: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m JobTyreMove
			if err := rows.Scan(&m.SetID, &m.Set, &m.Kind, &m.Location, &m.Why, &m.At, &m.By); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// errTyresLoose is a car that cannot be called ready while a tyre set the job
// took out is neither on the car nor back in a place, or the set that came off
// has not been taken in.
var errTyresLoose = fmt.Errorf("%w: a tyre set is not stored and placed yet: a set the job took out is not fitted or put back, or the set that came off the car is not checked in", ErrInvalid)

// looseTyresTx says whether a tyre change on the job is unfinished. Only a job
// with tyre moves is asked: a set the job holds and has not fitted or put
// back, or, once a set is fitted, another set still counted as on the car.
func looseTyresTx(ctx context.Context, tx pgx.Tx, jobID string) (bool, error) {
	var loose bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM (SELECT DISTINCT tyre_set_id FROM tyre_job_moves WHERE work_order_id = $1) s
		    WHERE EXISTS (SELECT 1 FROM (SELECT m.kind, m.work_order_id FROM tyre_job_moves m WHERE m.tyre_set_id = s.tyre_set_id ORDER BY m.seq DESC LIMIT 1) last
		                  WHERE last.kind = 'checked_out' AND last.work_order_id = $1)
		) OR (
		    EXISTS (SELECT 1 FROM tyre_job_moves WHERE work_order_id = $1 AND kind = 'fitted')
		    AND EXISTS (
		        SELECT 1 FROM tyre_sets ts
		        JOIN work_orders w ON w.id = $1
		        WHERE ts.id NOT IN (SELECT tyre_set_id FROM tyre_job_moves WHERE work_order_id = $1 AND kind = 'fitted')
		          AND (SELECT e.vehicle_id FROM tyre_set_vehicle_events e WHERE e.tyre_set_id = ts.id ORDER BY e.seq DESC LIMIT 1) = w.vehicle_id
		    )
		)`, jobID).Scan(&loose)
	return loose, err
}
