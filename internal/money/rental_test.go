package money

import (
	"errors"
	"testing"
	"time"
)

func price(v int64) *int64 { return &v }

var stockholm = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		panic(err)
	}
	return loc
}()

func sthlm(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, stockholm)
	if err != nil {
		panic(err)
	}
	return t
}

func total(lines []RentalLine) int64 {
	var n int64
	for _, l := range lines {
		n += l.Net()
	}
	return n
}

// A day and an hour: twenty-five hours is a day and an hour, or two days
// when the hour costs more than the day it would round up to; within the
// grace minutes it is the day alone.
func TestATwentyFiveHourRental(t *testing.T) {
	terms := RateTerms{Prices: RatePrices{Hour: price(15000), Day: price(50000)}}
	from := sthlm("2026-11-10 09:00")
	lines, err := PriceRental(from, from.Add(25*time.Hour), stockholm, terms)
	if err != nil || len(lines) != 2 || total(lines) != 65000 {
		t.Errorf("25 hours, no grace: %+v, %v", lines, err)
	}
	terms.Grace = 59 * time.Minute
	lines, err = PriceRental(from, from.Add(24*time.Hour+59*time.Minute), stockholm, terms)
	if err != nil || len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitDay, Count: 1, UnitPrice: 50000}) {
		t.Errorf("24 hours and 59 minutes within an hour's grace: %+v, %v", lines, err)
	}
	terms.Grace = 0
	terms.Prices.Hour = price(60000)
	if lines, _ := PriceRental(from, from.Add(25*time.Hour), stockholm, terms); len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitDay, Count: 2, UnitPrice: 50000}) {
		t.Errorf("an hour dearer than a day: %+v", lines)
	}
}

// A day is a calendar day on the wall clock: Saturday 10:00 to Sunday
// 10:00 is one day, and Friday to Monday a weekend, the night the clocks go
// forward in March and the night they go back in October.
func TestAWeekendAcrossTheChangeOfHour(t *testing.T) {
	terms := RateTerms{Prices: RatePrices{Day: price(50000), Weekend: price(90000)}}
	for _, c := range []struct{ from, to string }{
		{"2026-03-27 15:00", "2026-03-30 09:00"},
		{"2026-10-23 15:00", "2026-10-26 09:00"},
	} {
		lines, err := PriceRental(sthlm(c.from), sthlm(c.to), stockholm, terms)
		if err != nil || len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitWeekend, Count: 1, UnitPrice: 90000}) {
			t.Errorf("%s to %s: %+v, %v", c.from, c.to, lines, err)
		}
	}
	for _, c := range []struct{ from, to string }{
		{"2026-03-28 10:00", "2026-03-29 10:00"}, // 23 hours elapsed
		{"2026-10-24 10:00", "2026-10-25 10:00"}, // 25 hours elapsed
	} {
		lines, err := PriceRental(sthlm(c.from), sthlm(c.to), stockholm, RateTerms{Prices: RatePrices{Day: price(50000), Hour: price(10000)}})
		if err != nil || len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitDay, Count: 1, UnitPrice: 50000}) {
			t.Errorf("%s to %s is one day: %+v, %v", c.from, c.to, lines, err)
		}
	}
	// Starting Friday morning is not the weekend.
	lines, _ := PriceRental(sthlm("2026-11-06 09:00"), sthlm("2026-11-09 09:00"), stockholm, terms)
	if len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitDay, Count: 3, UnitPrice: 50000}) {
		t.Errorf("Friday morning to Monday: %+v", lines)
	}
}

// Across a season boundary: priced under the season it starts in, or split
// at the midnight the new season starts.
func TestASeasonBoundaryInsideARental(t *testing.T) {
	low := RateTerms{Prices: RatePrices{Day: price(40000)}, Grace: time.Hour}
	high := RateTerms{Prices: RatePrices{Day: price(70000)}, Grace: time.Hour}
	from, to, boundary := sthlm("2026-05-30 10:00"), sthlm("2026-06-02 10:30"), sthlm("2026-06-01 00:00")
	seasons := []Season{{sthlm("2026-01-01 00:00"), low}, {boundary, high}, {sthlm("2026-09-01 00:00"), low}}
	at, err := PriceRentalSeasons(from, to, stockholm, seasons, false)
	if err != nil || total(at) != 3*40000 {
		t.Errorf("priced at the start: %+v, %v", at, err)
	}
	split, err := PriceRentalSeasons(from, to, stockholm, seasons, true)
	// 30 May 10:00 to 1 June 00:00 is a day and 14 hours: two days low;
	// 1 June 00:00 to 2 June 10:30 is a day and 10.5 hours, within an
	// hour's grace of neither: two days high.
	if err != nil || total(split) != 2*40000+2*70000 || len(split) != 2 {
		t.Errorf("split: %+v, %v", split, err)
	}
	if _, err := PriceRentalSeasons(from, to, stockholm, seasons[1:], true); !errors.Is(err, ErrNoPriceFor) {
		t.Errorf("no season at the start: %v", err)
	}
}

// A courtesy plan prices at nothing; a plan that prices nothing that
// covers the rental says so; a minimum is charged in full.
func TestZeroPricesMinimumsAndNoPrice(t *testing.T) {
	from := sthlm("2026-11-10 09:00")
	lines, err := PriceRental(from, from.Add(50*time.Hour), stockholm, RateTerms{Prices: RatePrices{Day: price(0)}})
	if err != nil || total(lines) != 0 || lines[0].Count != 3 {
		t.Errorf("a courtesy plan: %+v, %v", lines, err)
	}
	if _, err := PriceRental(from, from.Add(2*time.Hour), stockholm, RateTerms{Prices: RatePrices{Day: price(50000)}, Minimum: 0}); err != nil {
		t.Errorf("two hours by the day: %v", err)
	}
	if _, err := PriceRental(from, from.Add(2*time.Hour), stockholm, RateTerms{Prices: RatePrices{}}); !errors.Is(err, ErrNoPriceFor) {
		t.Errorf("no prices at all: %v", err)
	}
	lines, _ = PriceRental(from, from.Add(time.Hour), stockholm, RateTerms{Prices: RatePrices{Hour: price(10000)}, Minimum: 4 * time.Hour})
	if total(lines) != 40000 {
		t.Errorf("an hour against a four-hour minimum: %+v", lines)
	}
	// A week costs less than seven days, a month less than four weeks and
	// some days: the cheaper is taken.
	lines, _ = PriceRental(from, from.AddDate(0, 0, 6), stockholm, RateTerms{Prices: RatePrices{Day: price(50000), Week: price(250000)}})
	if len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitWeek, Count: 1, UnitPrice: 250000}) {
		t.Errorf("six days against a week: %+v", lines)
	}
	lines, _ = PriceRental(from, from.AddDate(0, 1, 0), stockholm, RateTerms{Prices: RatePrices{Day: price(50000), Week: price(250000), Month: price(800000)}})
	if len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitMonth, Count: 1, UnitPrice: 800000}) {
		t.Errorf("a month: %+v", lines)
	}
}

// Grace applies to a weekend as to any unit: Friday afternoon to Monday
// 12:30 with an hour's grace is the weekend (review #6).
func TestGraceKeepsTheWeekend(t *testing.T) {
	terms := RateTerms{Prices: RatePrices{Day: price(50000), Weekend: price(90000)}, Grace: time.Hour}
	lines, err := PriceRental(sthlm("2026-11-06 14:00"), sthlm("2026-11-09 12:30"), stockholm, terms)
	if err != nil || len(lines) != 1 || lines[0].Unit != UnitWeekend {
		t.Errorf("returned within grace of Monday noon: %+v, %v", lines, err)
	}
}
