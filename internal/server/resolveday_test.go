package server

import (
	"testing"
	"time"
)

// A date typed into a form is a moment, and the moment must not be before
// something recorded earlier the same day.
func TestADateTypedForTodayIsNowWhateverTheHour(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	for _, hour := range []int{0, 7, 11, 12, 13, 18, 23} {
		now := time.Date(2026, 10, 16, hour, 30, 0, 0, loc)
		got, ok := resolveDay("2026-10-16", now, loc)
		if !ok || !got.Equal(now) {
			t.Errorf("at %02d:30 today resolved to %v, want now", hour, got)
		}
	}
	now := time.Date(2026, 10, 16, 15, 0, 0, 0, loc)
	if got, _ := resolveDay("2026-10-15", now, loc); !got.Equal(time.Date(2026, 10, 15, 12, 0, 0, 0, loc)) {
		t.Errorf("yesterday resolved to %v, want noon", got)
	}
	if got, _ := resolveDay("2026-10-20", now, loc); !got.Equal(now) {
		t.Errorf("a day to come resolved to %v, want now", got)
	}
	if _, ok := resolveDay("16/10/2026", now, loc); ok {
		t.Error("a date in another format was accepted")
	}
	// Just after midnight, still the shop's today and not UTC's.
	late := time.Date(2026, 10, 17, 0, 30, 0, 0, loc)
	if got, _ := resolveDay("2026-10-17", late, loc); !got.Equal(late) {
		t.Errorf("half past midnight resolved to %v", got)
	}
}
