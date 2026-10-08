package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Who is supposed to work when.
//
// A rota is a person's usual hours, repeating every one to four weeks -- week
// A and week B without entering every day. A shift change is one day's hours
// other than the rota's. An absence (#73) is a day not worked at all. All
// three are rows that are added and never edited, so a schedule can always
// say what it was as well as what it is.
//
// Times are wall clock in the shop's zone and turned into instants per day,
// so a shift across midnight, or across the change of hour, has the hours it
// really has: 22:00 to 06:00 on the night the clocks go back is nine.

// RotaHours are one weekday's hours in one week of a rota. Week 0 is the
// week the rota starts in; Weekday is ISO, Monday 1. Times are "15:04".
type RotaHours struct {
	Week, Weekday int
	Starts, Ends  string
}

// Rota is a person's usual hours from a date.
type Rota struct {
	ID        string
	UserID    string
	ValidFrom time.Time // a date, at midnight UTC
	Weeks     int
	Hours     []RotaHours
	CreatedAt time.Time
}

// ShiftChange is one day's hours other than the rota's; empty times are
// "back to the rota".
type ShiftChange struct {
	UserID       string
	Day          time.Time // a date, at midnight UTC
	Starts, Ends string
	CreatedAt    time.Time
}

// ScheduledDay is one day of one person's schedule.
type ScheduledDay struct {
	Day time.Time // midnight in the shop's zone
	// Known is false when nothing says: no rota from before the day, and no
	// change on it. The planner does not guess.
	Known   bool
	Working bool
	// The shift, as instants. The end is the next day for a night shift.
	Starts, Ends time.Time
	// Hours other than the rota's, from a shift change.
	Changed bool
	// Why the person is away, as a catalogue key; "away" when the reader
	// may not know why. Empty when not away.
	Away string
}

// Minutes is how long the shift is, by the clock that was on the wall.
func (d ScheduledDay) Minutes() int {
	if !d.Working {
		return 0
	}
	return int(d.Ends.Sub(d.Starts).Minutes())
}

// RestWarning is a stretch of the schedule with less rest than Swedish
// working time law asks for. A warning, never a refusal: collective
// agreements vary the rules, and FreeSMS is not a legal adviser.
type RestWarning struct {
	// "daily" (11 hours between shifts) or "weekly" (36 hours in a week).
	Kind string
	// Where: the day the short rest ends on, or the week's Monday.
	Day time.Time
	// The longest rest there was, in minutes.
	Minutes int
}

const (
	dailyRest  = 11 * time.Hour
	weeklyRest = 36 * time.Hour
)

// Says is the warning's catalogue key, taking the hours of rest.
func (w RestWarning) Says() string {
	if w.Kind == "weekly" {
		return "Only %s hours' rest in this week; the law asks for 36"
	}
	return "Only %s hours' rest before this shift; the law asks for 11"
}

// RestHours is the rest, in hours with a decimal point, for the page to write
// in the reader's language.
func (w RestWarning) RestHours() string { return Hours(w.Minutes) }

// civil is a day as a count, unaffected by any zone's change of hour.
func civil(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

func isoWeekday(t time.Time) int {
	if wd := int(t.Weekday()); wd != 0 {
		return wd
	}
	return 7
}

// clockMinutes reads "15:04" or Postgres's "15:04:05".
func clockMinutes(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return 0, fmt.Errorf("%w: %q is not a time", ErrInvalid, s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%w: %q is not a time", ErrInvalid, s)
	}
	return h*60 + m, nil
}

// shiftOn places a shift on a day in the zone. An end at or before the start
// is the next day.
func shiftOn(day time.Time, starts, ends string, loc *time.Location) (time.Time, time.Time, error) {
	a, err := clockMinutes(starts)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	b, err := clockMinutes(ends)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	y, m, d := day.Date()
	from := time.Date(y, m, d, a/60, a%60, 0, 0, loc)
	to := time.Date(y, m, d, b/60, b%60, 0, 0, loc)
	if b <= a {
		to = time.Date(y, m, d+1, b/60, b%60, 0, 0, loc)
	}
	return from, to, nil
}

// BuildSchedule lays one person's rotas, changes and absences over days from
// first, in the shop's zone. Rotas are in any order; the latest change for a
// day wins.
func BuildSchedule(rotas []Rota, changes []ShiftChange, absent map[string]string, first time.Time, days int, loc *time.Location) []ScheduledDay {
	sorted := append([]Rota(nil), rotas...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].ValidFrom.Equal(sorted[j].ValidFrom) {
			return sorted[i].ValidFrom.Before(sorted[j].ValidFrom)
		}
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	latest := map[int64]ShiftChange{}
	for _, c := range changes {
		k := civil(c.Day)
		if was, ok := latest[k]; !ok || c.CreatedAt.After(was.CreatedAt) {
			latest[k] = c
		}
	}
	y, m, d := first.In(loc).Date()
	out := make([]ScheduledDay, 0, days)
	for i := 0; i < days; i++ {
		day := time.Date(y, m, d+i, 0, 0, 0, 0, loc)
		k := civil(day)
		sd := ScheduledDay{Day: day}
		if reason, ok := absent[day.Format("2006-01-02")]; ok {
			sd.Away = reason
		}

		starts, ends := "", ""
		if c, ok := latest[k]; ok && c.Starts != "" {
			starts, ends, sd.Changed, sd.Known = c.Starts, c.Ends, true, true
		} else {
			// The newest rota that has started by this day.
			var r *Rota
			for j := range sorted {
				if civil(sorted[j].ValidFrom) <= k {
					r = &sorted[j]
				}
			}
			if r != nil {
				sd.Known = true
				anchor := civil(r.ValidFrom) - int64(isoWeekday(r.ValidFrom)-1)
				week := int(((k - int64(isoWeekday(day)-1) - anchor) / 7) % int64(r.Weeks))
				for _, h := range r.Hours {
					if h.Week == week && h.Weekday == isoWeekday(day) {
						starts, ends = h.Starts, h.Ends
					}
				}
			}
		}
		if starts != "" && sd.Away == "" {
			if from, to, err := shiftOn(day, starts, ends, loc); err == nil {
				sd.Working, sd.Starts, sd.Ends = true, from, to
			}
		}
		out = append(out, sd)
	}
	return out
}

// RestWarnings finds less rest than the law asks for in a schedule: under 11
// hours between two shifts, and under 36 hours in one stretch in a Monday to
// Sunday week the schedule covers all of.
func RestWarnings(days []ScheduledDay, loc *time.Location) []RestWarning {
	var shifts []ScheduledDay
	for _, d := range days {
		if d.Working {
			shifts = append(shifts, d)
		}
	}
	var out []RestWarning
	for i := 1; i < len(shifts); i++ {
		if gap := shifts[i].Starts.Sub(shifts[i-1].Ends); gap < dailyRest {
			out = append(out, RestWarning{Kind: "daily", Day: shifts[i].Day, Minutes: max(int(gap.Minutes()), 0)})
		}
	}
	for i, d := range days {
		if isoWeekday(d.Day) != 1 || i+7 > len(days) {
			continue
		}
		y, m, dd := d.Day.Date()
		start, end := d.Day, time.Date(y, m, dd+7, 0, 0, 0, 0, loc)
		longest, cursor := time.Duration(0), start
		for _, s := range shifts {
			if !s.Ends.After(start) || !s.Starts.Before(end) {
				continue
			}
			if s.Starts.After(cursor) {
				longest = max(longest, s.Starts.Sub(cursor))
			}
			if s.Ends.After(cursor) {
				cursor = s.Ends
			}
		}
		if end.After(cursor) {
			longest = max(longest, end.Sub(cursor))
		}
		if longest < weeklyRest {
			out = append(out, RestWarning{Kind: "weekly", Day: start, Minutes: int(longest.Minutes())})
		}
	}
	return out
}

// StaffRow is one person's days on the rota page.
type StaffRow struct {
	UserID string
	Name   string
	Days   []ScheduledDay
	// Only for whoever plans staff.
	Warnings []RestWarning
}

// schedules reads the rotas, changes and absences for everybody active, or
// one person, over days from first.
func schedules(ctx context.Context, tx pgx.Tx, only string, first time.Time, days int, loc *time.Location) (map[string][]ScheduledDay, []StaffRow, error) {
	from := first.In(loc).Format("2006-01-02")
	y, m, d := first.In(loc).Date()
	to := time.Date(y, m, d+days, 0, 0, 0, 0, loc).Format("2006-01-02")

	rows, err := tx.Query(ctx, `
		SELECT u.id, p.display_name FROM users u JOIN people p ON p.id = u.person_id
		WHERE u.active AND ($1 = '' OR u.id::text = $1) ORDER BY p.display_name`, only)
	if err != nil {
		return nil, nil, fmt.Errorf("staff: %w", err)
	}
	var people []StaffRow
	for rows.Next() {
		var r StaffRow
		if err := rows.Scan(&r.UserID, &r.Name); err != nil {
			rows.Close()
			return nil, nil, err
		}
		people = append(people, r)
	}
	rows.Close()

	rotas := map[string][]Rota{}
	byID := map[string]*Rota{}
	var order []string
	rows, err = tx.Query(ctx, `
		SELECT id, user_id, valid_from, weeks, created_at FROM rotas
		WHERE valid_from < $1::date AND ($2 = '' OR user_id::text = $2)`, to, only)
	if err != nil {
		return nil, nil, fmt.Errorf("rotas: %w", err)
	}
	for rows.Next() {
		var r Rota
		if err := rows.Scan(&r.ID, &r.UserID, &r.ValidFrom, &r.Weeks, &r.CreatedAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		byID[r.ID] = &r
		order = append(order, r.ID)
	}
	rows.Close()
	// Asked even with no rotas, so the page costs the same whatever the shop
	// holds; the query budget holds it to that.
	rows, err = tx.Query(ctx, `SELECT rota_id, week, weekday, starts::text, ends::text FROM rota_hours WHERE rota_id = ANY($1::uuid[])`, append([]string{}, order...))
	if err != nil {
		return nil, nil, fmt.Errorf("rota hours: %w", err)
	}
	for rows.Next() {
		var id string
		var h RotaHours
		if err := rows.Scan(&id, &h.Week, &h.Weekday, &h.Starts, &h.Ends); err != nil {
			rows.Close()
			return nil, nil, err
		}
		byID[id].Hours = append(byID[id].Hours, h)
	}
	rows.Close()
	for _, id := range order {
		r := byID[id]
		rotas[r.UserID] = append(rotas[r.UserID], *r)
	}

	changes := map[string][]ShiftChange{}
	rows, err = tx.Query(ctx, `
		SELECT user_id, day, coalesce(starts::text, ''), coalesce(ends::text, ''), created_at FROM shift_changes
		WHERE day >= $1::date AND day < $2::date AND ($3 = '' OR user_id::text = $3)`, from, to, only)
	if err != nil {
		return nil, nil, fmt.Errorf("shift changes: %w", err)
	}
	for rows.Next() {
		var c ShiftChange
		if err := rows.Scan(&c.UserID, &c.Day, &c.Starts, &c.Ends, &c.CreatedAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		changes[c.UserID] = append(changes[c.UserID], c)
	}
	rows.Close()

	absent := map[string]map[string]string{}
	rows, err = tx.Query(ctx, `
		SELECT user_id, day, reason FROM staff_absences
		WHERE day >= $1::date AND day < $2::date AND ($3 = '' OR user_id::text = $3)`, from, to, only)
	if err != nil {
		return nil, nil, fmt.Errorf("absences: %w", err)
	}
	for rows.Next() {
		var user, reason string
		var day time.Time
		if err := rows.Scan(&user, &day, &reason); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if absent[user] == nil {
			absent[user] = map[string]string{}
		}
		absent[user][day.Format("2006-01-02")] = AbsenceReasons[reason]
	}
	rows.Close()

	out := map[string][]ScheduledDay{}
	for i := range people {
		p := &people[i]
		p.Days = BuildSchedule(rotas[p.UserID], changes[p.UserID], absent[p.UserID], first, days, loc)
		out[p.UserID] = p.Days
	}
	return out, people, nil
}

// maskAway leaves why somebody is away to whoever may know: the person, and
// whoever plans staff. Everybody else reads "away".
func maskAway(scope access.Scope, user string, days []ScheduledDay) {
	if scope.PlansStaff() || user == scope.UserID {
		return
	}
	for i := range days {
		if days[i].Away != "" {
			days[i].Away = AbsenceReasons["other"]
		}
	}
}

// StaffWeek is who is in when, for everybody who works here. Colleagues see
// the hours and that somebody is away; why, and the rest warnings, are for
// whoever plans staff. Nobody's clocked hours are on it.
func StaffWeek(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, first time.Time, days int) ([]StaffRow, error) {
	planner := scope.PlansStaff()
	var people []StaffRow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		_, people, err = schedules(ctx, tx, "", first, days, loc)
		if err != nil {
			return err
		}
		for i := range people {
			maskAway(scope, people[i].UserID, people[i].Days)
			if planner {
				people[i].Warnings = RestWarnings(people[i].Days, loc)
			}
		}
		return nil
	})
	return people, err
}

// MySchedule is the caller's own days, with what they clocked on each.
type MySchedule struct {
	Days []ScheduledDay
	// Minutes clocked, by day as "2006-01-02".
	Clocked map[string]int
}

// ScheduleFor is the caller's own schedule over days from first, and what
// they clocked. Their own, so every role may ask, and the reasons are theirs.
func ScheduleFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, first time.Time, days int) (MySchedule, error) {
	out := MySchedule{Clocked: map[string]int{}}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		all, _, err := schedules(ctx, tx, scope.UserID, first, days, loc)
		if err != nil {
			return err
		}
		out.Days = all[scope.UserID]
		if len(out.Days) == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `
			SELECT (started_at AT TIME ZONE $4)::date::text,
			       sum(extract(epoch FROM coalesce(ended_at, now()) - started_at) / 60)::int
			FROM time_entries
			WHERE user_id = $1 AND started_at >= $2 AND started_at < $3
			GROUP BY 1`, scope.UserID, out.Days[0].Day, out.Days[len(out.Days)-1].Day.AddDate(0, 0, 1), loc.String())
		if err != nil {
			return fmt.Errorf("clocked: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var day string
			var minutes int
			if err := rows.Scan(&day, &minutes); err != nil {
				return err
			}
			out.Clocked[day] = minutes
		}
		return rows.Err()
	})
	return out, err
}

// MarkScheduleSeen records that the caller has looked at their own schedule,
// so a change made before now no longer waits for them.
func MarkScheduleSeen(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET schedule_seen_at = clock_timestamp() WHERE id = $1`, scope.UserID)
		return err
	})
}

// SetRota gives somebody new usual hours from a date. The old rota stays,
// for the days before it.
func SetRota(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string, validFrom time.Time, weeks int, hours []RotaHours) error {
	if !scope.PlansStaff() {
		return access.ErrForbidden
	}
	if weeks < 1 || weeks > 4 {
		return fmt.Errorf("%w: a rota repeats every one to four weeks", ErrInvalid)
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	for _, h := range hours {
		if h.Week < 0 || h.Week >= weeks || h.Weekday < 1 || h.Weekday > 7 {
			return fmt.Errorf("%w: a day outside the rota", ErrInvalid)
		}
		a, err := clockMinutes(h.Starts)
		if err != nil {
			return err
		}
		b, err := clockMinutes(h.Ends)
		if err != nil {
			return err
		}
		if a == b {
			return fmt.Errorf("%w: a shift starts and ends at %s", ErrInvalid, h.Starts)
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO rotas (shop_id, user_id, valid_from, weeks, created_by)
			SELECT $1, u.id, $3::date, $4, $5 FROM users u WHERE u.id = $2 AND u.active
			RETURNING id`, scope.ShopID, userID, validFrom.Format("2006-01-02"), weeks, scope.UserID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("save rota: %w", err)
		}
		for _, h := range hours {
			if _, err := tx.Exec(ctx, `
				INSERT INTO rota_hours (shop_id, rota_id, week, weekday, starts, ends) VALUES ($1, $2, $3, $4, $5::time, $6::time)`,
				scope.ShopID, id, h.Week, h.Weekday, h.Starts, h.Ends); err != nil {
				return fmt.Errorf("save rota hours: %w", err)
			}
		}
		return nil
	})
}

// ChangeShift gives somebody other hours on one day; empty times put the day
// back to the rota.
func ChangeShift(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string, day time.Time, starts, ends string) error {
	if !scope.PlansStaff() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	if (starts == "") != (ends == "") {
		return fmt.Errorf("%w: give both times, or neither to follow the rota", ErrInvalid)
	}
	if starts != "" {
		a, err := clockMinutes(starts)
		if err != nil {
			return err
		}
		b, err := clockMinutes(ends)
		if err != nil {
			return err
		}
		if a == b {
			return fmt.Errorf("%w: a shift starts and ends at %s", ErrInvalid, starts)
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO shift_changes (shop_id, user_id, day, starts, ends, created_by)
			SELECT $1, u.id, $3::date, nullif($4, '')::time, nullif($5, '')::time, $6 FROM users u WHERE u.id = $2 AND u.active`,
			scope.ShopID, userID, day.Format("2006-01-02"), starts, ends, scope.UserID)
		if err != nil {
			return fmt.Errorf("change shift: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetPlanner gives or takes the planning of people. Whoever runs the shop
// decides that.
func SetPlanner(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string, on bool) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET plans_staff = $2 WHERE id = $1`, userID, on)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
