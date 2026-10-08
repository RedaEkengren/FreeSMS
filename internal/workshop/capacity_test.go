package workshop_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Capacity is people and lifts: four technicians and two lifts is not eight
// parallel hours, and a planner that only knows bookings fills a day nobody
// is in to work.

func TestABookingOnAShutDayIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := workshop.CloseDay(ctx, pool, advisor(), at("2026-06-19", 0, 0), "Midsommarafton"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk shut the shop: %v", err)
	}
	if err := workshop.CloseDay(ctx, pool, owner(), at("2026-06-19", 0, 0), "Midsommarafton"); err != nil {
		t.Fatalf("CloseDay: %v", err)
	}
	if err := workshop.CloseDay(ctx, pool, owner(), at("2026-06-19", 0, 0), "again"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("shutting a shut day twice: %v", err)
	}
	// Half past midnight in Stockholm is still the day before in UTC: the
	// shop's day is the one that counts.
	if _, err := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
		Starts: at("2026-06-19", 0, 30), Ends: at("2026-06-19", 1, 0), What: "Service"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("booked on a shut day: %v", err)
	}
	id, err := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
		Starts: at("2026-06-18", 8, 0), Ends: at("2026-06-18", 9, 0), What: "Service"})
	if err != nil {
		t.Fatalf("the day before is open: %v", err)
	}
	if err := workshop.MoveBooking(ctx, pool, advisor(), id, at("2026-06-19", 8, 0), at("2026-06-19", 9, 0), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("moved onto a shut day: %v", err)
	}
	if err := workshop.OpenDay(ctx, pool, owner(), at("2026-06-19", 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveBooking(ctx, pool, advisor(), id, at("2026-06-19", 8, 0), at("2026-06-19", 9, 0), ""); err != nil {
		t.Errorf("opened again and still refused: %v", err)
	}
}

func TestSomebodyAwayLeavesTheirBookingsNeedingSomebodyElse(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	id, err := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
		Starts: at("2026-10-15", 9, 0), Ends: at("2026-10-15", 10, 0), What: "Service", TechnicianID: techUserID})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetAbsence(ctx, pool, technician(), techUserID, at("2026-10-15", 0, 0), "sick"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician wrote the absence list: %v", err)
	}
	if err := workshop.SetAbsence(ctx, pool, advisor(), "44444444-4444-4444-4444-444444444444", at("2026-10-15", 0, 0), "sick"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("somebody who does not work here was marked away: %v", err)
	}
	if err := workshop.SetAbsence(ctx, pool, advisor(), techUserID, at("2026-10-15", 0, 0), "lunch"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unknown reason: %v", err)
	}
	if err := workshop.SetAbsence(ctx, pool, advisor(), techUserID, at("2026-10-15", 0, 0), "holiday"); err != nil {
		t.Fatalf("SetAbsence: %v", err)
	}
	// Rang in sick on a day already down as holiday: the reason changes,
	// the day is not counted twice.
	if err := workshop.SetAbsence(ctx, pool, advisor(), techUserID, at("2026-10-15", 0, 0), "sick"); err != nil {
		t.Fatalf("SetAbsence again: %v", err)
	}

	week := func() workshop.PlannerDay {
		t.Helper()
		from, to := at("2026-10-15", 0, 0), at("2026-10-16", 0, 0)
		cap, err := workshop.CapacityBetween(ctx, pool, advisor(), from, to)
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := workshop.PlannerRows(ctx, pool, advisor())
		bookings, _ := workshop.BookingsBetween(ctx, pool, advisor(), from, to)
		return workshop.BuildWeek(from, 1, rows, bookings, cap, time.Time{}, stockholm)[0]
	}
	var lane *workshop.PlannerLane
	day := week()
	for i := range day.Lanes {
		if day.Lanes[i].Row.ID == techUserID {
			lane = &day.Lanes[i]
		}
	}
	if lane == nil || lane.Away != "off sick" || len(lane.Blocks) != 1 || !lane.Blocks[0].Away || lane.Blocks[0].Booking.ID != id {
		t.Fatalf("the away technician's lane: %+v", lane)
	}

	if err := workshop.ClearAbsence(ctx, pool, advisor(), techUserID, at("2026-10-15", 0, 0)); err != nil {
		t.Fatal(err)
	}
	for _, l := range week().Lanes {
		if l.Row.ID == techUserID && (l.Away != "" || l.Blocks[0].Away) {
			t.Errorf("came in after all and still shown away: %+v", l)
		}
	}
}

func TestOnePersonWithTwoCarsAndMoreCarsThanLiftsAreShown(t *testing.T) {
	rows := []workshop.PlannerRow{{ID: "t1", Name: "Erik"}, {ID: "t2", Name: "Sara"}}
	day := "2026-10-15"
	bookings := []workshop.Booking{
		{ID: "a", TechnicianID: "t1", Starts: at(day, 8, 0), Ends: at(day, 10, 0)},
		{ID: "b", TechnicianID: "t1", Starts: at(day, 9, 0), Ends: at(day, 11, 0)},
		// Starts as the other ends: back to back, not at once.
		{ID: "c", TechnicianID: "t1", Starts: at(day, 11, 0), Ends: at(day, 12, 0)},
		{ID: "d", TechnicianID: "t2", Starts: at(day, 9, 30), Ends: at(day, 10, 30)},
	}
	got := workshop.BuildWeek(at(day, 0, 0), 1, rows, bookings, workshop.Capacity{Lifts: 2}, time.Time{}, stockholm)[0]
	clash := map[string]bool{}
	for _, l := range got.Lanes {
		for _, b := range l.Blocks {
			clash[b.Booking.ID] = b.Clash
		}
	}
	if want := map[string]bool{"a": true, "b": true, "c": false, "d": false}; !reflect.DeepEqual(clash, want) {
		t.Errorf("clashes = %v, want %v", clash, want)
	}
	// Three cars from 09:30 to 10:00 on two lifts.
	if want := []string{"09:30–10:00"}; !reflect.DeepEqual(got.LiftsOver, want) {
		t.Errorf("more cars than lifts = %q, want %q", got.LiftsOver, want)
	}
	if none := workshop.BuildWeek(at(day, 0, 0), 1, rows, bookings, workshop.Capacity{}, time.Time{}, stockholm)[0]; none.LiftsOver != nil {
		t.Errorf("no lifts given is not limited, got %q", none.LiftsOver)
	}
}

func TestLiftsAreTheOwnersToSay(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := workshop.SetLifts(ctx, pool, advisor(), 3); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk set the lifts: %v", err)
	}
	if err := workshop.SetLifts(ctx, pool, owner(), -1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("negative lifts: %v", err)
	}
	for _, n := range []int{3, 0} {
		if err := workshop.SetLifts(ctx, pool, owner(), n); err != nil {
			t.Fatal(err)
		}
		c, err := workshop.CapacityBetween(ctx, pool, advisor(), at("2026-10-15", 0, 0), at("2026-10-16", 0, 0))
		if err != nil || c.Lifts != n {
			t.Errorf("set %d lifts, read %d, %v", n, c.Lifts, err)
		}
	}
}
