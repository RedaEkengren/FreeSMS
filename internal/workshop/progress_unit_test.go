package workshop

import (
	"testing"
	"time"
)

func at(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// No labour line is no estimate: not 0 per cent, not a division by zero,
// and no verdict that pretends to know.
func TestNoEstimateIsNoEstimate(t *testing.T) {
	p := Progress{ClockedMinutes: 90, AsOf: *at("2026-10-06T10:00:00Z"), PromisedAt: at("2026-10-06T16:00:00Z")}
	if p.Known() || p.Percent() != 0 || p.Verdict() != "" {
		t.Errorf("known %v, %d%%, verdict %q; want no estimate and no verdict", p.Known(), p.Percent(), p.Verdict())
	}
}

// Over the estimate is not clamped: 140 per cent is what it says, the bar
// is full, and the estimate's mark moves to where 100 per cent falls.
func TestOverTheEstimateIsNotClamped(t *testing.T) {
	p := Progress{EstimateMinutes: 100, ClockedMinutes: 140}
	if p.Percent() != 140 || !p.Over() || p.LeftMinutes() != 0 {
		t.Errorf("%d%%, over %v, %d left; want 140, over, nothing left", p.Percent(), p.Over(), p.LeftMinutes())
	}
	if fill, mark := p.Bar(); fill != 100 || mark != 71 {
		t.Errorf("bar fill %d mark %d, want the bar full and the estimate at 71", fill, mark)
	}
	under := Progress{EstimateMinutes: 240, ClockedMinutes: 60}
	if fill, mark := under.Bar(); fill != 25 || mark != 100 {
		t.Errorf("under the estimate: fill %d mark %d, want 25 and 100", fill, mark)
	}
}

// The promise against what is left: on time when it fits, at risk when it
// does not or the estimate is used up, late when the promise has passed --
// and nothing for a car that is finished.
func TestTheVerdict(t *testing.T) {
	promised := at("2026-10-06T14:00:00Z") // 16:00 in Stockholm
	for _, c := range []struct {
		name string
		p    Progress
		want string
	}{
		{"two hours left at 15:00 for 16:00", Progress{EstimateMinutes: 240, ClockedMinutes: 120, AsOf: *at("2026-10-06T13:00:00Z")}, "at risk"},
		{"two hours left at 13:00 for 16:00", Progress{EstimateMinutes: 240, ClockedMinutes: 120, AsOf: *at("2026-10-06T11:00:00Z")}, "on time"},
		{"over the estimate before the promise", Progress{EstimateMinutes: 60, ClockedMinutes: 90, AsOf: *at("2026-10-06T09:00:00Z")}, "at risk"},
		{"past the promise", Progress{EstimateMinutes: 240, ClockedMinutes: 230, AsOf: *at("2026-10-06T14:30:00Z")}, "late"},
		{"past the promise, finished", Progress{EstimateMinutes: 240, ClockedMinutes: 230, AsOf: *at("2026-10-06T14:30:00Z"), Finished: true}, ""},
		{"nothing promised", Progress{EstimateMinutes: 240, AsOf: *at("2026-10-06T14:30:00Z")}, ""},
	} {
		c.p.PromisedAt = promised
		if c.name == "nothing promised" {
			c.p.PromisedAt = nil
		}
		if got := c.p.Verdict(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The night the clocks go forward, 01:30 to 04:30 on the wall is two hours,
// not three. Two and a half hours of work does not fit -- which a sum on the
// wall clock would get wrong.
func TestTheVerdictCountsRealTimeAcrossTheClockChange(t *testing.T) {
	p := Progress{
		EstimateMinutes: 150,
		AsOf:            *at("2026-03-29T00:30:00Z"), // 01:30 in Stockholm
		PromisedAt:      at("2026-03-29T02:30:00Z"),  // 04:30 in Stockholm
	}
	if got := p.Verdict(); got != "at risk" {
		t.Errorf("verdict %q, want at risk: two real hours for two and a half of work", got)
	}
}
