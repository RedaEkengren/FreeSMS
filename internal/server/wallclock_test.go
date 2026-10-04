package server

import (
	"testing"
	"time"
)

// A wall clock typed in a Stockholm workshop is read as Stockholm, whatever
// zone the server or the browser is in, and a field sent back unchanged keeps
// its instant -- including the two hours a year a wall clock is not one.
func TestWallClocksAreTheShops(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	for _, c := range []struct {
		name, value, was string
		want             time.Time
		refused          bool
	}{
		{name: "winter is UTC+1", value: "2026-01-15T07:30", want: utc("2026-01-15T06:30:00Z")},
		{name: "summer is UTC+2", value: "2026-07-15T07:30", want: utc("2026-07-15T05:30:00Z")},

		// 29 March 2026: 02:00 becomes 03:00. 02:30 never happened.
		{name: "a time that does not exist", value: "2026-03-29T02:30", refused: true},

		// 25 October 2026: 03:00 becomes 02:00. 02:30 happened twice.
		{name: "a time that happened twice is the first", value: "2026-10-25T02:30", want: utc("2026-10-25T00:30:00Z")},
		{name: "the second one, sent back unchanged, stays the second",
			value: "2026-10-25T02:30", was: "2026-10-25T01:30:00Z", want: utc("2026-10-25T01:30:00Z")},

		// was only holds when the field is unchanged.
		{name: "an edited field is read, not kept",
			value: "2026-07-15T08:00", was: "2026-07-15T05:30:00Z", want: utc("2026-07-15T06:00:00Z")},
		{name: "rubbish", value: "half seven", refused: true},
	} {
		got, err := parseWallClock(c.value, stockholm, c.was)
		if c.refused {
			if err == nil {
				t.Errorf("%s: %q read as %s, want refused", c.name, c.value, got.UTC())
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%s: %q read as %s, want %s", c.name, c.value, got.UTC(), c.want)
		}
	}
}

// What the page writes into the control is what parseWallClock reads back,
// for every hour of a year that has both changes in it. The bug was exactly a
// round trip that did not close: written in one zone, read in another.
func TestAnUnchangedTimeRoundTrips(t *testing.T) {
	stockholm, _ := time.LoadLocation("Europe/Stockholm")
	d := pageData{zone: stockholm}
	for at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); at.Year() == 2026; at = at.Add(time.Hour) {
		shown := d.Local(at).Format(wallClockLayout)
		got, err := parseWallClock(shown, stockholm, d.Instant(at))
		if err != nil || !got.Equal(at) {
			t.Fatalf("%s shown as %q came back as %s (%v)", at, shown, got.UTC(), err)
		}
	}
}
