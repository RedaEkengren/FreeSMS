package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Who is on an open job and when the rest of it is going to be done.
//
// Both are rows (job_assignments, job_plans), so a technician who was sick on
// Wednesday is still on record as having been on the job on Tuesday, and a
// repair that slipped to Thursday still shows that it was Wednesday first.
// Neither touches time_entries: hours already clocked stay with the person
// who clocked them, whoever the job is handed to.
//
// The plan is not the booking (the slot sold), not promised_at (what the
// customer was told) and not vehicle_presence (where the car is). Moving the
// shop's own plan must not move a promise somebody made on the phone.

// ErrPlanChanged is returned when somebody else changed the assignment or the
// plan between the page being drawn and it being submitted. Nothing is
// written; the caller shows the current state and the person decides again.
var ErrPlanChanged = errors.New("workshop: the plan was changed by somebody else; look at it again")

// ErrPlanClash is returned with the clashes when a plan collides with a rota,
// an absence, a closed day, another job or the lifts, and no reason was given
// for going ahead.
var ErrPlanClash = errors.New("workshop: the plan clashes; give a reason to go ahead")

// Clash is one way a plan does not fit the shop. Shown, never silently
// resolved: whether to ring the customer, ask somebody to stay late or take a
// lift from another car is a person's decision.
type Clash struct {
	// closed, absent, off_rota, off_shift, double or lifts.
	Kind string
	// Whose day it is, where it is somebody's.
	Who string
	// The shop-local day, as 2006-01-02, where it is a day's.
	Day string
	// What it collides with: the other job, or the reason the shop is shut.
	Detail string
}

// stored and parseClash write a clash into job_plans.clashes and back.
func (c Clash) stored() string {
	return strings.Join([]string{c.Kind, c.Who, c.Day, c.Detail}, "|")
}

func parseClash(s string) Clash {
	p := strings.SplitN(s, "|", 4)
	for len(p) < 4 {
		p = append(p, "")
	}
	return Clash{Kind: p[0], Who: p[1], Day: p[2], Detail: p[3]}
}

// Assignee is somebody on a job.
type Assignee struct {
	UserID string
	Name   string
	Since  time.Time
	// Released is set on the history, not on who is on it now.
	Released *time.Time
	Reason   string
	By       string
}

// Plan is one version of when the rest of a job is going to be done.
type Plan struct {
	Version          int
	Starts, Ends     *time.Time
	RemainingMinutes *int
	OnLift           bool
	Override         string
	Clashes          []Clash
	Note             string
	By               string
	At               time.Time
}

// RemainingHours is the work left as a page writes it.
func (p Plan) RemainingHours() string {
	if p.RemainingMinutes == nil {
		return ""
	}
	return Hours(*p.RemainingMinutes)
}

// JobPlan is everything about who and when for one job.
type JobPlan struct {
	// Who is on it now; Before is everybody who was, newest release first.
	On     []Assignee
	Before []Assignee
	// Current is the latest plan, or nil when nobody has made one.
	Current *Plan
	// Earlier plans, newest first, so a moved job shows where it was.
	Earlier []Plan
	// Clashes of the current plan against the shop as it is now: somebody
	// may have been taken sick since it was made.
	Clashes []Clash
	// Version is the number of the current plan, or zero: what a form
	// carries back, so a plan changed in between is noticed.
	Version int
}

// Staffed reports whether anybody is on the job.
func (p JobPlan) Staffed() bool { return len(p.On) > 0 }

// Names is who is on the job, in words.
func (p JobPlan) Names() string {
	var n []string
	for _, a := range p.On {
		n = append(n, a.Name)
	}
	return strings.Join(n, ", ")
}

// PlanInput is a plan as a planner proposes it.
type PlanInput struct {
	// Version is the plan version the planner was looking at; zero for none.
	Version int
	// Starts and Ends are both given or both empty.
	Starts, Ends     *time.Time
	RemainingMinutes *int
	OnLift           bool
	// Override is why the planner is going ahead despite clashes.
	Override string
	Note     string
}

// Assigning and scheduling are the counter's and the owner's. The technician
// sees their own plan; moving it is a conversation with the desk.

// lockJobTx locks the job's row for the rest of the transaction, so two
// planners are done one after the other, and says whether the job still
// takes work.
func lockJobTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	var state State
	err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock job: %w", err)
	}
	if !state.AcceptsWork() {
		return ErrFinished
	}
	return nil
}

// assignTechnicianTx puts somebody on a job, and keeps work_orders.assigned_to
// as the one who has been on it longest, which is what the previous release
// and the technician's own list read. Already on it is not an error.
func assignTechnicianTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, userID, reason string) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND active AND role = 'technician')`, userID).Scan(&ok); err != nil {
		return fmt.Errorf("check technician: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: choose an active technician", ErrInvalid)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_assignments (shop_id, work_order_id, user_id, assigned_by, reason)
		VALUES ($1, $2, $3, $4, nullif($5, ''))
		ON CONFLICT (work_order_id, user_id) WHERE released_at IS NULL DO NOTHING`,
		scope.ShopID, jobID, userID, scope.UserID, reason); err != nil {
		return fmt.Errorf("assign technician: %w", err)
	}
	return syncLeadTx(ctx, tx, jobID)
}

func releaseTechnicianTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, userID, reason string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE job_assignments SET released_at = clock_timestamp(), released_by = $3, reason = coalesce(nullif($4, ''), reason)
		WHERE work_order_id = $1 AND user_id = $2 AND released_at IS NULL`,
		jobID, userID, scope.UserID, reason)
	if err != nil {
		return false, fmt.Errorf("release technician: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, syncLeadTx(ctx, tx, jobID)
}

func syncLeadTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE work_orders SET assigned_to =
		  (SELECT a.user_id FROM job_assignments a
		    WHERE a.work_order_id = $1 AND a.released_at IS NULL ORDER BY a.assigned_at, a.id LIMIT 1)
		WHERE id = $1`, jobID)
	if err != nil {
		return fmt.Errorf("record who is answerable: %w", err)
	}
	return nil
}

// AssignTechnician puts a technician on a job, alongside anybody already on
// it. The reason is optional: "needs a second pair of hands".
func AssignTechnician(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, userID, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return ErrNotFound
	}
	if !looksLikeUUID(userID) {
		return fmt.Errorf("%w: choose a technician", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		return assignTechnicianTx(ctx, tx, scope, jobID, userID, strings.TrimSpace(reason))
	})
}

// ReleaseTechnician takes somebody off a job. Their clocked time stays theirs.
func ReleaseTechnician(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, userID, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(jobID) || !looksLikeUUID(userID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		done, err := releaseTechnicianTx(ctx, tx, scope, jobID, userID, strings.TrimSpace(reason))
		if err != nil {
			return err
		}
		if !done {
			return ErrPlanChanged
		}
		return nil
	})
}

// ReassignJob hands a job from one technician to another in one step: the
// first is released and the second put on, or neither. If the first is no
// longer on the job -- somebody else got there first -- nothing is written.
func ReassignJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, fromUserID, toUserID, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return ErrNotFound
	}
	if !looksLikeUUID(fromUserID) || !looksLikeUUID(toUserID) {
		return fmt.Errorf("%w: choose who it moves from and to", ErrInvalid)
	}
	reason = strings.TrimSpace(reason)
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		done, err := releaseTechnicianTx(ctx, tx, scope, jobID, fromUserID, reason)
		if err != nil {
			return err
		}
		if !done {
			return ErrPlanChanged
		}
		return assignTechnicianTx(ctx, tx, scope, jobID, toUserID, reason)
	})
}

// PlanFor reads who is on a job and its plan.
//
// Any role: a technician needs to know what they are on and when, and
// nothing in it is about the customer.
func PlanFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (JobPlan, error) {
	var p JobPlan
	if !looksLikeUUID(jobID) {
		return p, ErrNotFound
	}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if p, err = planTx(ctx, tx, jobID); err != nil {
			return err
		}
		if p.Current != nil && p.Current.Starts != nil {
			loc, err := shopLocationTx(ctx, tx)
			if err != nil {
				return err
			}
			p.Clashes, err = clashesTx(ctx, tx, jobID, p.On, *p.Current.Starts, *p.Current.Ends, p.Current.OnLift, loc)
			return err
		}
		return nil
	})
	return p, err
}

func planTx(ctx context.Context, tx pgx.Tx, jobID string) (JobPlan, error) {
	var p JobPlan
	rows, err := tx.Query(ctx, `
		SELECT a.user_id, coalesce(pe.display_name, ''), a.assigned_at, a.released_at,
		       coalesce(a.reason, ''), coalesce(bp.display_name, '')
		FROM job_assignments a
		JOIN users u ON u.id = a.user_id JOIN people pe ON pe.id = u.person_id
		LEFT JOIN users bu ON bu.id = coalesce(a.released_by, a.assigned_by)
		LEFT JOIN people bp ON bp.id = bu.person_id
		WHERE a.work_order_id = $1
		ORDER BY a.released_at DESC NULLS FIRST, a.assigned_at, a.id`, jobID)
	if err != nil {
		return p, fmt.Errorf("read assignments: %w", err)
	}
	for rows.Next() {
		var a Assignee
		if err := rows.Scan(&a.UserID, &a.Name, &a.Since, &a.Released, &a.Reason, &a.By); err != nil {
			rows.Close()
			return p, fmt.Errorf("scan assignment: %w", err)
		}
		if a.Released == nil {
			p.On = append(p.On, a)
		} else {
			p.Before = append(p.Before, a)
		}
	}
	rows.Close()

	prows, err := tx.Query(ctx, `
		SELECT p.version, p.starts_at, p.ends_at, p.remaining_minutes, p.on_lift,
		       coalesce(p.override_reason, ''), p.clashes, coalesce(p.note, ''),
		       coalesce(pe.display_name, ''), p.at
		FROM job_plans p
		LEFT JOIN users u ON u.id = p.by_user LEFT JOIN people pe ON pe.id = u.person_id
		WHERE p.work_order_id = $1 ORDER BY p.version DESC`, jobID)
	if err != nil {
		return p, fmt.Errorf("read plans: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var pl Plan
		var clashes []string
		if err := prows.Scan(&pl.Version, &pl.Starts, &pl.Ends, &pl.RemainingMinutes, &pl.OnLift,
			&pl.Override, &clashes, &pl.Note, &pl.By, &pl.At); err != nil {
			return p, fmt.Errorf("scan plan: %w", err)
		}
		for _, c := range clashes {
			pl.Clashes = append(pl.Clashes, parseClash(c))
		}
		if p.Current == nil {
			cur := pl
			p.Current = &cur
		} else {
			p.Earlier = append(p.Earlier, pl)
		}
	}
	if err := prows.Err(); err != nil {
		return p, err
	}
	if p.Current != nil {
		p.Version = p.Current.Version
	}
	return p, nil
}

// CheckPlan says what a plan would clash with, without writing it, for the
// page to show before the planner decides.
func CheckPlan(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, in PlanInput) ([]Clash, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	var out []Clash
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		p, err := planTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if in.Starts == nil {
			return nil
		}
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		out, err = clashesTx(ctx, tx, jobID, p.On, *in.Starts, *in.Ends, in.OnLift, loc)
		return err
	})
	return out, err
}

// PlanJob writes a new version of a job's plan.
//
// in.Version must be the version the planner saw: if another planner got in
// first, nothing is written and ErrPlanChanged says so, rather than the
// second silently overwriting the first. Clashes are returned and, unless a
// reason is given to go ahead, refused with ErrPlanClash; the reason and the
// clashes it was given against are kept on the row.
func PlanJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, in PlanInput) ([]Clash, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	if err := checkPlanInput(&in); err != nil {
		return nil, err
	}
	var clashes []Clash
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		p, err := planTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		latest := 0
		if p.Current != nil {
			latest = p.Current.Version
		}
		if in.Version != latest {
			return ErrPlanChanged
		}
		if in.Starts != nil {
			loc, err := shopLocationTx(ctx, tx)
			if err != nil {
				return err
			}
			if clashes, err = clashesTx(ctx, tx, jobID, p.On, *in.Starts, *in.Ends, in.OnLift, loc); err != nil {
				return err
			}
		}
		if len(clashes) > 0 && in.Override == "" {
			return ErrPlanClash
		}
		stored := make([]string, 0, len(clashes))
		for _, c := range clashes {
			stored = append(stored, c.stored())
		}
		reason := in.Override
		if len(clashes) == 0 {
			reason = ""
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_plans (shop_id, work_order_id, version, starts_at, ends_at, remaining_minutes,
			                       on_lift, override_reason, clashes, note, by_user)
			VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9, nullif($10, ''), $11)`,
			scope.ShopID, jobID, latest+1, in.Starts, in.Ends, in.RemainingMinutes,
			in.OnLift, reason, stored, in.Note, scope.UserID); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
		return nil
	})
	return clashes, err
}

// maxPlannedStretch is the longest a job can be planned to take.
const maxPlannedStretch = 366 * 24 * time.Hour

func checkPlanInput(in *PlanInput) error {
	in.Override = strings.TrimSpace(in.Override)
	in.Note = strings.TrimSpace(in.Note)
	switch {
	case (in.Starts == nil) != (in.Ends == nil):
		return fmt.Errorf("%w: give both when it starts and when it ends, or neither", ErrInvalid)
	case in.Starts != nil && !in.Ends.After(*in.Starts):
		return fmt.Errorf("%w: it has to end after it starts", ErrInvalid)
	case in.Starts != nil && in.Ends.Sub(*in.Starts) > maxPlannedStretch:
		// The planner walks the days between the two, so the length is bounded
		// by what a job can take, not by what a form can be made to say.
		return fmt.Errorf("%w: a plan cannot run for more than a year", ErrInvalid)
	case in.RemainingMinutes != nil && *in.RemainingMinutes < 0:
		return fmt.Errorf("%w: the work left cannot be negative", ErrInvalid)
	case len(in.Override) > 300 || len(in.Note) > 300:
		return fmt.Errorf("%w: keep the reason and the note short", ErrInvalid)
	}
	return nil
}

// CarryOver moves an unfinished job's plan forward by whole days until it
// ends in the future, skipping days the shop is shut: the same job, the same
// history, a later window. It is a new version of the plan and nothing else:
// no booking, no job, no change to the promise.
func CarryOver(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, version int, override string) ([]Clash, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	var in PlanInput
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		p, err := planTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if p.Current == nil || p.Current.Starts == nil {
			return fmt.Errorf("%w: there is no plan to carry over", ErrInvalid)
		}
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		starts, ends := *p.Current.Starts, *p.Current.Ends
		now := time.Now()
		// Straight to the day it would end in the future, then on past any
		// shut days. A plan that ran out years ago is one jump, not a loop
		// of a thousand.
		if !ends.After(now) {
			n := int(now.Sub(ends).Hours()/24) + 1
			starts, ends = addDays(starts, n, loc), addDays(ends, n, loc)
			for !ends.After(now) {
				starts, ends = addDays(starts, 1, loc), addDays(ends, 1, loc)
			}
		}
		for step := 0; step < 400 && shut(ctx, tx, starts, loc); step++ {
			starts, ends = addDays(starts, 1, loc), addDays(ends, 1, loc)
		}
		in = PlanInput{Version: version, Starts: &starts, Ends: &ends,
			RemainingMinutes: p.Current.RemainingMinutes, OnLift: p.Current.OnLift,
			Override: override, Note: p.Current.Note}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return PlanJob(ctx, pool, scope, jobID, in)
}

func shut(ctx context.Context, tx pgx.Tx, at time.Time, loc *time.Location) bool {
	var closed bool
	_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shop_closures WHERE day = $1::date)`, at.In(loc).Format("2006-01-02")).Scan(&closed)
	return closed
}

// addDays moves an instant by calendar days at the same wall clock in the
// shop's zone, not by 24 hours: a shift at eight stays at eight across the
// change of hour in March.
func addDays(t time.Time, n int, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+n, l.Hour(), l.Minute(), l.Second(), 0, loc)
}

// clashesTx finds what a window collides with: a shut day, an assignee who is
// away or off the rota, another job or booking on the same person at the same
// time, or more cars on lifts than the shop has.
func clashesTx(ctx context.Context, tx pgx.Tx, jobID string, on []Assignee, starts, ends time.Time, onLift bool, loc *time.Location) ([]Clash, error) {
	var out []Clash
	first := time.Date(starts.In(loc).Year(), starts.In(loc).Month(), starts.In(loc).Day(), 0, 0, 0, 0, loc)
	days := int(civil(ends.In(loc).Add(-time.Second))-civil(starts.In(loc))) + 1
	if days < 1 {
		days = 1
	}
	closed := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT day, reason FROM shop_closures WHERE day >= $1::date AND day < $2::date`,
		first.Format("2006-01-02"), addDays(first, days, loc).Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("closures: %w", err)
	}
	for rows.Next() {
		var d time.Time
		var reason string
		if err := rows.Scan(&d, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		closed[d.Format("2006-01-02")] = reason
	}
	rows.Close()
	for i := 0; i < days; i++ {
		day := addDays(first, i, loc).Format("2006-01-02")
		if reason, ok := closed[day]; ok {
			out = append(out, Clash{Kind: "closed", Day: day, Detail: reason})
		}
	}

	sameDay := civil(starts.In(loc)) == civil(ends.In(loc).Add(-time.Second))
	for _, a := range on {
		sched, _, err := schedules(ctx, tx, a.UserID, first, days, loc)
		if err != nil {
			return nil, err
		}
		for _, d := range sched[a.UserID] {
			day := d.Day.Format("2006-01-02")
			if _, shutDay := closed[day]; shutDay {
				continue
			}
			switch {
			case d.Away != "":
				out = append(out, Clash{Kind: "absent", Who: a.Name, Day: day})
			case d.Known && !d.Working:
				out = append(out, Clash{Kind: "off_rota", Who: a.Name, Day: day})
			case sameDay && d.Known && (starts.Before(d.Starts) || ends.After(d.Ends)):
				out = append(out, Clash{Kind: "off_shift", Who: a.Name, Day: day})
			}
		}
		other, err := othersOnTx(ctx, tx, jobID, a.UserID, starts, ends)
		if err != nil {
			return nil, err
		}
		for _, o := range other {
			out = append(out, Clash{Kind: "double", Who: a.Name, Detail: o})
		}
	}

	if onLift {
		over, err := liftsClashTx(ctx, tx, jobID, starts, ends, loc)
		if err != nil {
			return nil, err
		}
		out = append(out, over...)
	}
	return out, nil
}

// latestPlan restricts a job_plans row p to its job's newest version.
const latestPlan = `p.version = (SELECT max(p2.version) FROM job_plans p2 WHERE p2.work_order_id = p.work_order_id)`

// othersOnTx names the other jobs and bookings that put one person in two
// places in a window.
func othersOnTx(ctx context.Context, tx pgx.Tx, jobID, userID string, starts, ends time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT 'job ' || w.number::text FROM job_plans p
		JOIN work_orders w ON w.id = p.work_order_id
		JOIN job_assignments a ON a.work_order_id = w.id AND a.released_at IS NULL AND a.user_id = $2
		WHERE `+latestPlan+` AND p.work_order_id <> $1 AND `+openJob+`
		  AND p.starts_at < $4 AND p.ends_at > $3
		UNION ALL
		SELECT 'booking ' || to_char(b.starts_at AT TIME ZONE (SELECT timezone FROM shops LIMIT 1), 'HH24:MI')
		FROM bookings b
		WHERE b.technician_id = $2 AND b.status = 'booked' AND b.work_order_id IS NULL
		  AND b.starts_at < $4 AND b.ends_at > $3
		ORDER BY 1`, jobID, userID, starts, ends)
	if err != nil {
		return nil, fmt.Errorf("others on a technician: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// carAtSupplier is a car that was sent to a supplier and is not back: it is
// not on a lift, whatever its plan says. Only the car itself; a part off it
// leaves the lift occupied.
const carAtSupplier = `
	SELECT 1 FROM outsourced_work ow
	WHERE ow.work_order_id = w.id AND ow.item_kind = 'vehicle'
	  AND (SELECT e.kind FROM outsourced_events e
	        WHERE e.outsourced_id = ow.id AND e.kind IN ('requested', 'sent', 'waiting', 'returned', 'checked', 'check_failed', 'cancelled')
	        ORDER BY e.at DESC, e.id DESC LIMIT 1) IN ('sent', 'waiting')`

// liftsClashTx finds the spans in a window where every lift is taken by
// another car. A car takes a lift when its plan says so and it is here: a car
// that went home for the night, or waits for a part with its plan off the
// lift, does not.
func liftsClashTx(ctx context.Context, tx pgx.Tx, jobID string, starts, ends time.Time, loc *time.Location) ([]Clash, error) {
	var lifts *int
	if err := tx.QueryRow(ctx, `SELECT lifts FROM shops LIMIT 1`).Scan(&lifts); err != nil {
		return nil, fmt.Errorf("read lifts: %w", err)
	}
	if lifts == nil {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT greatest(p.starts_at, $2), least(p.ends_at, $3)
		FROM job_plans p JOIN work_orders w ON w.id = p.work_order_id
		WHERE `+latestPlan+` AND p.work_order_id <> $1 AND p.on_lift AND `+openJob+`
		  AND p.starts_at < $3 AND p.ends_at > $2
		  AND NOT coalesce((SELECT v.event IN ('collected', 'rebooked') FROM vehicle_presence v
		                     WHERE v.work_order_id = w.id ORDER BY v.at DESC, v.id DESC LIMIT 1), false)
		  AND NOT exists(`+carAtSupplier+`)
		UNION ALL
		SELECT greatest(b.starts_at, $2), least(b.ends_at, $3)
		FROM bookings b
		WHERE b.status = 'booked' AND b.work_order_id IS NULL AND b.starts_at < $3 AND b.ends_at > $2`,
		jobID, starts, ends)
	if err != nil {
		return nil, fmt.Errorf("read lift use: %w", err)
	}
	type span struct{ from, to time.Time }
	var spans []span
	for rows.Next() {
		var s span
		if err := rows.Scan(&s.from, &s.to); err != nil {
			rows.Close()
			return nil, err
		}
		spans = append(spans, s)
	}
	rows.Close()
	// Cars at once, counted at every moment one of them starts.
	var points []time.Time
	for _, s := range spans {
		points = append(points, s.from)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Before(points[j]) })
	var worst []time.Time
	for _, at := range points {
		n := 0
		for _, s := range spans {
			if !s.from.After(at) && s.to.After(at) {
				n++
			}
		}
		if n >= *lifts {
			worst = append(worst, at)
		}
	}
	if len(worst) == 0 {
		return nil, nil
	}
	return []Clash{{Kind: "lifts", Day: worst[0].In(loc).Format("2006-01-02"),
		Detail: fmt.Sprintf("%d of %d lifts taken from %s", *lifts, *lifts, worst[0].In(loc).Format("15:04"))}}, nil
}

// BacklogItem is an open job that needs a decision about who or when.
type BacklogItem struct {
	JobID        string
	Number       int64
	Registration string
	// unstaffed, away (somebody on it is away), or overdue.
	Kind string
	// Who is away, for "away"; when the plan ended, for "overdue".
	Who       string
	State     State
	PlanEnded *time.Time
	// Version is the plan version a carry-over was drawn against.
	Version int
}

// Backlog is the open jobs nobody is on, the ones whose technician is away
// today or on the day the plan starts, and the ones whose plan has run out
// with the job still open. Nothing stored: it is true until somebody deals
// with it, and then it stops being true.
//
// Any role: no customer is in it. A technician is the one who can pick an
// unstaffed job up.
func Backlog(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]BacklogItem, error) {
	var out []BacklogItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH today AS (SELECT (now() AT TIME ZONE (SELECT timezone FROM shops LIMIT 1))::date AS d),
			open AS (
			  SELECT w.id, w.number, coalesce(r.registration, '') AS registration, w.state
			  FROM work_orders w JOIN vehicles v ON v.id = w.vehicle_id
			  LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			  WHERE `+openJob+`
			),
			plan AS (
			  SELECT p.work_order_id, p.version, p.starts_at, p.ends_at FROM job_plans p WHERE `+latestPlan+`
			)
			SELECT 'unstaffed', o.id, o.number, o.registration, '', o.state, pl.ends_at, coalesce(pl.version, 0)
			FROM open o LEFT JOIN plan pl ON pl.work_order_id = o.id
			WHERE NOT EXISTS (SELECT 1 FROM job_assignments a WHERE a.work_order_id = o.id AND a.released_at IS NULL)
			UNION ALL
			SELECT DISTINCT 'away', o.id, o.number, o.registration, pe.display_name, o.state, pl.ends_at, coalesce(pl.version, 0)
			FROM open o
			JOIN job_assignments a ON a.work_order_id = o.id AND a.released_at IS NULL
			JOIN users u ON u.id = a.user_id JOIN people pe ON pe.id = u.person_id
			LEFT JOIN plan pl ON pl.work_order_id = o.id, today
			WHERE EXISTS (SELECT 1 FROM staff_absences s WHERE s.user_id = a.user_id
			              AND (s.day = today.d
			                   OR s.day = (pl.starts_at AT TIME ZONE (SELECT timezone FROM shops LIMIT 1))::date))
			UNION ALL
			SELECT 'overdue', o.id, o.number, o.registration, '', o.state, pl.ends_at, pl.version
			FROM open o JOIN plan pl ON pl.work_order_id = o.id
			WHERE pl.ends_at IS NOT NULL AND pl.ends_at < now()
			ORDER BY 1, 3`)
		if err != nil {
			return fmt.Errorf("backlog: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b BacklogItem
			if err := rows.Scan(&b.Kind, &b.JobID, &b.Number, &b.Registration, &b.Who, &b.State, &b.PlanEnded, &b.Version); err != nil {
				return fmt.Errorf("scan backlog: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// PlannedJob is a job with a plan, for the planner to draw next to the
// bookings.
type PlannedJob struct {
	JobID        string
	Number       int64
	Registration string
	What         string
	Starts, Ends time.Time
	OnLift       bool
	// The technicians on it. Empty is unstaffed.
	Technicians []string
}

// PlannedJobs are the open jobs whose latest plan overlaps a span.
func PlannedJobs(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) ([]PlannedJob, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []PlannedJob
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT w.id, w.number, coalesce(r.registration, ''), coalesce(w.complaint, ''),
			       p.starts_at, p.ends_at, p.on_lift,
			       coalesce((SELECT array_agg(a.user_id::text ORDER BY a.assigned_at) FROM job_assignments a
			                  WHERE a.work_order_id = w.id AND a.released_at IS NULL), '{}')
			FROM job_plans p JOIN work_orders w ON w.id = p.work_order_id
			JOIN vehicles v ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE `+latestPlan+` AND `+openJob+` AND p.starts_at IS NOT NULL
			  AND p.starts_at < $2 AND p.ends_at > $1
			ORDER BY p.starts_at`, from, to)
		if err != nil {
			return fmt.Errorf("planned jobs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var j PlannedJob
			if err := rows.Scan(&j.JobID, &j.Number, &j.Registration, &j.What, &j.Starts, &j.Ends, &j.OnLift, &j.Technicians); err != nil {
				return fmt.Errorf("scan planned job: %w", err)
			}
			out = append(out, j)
		}
		return rows.Err()
	})
	return out, err
}

// claimIfNobodyTx puts the person who picks a job up on it when nobody is on
// it. Asking somebody to assign themselves before starting is a step that
// gets skipped, and then the job has no one on it and the list is useless.
func claimIfNobodyTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_assignments (shop_id, work_order_id, user_id, assigned_by)
		SELECT $1, $2, $3, $3
		WHERE NOT EXISTS (SELECT 1 FROM job_assignments WHERE work_order_id = $2 AND released_at IS NULL)`,
		scope.ShopID, jobID, scope.UserID); err != nil {
		return fmt.Errorf("assign job: %w", err)
	}
	return syncLeadTx(ctx, tx, jobID)
}

// PlannedBlocks are open jobs' plans as planner blocks, one per technician on
// the job and one for the unassigned row when nobody is. A booking whose car
// has arrived and whose job has a plan is replaced by the plan: the slot sold
// was a promise about the start, the plan is where the rest of the work is.
func PlannedBlocks(jobs []PlannedJob, bookings []Booking) []Booking {
	planned := map[string]bool{}
	for _, j := range jobs {
		planned[j.JobID] = true
	}
	out := make([]Booking, 0, len(bookings)+len(jobs))
	for _, b := range bookings {
		if b.Status == "arrived" && planned[b.WorkOrderID] {
			continue
		}
		out = append(out, b)
	}
	for _, j := range jobs {
		ids := j.Technicians
		if len(ids) == 0 {
			ids = []string{""}
		}
		for _, id := range ids {
			out = append(out, Booking{
				TechnicianID: id, Registration: j.Registration, What: j.What,
				Starts: j.Starts, Ends: j.Ends, Status: "planned",
				WorkOrderID: j.JobID, JobID: j.JobID, JobOpen: true, OffLift: !j.OnLift,
			})
		}
	}
	return out
}
