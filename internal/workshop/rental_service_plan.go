package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// When a fleet car is next due for service (#205). A plan is by months, by
// kilometres or both; what was last done is a row; the next due date is worked
// out, never written down.
//
// The rule, written once so it can be tested:
//   - By date: the day the kind was last done, or the day the plan was made if it
//     never was, plus the plan's months.
//   - By kilometres: the reading at the last time it was done, or the first
//     reading after the plan was made if it never was, plus the plan's kilometres.
//   - A kilometre limit is turned into a day by the unit's own pace: the readings
//     of the last year that were not lower than the one before them, from the
//     first to the last, over at least a fortnight, in kilometres a day. With
//     fewer readings, or a shorter stretch, or no kilometres driven, there is no
//     pace, and kilometres alone give no date -- the limit is then only "due" once
//     a reading is past it.
//   - A limit already passed is due today.
//   - The earlier of the two days is the next due date, and which one it was is said.

// The kinds of service.
var ServiceKinds = []string{"service", "inspection", "tyre_change", "other"}

var (
	errServicePlan = fmt.Errorf("%w: a plan is by months, by kilometres or both, and takes from 15 minutes to two weeks", ErrInvalid)
	errServiceKind = fmt.Errorf("%w: a service, an inspection, a tyre change or something else", ErrInvalid)
	errServiceDone = fmt.Errorf("%w: when it was done, and its kilometres if they are known", ErrInvalid)
)

// ServicePlan is what the shop plans for a unit.
type ServicePlan struct {
	ID              string
	Kind            string
	IntervalMonths  int // 0 when not by months
	IntervalKm      int // 0 when not by kilometres
	DurationMinutes int
	InUse           bool
	CreatedAt       time.Time
}

// SetServicePlan makes or changes a unit's plan for a kind of service: a new
// row that replaces the one standing, so what it said before is still there.
// A plan not in use is how one ends. For the rental desk, in a shop that runs
// the rental module.
func SetServicePlan(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string, p ServicePlan) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Rental); err != nil {
		return err
	}
	if !looksLikeUUID(unitID) {
		return ErrNotFound
	}
	if !slices.Contains(ServiceKinds, p.Kind) {
		return errServiceKind
	}
	if p.DurationMinutes < 15 || p.DurationMinutes > 20160 || p.IntervalMonths < 0 || p.IntervalMonths > 120 || p.IntervalKm < 0 || p.IntervalKm > 1_000_000 ||
		(p.IntervalKm != 0 && p.IntervalKm < 100) || (p.InUse && p.IntervalMonths == 0 && p.IntervalKm == 0) {
		return errServicePlan
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The unit is the lock, as for everything done to it.
		if _, err := lockResourceOfTx(ctx, tx, `SELECT resource_id FROM rental_units WHERE id = $1`, unitID); err != nil {
			return err
		}
		var prev *string
		var id string
		if err := tx.QueryRow(ctx, `
			SELECT p.id::text FROM fleet_service_plans p
			WHERE p.unit_id = $1 AND p.kind = $2 AND NOT EXISTS (SELECT 1 FROM fleet_service_plans n WHERE n.replaces_id = p.id)`, unitID, p.Kind).Scan(&id); err == nil {
			prev = &id
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO fleet_service_plans (shop_id, unit_id, kind, interval_months, interval_km, duration_minutes, in_use, replaces_id, created_by)
			VALUES ($1, $2, $3, NULLIF($4, 0), NULLIF($5, 0), $6, $7, $8, NULLIF($9, '')::uuid)`,
			scope.ShopID, unitID, p.Kind, p.IntervalMonths, p.IntervalKm, p.DurationMinutes, p.InUse, prev, scope.UserID)
		return err
	})
}

// ServicePlansOf lists the plans standing for a unit, one for each kind. For
// the rental desk. Reading is Core's.
func ServicePlansOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) ([]ServicePlan, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	var out []ServicePlan
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = servicePlansTx(ctx, tx, unitID)
		return err
	})
	return out, err
}

func servicePlansTx(ctx context.Context, tx pgx.Tx, unitID string) ([]ServicePlan, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.id::text, p.kind, coalesce(p.interval_months, 0), coalesce(p.interval_km, 0), p.duration_minutes, p.in_use, p.created_at
		FROM fleet_service_plans p
		WHERE p.unit_id = $1 AND NOT EXISTS (SELECT 1 FROM fleet_service_plans n WHERE n.replaces_id = p.id)
		ORDER BY p.kind`, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServicePlan
	for rows.Next() {
		var p ServicePlan
		if err := rows.Scan(&p.ID, &p.Kind, &p.IntervalMonths, &p.IntervalKm, &p.DurationMinutes, &p.InUse, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordServiceDone records that a kind of service was done on a unit, when and
// at how many kilometres if known, and for a job when it was done in the shop.
// The kilometres are a reading of the unit's car. The next due date follows from
// it. For the rental desk, in a shop that runs the rental module.
func RecordServiceDone(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID, kind string, doneOn time.Time, km *int64, jobID string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Rental); err != nil {
		return err
	}
	if !looksLikeUUID(unitID) || (jobID != "" && !looksLikeUUID(jobID)) {
		return ErrNotFound
	}
	if !slices.Contains(ServiceKinds, kind) {
		return errServiceKind
	}
	if doneOn.IsZero() || (km != nil && (*km < 0 || *km >= 10_000_000)) {
		return errServiceDone
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := lockResourceOfTx(ctx, tx, `SELECT resource_id FROM rental_units WHERE id = $1`, unitID); err != nil {
			return err
		}
		var vehicle *string
		if err := tx.QueryRow(ctx, `SELECT r.vehicle_id::text FROM rental_units u JOIN resources r ON r.id = u.resource_id WHERE u.id = $1`, unitID).Scan(&vehicle); err != nil {
			return err
		}
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		day := time.Date(doneOn.Year(), doneOn.Month(), doneOn.Day(), 0, 0, 0, 0, time.UTC)
		today := time.Now().In(loc)
		if day.After(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)) {
			return fmt.Errorf("%w: it cannot have been done in the future", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO fleet_service_done (shop_id, unit_id, kind, done_on, km, work_order_id, created_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid)`, scope.ShopID, unitID, kind, day, km, jobID, scope.UserID); err != nil {
			return err
		}
		if km != nil && vehicle != nil {
			at := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc)
			_, err := tx.Exec(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, read_at, source, decreasing)
				VALUES ($1, $2, $3, $4, 'service',
				        EXISTS (SELECT 1 FROM odometer_readings o WHERE o.vehicle_id = $2 AND o.read_at < $4 AND o.km > $3))`,
				scope.ShopID, *vehicle, *km, at)
			return err
		}
		return nil
	})
}

// ServiceDue is when a kind of service next falls due for a unit.
type ServiceDue struct {
	Kind string
	// The day it falls due, by whichever limit comes first; nil when neither
	// gives one (a kilometre limit with no pace to turn it into a day).
	DueOn *time.Time
	// Which gave it: "date" or "km".
	Basis string
	// The kilometre limit and the reading it is counted against, when the plan
	// has one.
	DueKm     *int64
	LatestKm  *int64
	KmPerDay  float64
	LastDone  *time.Time
	PlanID    string
	Duration  time.Duration
	Overdue   bool
	DateBased *time.Time
	KmBased   *time.Time
}

// paceOf is the unit's kilometres a day: from its readings of the last year
// that were not lower than the one before them, first to last, over at least a
// fortnight. Zero when there is no pace to speak of.
func paceOf(rs []reading) float64 {
	if len(rs) < 2 {
		return 0
	}
	first, last := rs[0], rs[len(rs)-1]
	days := last.at.Sub(first.at).Hours() / 24
	if days < 14 || last.km <= first.km {
		return 0
	}
	return float64(last.km-first.km) / days
}

type reading struct {
	at time.Time
	km int64
}

// nextDue works out one plan's next due date at the given moment. The readings
// are the unit's, oldest first, the ones flagged as lower than their predecessor
// already left out. It is the one place the rule above is written in code.
func nextDue(p ServicePlan, lastDone *time.Time, lastKm *int64, firstKmAfterPlan *int64, rs []reading, now time.Time, loc *time.Location) ServiceDue {
	d := ServiceDue{Kind: p.Kind, PlanID: p.ID, LastDone: lastDone, Duration: time.Duration(p.DurationMinutes) * time.Minute}
	today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	if p.IntervalMonths > 0 {
		base := time.Date(p.CreatedAt.In(loc).Year(), p.CreatedAt.In(loc).Month(), p.CreatedAt.In(loc).Day(), 0, 0, 0, 0, loc)
		if lastDone != nil {
			// A day, kept as a date: the zone it was read in must not move it.
			d := lastDone.UTC()
			base = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
		}
		due := base.AddDate(0, p.IntervalMonths, 0)
		d.DateBased = &due
	}
	if p.IntervalKm > 0 {
		var base *int64
		switch {
		case lastDone != nil && lastKm != nil:
			base = lastKm
		case lastDone == nil:
			base = firstKmAfterPlan
		}
		if base != nil {
			limit := *base + int64(p.IntervalKm)
			d.DueKm = &limit
		}
		if len(rs) > 0 {
			latest := rs[len(rs)-1].km
			d.LatestKm = &latest
		}
		if d.DueKm != nil && d.LatestKm != nil {
			d.KmPerDay = paceOf(rs)
			switch {
			case *d.LatestKm >= *d.DueKm:
				due := today
				d.KmBased = &due
			case d.KmPerDay > 0:
				days := int(float64(*d.DueKm-*d.LatestKm) / d.KmPerDay)
				due := today.AddDate(0, 0, days)
				d.KmBased = &due
			}
		}
	}
	switch {
	case d.DateBased != nil && (d.KmBased == nil || !d.KmBased.Before(*d.DateBased)):
		d.DueOn, d.Basis = d.DateBased, "date"
	case d.KmBased != nil:
		d.DueOn, d.Basis = d.KmBased, "km"
	}
	d.Overdue = d.DueOn != nil && d.DueOn.Before(today)
	return d
}

// ServiceDueFor lists what is due for a unit, soonest first, from the plans in
// use. For the rental desk. Reading is Core's.
func ServiceDueFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) ([]ServiceDue, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	var out []ServiceDue
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = serviceDueTx(ctx, tx, unitID, time.Now())
		return err
	})
	return out, err
}

func serviceDueTx(ctx context.Context, tx pgx.Tx, unitID string, now time.Time) ([]ServiceDue, error) {
	var vehicle *string
	if err := tx.QueryRow(ctx, `SELECT r.vehicle_id::text FROM rental_units u JOIN resources r ON r.id = u.resource_id WHERE u.id = $1`, unitID).Scan(&vehicle); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	loc, err := shopLocationTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	plans, err := servicePlansTx(ctx, tx, unitID)
	if err != nil {
		return nil, err
	}
	var rs []reading
	if vehicle != nil {
		rows, err := tx.Query(ctx, `
			SELECT read_at, km FROM odometer_readings
			WHERE vehicle_id = $1 AND NOT decreasing AND read_at >= $2::timestamptz - interval '365 days' AND read_at <= $2
			ORDER BY read_at`, *vehicle, now)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r reading
			if err := rows.Scan(&r.at, &r.km); err != nil {
				rows.Close()
				return nil, err
			}
			rs = append(rs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var out []ServiceDue
	for _, p := range plans {
		if !p.InUse {
			continue
		}
		var doneOn *time.Time
		var km *int64
		var on time.Time
		var k *int64
		err := tx.QueryRow(ctx, `SELECT done_on::timestamptz, km FROM fleet_service_done WHERE unit_id = $1 AND kind = $2 ORDER BY done_on DESC, created_at DESC LIMIT 1`, unitID, p.Kind).Scan(&on, &k)
		if err == nil {
			doneOn, km = &on, k
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var first *int64
		if vehicle != nil && doneOn == nil {
			var f int64
			if err := tx.QueryRow(ctx, `SELECT km FROM odometer_readings WHERE vehicle_id = $1 AND NOT decreasing AND read_at >= $2 ORDER BY read_at LIMIT 1`, *vehicle, p.CreatedAt).Scan(&f); err == nil {
				first = &f
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
		}
		out = append(out, nextDue(p, doneOn, km, first, rs, now, loc))
	}
	slices.SortStableFunc(out, func(a, b ServiceDue) int {
		switch {
		case a.DueOn == nil && b.DueOn == nil:
			return 0
		case a.DueOn == nil:
			return 1
		case b.DueOn == nil:
			return -1
		}
		return a.DueOn.Compare(*b.DueOn)
	})
	return out, nil
}
