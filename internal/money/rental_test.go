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
	// Three days and half an hour, within an hour's grace of three days:
	// the days that start on 30 and 31 May are low, the one that starts on
	// 1 June high. Splitting adds no unit the customer did not use.
	if err != nil || total(split) != 2*40000+70000 || len(split) != 2 {
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

// A day across the midnight a season starts is one day, of the season it
// started in; a minimum is the whole rental's, not a part's (review #7).
func TestSplittingAddsNoUnits(t *testing.T) {
	low := RateTerms{Prices: RatePrices{Day: price(40000), Hour: price(10000)}, Minimum: 4 * time.Hour}
	high := RateTerms{Prices: RatePrices{Day: price(70000), Hour: price(20000)}, Minimum: 4 * time.Hour}
	seasons := []Season{{sthlm("2026-01-01 00:00"), low}, {sthlm("2026-06-01 00:00"), high}}
	lines, err := PriceRentalSeasons(sthlm("2026-05-31 10:00"), sthlm("2026-06-01 10:00"), stockholm, seasons, true)
	if err != nil || len(lines) != 1 || lines[0] != (RentalLine{Unit: UnitDay, Count: 1, UnitPrice: 40000}) {
		t.Errorf("a day across the boundary: %+v, %v", lines, err)
	}
	lines, err = PriceRentalSeasons(sthlm("2026-05-31 22:00"), sthlm("2026-06-01 01:00"), stockholm, seasons, true)
	// Three hours against a four-hour minimum: four hours, two before
	// midnight and two after.
	if err != nil || total(lines) != 2*10000+2*20000 {
		t.Errorf("a minimum across the boundary: %+v, %v", lines, err)
	}
}

// A rental is priced up to five years; longer is refused rather than
// searched for (review).
func TestARentalLongerThanFiveYearsIsRefused(t *testing.T) {
	terms := RateTerms{Prices: RatePrices{Day: price(1), Week: price(5), Month: price(20)}}
	from := sthlm("2026-01-01 00:00")
	if _, err := PriceRental(from, from.AddDate(5, 0, 1), stockholm, terms); !errors.Is(err, ErrTooLong) {
		t.Errorf("five years and a day: %v", err)
	}
	if _, err := PriceRental(from, from.AddDate(5, 0, 0), stockholm, terms); err != nil {
		t.Errorf("five years: %v", err)
	}
}

// Split by units, the months are counted from the start as priceWall counts
// them: from 31 January two months end on 31 March, and the day that starts
// then is the low season's (review 2 #5).
func TestSplitMonthsAreCountedFromTheStart(t *testing.T) {
	low := RateTerms{Prices: RatePrices{Day: price(100), Month: price(1000)}}
	high := RateTerms{Prices: RatePrices{Day: price(200), Month: price(2000)}}
	seasons := []Season{{sthlm("2031-01-01 00:00"), low}, {sthlm("2031-04-01 00:00"), high}}
	lines, err := PriceRentalSeasons(sthlm("2031-01-31 10:00"), sthlm("2031-04-05 10:00"), stockholm, seasons, true)
	if err != nil || total(lines) != 2*1000+100+4*200 {
		t.Errorf("two months and five days from 31 January: %+v (%d), %v", lines, total(lines), err)
	}
}

// A one-hour rental under a plan with a three-hour minimum and an hour's
// grace is charged as the minimum: three hours. The grace is time past the
// end of what is paid for (rule 4), not time taken off the minimum (rule 3).
func TestTheGraceDoesNotCutTheMinimum(t *testing.T) {
	hour := int64(10000)
	terms := RateTerms{Prices: RatePrices{Hour: &hour}, Minimum: 3 * time.Hour, Grace: time.Hour}
	from := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	lines, err := PriceRental(from, from.Add(time.Hour), time.UTC, terms)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, l := range lines {
		n += l.Net()
	}
	if n != 3*hour {
		t.Fatalf("a one-hour rental with a three-hour minimum is charged %d, want %d (lines %+v)", n, 3*hour, lines)
	}
}

// An extension is priced on the rental it extends: the minimum the rental
// already paid for is not charged again, nothing is added within the grace,
// and past both only the time added is charged.
func TestAnExtensionIsPricedOnTheWholeRental(t *testing.T) {
	day := int64(40000)
	seasons := []Season{{Starts: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		Terms: RateTerms{Prices: RatePrices{Day: &day}, Minimum: 72 * time.Hour, Grace: time.Hour}}}
	start := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	sum := func(end, to time.Time) int64 {
		t.Helper()
		lines, err := PriceExtension(start, end, to, time.UTC, seasons, false)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, l := range lines {
			n += l.Net()
		}
		return n
	}
	d := func(n int) time.Time { return start.Add(time.Duration(n) * 24 * time.Hour) }
	for _, c := range []struct {
		name    string
		end, to time.Time
		want    int64
	}{
		{"three days booked, one added", d(3), d(4), day},
		{"one day booked and three paid, to two", d(1), d(2), 0},
		{"one day booked and three paid, to four", d(1), d(4), day},
		{"within the grace", d(3), d(3).Add(30 * time.Minute), 0},
		{"just past the grace", d(3), d(3).Add(61 * time.Minute), day},
	} {
		if got := sum(c.end, c.to); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
