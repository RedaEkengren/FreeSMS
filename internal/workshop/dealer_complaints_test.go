package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Complaints (#181).

// deliveredOn is a consumer's deal on unit, invoiced, paid and delivered at
// a moment.
func deliveredOn(t *testing.T, pool *pgxpool.Pool, unit string, at time.Time) workshop.Deal {
	t.Helper()
	ctx := context.Background()
	d := completedDealOn(t, pool, unit, workshop.TaxMarginScheme)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, everything(owner()), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.DeliverCar(ctx, pool, sales(), d.ID, workshop.NewDelivery{At: at}); err != nil {
		t.Fatal(err)
	}
	return d
}

// A complaint received two years and a day after delivery is outside the
// presumption and inside the liability; one made more than two months
// after the buyer noticed the defect says so; neither decides anything.
// What happens to it is rows; closed, nothing more is added.
func TestAComplaintShowsTheClocksFromDelivery(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	delivered := time.Now().AddDate(-2, 0, -1)
	d := deliveredOn(t, pool, eligibleCar(t, pool), delivered)
	noticed := time.Now().AddDate(0, -3, 0)
	c, err := workshop.OpenComplaint(ctx, pool, sales(), d.ID, workshop.NewComplaint{ReceivedAt: time.Now(), Channel: "phone",
		Description: "Växellådan slirar", NoticedOn: &noticed, Key: "complaint-1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.WithinPresumption() || !c.WithinLiability() || !c.LateAfterNoticing() || c.Status() != "received" || !c.Open() {
		t.Errorf("the complaint's clocks: presumption %v, liability %v, late %v, status %s",
			c.WithinPresumption(), c.WithinLiability(), c.LateAfterNoticing(), c.Status())
	}
	if again, err := workshop.OpenComplaint(ctx, pool, sales(), d.ID, workshop.NewComplaint{ReceivedAt: time.Now(), Channel: "phone",
		Description: "Växellådan slirar", Key: "complaint-1"}); err != nil || again.ID != c.ID {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	if err := workshop.MoveComplaint(ctx, pool, sales(), c.ID, "remedy_offered", "", "Vi lagar den"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a remedy offered without saying which: %v", err)
	}
	for _, step := range [][2]string{{"assessed", ""}, {"remedy_offered", "repair"}, {"remedy_accepted", "repair"}, {"remedy_done", "repair"}, {"closed", ""}} {
		if err := workshop.MoveComplaint(ctx, pool, sales(), c.ID, step[0], step[1], ""); err != nil {
			t.Fatalf("%s: %v", step[0], err)
		}
	}
	if err := workshop.MoveComplaint(ctx, pool, sales(), c.ID, "assessed", "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a closed complaint moved again: %v", err)
	}
	read, err := workshop.ComplaintByID(ctx, pool, sales(), c.ID)
	if err != nil || len(read.Events) != 6 || read.Open() {
		t.Errorf("read back: %d events, open %v, %v", len(read.Events), read.Open(), err)
	}
}

// A complaint is about a car delivered; and one that has stood still for a
// week is in front of whoever sells cars.
func TestAComplaintNeedsADeliveryAndIsNotForgotten(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	undelivered := completedDealOn(t, pool, eligibleCar(t, pool), workshop.TaxMarginScheme)
	if _, err := workshop.OpenComplaint(ctx, pool, sales(), undelivered.ID, workshop.NewComplaint{ReceivedAt: time.Now(), Channel: "email",
		Description: "Repa"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a complaint about a car not delivered: %v", err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), undelivered.ID, "Test"); err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, undelivered.StockUnitID, workshop.StockForSale)
	d := deliveredOn(t, pool, undelivered.StockUnitID, time.Now().AddDate(0, -1, 0))
	c, err := workshop.OpenComplaint(ctx, pool, sales(), d.ID, workshop.NewComplaint{ReceivedAt: time.Now(), Channel: "email", Description: "Repa"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.WithinPresumption() {
		t.Error("a month after delivery is within the presumption")
	}
	if _, err := pool.Exec(ctx, `UPDATE vehicle_sale_complaint_events SET at = now() - interval '8 days' WHERE complaint_id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	items, err := workshop.WaitingFor(ctx, pool, sales())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		found = found || (it.Kind == "complaint_stalled" && it.Link() == "/cars/deals/"+d.ID)
	}
	if !found {
		t.Errorf("a complaint standing still for a week is not in the queue: %+v", items)
	}
}
