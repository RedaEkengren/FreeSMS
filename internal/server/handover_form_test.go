package server

import (
	"net/url"
	"slices"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The finish form names what the page showed as new and what stayed
// ticked. A page cached before it did -- held by a phone's offline queue
// -- sends only the ticked: those, and no others, are the damage
// confirmed (review r5, #5).
func TestTheFinishFormIsReadInBothShapes(t *testing.T) {
	dismissedNow, confirmedNow := findingsChosen(url.Values{"finish_form": {"2"}, "shown": {"a", "b"}, "damage": {"a"}})
	if !slices.Equal(dismissedNow, []string{"b"}) || confirmedNow != nil {
		t.Errorf("today's form: let go %v, confirmed %v", dismissedNow, confirmedNow)
	}
	dismissedOld, confirmedOld := findingsChosen(url.Values{"damage": {"a"}})
	if dismissedOld != nil || !slices.Equal(confirmedOld, []string{"a"}) {
		t.Errorf("the old form: let go %v, confirmed %v", dismissedOld, confirmedOld)
	}
	// The old form with nothing ticked confirmed nothing (review r6, #2);
	// today's with nothing shown lets nothing go.
	if _, none := findingsChosen(url.Values{"note": {"x"}}); none == nil || len(none) != 0 {
		t.Errorf("the old form with nothing ticked confirms %v; want none", none)
	}
	if dismissed, confirmed := findingsChosen(url.Values{"finish_form": {"2"}}); dismissed != nil || confirmed != nil {
		t.Errorf("today's form with nothing shown: let go %v, confirmed %v", dismissed, confirmed)
	}
}

// The register's CSV holds a rental once, though the period and the
// moment asked both found it (review r6, #9).
func TestTheRegistersRowsHoldEachRentalOnce(t *testing.T) {
	a := workshop.RegisterEntry{BookingID: "a"}
	b := workshop.RegisterEntry{BookingID: "b"}
	if got := registerRows([]workshop.RegisterEntry{a, b}, &a); len(got) != 2 {
		t.Errorf("the moment's rental, found by the period too: %d rows", len(got))
	}
	if got := registerRows([]workshop.RegisterEntry{a}, &b); len(got) != 2 {
		t.Errorf("the moment's rental, outside the period: %d rows", len(got))
	}
}
