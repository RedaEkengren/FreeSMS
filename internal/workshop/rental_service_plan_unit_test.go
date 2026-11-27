package workshop

import (
	"testing"
	"time"
)

// The rule for when a service next falls due, on its own: the date, the
// kilometres turned into a day by the unit's pace, the earlier of the two, and
// the cases with no pace.
func TestTheNextServiceIsTheEarlierOfTheDateAndTheKilometresAtThePace(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, loc) }
	now := day(time.June, 1).Add(12 * time.Hour)
	done := func(m time.Month, d int) *time.Time { t := time.Date(2026, m, d, 0, 0, 0, 0, time.UTC); return &t }
	km := func(n int64) *int64 { return &n }
	// 100 km a day: 10 000 km over a hundred days up to now.
	pace := []reading{{day(time.February, 21), 40000}, {now, 50000}}

	months := ServicePlan{Kind: "service", IntervalMonths: 12, DurationMinutes: 240, InUse: true, CreatedAt: day(time.January, 1)}
	d := nextDue(months, done(time.March, 10), nil, nil, pace, now, loc)
	if d.Basis != "date" || d.DueOn == nil || d.DueOn.Format("2006-01-02") != "2027-03-10" {
		t.Errorf("twelve months after 10 March: %v %q", d.DueOn, d.Basis)
	}

	// By kilometres: 5 000 km after 48 000, at 100 a day: from 50 000 it is 30
	// days to go, which is earlier than a date a year away.
	both := ServicePlan{Kind: "service", IntervalMonths: 12, IntervalKm: 5000, DurationMinutes: 240, InUse: true, CreatedAt: day(time.January, 1)}
	d = nextDue(both, done(time.March, 10), km(48000), nil, pace, now, loc)
	if d.Basis != "km" || d.DueOn == nil || d.DueOn.Format("2006-01-02") != "2026-07-01" || d.DueKm == nil || *d.DueKm != 53000 {
		t.Errorf("3 000 km to go at 100 a day: %v %q limit %v", d.DueOn, d.Basis, d.DueKm)
	}
	if d.KmPerDay < 99 || d.KmPerDay > 101 {
		t.Errorf("the pace is %.1f", d.KmPerDay)
	}

	// The date can be the earlier one: a month's interval since 10 March is past
	// before the kilometres, 30 days off, are reached.
	soon := ServicePlan{Kind: "service", IntervalMonths: 1, IntervalKm: 5000, DurationMinutes: 240, InUse: true, CreatedAt: day(time.January, 1)}
	d = nextDue(soon, done(time.March, 10), km(48000), nil, pace, now, loc)
	if d.Basis != "date" || d.DueOn == nil || d.DueOn.Format("2006-01-02") != "2026-04-10" || !d.Overdue {
		t.Errorf("the date is the earlier: %v %q overdue %v", d.DueOn, d.Basis, d.Overdue)
	}

	// A limit already passed is due today, and overdue the day after.
	passed := ServicePlan{Kind: "service", IntervalKm: 1000, DurationMinutes: 60, InUse: true, CreatedAt: day(time.January, 1)}
	d = nextDue(passed, done(time.March, 10), km(48000), nil, pace, now, loc)
	if d.Basis != "km" || d.DueOn == nil || !d.DueOn.Equal(day(time.June, 1)) || d.Overdue {
		t.Errorf("a limit passed: %v %q overdue %v", d.DueOn, d.Basis, d.Overdue)
	}
	// A date gone by is overdue; a limit passed is due today, again each day.
	d = nextDue(months, func() *time.Time { t := time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC); return &t }(), nil, nil, pace, now, loc)
	if !d.Overdue || d.Basis != "date" {
		t.Errorf("a year and more since it was done: %v overdue %v", d.DueOn, d.Overdue)
	}

	// Never done: the plan's own day starts the count, and the first reading after
	// it the kilometres.
	d = nextDue(both, nil, nil, km(50000), pace, now, loc)
	if d.DueKm == nil || *d.DueKm != 55000 || d.Basis != "km" {
		t.Errorf("never done: %v %q limit %v", d.DueOn, d.Basis, d.DueKm)
	}

	// No pace: two readings a week apart, or one, or none, or going backwards.
	for name, rs := range map[string][]reading{
		"a week":    {{now.AddDate(0, 0, -7), 49000}, {now, 50000}},
		"one":       {{now, 50000}},
		"none":      nil,
		"backwards": {{now.AddDate(0, 0, -100), 50000}, {now, 49000}},
	} {
		d = nextDue(both, done(time.March, 10), km(48000), nil, rs, now, loc)
		if name != "none" && name != "one" && d.KmPerDay != 0 {
			t.Errorf("%s: a pace of %.1f", name, d.KmPerDay)
		}
		if d.Basis != "date" || d.DueOn == nil || d.DueOn.Format("2006-01-02") != "2027-03-10" {
			t.Errorf("%s: with no pace the date stands: %v %q", name, d.DueOn, d.Basis)
		}
	}
	// A unit with no odometer, by kilometres only: nothing to say.
	d = nextDue(ServicePlan{Kind: "service", IntervalKm: 5000, DurationMinutes: 60, InUse: true, CreatedAt: day(time.January, 1)}, nil, nil, nil, nil, now, loc)
	if d.DueOn != nil {
		t.Errorf("kilometres with no readings gave %v", d.DueOn)
	}
}

// The first stretch long enough before the deadline, never touching what is
// booked, and none when there is none -- an open-ended block or a deadline too
// near ends the search.
func TestTheFirstFreeWindowIsTheEarliestThatFitsAndTouchesNothing(t *testing.T) {
	at := func(d, h int) time.Time { return time.Date(2026, 6, d, h, 0, 0, 0, time.UTC) }
	four := 4 * time.Hour
	free := []busyPeriod{}
	if got, ok := firstFreeWindow(free, at(2, 8), at(5, 0), four); !ok || !got.Equal(at(2, 8)) {
		t.Errorf("nothing booked: %v %v", got, ok)
	}
	// Booked 8 to 14 on the 2nd, and from 16 the same day: two hours do not hold four.
	b := []busyPeriod{{from: at(2, 8), to: at(2, 14)}, {from: at(2, 16), to: at(2, 22)}}
	if got, ok := firstFreeWindow(b, at(2, 8), at(5, 0), four); !ok || !got.Equal(at(2, 22)) {
		t.Errorf("after the second block: %v %v", got, ok)
	}
	// A gap of exactly four hours fits.
	b = []busyPeriod{{from: at(2, 8), to: at(2, 12)}, {from: at(2, 16), to: at(2, 20)}}
	if got, ok := firstFreeWindow(b, at(2, 8), at(5, 0), four); !ok || !got.Equal(at(2, 12)) {
		t.Errorf("a gap that just fits: %v %v", got, ok)
	}
	// Busy before the search starts does not matter; out of order does not either.
	b = []busyPeriod{{from: at(3, 9), to: at(3, 17)}, {from: at(1, 8), to: at(2, 7)}, {from: at(2, 8), to: at(2, 9)}}
	if got, ok := firstFreeWindow(b, at(2, 8), at(5, 0), four); !ok || !got.Equal(at(2, 9)) {
		t.Errorf("out of order: %v %v", got, ok)
	}
	// Past the deadline is no window; so is an open-ended block ahead of a fit.
	if _, ok := firstFreeWindow([]busyPeriod{{from: at(2, 8), to: at(4, 20)}}, at(2, 8), at(5, 0), 8*time.Hour); ok {
		t.Error("a window after the deadline")
	}
	if _, ok := firstFreeWindow([]busyPeriod{{from: at(2, 9), open: true}}, at(2, 8), at(5, 0), four); ok {
		t.Error("a window before an open-ended block that is too short, or after it")
	}
	if got, ok := firstFreeWindow([]busyPeriod{{from: at(2, 13), open: true}}, at(2, 8), at(5, 0), four); !ok || !got.Equal(at(2, 8)) {
		t.Errorf("a fit before an open-ended block: %v %v", got, ok)
	}
}
