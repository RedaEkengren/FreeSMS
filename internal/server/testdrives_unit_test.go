package server

import (
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The test-drive page asks whether a licence is good on the shop's own
// day, as StartTestDrive does: half past midnight in Stockholm is already
// the next day there, though not yet in UTC, and a licence that ran out the
// day before is not good, on the page or at the start.
func TestALicenceIsJudgedOnTheShopsDay(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, time.November, 3, 0, 30, 0, 0, stockholm)
	day := shopDay(now, stockholm)
	if want := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC); !day.Equal(want) {
		t.Fatalf("the shop's day at %s: %s, want %s", now, day, want)
	}
	until := time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)
	c := workshop.LicenceCheck{IDChecked: true, AgeOK: true, Categories: []string{"B"}, ValidUntil: &until}
	if c.Passes(day) {
		t.Error("a licence that ran out yesterday passes on the shop's day")
	}
	until = day
	if !c.Passes(day) {
		t.Error("a licence valid until today does not pass today")
	}
}
