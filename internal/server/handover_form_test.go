package server

import (
	"net/url"
	"slices"
	"testing"
)

// The finish form names what the page showed as new and what stayed
// ticked. A page cached before it did -- held by a phone's offline queue
// -- sends only the ticked: those, and no others, are the damage
// confirmed (review r5, #5).
func TestTheFinishFormIsReadInBothShapes(t *testing.T) {
	dismissedNow, confirmedNow := findingsChosen(url.Values{"shown": {"a", "b"}, "damage": {"a"}})
	if !slices.Equal(dismissedNow, []string{"b"}) || confirmedNow != nil {
		t.Errorf("today's form: let go %v, confirmed %v", dismissedNow, confirmedNow)
	}
	dismissedOld, confirmedOld := findingsChosen(url.Values{"damage": {"a"}})
	if dismissedOld != nil || !slices.Equal(confirmedOld, []string{"a"}) {
		t.Errorf("the old form: let go %v, confirmed %v", dismissedOld, confirmedOld)
	}
}
