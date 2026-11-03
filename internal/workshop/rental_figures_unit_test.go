package workshop

import (
	"testing"
	"time"
)

// The figures' definitions (#203), on spans alone.
func TestTheRentalFiguresAreWhatTheirDefinitionsSay(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
	period := span{day(1, 0), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	joined := []span{{day(11, 0), day(31, 0).Add(24 * time.Hour)}} // pro rata: 21 days of the month
	rentals := []span{{day(12, 10), day(15, 10)}}
	down := []span{{day(25, 0), day(27, 0)}}
	buffers := []span{{day(15, 10), day(15, 12)}}
	// Closed on the 13th, when it was out -- not lost -- and the 20th.
	closed := []span{{day(13, 0), day(14, 0)}, {day(20, 0), day(21, 0)}}
	available, rented, downtime, held := figuresFor(period, joined, rentals, down, buffers, closed)
	d := 24 * time.Hour
	f := UnitFigures{Available: available, Rented: rented}
	switch {
	case available != 20*d:
		t.Errorf("available %v, want 20 days: 21 in the fleet less one closed and not rented", available)
	case rented != 3*d:
		t.Errorf("rented %v, want 3 days", rented)
	case downtime != 2*d:
		t.Errorf("downtime %v, want 2 days", downtime)
	case held != 2*time.Hour:
		t.Errorf("buffers %v, want 2 hours", held)
	case f.Utilisation() != 150:
		t.Errorf("utilisation %d tenths of a per cent, want 150", f.Utilisation())
	}
	// Left the fleet on the 6th: five days, whatever is booked after.
	if a, r, _, _ := figuresFor(period, []span{{day(1, 0), day(6, 0)}}, rentals, nil, nil, nil); a != 5*d || r != 0 {
		t.Errorf("a unit gone on the 6th: available %v, rented %v", a, r)
	}
	if (UnitFigures{}).Utilisation() != 0 {
		t.Error("a unit never available has a utilisation")
	}
	// Rent spread over the days, rounded half away from zero, adds up.
	if a, b := share(1000, d, 3*d), share(1000, 2*d, 3*d); a != 333 || b != 667 || a+b != 1000 {
		t.Errorf("1000 over 1 and 2 of 3 days: %d and %d", a, b)
	}
	if share(-1000, d, 3*d) != -333 || share(1000, d, 0) != 0 {
		t.Error("a credit spread, or nothing to spread over")
	}
}
