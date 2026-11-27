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

// Time set aside for a service that falls due (#205): the first free stretch
// before the due date, long enough, as a held block on the unit's resource. It
// never takes time from anything booked, and when there is none it moves and
// cancels nothing: the service is left for somebody to decide on.

// ServiceHorizonDays is how far ahead a service that falls due is set aside for.
const ServiceHorizonDays = 30

type busyPeriod struct {
	from, to time.Time
	open     bool
}

// firstFreeWindow is the earliest start from from at which dur fits before
// deadline without touching any of the busy periods, and whether there is one.
func firstFreeWindow(busy []busyPeriod, from, deadline time.Time, dur time.Duration) (time.Time, bool) {
	slices.SortFunc(busy, func(a, b busyPeriod) int { return a.from.Compare(b.from) })
	cur := from
	for _, b := range busy {
		if !b.open && !b.to.After(cur) {
			continue
		}
		if !b.from.Before(cur.Add(dur)) {
			break
		}
		if b.open {
			return time.Time{}, false
		}
		cur = b.to
	}
	if cur.Add(dur).After(deadline) {
		return time.Time{}, false
	}
	return cur, true
}

// ServicesDueSoon lists what falls due within the horizon, soonest first, and
// whether time is set aside for each. For the rental desk. Reading is Core's.
func ServicesDueSoon(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, horizonDays int) ([]ServiceRow, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []ServiceRow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = dueSoonTx(ctx, tx, horizonDays, time.Now())
		return err
	})
	return out, err
}

func dueSoonTx(ctx context.Context, tx pgx.Tx, horizonDays int, now time.Time) ([]ServiceRow, error) {
	rows, err := serviceRowsTx(ctx, tx, "", now)
	if err != nil {
		return nil, err
	}
	loc, err := shopLocationTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	n := now.In(loc)
	limit := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, horizonDays+1)
	var out []ServiceRow
	for _, r := range rows {
		if r.DueOn != nil && r.DueOn.Before(limit) {
			out = append(out, r)
		}
	}
	return out, nil
}

// PlanDueServices sets time aside for every service that falls due within the
// horizon and has none: a held block in the first free stretch before the due
// date, held until that date. Services for which there is no stretch are
// returned, with nothing moved or cancelled, for somebody who plans to decide
// on. Run twice it sets nothing aside twice. For the rental desk, in a shop
// that runs the rental module.
func PlanDueServices(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, horizonDays int) (planned int, unplaced []ServiceRow, err error) {
	if !scope.Role.HandsOverRentals() {
		return 0, nil, access.ErrForbidden
	}
	if err := scope.Require(access.Rental); err != nil {
		return 0, nil, err
	}
	if horizonDays < 1 || horizonDays > 365 {
		return 0, nil, fmt.Errorf("%w: from 1 to 365 days ahead", ErrInvalid)
	}
	now := time.Now()
	var due []ServiceRow
	var loc *time.Location
	if err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if due, err = dueSoonTx(ctx, tx, horizonDays, now); err != nil {
			return err
		}
		loc, err = shopLocationTx(ctx, tx)
		return err
	}); err != nil {
		return 0, nil, err
	}
	for _, r := range due {
		if r.Planned() {
			continue
		}
		id, err := planOne(ctx, pool, scope, r, now, loc)
		if err != nil {
			return planned, unplaced, err
		}
		if id == "" {
			unplaced = append(unplaced, r)
			continue
		}
		planned++
	}
	return planned, unplaced, nil
}

// planOne sets time aside for one row, or returns "" when there is none to be
// had. The block is booked first, with a key of its own so a repeat finds it,
// and the row saying what it is for written after.
func planOne(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, r ServiceRow, now time.Time, loc *time.Location) (string, error) {
	n := now.In(loc)
	// From eight the next morning, to the end of the due day; a service already
	// due or past is looked for a fortnight ahead.
	from := time.Date(n.Year(), n.Month(), n.Day(), 8, 0, 0, 0, loc)
	if !from.After(now) {
		from = from.AddDate(0, 0, 1)
	}
	deadline := time.Date(r.DueOn.Year(), r.DueOn.Month(), r.DueOn.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	if !deadline.After(from.Add(r.Duration)) {
		deadline = from.AddDate(0, 0, 14)
	}
	blocks, err := ResourceBlocks(ctx, pool, scope, r.ResourceID, now, deadline)
	if err != nil {
		return "", err
	}
	var busy []busyPeriod
	for _, b := range blocks {
		if !b.Active(now) {
			continue
		}
		busy = append(busy, busyPeriod{from: b.From, to: b.To.UTC(), open: b.To.IsZero()})
	}
	start, ok := firstFreeWindow(busy, from, deadline, r.Duration)
	if !ok {
		return "", nil
	}
	hold := deadline
	id, err := BookResource(ctx, pool, scope, ResourceBooking{
		ResourceID: r.ResourceID, Kind: BlockService, From: start, To: start.Add(r.Duration), HoldUntil: &hold,
		Key:    "service:" + r.PlanID + ":" + r.DueOn.Format("2006-01-02"),
		Reason: "Service due",
	})
	if err != nil {
		// Something took the time between looking and booking: no stretch.
		if isTaken(err) {
			return "", nil
		}
		return "", err
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO fleet_service_blocks (shop_id, plan_id, block_id, due_on, created_by) VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid)
			ON CONFLICT (block_id) DO NOTHING`, scope.ShopID, r.PlanID, id, r.DueOn.Format("2006-01-02"), scope.UserID)
		return err
	})
	return id, err
}

func isTaken(err error) bool { return err != nil && errors.Is(err, ErrResourceTaken) }

// serviceWaiting is the queue's part of this (#158): how many services fall due
// within the horizon with no time set aside, for the rental desk. One count; the
// page it opens says which.
func serviceWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, nil
	}
	var out []ActionItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := dueSoonTx(ctx, tx, ServiceHorizonDays, time.Now())
		if err != nil {
			return err
		}
		unplanned := 0
		for _, r := range rows {
			if !r.Planned() {
				unplanned++
			}
		}
		if unplanned > 0 {
			out = append(out, ActionItem{Kind: "service_due", Module: access.Rental, SubjectKind: "rental_service", Count: unplanned, Urgency: UrgencySoon})
		}
		return nil
	})
	return out, err
}
