package workshop_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const secondTechID = "33333333-3333-3333-3333-333333333335"

func secondTechnician() access.Scope {
	return access.Scope{ShopID: shopID, UserID: secondTechID, Role: access.RoleTechnician}
}

// addSecondTechnician is a fixture, like addTechnician: a person to clock.
// What they clock, and against what, goes through the application.
func addSecondTechnician(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO people (id, shop_id, display_name) VALUES
			('22222222-2222-2222-2222-222222222225', $1, 'Another Technician')`, shopID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
			($1, $2, '22222222-2222-2222-2222-222222222225', 'technician', 'x')`, secondTechID, shopID)
		return err
	})
	if err != nil {
		t.Fatalf("add second technician: %v", err)
	}
}

var nameOf = map[string]string{techUserID: "A Technician", secondTechID: "Another Technician"}

type session struct {
	who     access.Scope
	minutes int
}

// doneJob does a job the way the shop would: a labour line priced at the
// counter (from the library when labourTimeID is set), each session clocked
// on and off by the technician, the hours corrected at the front desk to what
// they were -- a test cannot wait two hours -- and the job finished, and
// invoiced when there is anything to charge.
//
// No SQL. The bug this guards was that the figures only ever had data
// because tests wrote the link between time and work by hand.
func doneJob(t *testing.T, pool *pgxpool.Pool, labourTimeID string, hours int64, costBearer string, sessions []session) string {
	t.Helper()
	ctx := context.Background()
	id := newJob(t, pool)
	price := int64(89500)
	if costBearer != "customer" {
		price = 0 // the customer is not charged for warranty work
	}
	if err := workshop.AddLine(ctx, pool, advisor(), id, workshop.NewLine{
		Kind: "labour", Description: "The work", QuantityMilli: hours * 1000,
		UnitPriceMinor: price, VATRateBasis: 2500, LabourTimeID: labourTimeID, CostBearer: costBearer,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)

	for _, s := range sessions {
		if err := workshop.ClockIn(ctx, pool, s.who, id); err != nil {
			t.Fatalf("ClockIn: %v", err)
		}
		if err := workshop.ClockOut(ctx, pool, s.who, id); err != nil {
			t.Fatalf("ClockOut: %v", err)
		}
	}
	entries, err := workshop.TimeFor(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("TimeFor: %v", err)
	}
	if len(entries) != len(sessions) {
		t.Fatalf("%d entries for %d sessions", len(entries), len(sessions))
	}
	// TimeFor's order is not the order clocked, so match by person, in turn.
	used := map[int]bool{}
	start := time.Now().Add(-72 * time.Hour)
	for _, s := range sessions {
		for i, e := range entries {
			if used[i] || e.UserName != nameOf[s.who.UserID] {
				continue
			}
			used[i] = true
			if err := workshop.CorrectTime(ctx, pool, advisor(), e.ID,
				start, start.Add(time.Duration(s.minutes)*time.Minute), "as it was"); err != nil {
				t.Fatalf("CorrectTime: %v", err)
			}
			start = start.Add(12 * time.Hour)
			break
		}
	}

	if len(sessions) == 0 {
		// Clocking on is what starts a job; with nobody clocking, the front
		// desk says so.
		move(t, pool, id, workshop.StateInProgress)
	}
	move(t, pool, id, workshop.StateReady)
	// Warranty work has nothing to charge, so it is finished and never
	// invoiced; the supplier is claimed from separately.
	if costBearer == "customer" {
		if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
			t.Fatalf("Issue: %v", err)
		}
	}
	return id
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// The library learns from jobs done through the ordinary screens. It used to
// read a link between time and line that nothing in the application made.
func TestTheLibraryLearnsFromOrdinaryWork(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	addSecondTechnician(t, pool)
	ctx := context.Background()

	store(t, pool, workshop.LabourTime{Operation: "Cambelt", Make: "Volvo", Minutes: 180})
	suggestions, _ := workshop.SuggestFor(ctx, pool, advisor(), vehicleA)
	timeID := suggestions[0].ID

	// One person, twice over the day: 100 + 50 minutes.
	doneJob(t, pool, timeID, 3, "customer", []session{{technician(), 100}, {technician(), 50}})
	// Two people on one cambelt: 120 + 90.
	doneJob(t, pool, timeID, 3, "customer", []session{{technician(), 120}, {secondTechnician(), 90}})
	// Under warranty: the work took what it took, whoever pays.
	doneJob(t, pool, timeID, 3, "supplier", []session{{technician(), 240}})

	times, err := workshop.LabourTimes(ctx, pool, advisor())
	if err != nil {
		t.Fatalf("LabourTimes: %v", err)
	}
	a := times[0].Actuals
	if a.Jobs != 3 {
		t.Fatalf("learned from %d jobs, want 3", a.Jobs)
	}
	if a.FastestMinutes != 150 || a.SlowestMinutes != 240 || a.MedianMinutes != 210 {
		t.Errorf("spread %d–%d, median %d; want 150–240, median 210", a.FastestMinutes, a.SlowestMinutes, a.MedianMinutes)
	}
}

// A job that had more than one thing done cannot say how long one of them
// took, and an unfinished one has not finished taking it.
func TestTheLibraryIgnoresJobsThatCannotSay(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	store(t, pool, workshop.LabourTime{Operation: "Cambelt", Make: "Volvo", Minutes: 180})
	suggestions, _ := workshop.SuggestFor(ctx, pool, advisor(), vehicleA)
	timeID := suggestions[0].ID

	// Cambelt and brakes on one job, clocked as one.
	id := newJob(t, pool)
	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Cambelt", QuantityMilli: 3000, UnitPriceMinor: 89500, VATRateBasis: 2500, LabourTimeID: timeID},
		{Kind: "labour", Description: "Brakes", QuantityMilli: 1000, UnitPriceMinor: 89500, VATRateBasis: 2500},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), id, l); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	workshop.ClockIn(ctx, pool, technician(), id)
	workshop.ClockOut(ctx, pool, technician(), id)
	move(t, pool, id, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// The cambelt alone, but not invoiced yet.
	open := newJob(t, pool)
	workshop.AddLine(ctx, pool, advisor(), open, workshop.NewLine{
		Kind: "labour", Description: "Cambelt", QuantityMilli: 3000, UnitPriceMinor: 89500, VATRateBasis: 2500, LabourTimeID: timeID})
	move(t, pool, open, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	workshop.ClockIn(ctx, pool, technician(), open)
	workshop.ClockOut(ctx, pool, technician(), open)

	times, _ := workshop.LabourTimes(ctx, pool, advisor())
	if a := times[0].Actuals; a.Known() {
		t.Errorf("learned from %d jobs, want none: one did two things, one is not finished", a.Jobs)
	}
}

// Sold hours per person: each invoiced job's labour, shared by who clocked
// how long on it. The shares add up to the shop's total, however many times
// anybody clocked on and off; hours nobody clocked are nobody's.
func TestSoldHoursAreSharedByWhoDidTheWork(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	addSecondTechnician(t, pool)
	ctx := context.Background()

	// 3 hours sold. The first technician 120 minutes in three sessions, the
	// second 60: two thirds and one third.
	doneJob(t, pool, "", 3, "customer", []session{
		{technician(), 40}, {secondTechnician(), 60}, {technician(), 40}, {technician(), 40}})
	// 2 hours sold, nobody clocked.
	doneJob(t, pool, "", 2, "customer", nil)
	// Warranty: not a sale to a customer, so not in sold -- as in the total.
	doneJob(t, pool, "", 5, "supplier", []session{{secondTechnician(), 60}})

	now := time.Now()
	d, err := workshop.Summary(ctx, pool, owner(), now.AddDate(0, -1, 0), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("DashboardFor: %v", err)
	}
	if !near(d.HoursBilled, 5) {
		t.Fatalf("the shop sold %.2f hours, want 5", d.HoursBilled)
	}
	billed := map[string]float64{}
	var sum float64
	for _, tt := range d.Technicians {
		billed[tt.Name] = tt.Billed
		sum += tt.Billed
	}
	if !near(billed["A Technician"], 2) || !near(billed["Another Technician"], 1) {
		t.Errorf("sold per person = %v, want 2 and 1", billed)
	}
	if !near(d.Unattributed, 2) {
		t.Errorf("unattributed = %.2f, want the 2 hours nobody clocked", d.Unattributed)
	}
	if !near(sum+d.Unattributed, d.HoursBilled) {
		t.Errorf("people %.2f + nobody %.2f does not make the shop's %.2f", sum, d.Unattributed, d.HoursBilled)
	}
}
