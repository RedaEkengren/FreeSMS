package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A part used to leave the shelf only on paper, at invoicing.

func pricedPartJob(t *testing.T, pool *pgxpool.Pool, quantityMilli int64) string {
	t.Helper()
	job := newJob(t, pool)
	if err := workshop.AddLine(context.Background(), pool, advisor(), job, workshop.NewLine{
		Kind: "part", Description: "Brake pad set, front (BP-100)", QuantityMilli: quantityMilli,
		UnitPriceMinor: 65250, VATRateBasis: 2500, PartID: stockedPart,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	return job
}

func jobPart(t *testing.T, pool *pgxpool.Pool, job string) workshop.JobPart {
	t.Helper()
	parts, err := workshop.PartsOnJob(context.Background(), pool, technician(), job)
	if err != nil {
		t.Fatalf("PartsOnJob: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("%d parts on the job, want 1", len(parts))
	}
	return parts[0]
}

func invoice(t *testing.T, pool *pgxpool.Pool, job string) {
	t.Helper()
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if _, err := workshop.Issue(context.Background(), pool, advisor(), job); err != nil {
		t.Fatalf("Issue: %v", err)
	}
}

// Taken out at the bench, it leaves the shelf then -- and invoicing does not
// take it a second time.
func TestAPartTakenOutLeavesTheShelfOnceAndInvoicingDoesNotTakeItAgain(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 6)
	job := pricedPartJob(t, pool, 1000)

	if jp := jobPart(t, pool, job); !jp.NotTakenOut() || jp.Remaining() != 1 {
		t.Fatalf("before: %+v, want priced and not taken out", jp)
	}
	if err := workshop.TakeOut(ctx, pool, technician(), job, stockedPart, 1); err != nil {
		t.Fatalf("TakeOut: %v", err)
	}
	p := shelf(t, pool)
	if p.OnHand != 5 || p.Reserved != 0 {
		t.Errorf("after taking out: on hand %v, reserved %v; want 5 and 0", p.OnHand, p.Reserved)
	}
	if jp := jobPart(t, pool, job); jp.NotTakenOut() || jp.NotPriced() {
		t.Errorf("after taking out: %+v, want priced and taken out", jp)
	}

	invoice(t, pool, job)
	if p := shelf(t, pool); p.OnHand != 5 || p.Reserved != 0 {
		t.Errorf("after invoicing: on hand %v, reserved %v; want 5 and 0 -- one movement per unit", p.OnHand, p.Reserved)
	}
}

// Priced and never taken out still leaves at invoicing, as it always did.
func TestAPricedPartNeverTakenOutLeavesAtInvoicing(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	stockShelf(t, pool, 6)
	job := pricedPartJob(t, pool, 2000)
	if err := workshop.TakeOut(context.Background(), pool, technician(), job, stockedPart, 1); err != nil {
		t.Fatal(err)
	}
	invoice(t, pool, job)
	if p := shelf(t, pool); p.OnHand != 4 || p.Reserved != 0 {
		t.Errorf("on hand %v, reserved %v; want 4 and 0 -- one taken out, the other issued", p.OnHand, p.Reserved)
	}
}

// Found needed under the car and not priced: allowed, and shown, which is
// exactly what stops it being fitted for nothing.
func TestAPartTakenOutButNotPricedIsShownAndNotIssuedAgain(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 3)
	job := newJob(t, pool)

	number, name, err := workshop.TakeOutByCode(ctx, pool, technician(), job, "BP-100", 1)
	if err != nil || number != "BP-100" || name == "" {
		t.Fatalf("TakeOutByCode: %q %q %v", number, name, err)
	}
	if jp := jobPart(t, pool, job); !jp.NotPriced() {
		t.Errorf("%+v, want taken out and not priced", jp)
	}
	if p := shelf(t, pool); p.OnHand != 2 {
		t.Errorf("on hand %v, want 2", p.OnHand)
	}
	if _, _, err := workshop.TakeOutByCode(ctx, pool, technician(), job, "NO-SUCH", 1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unknown code: %v", err)
	}
}

// More than was priced and reserved is allowed; it shows as a shortfall, the
// way reserving more than is on the shelf does.
func TestTakingOutMoreThanTheShelfHoldsIsAllowed(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	stockShelf(t, pool, 1)
	job := pricedPartJob(t, pool, 1000)
	if err := workshop.TakeOut(context.Background(), pool, technician(), job, stockedPart, 2); err != nil {
		t.Fatalf("TakeOut: %v", err)
	}
	if p := shelf(t, pool); p.OnHand != -1 || !p.Negative() {
		t.Errorf("on hand %v, want -1 and flagged", p.OnHand)
	}
}

// Not needed after all: a movement back, never more than was taken out.
func TestPuttingAPartBackIsAMovementAndNoMoreThanWasTakenOut(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 5)
	job := newJob(t, pool)
	if err := workshop.TakeOut(ctx, pool, technician(), job, stockedPart, 2); err != nil {
		t.Fatal(err)
	}
	if err := workshop.PutBack(ctx, pool, technician(), job, stockedPart, 1); err != nil {
		t.Fatalf("PutBack: %v", err)
	}
	if p := shelf(t, pool); p.OnHand != 4 {
		t.Errorf("on hand %v, want 4", p.OnHand)
	}
	if err := workshop.PutBack(ctx, pool, technician(), job, stockedPart, 2); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("put back more than was out: %v", err)
	}
	movements, _ := workshop.MovementsFor(ctx, pool, partsDeskScope(), stockedPart)
	if len(movements) < 3 {
		t.Errorf("%d movements, want the receipt, the take-out and the put-back all kept", len(movements))
	}
}

// Two put-backs of the last one at once: one succeeds.
func TestTwoPutBacksOfTheLastOneAtOnceReturnOne(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 5)
	job := newJob(t, pool)
	if err := workshop.TakeOut(ctx, pool, technician(), job, stockedPart, 1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- workshop.PutBack(ctx, pool, technician(), job, stockedPart, 1) }()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d put-backs succeeded, want 1", ok)
	}
	if p := shelf(t, pool); p.OnHand != 5 {
		t.Errorf("on hand %v, want 5", p.OnHand)
	}
}

// Fitting is the technician's and stock is the parts desk's; pricing stays
// at the counter, and a settled job takes nothing more.
func TestWhoTakesOutAndWhen(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 5)
	job := pricedPartJob(t, pool, 1000)

	if err := workshop.TakeOut(ctx, pool, partsDesk(), job, stockedPart, 1); err != nil {
		t.Errorf("the parts desk could not take out: %v", err)
	}
	bookkeeper := access.Scope{ShopID: shopID, UserID: userID, Role: "bookkeeper"}
	if err := workshop.TakeOut(ctx, pool, bookkeeper, job, stockedPart, 1); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a role that neither fits nor stocks took out a part: %v", err)
	}
	invoice(t, pool, job)
	if err := workshop.TakeOut(ctx, pool, technician(), job, stockedPart, 1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("took out to an invoiced job: %v", err)
	}
}
