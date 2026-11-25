package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// inDays is ten in the morning, some days from now, in UTC.
func inDays(n int) time.Time {
	d := time.Now().UTC().AddDate(0, 0, n)
	return time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, time.UTC)
}

// A change is a booking as long as its sets take; the sets are booked while it
// stands and free again when it is cancelled; and a customer's sets only go
// together with their own.
func TestAChangeIsABookingForTheSetsAndFreesThemWhenCancelled(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 10)
	one := aStoredSet(t, pool, customerA, vehicleA, workshop.SeasonSummer, place)
	two := aStoredSet(t, pool, customerA, anotherCar(t, pool, customerA), workshop.SeasonSummer, place)
	other := aContact(t, pool, "Bea", "+46702222222", "")
	foreign := aStoredSet(t, pool, other, "", workshop.SeasonSummer, place)
	loose, err := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetTyreChangeSettings(ctx, pool, owner(), workshop.TyreChangeSettings{MinutesPerSet: 25, SetsPerDay: 10, FollowUpDays: 14, AbandonDays: 365}); err != nil {
		t.Fatal(err)
	}
	when := inDays(5)

	if _, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{one, foreign}, Starts: when, What: "Tyre change"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("sets of two customers: %v", err)
	}
	if _, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{loose}, Starts: when, What: "Tyre change"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set that is not in storage: %v", err)
	}
	booking, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{one, two}, Starts: when, What: "Tyre change"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := workshop.BookingByID(ctx, pool, advisor(), booking)
	if err != nil || b.SlotMinutes() != 50 || b.What != "Tyre change" || b.CustomerName != "A Customer" {
		t.Fatalf("the booking: %+v, %v", b, err)
	}
	if at, err := workshop.TyreChangeOf(ctx, pool, technician(), one); err != nil || at == nil || !at.Equal(when) {
		t.Errorf("the set is booked for %v, %v", at, err)
	}
	if _, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{two}, Starts: inDays(6), What: "Tyre change"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set booked twice: %v", err)
	}
	days, err := workshop.TyreChangeDays(ctx, pool, advisor(), time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var booked, free int
	for _, d := range days {
		if d.Booked > 0 {
			booked, free = d.Booked, d.Free
		}
	}
	if booked != 2 || free != 8 {
		t.Errorf("the busy day: %d booked, %d free", booked, free)
	}

	if err := workshop.CancelBooking(ctx, pool, advisor(), booking, "changed their mind"); err != nil {
		t.Fatal(err)
	}
	if at, _ := workshop.TyreChangeOf(ctx, pool, technician(), one); at != nil {
		t.Errorf("a cancelled booking still holds the set: %v", at)
	}
	if _, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{one}, Starts: inDays(6), What: "Tyre change"}); err != nil {
		t.Errorf("a set freed by a cancellation: %v", err)
	}
}

// A day holds the sets the shop says, and two bookings made together for the
// last place do not both get it.
func TestADayHoldsOnlyTheSetsTheShopSaysEvenWhenBookedTogether(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 10)
	if err := workshop.SetTyreChangeSettings(ctx, pool, owner(), workshop.TyreChangeSettings{MinutesPerSet: 30, SetsPerDay: 2, FollowUpDays: 14, AbandonDays: 365}); err != nil {
		t.Fatal(err)
	}
	var sets []string
	for _, name := range []string{"Bea", "Cleo", "Dan"} {
		c := aContact(t, pool, name, "+4670000000"+name[:1], "")
		sets = append(sets, aStoredSet(t, pool, c, "", workshop.SeasonSummer, place))
	}
	when := inDays(8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, full := 0, 0
	for _, s := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{s}, Starts: when, What: "Tyre change"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, workshop.ErrInvalid):
				full++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 2 || full != 1 {
		t.Errorf("a day for two sets took %d and refused %d", ok, full)
	}
}

func TestTheFollowUpListIsWhoWasWrittenToAndHasNotBooked(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 10)
	quiet := aContact(t, pool, "Bea", "+46702222222", "")
	booked := aContact(t, pool, "Cleo", "+46703333333", "")
	recent := aContact(t, pool, "Dan", "+46704444444", "")
	aStoredSet(t, pool, quiet, "", workshop.SeasonSummer, place)
	bookedSet := aStoredSet(t, pool, booked, "", workshop.SeasonSummer, place)
	aStoredSet(t, pool, recent, "", workshop.SeasonSummer, place)
	id, err := workshop.CreateCampaign(ctx, pool, owner(), workshop.NewCampaign{Name: "Spring", Season: "summer", Body: "Book: {sets}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RunCampaign(ctx, pool, owner(), id, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Bea and Cleo were written to three weeks ago; Dan just now.
	if _, err := pool.Exec(ctx, `UPDATE tyre_campaign_recipients SET at = clock_timestamp() - interval '21 days' WHERE customer_id IN ($1, $2)`, quiet, booked); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.BookTyreChange(ctx, pool, advisor(), workshop.ChangeBooking{SetIDs: []string{bookedSet}, Starts: inDays(4), What: "Tyre change"}); err != nil {
		t.Fatal(err)
	}
	list, err := workshop.CampaignFollowUp(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].CustomerID != quiet || list[0].Phone != "+46702222222" || len(list[0].Sets) != 1 {
		t.Fatalf("the follow-up list: %+v", list)
	}
	if err := workshop.SetTyreChangeSettings(ctx, pool, owner(), workshop.TyreChangeSettings{MinutesPerSet: 30, SetsPerDay: 12, FollowUpDays: 30, AbandonDays: 365}); err != nil {
		t.Fatal(err)
	}
	if list, _ := workshop.CampaignFollowUp(ctx, pool, advisor(), id); len(list) != 0 {
		t.Errorf("with thirty days to wait: %d customers, want none", len(list))
	}
}

func TestWhoMayPlanAndBookAChange(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	s := workshop.TyreChangeSettings{MinutesPerSet: 30, SetsPerDay: 12, FollowUpDays: 14, AbandonDays: 365}
	if err := workshop.SetTyreChangeSettings(ctx, pool, advisor(), s); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("an advisor changed the settings: %v", err)
	}
	if err := workshop.SetTyreChangeSettings(ctx, pool, owner(), workshop.TyreChangeSettings{MinutesPerSet: 1, SetsPerDay: 12, FollowUpDays: 14, AbandonDays: 365}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set in one minute: %v", err)
	}
	if _, err := workshop.BookTyreChange(ctx, pool, technician(), workshop.ChangeBooking{SetIDs: []string{customerA}, Starts: inDays(3), What: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician booked a change: %v", err)
	}
	if _, err := workshop.TyreChangeDays(ctx, pool, technician(), time.Now(), 7); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the planner's view: %v", err)
	}
	if _, err := workshop.CampaignFollowUp(ctx, pool, technician(), customerA); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the follow-up list: %v", err)
	}
	if got, err := workshop.TyreChangeSettingsOf(ctx, pool, advisor()); err != nil || got.MinutesPerSet != 30 || got.SetsPerDay != 12 || got.FollowUpDays != 14 {
		t.Errorf("the defaults: %+v, %v", got, err)
	}
}
