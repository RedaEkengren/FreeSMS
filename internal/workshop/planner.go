package workshop

import "time"

// The planner draws a day as half-hour slots between opening and closing,
// one row per technician and one for bookings not yet given to anybody. A
// block's place is its wall clock in the shop's zone, so it says *when* on
// Thursday -- the incumbent's blocks only said how long -- and a Sunday
// morning slot stays at nine across the change of hour in March.

const (
	plannerOpen  = 7 * 60  // 07:00
	plannerClose = 18 * 60 // 18:00
	plannerSlot  = 30      // minutes
)

// PlannerSlots is how many slots a day has.
const PlannerSlots = (plannerClose - plannerOpen) / plannerSlot

// PlannerBlock is a booking placed on a row.
type PlannerBlock struct {
	Booking Booking
	// The first slot, from zero, and how many it covers.
	Start, Span int
	// Starts before opening or ends after closing: drawn to the edge and
	// said so, rather than drawn somewhere it is not.
	Clipped bool
}

// PlannerLane is one row of a day.
type PlannerLane struct {
	Row    PlannerRow
	Blocks []PlannerBlock
}

// PlannerDay is one day of the planner.
type PlannerDay struct {
	Date  time.Time
	Lanes []PlannerLane
	Count int
}

// PlannerHours are the hour marks for the axis: 07, 08 ... 17.
func PlannerHours() []int {
	var out []int
	for m := plannerOpen; m < plannerClose; m += 60 {
		out = append(out, m/60)
	}
	return out
}

// BuildWeek lays bookings out over days days from first, in the shop's zone.
func BuildWeek(first time.Time, days int, rows []PlannerRow, bookings []Booking, loc *time.Location) []PlannerDay {
	unassigned := PlannerRow{ID: "", Name: ""}
	all := append(append([]PlannerRow{}, rows...), unassigned)
	var out []PlannerDay
	for d := 0; d < days; d++ {
		y, m, dd := first.In(loc).Date()
		date := time.Date(y, m, dd+d, 0, 0, 0, 0, loc)
		next := time.Date(y, m, dd+d+1, 0, 0, 0, 0, loc)
		day := PlannerDay{Date: date}
		known := map[string]bool{}
		for _, r := range rows {
			known[r.ID] = true
		}
		for _, r := range all {
			lane := PlannerLane{Row: r}
			for _, b := range bookings {
				owner := b.TechnicianID
				if !known[owner] {
					owner = ""
				}
				if owner != r.ID || !b.Starts.Before(next) || !b.Ends.After(date) {
					continue
				}
				lane.Blocks = append(lane.Blocks, place(b, date, next, loc))
				day.Count++
			}
			// An empty "not given to anybody" row is noise.
			if r.ID == "" && len(lane.Blocks) == 0 {
				continue
			}
			day.Lanes = append(day.Lanes, lane)
		}
		out = append(out, day)
	}
	return out
}

// place puts a booking on a day by its wall clock: minutes since midnight in
// the shop's zone, not the time since midnight, which is an hour out on the
// two days a year the clocks change.
func place(b Booking, day, next time.Time, loc *time.Location) PlannerBlock {
	wall := func(t time.Time) int {
		lt := t.In(loc)
		return lt.Hour()*60 + lt.Minute()
	}
	from, to := plannerOpen, plannerClose
	start, end := from, to
	if !b.Starts.Before(day) {
		start = wall(b.Starts)
	}
	if b.Ends.Before(next) {
		end = wall(b.Ends)
	}
	block := PlannerBlock{Booking: b}
	if start < from {
		start, block.Clipped = from, true
	}
	if end > to {
		end, block.Clipped = to, true
	}
	if end <= start {
		end = start + plannerSlot
		block.Clipped = true
	}
	block.Start = (start - from) / plannerSlot
	block.Span = max((end-from+plannerSlot-1)/plannerSlot-block.Start, 1)
	if block.Start >= PlannerSlots {
		block.Start, block.Span = PlannerSlots-1, 1
	}
	return block
}
