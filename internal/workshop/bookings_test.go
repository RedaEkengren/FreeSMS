package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// A workshop books three weeks ahead and FreeSMS had nowhere to say so.

var stockholm = func() *time.Location { l, _ := time.LoadLocation("Europe/Stockholm"); return l }()

func at(day string, hh, mm int) time.Time {
	d, _ := time.ParseInLocation("2006-01-02", day, stockholm)
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, stockholm)
}

func TestABookingHoldsASlotWithoutInventingAJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	var before, after int
	count := func() int {
		var n int
		database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM work_orders`).Scan(&n)
		})
		return n
	}
	before = count()
	id, err := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
		Starts: at("2026-10-15", 8, 0), Ends: at("2026-10-15", 10, 0),
		Registration: "abc 123", CustomerName: "Karin Kund", What: "Service"})
	if err != nil {
		t.Fatalf("CreateBooking: %v", err)
	}
	after = count()
	if after != before {
		t.Errorf("a booking made %d work orders", after-before)
	}
	b, events, err := workshop.BookingByID(ctx, pool, advisor(), id)
	if err != nil || b.Registration != "ABC 123" || b.Status != "booked" || len(events) != 1 || events[0].By != "An Advisor" {
		t.Errorf("booking = %+v, events = %+v, %v", b, events, err)
	}
}

// Placed by its wall clock, so it says when on the day -- and stays at nine
// on the Sunday the clocks go forward.
func TestABlockIsPlacedByItsWallClockAcrossTheChangeOfHour(t *testing.T) {
	rows := []workshop.PlannerRow{{ID: "t1", Name: "Erik"}}
	for _, day := range []string{"2026-10-15", "2027-03-28", "2026-10-25"} {
		b := workshop.Booking{ID: "b", TechnicianID: "t1", Starts: at(day, 9, 0), Ends: at(day, 10, 30), Status: "booked"}
		week := workshop.BuildWeek(at(day, 0, 0), 1, rows, []workshop.Booking{b}, workshop.Capacity{}, stockholm)
		if len(week) != 1 || len(week[0].Lanes) != 1 || len(week[0].Lanes[0].Blocks) != 1 {
			t.Fatalf("%s: %+v", day, week)
		}
		blk := week[0].Lanes[0].Blocks[0]
		// 09:00 is the fourth half-hour after 07:00; 90 minutes is three.
		if blk.Start != 4 || blk.Span != 3 || blk.Clipped {
			t.Errorf("%s: 09:00-10:30 placed at slot %d for %d (clipped %v), want 4 for 3", day, blk.Start, blk.Span, blk.Clipped)
		}
	}
}

func TestABookingOutsideOpeningHoursIsDrawnToTheEdgeAndSaysSo(t *testing.T) {
	rows := []workshop.PlannerRow{{ID: "t1", Name: "Erik"}}
	b := workshop.Booking{ID: "b", TechnicianID: "t1", Starts: at("2026-10-15", 6, 0), Ends: at("2026-10-15", 8, 0)}
	blk := workshop.BuildWeek(at("2026-10-15", 0, 0), 1, rows, []workshop.Booking{b}, workshop.Capacity{}, stockholm)[0].Lanes[0].Blocks[0]
	if blk.Start != 0 || blk.Span != 2 || !blk.Clipped {
		t.Errorf("06:00-08:00 placed at %d for %d, clipped %v", blk.Start, blk.Span, blk.Clipped)
	}
}

func TestMovingCancellingAndArrivingAreRecorded(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, _ := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
		Starts: at("2026-10-15", 8, 0), Ends: at("2026-10-15", 9, 0), What: "Däckbyte", Registration: "XYZ 789",
		EstimateMinutes: ptr(90)})
	b, _, _ := workshop.BookingByID(ctx, pool, advisor(), id)
	if !b.Overbooked() {
		t.Error("ninety minutes of work in an hour's slot is not flagged")
	}
	if err := workshop.MoveBooking(ctx, pool, advisor(), id, at("2026-10-16", 13, 0), at("2026-10-16", 15, 0), ""); err != nil {
		t.Fatalf("MoveBooking: %v", err)
	}
	found, err := workshop.SearchBookings(ctx, pool, advisor(), "xyz789")
	if err != nil || len(found) != 1 || !found[0].Starts.Equal(at("2026-10-16", 13, 0)) {
		t.Errorf("search found %+v, %v", found, err)
	}

	job := newJob(t, pool)
	if err := workshop.ArriveBooking(ctx, pool, advisor(), id, job); err != nil {
		t.Fatalf("ArriveBooking: %v", err)
	}
	b, events, _ := workshop.BookingByID(ctx, pool, advisor(), id)
	if b.Status != "arrived" || b.WorkOrderID != job || len(events) != 3 {
		t.Errorf("after arriving: %+v with %d events", b, len(events))
	}
	if err := workshop.CancelBooking(ctx, pool, advisor(), id, "Kunden ringde"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("an arrived booking was cancelled: %v", err)
	}

	other, _ := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
		Starts: at("2026-10-15", 8, 0), Ends: at("2026-10-15", 9, 0), What: "Service"})
	if err := workshop.CancelBooking(ctx, pool, advisor(), other, "Kunden ringde"); err != nil {
		t.Fatal(err)
	}
	week, _ := workshop.BookingsBetween(ctx, pool, advisor(), at("2026-10-12", 0, 0), at("2026-10-19", 0, 0))
	for _, w := range week {
		if w.ID == other {
			t.Error("a cancelled booking is still on the planner")
		}
	}
}

func TestBookingsAreTheCounters(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	if _, err := workshop.CreateBooking(ctx, pool, technician(), workshop.NewBooking{
		Starts: at("2026-10-15", 8, 0), Ends: at("2026-10-15", 9, 0), What: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician took a booking: %v", err)
	}
	if _, err := workshop.BookingsBetween(ctx, pool, technician(), time.Now(), time.Now().Add(time.Hour)); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read bookings with customers in them: %v", err)
	}
}

func ptr(n int) *int { return &n }
