package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

func date(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }

func weekdays(week int, starts, ends string) []workshop.RotaHours {
	var out []workshop.RotaHours
	for wd := 1; wd <= 5; wd++ {
		out = append(out, workshop.RotaHours{Week: week, Weekday: wd, Starts: starts, Ends: ends})
	}
	return out
}

// Week A and week B, without entering every day: a two-week rota repeats
// from the week it starts in, whatever day of that week it starts on.
func TestATwoWeekRotaAlternatesFromItsFirstWeek(t *testing.T) {
	rota := workshop.Rota{ValidFrom: date("2026-10-07"), Weeks: 2, // a Wednesday
		Hours: append(weekdays(0, "07:00", "16:00"), weekdays(1, "10:00", "19:00")...)}
	days := workshop.BuildSchedule([]workshop.Rota{rota}, nil, nil, at("2026-10-05", 0, 0), 21, stockholm)
	for _, c := range []struct {
		day   string
		start string
	}{
		{"2026-10-05", ""},      // before the rota: nothing is known
		{"2026-10-07", "07:00"}, // week A
		{"2026-10-13", "10:00"}, // week B
		{"2026-10-20", "07:00"}, // week A again
	} {
		var d workshop.ScheduledDay
		for _, x := range days {
			if x.Day.Format("2006-01-02") == c.day {
				d = x
			}
		}
		got := ""
		if d.Working {
			got = d.Starts.In(stockholm).Format("15:04")
		}
		if got != c.start || d.Known != (c.start != "") {
			t.Errorf("%s: starts %q (known %v), want %q", c.day, got, d.Known, c.start)
		}
	}
	// A weekend in a five-day rota is known and not worked.
	if sat := days[5]; !sat.Known || sat.Working {
		t.Errorf("the Saturday: %+v", sat)
	}
}

// A night shift ends the next morning, and across the change of hour has
// the hours it really has: nine on the night the clocks go back.
func TestANightShiftHasTheHoursItReallyHas(t *testing.T) {
	night := []workshop.RotaHours{{Week: 0, Weekday: 6, Starts: "22:00", Ends: "06:00"}}
	rota := workshop.Rota{ValidFrom: date("2026-10-01"), Weeks: 1, Hours: night}
	for _, c := range []struct {
		saturday string
		minutes  int
	}{{"2026-10-17", 8 * 60}, {"2026-10-24", 9 * 60}} {
		d := workshop.BuildSchedule([]workshop.Rota{rota}, nil, nil, at(c.saturday, 0, 0), 1, stockholm)[0]
		if !d.Working || d.Minutes() != c.minutes || d.Ends.In(stockholm).Format("2006-01-02 15:04") != at(c.saturday, 0, 0).AddDate(0, 0, 1).Format("2006-01-02")+" 06:00" {
			t.Errorf("%s: %v to %v, %d minutes, want %d", c.saturday, d.Starts, d.Ends, d.Minutes(), c.minutes)
		}
	}
}

// A newer rota takes over from its date; a change to a day wins over the
// rota, and the latest change wins over an earlier one -- including the one
// that puts the day back.
func TestTheLatestRotaAndTheLatestChangeHold(t *testing.T) {
	early := workshop.Rota{ValidFrom: date("2026-10-01"), Weeks: 1, Hours: weekdays(0, "07:00", "16:00")}
	late := workshop.Rota{ValidFrom: date("2026-10-19"), Weeks: 1, Hours: weekdays(0, "09:00", "18:00"), CreatedAt: time.Now()}
	t0 := time.Now()
	changes := []workshop.ShiftChange{
		{Day: date("2026-10-14"), Starts: "12:00", Ends: "20:00", CreatedAt: t0},
		{Day: date("2026-10-15"), Starts: "12:00", Ends: "20:00", CreatedAt: t0},
		{Day: date("2026-10-15"), CreatedAt: t0.Add(time.Minute)}, // back to the rota
	}
	days := workshop.BuildSchedule([]workshop.Rota{late, early}, changes, nil, at("2026-10-14", 0, 0), 6, stockholm)
	starts := func(i int) string { return days[i].Starts.In(stockholm).Format("15:04") }
	if starts(0) != "12:00" || !days[0].Changed {
		t.Errorf("the changed day starts %s (changed %v)", starts(0), days[0].Changed)
	}
	if starts(1) != "07:00" || days[1].Changed {
		t.Errorf("the day put back starts %s (changed %v)", starts(1), days[1].Changed)
	}
	if starts(5) != "09:00" {
		t.Errorf("the new rota's first Monday starts %s", starts(5))
	}
}

// Under 11 hours between shifts, and under 36 in a week, is said; a
// schedule that keeps both is not.
func TestShortRestIsWarnedAbout(t *testing.T) {
	ok := workshop.Rota{ValidFrom: date("2026-10-05"), Weeks: 1, Hours: weekdays(0, "07:00", "16:00")}
	if w := workshop.RestWarnings(workshop.BuildSchedule([]workshop.Rota{ok}, nil, nil, at("2026-10-05", 0, 0), 7, stockholm), stockholm); len(w) != 0 {
		t.Errorf("a normal week warned: %+v", w)
	}
	// Closing at 23:00 on Thursday and opening at 07:00 on Friday: eight hours.
	changes := []workshop.ShiftChange{{Day: date("2026-10-08"), Starts: "14:00", Ends: "23:00", CreatedAt: time.Now()}}
	days := workshop.BuildSchedule([]workshop.Rota{ok}, changes, nil, at("2026-10-05", 0, 0), 7, stockholm)
	w := workshop.RestWarnings(days, stockholm)
	if len(w) != 1 || w[0].Kind != "daily" || w[0].Minutes != 8*60 || w[0].Day.Format("2006-01-02") != "2026-10-09" {
		t.Errorf("a short night: %+v", w)
	}
	// Every day of the week, Saturday and Sunday too: no 36 hours anywhere.
	all := workshop.Rota{ValidFrom: date("2026-10-05"), Weeks: 1, Hours: append(weekdays(0, "07:00", "16:00"),
		workshop.RotaHours{Weekday: 6, Starts: "07:00", Ends: "16:00"}, workshop.RotaHours{Weekday: 7, Starts: "07:00", Ends: "16:00"})}
	w = workshop.RestWarnings(workshop.BuildSchedule([]workshop.Rota{all}, nil, nil, at("2026-10-05", 0, 0), 7, stockholm), stockholm)
	if len(w) != 1 || w[0].Kind != "weekly" {
		t.Errorf("a seven-day week: %+v", w)
	}
}

// Rotas are for whoever plans staff; why somebody is away is for them and
// the person; and a change somebody else made waits for the person until
// they have looked at their schedule.
func TestARotaIsPlannedReadAndNoticed(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	addTechnician(t, pool)
	monday := at("2026-10-12", 0, 0)
	hours := weekdays(0, "07:00", "16:00")

	for _, s := range []access.Scope{advisor(), technician()} {
		if err := workshop.SetRota(ctx, pool, s, techUserID, monday, 1, hours); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s set a rota: %v", s.Role, err)
		}
	}
	// Given the planning of people, the counter may.
	planner := advisor()
	planner.Planner = true
	if err := workshop.SetRota(ctx, pool, planner, techUserID, monday, 1, hours); err != nil {
		t.Fatalf("SetRota: %v", err)
	}
	if err := workshop.SetAbsence(ctx, pool, planner, techUserID, at("2026-10-14", 0, 0), "sick"); err != nil {
		t.Fatal(err)
	}

	week := func(s access.Scope) map[string]workshop.ScheduledDay {
		t.Helper()
		rows, err := workshop.StaffWeek(ctx, pool, s, monday, 7)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]workshop.ScheduledDay{}
		for _, r := range rows {
			if r.UserID == techUserID {
				for _, d := range r.Days {
					out[d.Day.Format("2006-01-02")] = d
				}
			}
		}
		return out
	}
	if d := week(planner)["2026-10-14"]; d.Away != "off sick" || d.Working {
		t.Errorf("the planner reads %+v", d)
	}
	if d := week(technician())["2026-10-14"]; d.Away != "off sick" {
		t.Errorf("the person reads %q about their own day", d.Away)
	}
	if d := week(advisor())["2026-10-14"]; d.Away != "away" {
		t.Errorf("a colleague reads %q", d.Away)
	}
	if d := week(advisor())["2026-10-13"]; !d.Working || d.Starts.In(stockholm).Format("15:04") != "07:00" {
		t.Errorf("a colleague cannot see when Erik is in: %+v", d)
	}

	if got := waiting(t, technician(), "schedule_changed"); len(got) != 1 {
		t.Fatalf("the technician is not told their schedule changed: %+v", got)
	}
	if got := waiting(t, planner, "schedule_changed"); len(got) != 0 {
		t.Errorf("the planner is told about a change they made to somebody else: %+v", got)
	}
	if err := workshop.MarkScheduleSeen(ctx, pool, technician()); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "schedule_changed"); len(got) != 0 {
		t.Errorf("seen, and still told: %+v", got)
	}
	if err := workshop.ChangeShift(ctx, pool, planner, techUserID, at("2099-01-02", 0, 0), "10:00", "18:00"); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, technician(), "schedule_changed"); len(got) != 1 {
		t.Errorf("a change after looking is not told: %+v", got)
	}

	// Somebody who leaves is ended, not deleted: off the rota, and their
	// rotas still there -- which the application cannot remove at all.
	if err := auth.Deactivate(ctx, pool, owner(), techUserID); err != nil {
		t.Fatal(err)
	}
	if _, ok := week(owner())["2026-10-13"]; ok {
		t.Error("somebody switched off is still on the rota")
	}
	err := database.InScope(ctx, pool, owner(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM rotas WHERE user_id = $1`, techUserID)
		return err
	})
	if err == nil {
		t.Error("the application deleted a rota")
	}
	var kept int
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM rotas WHERE user_id = $1`, techUserID).Scan(&kept)
	})
	if kept != 1 {
		t.Errorf("%d rotas kept for somebody who left, want 1", kept)
	}
}
