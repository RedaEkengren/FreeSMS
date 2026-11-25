package workshop_test

import (
	"context"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

func waitingKinds(t *testing.T, items []workshop.ActionItem) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, it := range items {
		out[it.Kind] += max(it.Count, 1)
	}
	return out
}

// The front desk is asked to ring the customers a campaign wrote to who have not
// booked, and to look at sets nobody has come for; nobody else is, and an empty
// hotel asks for nothing.
func TestTheTyreHotelPutsWhatItNeedsOnTheQueue(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	items, _ := workshop.WaitingFor(ctx, pool, advisor())
	if got := waitingKinds(t, items); got["tyre_follow_up"] != 0 || got["tyre_abandoned"] != 0 {
		t.Fatalf("an empty hotel asks for %v", got)
	}
	place := aPlace(t, pool, "Hall", 5)
	bea := aContact(t, pool, "Bea", "+46702222222", "")
	cleo := aContact(t, pool, "Cleo", "+46703333333", "")
	aStoredSet(t, pool, bea, "", workshop.SeasonSummer, place)
	dan := aContact(t, pool, "Dan", "+46704444444", "")
	aStoredSet(t, pool, dan, "", workshop.SeasonSummer, place)
	booked := aStoredSet(t, pool, cleo, "", workshop.SeasonSummer, place)
	id, err := workshop.CreateCampaign(ctx, pool, owner(), workshop.NewCampaign{Name: "Spring", Season: "summer", Body: "Book: {sets}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RunCampaign(ctx, pool, owner(), id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{booked}, Starts: inDays(4), What: "Tyre change"}); err != nil {
		t.Fatal(err)
	}
	// Written to just now: nothing to follow up yet.
	items, _ = workshop.WaitingFor(ctx, pool, advisor())
	if got := waitingKinds(t, items); got["tyre_follow_up"] != 0 {
		t.Errorf("a customer written to today is followed up: %v", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_campaign_recipients SET at = clock_timestamp() - interval '20 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_set_placements SET placed_at = clock_timestamp() - interval '400 days' WHERE tyre_set_id = $1`, booked); err != nil {
		t.Fatal(err)
	}
	items, _ = workshop.WaitingFor(ctx, pool, advisor())
	got := waitingKinds(t, items)
	if got["tyre_follow_up"] != 2 || got["tyre_abandoned"] != 0 {
		t.Errorf("Bea and Dan have not booked and Cleo has; the set placed long ago has a booking today: %v", got)
	}
	var link string
	for _, it := range items {
		if it.Kind == "tyre_follow_up" {
			link = it.Link()
		}
	}
	if link != "/tyres/changes" {
		t.Errorf("the item opens %q", link)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_change_bookings SET at = clock_timestamp() - interval '400 days'`); err != nil {
		t.Fatal(err)
	}
	items, _ = workshop.WaitingFor(ctx, pool, owner())
	if got := waitingKinds(t, items); got["tyre_abandoned"] != 1 {
		t.Errorf("a set with nothing for over a year: %v", got)
	}
	items, _ = workshop.WaitingFor(ctx, pool, technician())
	if got := waitingKinds(t, items); got["tyre_follow_up"] != 0 || got["tyre_abandoned"] != 0 {
		t.Errorf("a technician is asked about customers: %v", got)
	}
}
