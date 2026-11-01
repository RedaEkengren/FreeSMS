package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// #137: four small things that were accepted without a word, each decided.

// Refused. A line of nothing prints on the invoice as nought times a price,
// which is a forgotten quantity far more often than a meant one. A line that
// should not be there is withdrawn, which keeps the reason.
func TestALineWithNoQuantityIsRefused(t *testing.T) {
	pool := setup(t)
	job := newJob(t, pool)
	if err := workshop.AddLine(context.Background(), pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Service", QuantityMilli: 0, UnitPriceMinor: 89500, VATRateBasis: 2500,
	}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a line of quantity 0 = %v, want refused", err)
	}
}

// Accepted. Nought is how the hourly rate says it has not been set, by a shop
// that prices every job by hand. A line taken from the time library at that
// rate is free on the job page and in the quote the customer is asked to
// agree to, so nothing reaches an invoice unseen.
func TestAnHourlyRateOfNothingIsAcceptedAsNotSet(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := workshop.SetLabourRate(ctx, pool, owner(), 0); err != nil {
		t.Fatalf("an hourly rate of 0 = %v, want accepted", err)
	}
	if rate, err := workshop.LabourRate(ctx, pool, owner()); err != nil || rate != 0 {
		t.Errorf("the rate read back is %d, %v", rate, err)
	}
	if err := workshop.SetLabourRate(ctx, pool, owner(), -1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a negative rate = %v, want refused", err)
	}
}

// Confirmation. A booking that starts on a day that has gone is asked about:
// 2020 typed for 2026 is a booking nobody will come to, and a visit written
// down afterwards is real, so ticking that it is meant takes it. Earlier
// today is not asked about: the car that came at eight, written up at nine.
func TestABookingOnADayThatHasGoneIsAskedAbout(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	booking := func(starts time.Time, confirmed bool) error {
		_, err := workshop.CreateBooking(ctx, pool, advisor(), workshop.NewBooking{
			Starts: starts, Ends: starts.Add(time.Hour), What: "Service", ConfirmedPast: confirmed})
		return err
	}
	past := time.Date(2020, 3, 2, 9, 0, 0, 0, stockholm)
	if err := booking(past, false); !errors.Is(err, workshop.ErrBookingInThePast) {
		t.Errorf("a booking in 2020 = %v, want ErrBookingInThePast", err)
	}
	if err := booking(past, true); err != nil {
		t.Errorf("a booking in 2020, said to be meant = %v", err)
	}
	now := time.Now().In(stockholm)
	if err := booking(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, stockholm), false); err != nil {
		t.Errorf("a booking at midnight today = %v, want accepted unasked", err)
	}
}

// Accepted. Stopping a clock that is not running leaves it stopped, which is
// what was asked; the page answers with the clock as it really is. A double
// tap, a clock already closed by clocking in on another car and the offline
// queue replaying the tap all land here. A job that does not exist is not
// found, rather than a stopped clock on nothing.
func TestStoppingAClockThatIsNotRunningLeavesItStopped(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	job := newJob(t, pool)
	if err := workshop.ClockOut(ctx, pool, technician(), job); err != nil {
		t.Errorf("clocking out without clocking in = %v, want accepted", err)
	}
	var entries int
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM time_entries WHERE work_order_id = $1`, job).Scan(&entries)
	}); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Errorf("clocking out of nothing wrote %d entries", entries)
	}
	if err := workshop.ClockOut(ctx, pool, technician(), "aaaaaaaa-0000-0000-0000-0000000000ff"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("clocking out of a job that does not exist = %v, want ErrNotFound", err)
	}
}
