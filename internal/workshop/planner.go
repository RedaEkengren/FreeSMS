package workshop

import (
	"fmt"
	"sort"
	"time"
)

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
	// The technician is away that day: the booking needs somebody else.
	Away bool
	// Overlaps another booking on the same row: one person, two cars.
	Clash bool
	// Outside the hours the technician is on the rota for that day.
	OffShift bool
	// Minutes the car is past its slot with its job still open.
	Overrun int
	// Minutes this booking is expected to start late because the work
	// before it on the row runs over, and whose work that is. Shown, never
	// moved: whether to ring the customer, give the car to somebody else or
	// stay late is a person's decision.
	Late      int
	LateAfter string
}

// PlannerLane is one row of a day.
type PlannerLane struct {
	Row    PlannerRow
	Blocks []PlannerBlock
	// Why the row has no capacity that day, as a catalogue key.
	Away string
}

// PlannerDay is one day of the planner.
type PlannerDay struct {
	Date  time.Time
	Lanes []PlannerLane
	Count int
	// Why the shop is shut, when it is.
	Closed string
	// When more cars are booked at once than there are lifts, as "10:00-11:30".
	LiftsOver []string
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
func BuildWeek(first time.Time, days int, rows []PlannerRow, bookings []Booking, capacity Capacity, now time.Time, loc *time.Location) []PlannerDay {
	unassigned := PlannerRow{ID: "", Name: ""}
	all := append(append([]PlannerRow{}, rows...), unassigned)
	var out []PlannerDay
	for d := 0; d < days; d++ {
		y, m, dd := first.In(loc).Date()
		date := time.Date(y, m, dd+d, 0, 0, 0, 0, loc)
		next := time.Date(y, m, dd+d+1, 0, 0, 0, 0, loc)
		key := date.Format("2006-01-02")
		day := PlannerDay{Date: date, Closed: capacity.Closed[key]}
		known := map[string]bool{}
		for _, r := range rows {
			known[r.ID] = true
		}
		for _, r := range all {
			lane := PlannerLane{Row: r}
			shift, onRota := capacity.Shifts[r.ID][key]
			onRota = onRota && shift.Known
			if r.ID != "" {
				if reason, ok := capacity.Absent[r.ID][key]; ok {
					lane.Away = AbsenceReasons[reason]
				} else if onRota && !shift.Working {
					lane.Away = "not on the rota today"
				}
			}
			for _, b := range bookings {
				owner := b.TechnicianID
				if !known[owner] {
					owner = ""
				}
				if owner != r.ID || !b.Starts.Before(next) || !b.Ends.After(date) {
					continue
				}
				blk := place(b, date, next, loc)
				blk.Away = lane.Away != ""
				blk.OffShift = !blk.Away && onRota && shift.Working &&
					(b.Starts.Before(shift.Starts) || b.Ends.After(shift.Ends))
				lane.Blocks = append(lane.Blocks, blk)
				day.Count++
			}
			markClashes(lane.Blocks)
			if r.ID != "" {
				markLate(lane.Blocks, now)
			}
			// An empty "not given to anybody" row is noise.
			if r.ID == "" && len(lane.Blocks) == 0 {
				continue
			}
			day.Lanes = append(day.Lanes, lane)
		}
		if capacity.Lifts > 0 {
			day.LiftsOver = liftsOver(day.Lanes, capacity.Lifts)
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

// markClashes flags blocks on one row that overlap: one person, two cars.
func markClashes(blocks []PlannerBlock) {
	for i := range blocks {
		for j := range blocks {
			if i != j && blocks[i].Booking.Starts.Before(blocks[j].Booking.Ends) &&
				blocks[j].Booking.Starts.Before(blocks[i].Booking.Ends) {
				blocks[i].Clash = true
			}
		}
	}
}

// liftsOver is when more cars are on the planner at once than there are
// lifts, as wall-clock spans.
func liftsOver(lanes []PlannerLane, lifts int) []string {
	var count [PlannerSlots]int
	for _, l := range lanes {
		for _, b := range l.Blocks {
			for s := b.Start; s < b.Start+b.Span && s < PlannerSlots; s++ {
				count[s]++
			}
		}
	}
	clock := func(slot int) string {
		m := plannerOpen + slot*plannerSlot
		return fmt.Sprintf("%02d:%02d", m/60, m%60)
	}
	var out []string
	for s := 0; s < PlannerSlots; {
		if count[s] <= lifts {
			s++
			continue
		}
		e := s
		for e < PlannerSlots && count[e] > lifts {
			e++
		}
		out = append(out, clock(s)+"–"+clock(e))
		s = e
	}
	return out
}

// markLate carries an overrun down one person's day. Each booking starts
// when it was booked or when the work before it is expected to end,
// whichever is later, and takes its estimate -- or its slot, without one.
// A job still open past its slot is expected to end no earlier than now.
func markLate(blocks []PlannerBlock, now time.Time) {
	order := make([]int, len(blocks))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return blocks[order[a]].Booking.Starts.Before(blocks[order[b]].Booking.Starts)
	})
	var free time.Time
	var cause string
	for _, i := range order {
		blk := &blocks[i]
		b := blk.Booking
		start := b.Starts
		if free.After(start) && !blk.Clash {
			blk.Late = int(free.Sub(start).Minutes())
			blk.LateAfter = cause
			start = free
		}
		work := b.Ends.Sub(b.Starts)
		if b.EstimateMinutes != nil && time.Duration(*b.EstimateMinutes)*time.Minute > work {
			work = time.Duration(*b.EstimateMinutes) * time.Minute
		}
		end := start.Add(work)
		if b.JobOpen && now.After(b.Ends) {
			blk.Overrun = int(now.Sub(b.Ends).Minutes())
			if now.After(end) {
				end = now
			}
		}
		// The cause stays where the lateness began: three cars down a
		// morning, the third is late because of the first.
		if end.After(free) {
			free = end
			switch {
			case blk.Late > 0:
			case end.After(b.Ends):
				cause = b.Registration
			default:
				cause = ""
			}
		}
	}
}
