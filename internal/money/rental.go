package money

import (
	"errors"
	"time"
)

// Rental periods into billed units (#191). One rule, written here once:
//
//  1. A rental is measured on the wall clock of the shop's zone, so a day is
//     a calendar day: Friday 10:00 to Saturday 10:00 is one day whether the
//     clocks changed in between or not, as the customer reads it.
//  2. The customer pays for the cheapest combination of whole units the
//     plan prices -- months, weeks, days, hours, counted in that order from
//     the start -- that covers the rental. A month is a calendar month from
//     the start, a week seven days, a day twenty-four wall-clock hours.
//  3. A rental shorter than the plan's minimum is charged as the minimum.
//  4. The grace minutes are time past the end of what is paid for that is
//     not charged: returned within them, no further unit is added.
//  5. A weekend, if the plan prices one, is one unit covering a rental that
//     starts on a Friday at 12:00 or later and ends by the Monday after at
//     12:00. It is used when it is cheaper than rule 2; it is never
//     combined with other units.
//  6. A rental across the boundary of two seasons is priced, by the shop's
//     setting, either wholly under the season it starts in, or split at
//     midnight starting the new season, each part priced by rules 1-5 on
//     its own -- the grace minutes and the minimum on the last part only,
//     and a weekend only when it is not split.
//
// Every line is a whole number of units at its unit price; the amounts are
// LineNet of those, rounded per line then summed, as everywhere else.

// RatePrices are a plan's prices per unit, net, in minor units; nil for a
// unit it does not price.
type RatePrices struct {
	Hour, Day, Weekend, Week, Month *int64
}

// RateTerms are a plan's rules for a period.
type RateTerms struct {
	Prices  RatePrices
	Minimum time.Duration
	Grace   time.Duration
}

// The units, as a line names them.
const (
	UnitHour    = "hour"
	UnitDay     = "day"
	UnitWeekend = "weekend"
	UnitWeek    = "week"
	UnitMonth   = "month"
)

// RentalLine is units of one kind at one price.
type RentalLine struct {
	Unit      string
	Count     int64
	UnitPrice int64
	// Which of the seasons given it was priced under: 0 unless split.
	Season int
}

// Net is the line's amount.
func (l RentalLine) Net() int64 { return LineNet(l.Count*1000, l.UnitPrice) }

// ErrNoPriceFor is a plan that prices nothing that covers the rental.
var ErrNoPriceFor = errors.New("money: the plan has no price for a rental this long")

// wall is t read as a wall clock: the same date and time, in UTC, so that
// adding days and hours counts the wall clock and not elapsed time.
func wall(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// PriceRental bills from..to, instants, read in loc, under terms.
func PriceRental(from, to time.Time, loc *time.Location, terms RateTerms) ([]RentalLine, error) {
	if !to.After(from) {
		return nil, errors.New("money: a rental ends after it starts")
	}
	return priceWall(wall(from, loc), wallEnd(from, to, loc), terms, true)
}

// WallDuration is how long from..to reads on the wall clock in loc.
func WallDuration(from, to time.Time, loc *time.Location) time.Duration {
	return wallEnd(from, to, loc).Sub(wall(from, loc))
}

// wallEnd is to as a wall clock, after from's. The hour the clocks go back
// is lived twice: a rental inside it reads, on the wall clock, as ending
// before it began, and is counted by the time it took.
func wallEnd(from, to time.Time, loc *time.Location) time.Time {
	wf, wt := wall(from, loc), wall(to, loc)
	if !wt.After(wf) {
		return wf.Add(to.Sub(from))
	}
	return wt
}

// Season is a plan's terms from a moment on, to the next season's start.
type Season struct {
	Starts time.Time
	Terms  RateTerms
}

// PriceRentalSeasons bills from..to under seasons, in order of their start,
// the first starting by from, under rule 6: wholly by the season the rental
// starts in, or split at each later season's start.
func PriceRentalSeasons(from, to time.Time, loc *time.Location, seasons []Season, split bool) ([]RentalLine, error) {
	if len(seasons) == 0 || seasons[0].Starts.After(from) {
		return nil, ErrNoPriceFor
	}
	if !to.After(from) {
		return nil, errors.New("money: a rental ends after it starts")
	}
	// The season the rental starts in, and those starting inside it.
	first := 0
	for i, s := range seasons {
		if !s.Starts.After(from) {
			first = i
		}
	}
	inside := seasons[first:]
	n := 1
	for n < len(inside) && inside[n].Starts.Before(to) {
		n++
	}
	inside = inside[:n]
	if !split || len(inside) == 1 {
		lines, err := PriceRental(from, to, loc, inside[0].Terms)
		for j := range lines {
			lines[j].Season = first
		}
		return lines, err
	}
	var out []RentalLine
	for i, s := range inside {
		a, b := from, to
		if i > 0 {
			a = s.Starts
		}
		last := i == len(inside)-1
		if !last {
			b = inside[i+1].Starts
		}
		terms := s.Terms
		if !last {
			terms.Minimum, terms.Grace = 0, 0
		}
		lines, err := priceWall(wallEnd(from, a, loc), wallEnd(from, b, loc), terms, false)
		if err != nil {
			return nil, err
		}
		for j := range lines {
			lines[j].Season = first + i
		}
		out = append(out, lines...)
	}
	return out, nil
}

func priceWall(from, to time.Time, t RateTerms, weekend bool) ([]RentalLine, error) {
	if !to.After(from) {
		return nil, errors.New("money: a rental ends after it starts")
	}
	if min := from.Add(t.Minimum); to.Before(min) {
		to = min
	}
	need := to.Add(-t.Grace)
	if !need.After(from) {
		need = from.Add(time.Nanosecond)
	}
	p := t.Prices
	var best []RentalLine
	bestNet := int64(-1)
	consider := func(lines []RentalLine) {
		var n int64
		for _, l := range lines {
			n += l.Net()
		}
		if bestNet < 0 || n < bestNet {
			best, bestNet = lines, n
		}
	}
	if weekend && p.Weekend != nil && inOneWeekend(from, need) {
		consider([]RentalLine{{Unit: UnitWeekend, Count: 1, UnitPrice: *p.Weekend}})
	}
	maxMonths := int64(0)
	if p.Month != nil {
		for from.AddDate(0, int(maxMonths), 0).Before(need) {
			maxMonths++
		}
	}
	for m := int64(0); m <= maxMonths; m++ {
		afterM := from.AddDate(0, int(m), 0)
		rem := need.Sub(afterM)
		maxWeeks := int64(0)
		if p.Week != nil && rem > 0 {
			maxWeeks = ceilDiv(rem, 7*24*time.Hour)
		}
		for w := int64(0); w <= maxWeeks; w++ {
			afterW := afterM.Add(time.Duration(w) * 7 * 24 * time.Hour)
			rem := need.Sub(afterW)
			maxDays := int64(0)
			if p.Day != nil && rem > 0 {
				maxDays = ceilDiv(rem, 24*time.Hour)
			}
			for d := int64(0); d <= maxDays; d++ {
				rem := need.Sub(afterW.Add(time.Duration(d) * 24 * time.Hour))
				var h int64
				if rem > 0 {
					if p.Hour == nil {
						continue
					}
					h = ceilDiv(rem, time.Hour)
				}
				var lines []RentalLine
				for _, l := range []struct {
					unit  string
					n     int64
					price *int64
				}{{UnitMonth, m, p.Month}, {UnitWeek, w, p.Week}, {UnitDay, d, p.Day}, {UnitHour, h, p.Hour}} {
					if l.n > 0 {
						lines = append(lines, RentalLine{Unit: l.unit, Count: l.n, UnitPrice: *l.price})
					}
				}
				if len(lines) > 0 {
					consider(lines)
				}
			}
		}
	}
	if best == nil {
		return nil, ErrNoPriceFor
	}
	return best, nil
}

func ceilDiv(d, unit time.Duration) int64 { return int64((d + unit - 1) / unit) }

// inOneWeekend reports whether a wall-clock rental starts on a Friday at
// 12:00 or later and ends by the following Monday at 12:00.
func inOneWeekend(from, to time.Time) bool {
	var friday time.Time
	switch from.Weekday() {
	case time.Friday:
		if from.Hour() < 12 {
			return false
		}
		friday = from
	case time.Saturday:
		friday = from.AddDate(0, 0, -1)
	case time.Sunday:
		friday = from.AddDate(0, 0, -2)
	default:
		return false
	}
	monday := time.Date(friday.Year(), friday.Month(), friday.Day()+3, 12, 0, 0, 0, time.UTC)
	return !to.After(monday)
}
