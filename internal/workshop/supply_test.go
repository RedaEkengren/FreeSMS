package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Parts bought outside FreeSMS: what was ordered, what came, what went back.
// An order is the desk's answer to a technician's request, a receipt is what
// physically arrived, and the stock ledger only moves when something went onto
// the shelf.

// catalogued puts the brake pad set in the catalogue with nothing on the shelf.
func catalogued(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO parts (id, shop_id, number, name, unit, cost_minor)
		VALUES ($1, $2, 'BP-100', 'Brake pad set, front', 'each', 45000) ON CONFLICT DO NOTHING`, stockedPart, shopID); err != nil {
		t.Fatal(err)
	}
}

func jobWaitingFor(t *testing.T, pool *pgxpool.Pool, what string, qty float64) (job, request string) {
	t.Helper()
	ctx := context.Background()
	job = working(t, pool)
	if err := workshop.RequestParts(ctx, pool, technician(), job, what, qty); err != nil {
		t.Fatal(err)
	}
	rs, err := workshop.PartRequestsFor(ctx, pool, technician(), job)
	if err != nil || len(rs) != 1 {
		t.Fatalf("PartRequestsFor = %v, %v", rs, err)
	}
	return job, rs[0].ID
}

func orderOf(t *testing.T, pool *pgxpool.Pool, in workshop.OrderInput) string {
	t.Helper()
	id, err := workshop.RecordOrder(context.Background(), pool, partsDesk(), in)
	if err != nil {
		t.Fatalf("RecordOrder: %v", err)
	}
	return id
}

func orderView(t *testing.T, pool *pgxpool.Pool, id string) workshop.OrderView {
	t.Helper()
	o, err := workshop.OrderByID(context.Background(), pool, partsDesk(), id)
	if err != nil {
		t.Fatalf("OrderByID: %v", err)
	}
	return o
}

func deliver(t *testing.T, pool *pgxpool.Pool, line string, qty float64, route, ref string) string {
	t.Helper()
	id, _, err := workshop.ReceiveDelivery(context.Background(), pool, partsDesk(), workshop.DeliveryInput{LineID: line, Quantity: qty, Route: route, DeliveryRef: ref})
	if err != nil {
		t.Fatalf("ReceiveDelivery: %v", err)
	}
	return id
}

func requestOf(t *testing.T, pool *pgxpool.Pool, job string) workshop.PartRequest {
	t.Helper()
	rs, err := workshop.PartRequestsFor(context.Background(), pool, technician(), job)
	if err != nil || len(rs) == 0 {
		t.Fatalf("PartRequestsFor = %v, %v", rs, err)
	}
	return rs[0]
}

func TestOneOrderSuppliesTwoJobsAndTheShopFloorSeesWhenItComes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	jobA, reqA := jobWaitingFor(t, pool, "Track rod end, left", 1)
	jobB, reqB := jobWaitingFor(t, pool, "Track rod end, right", 1)
	eta := time.Now().AddDate(0, 0, 3)
	cost := int64(21000)
	order := orderOf(t, pool, workshop.OrderInput{
		Supplier: "Bildelsgrossisten", Reference: "BG-4411", ETA: &eta,
		Lines: []workshop.OrderLineInput{
			{RequestID: reqA, Quantity: 1, ExpectedUnitCostMinor: &cost},
			{RequestID: reqB, Quantity: 1, ExpectedUnitCostMinor: &cost},
		},
	})
	// The technician sees that it is ordered and when, and nothing about who
	// from or what it costs.
	r := requestOf(t, pool, jobA)
	if !r.Ordered || r.ETA == nil || r.Stage() != "ordered" {
		t.Errorf("the request after ordering: ordered %v, eta %v, stage %q", r.Ordered, r.ETA, r.Stage())
	}
	if _, err := workshop.OrderByID(ctx, pool, technician(), order); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the order: %v", err)
	}
	if d, err := workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now()); err != nil || len(d.Open) != 2 || len(d.Unordered) != 0 {
		t.Errorf("the desk after ordering: open %d, unordered %d, %v", len(d.Open), len(d.Unordered), err)
	}

	// The first part comes and goes straight onto its car.
	o := orderView(t, pool, order)
	deliver(t, pool, o.Lines[0].LineID, 1, "direct", "DN-1")
	if requestOf(t, pool, jobA).Stage() != "arrived" {
		t.Error("the first job's request is not done")
	}
	if ja, _, _ := workshop.JobByID(ctx, pool, technician(), jobA); ja.State != string(workshop.StateInProgress) {
		t.Errorf("the first job is %s, want it back at work", ja.State)
	}
	if jb, _, _ := workshop.JobByID(ctx, pool, technician(), jobB); jb.State != string(workshop.StateAwaitingParts) {
		t.Errorf("the second job is %s, want it still waiting", jb.State)
	}
	// Going straight to the car never touches the shelf.
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE kind = 'received' AND note = 'Supplier delivery'`); n != 0 {
		t.Errorf("a direct delivery wrote %d stock receipts", n)
	}
}

func TestTwoDeliveriesForOneLineAndTheSameNoteTwice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	catalogued(t, pool)
	job, req := jobWaitingFor(t, pool, "Brake pad set, front", 4)
	order := orderOf(t, pool, workshop.OrderInput{Supplier: "Bildelsgrossisten",
		Lines: []workshop.OrderLineInput{{RequestID: req, PartNumber: "BP-100", Quantity: 4}}})
	line := orderView(t, pool, order).Lines[0].LineID

	deliver(t, pool, line, 2, "stock", "DN-7")
	if r := requestOf(t, pool, job); r.Stage() != "partly here" || r.Received != 2 {
		t.Errorf("after two of four: stage %q, received %v", r.Stage(), r.Received)
	}
	if got := shelf(t, pool).OnHand; got != 2 {
		t.Errorf("on the shelf %v, want 2", got)
	}
	// The same delivery note again is the same delivery.
	id, already, err := workshop.ReceiveDelivery(ctx, pool, partsDesk(), workshop.DeliveryInput{LineID: line, Quantity: 2, Route: "stock", DeliveryRef: "dn-7"})
	if err != nil || !already || id == "" {
		t.Fatalf("the same note again = %q, %v, %v; want already", id, already, err)
	}
	if got := shelf(t, pool).OnHand; got != 2 {
		t.Errorf("a duplicate entry counted twice: %v on the shelf", got)
	}
	deliver(t, pool, line, 2, "stock", "DN-9")
	if r := requestOf(t, pool, job); r.Stage() != "arrived" {
		t.Errorf("after four of four the request is %q", r.Stage())
	}
	if got := shelf(t, pool).OnHand; got != 4 {
		t.Errorf("on the shelf %v, want 4", got)
	}
	o := orderView(t, pool, order)
	if len(o.Lines[0].Receipts) != 2 || o.Lines[0].Rest() != 0 {
		t.Errorf("the order shows %d deliveries, %v owed", len(o.Lines[0].Receipts), o.Lines[0].Rest())
	}
}

func TestAWrongPartReturnedAfterReceiptCreditedAndAskedForAgain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	catalogued(t, pool)
	job, req := jobWaitingFor(t, pool, "Brake pad set, front", 1)
	expected := int64(45000)
	order := orderOf(t, pool, workshop.OrderInput{Supplier: "Bildelsgrossisten",
		Lines: []workshop.OrderLineInput{{RequestID: req, PartNumber: "BP-100", Quantity: 1, ExpectedUnitCostMinor: &expected}}})
	line := orderView(t, pool, order).Lines[0].LineID
	receipt := deliver(t, pool, line, 1, "stock", "DN-1")
	if got := shelf(t, pool).OnHand; got != 1 {
		t.Fatalf("on the shelf %v, want 1", got)
	}

	// It is the wrong part. More than came cannot go back.
	credit := int64(45000)
	if _, err := workshop.PlanReturn(ctx, pool, partsDesk(), workshop.ReturnInput{ReceiptID: receipt, Quantity: 2, Reason: "wrong_part"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("returning more than came = %v, want ErrInvalid", err)
	}
	ret, err := workshop.PlanReturn(ctx, pool, partsDesk(), workshop.ReturnInput{ReceiptID: receipt, Quantity: 1, Reason: "wrong_part",
		ExpectedCreditMinor: &credit, NeedReplacement: true})
	if err != nil {
		t.Fatal(err)
	}
	// Planning is not leaving: the shelf is unchanged, and the job is waiting
	// for the right one.
	if got := shelf(t, pool).OnHand; got != 1 {
		t.Errorf("planning a return moved stock: %v", got)
	}
	if jb, _, _ := workshop.JobByID(ctx, pool, technician(), job); jb.State != string(workshop.StateAwaitingParts) {
		t.Errorf("the job is %s, want waiting for the replacement", jb.State)
	}
	rs, _ := workshop.PartRequestsFor(ctx, pool, technician(), job)
	if len(rs) != 2 || rs[1].Description != "Replacement: Brake pad set, front" || !rs[1].Open() {
		t.Errorf("the requests after a wrong part: %+v", rs)
	}

	// The credit can arrive before the box leaves; then it is credited and
	// still here, which stays on the desk's list.
	if err := workshop.ConfirmCredit(ctx, pool, partsDesk(), ret, 45000, "CR-88"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ConfirmCredit(ctx, pool, partsDesk(), ret, 45000, "CR-88"); err != nil {
		t.Errorf("the same credit again = %v, want quiet success", err)
	}
	if err := workshop.ConfirmCredit(ctx, pool, partsDesk(), ret, 100, "x"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a different credit for the same return = %v, want ErrInvalid", err)
	}
	d, _ := workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now())
	if len(d.Returns) != 1 || d.Returns[0].Status() != "credited, the part is still here" {
		t.Fatalf("the desk's returns: %+v", d.Returns)
	}
	if err := workshop.MarkReturned(ctx, pool, partsDesk(), ret); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MarkReturned(ctx, pool, partsDesk(), ret); err != nil {
		t.Errorf("returning twice = %v, want quiet success", err)
	}
	if got := shelf(t, pool).OnHand; got != 0 {
		t.Errorf("on the shelf %v after it left, want 0", got)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE kind = 'returned'`); n != 1 {
		t.Errorf("%d returned movements, want exactly 1", n)
	}
	if d, _ = workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now()); len(d.Returns) != 0 {
		t.Errorf("a credited, returned part is still on the list: %+v", d.Returns)
	}
}

func TestASubstituteCountsForNothingUntilSomebodySaysItWillDo(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	catalogued(t, pool)
	job, req := jobWaitingFor(t, pool, "Brake pad set, front", 1)
	order := orderOf(t, pool, workshop.OrderInput{Supplier: "Bildelsgrossisten",
		Lines: []workshop.OrderLineInput{{RequestID: req, PartNumber: "BP-100", Quantity: 1}}})
	line := orderView(t, pool, order).Lines[0].LineID

	// It cannot be shelved under the ordered part's number.
	if _, _, err := workshop.ReceiveDelivery(ctx, pool, partsDesk(), workshop.DeliveryInput{LineID: line, Quantity: 1, Route: "stock", Substitute: "Pad set, other brand"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("shelving a substitute = %v, want ErrInvalid", err)
	}
	receipt, _, err := workshop.ReceiveDelivery(ctx, pool, partsDesk(), workshop.DeliveryInput{LineID: line, Quantity: 1, Route: "direct", Substitute: "Pad set, other brand"})
	if err != nil {
		t.Fatal(err)
	}
	if r := requestOf(t, pool, job); r.Received != 0 || !r.Open() {
		t.Errorf("a substitute counted before it was accepted: %+v", r)
	}
	d, _ := workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now())
	if len(d.Substitutes) != 1 || d.Substitutes[0].Substitute != "Pad set, other brand" || len(d.Open) != 1 {
		t.Fatalf("the desk: %d substitutes, %d open lines", len(d.Substitutes), len(d.Open))
	}
	if _, err := workshop.PlanReturn(ctx, pool, partsDesk(), workshop.ReturnInput{ReceiptID: receipt, Quantity: 1, Reason: "other"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("returning a substitute nobody decided on = %v, want ErrInvalid", err)
	}
	if err := workshop.AcceptSubstitute(ctx, pool, partsDesk(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AcceptSubstitute(ctx, pool, partsDesk(), receipt); err != nil {
		t.Errorf("accepting twice = %v, want quiet success", err)
	}
	if requestOf(t, pool, job).Stage() != "arrived" {
		t.Error("the accepted substitute did not finish the request")
	}
	if got := shelf(t, pool).OnHand; got != 0 {
		t.Errorf("a substitute reached the shelf: %v", got)
	}

	// Turned down, it is planned back instead.
	job2, req2 := jobWaitingFor(t, pool, "Brake pad set, front", 1)
	_ = job2
	order2 := orderOf(t, pool, workshop.OrderInput{Supplier: "Bildelsgrossisten", Reference: "BG-2",
		Lines: []workshop.OrderLineInput{{RequestID: req2, Quantity: 1}}})
	rec2, _, err := workshop.ReceiveDelivery(ctx, pool, partsDesk(), workshop.DeliveryInput{LineID: orderView(t, pool, order2).Lines[0].LineID, Quantity: 1, Route: "direct", Substitute: "Something else"})
	if err != nil {
		t.Fatal(err)
	}
	ret, err := workshop.RejectSubstitute(ctx, pool, partsDesk(), rec2)
	if err != nil || ret == "" {
		t.Fatalf("RejectSubstitute = %q, %v", ret, err)
	}
	if d, _ = workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now()); len(d.Returns) != 1 || d.Returns[0].Reason != "substitute_rejected" || len(d.Substitutes) != 0 {
		t.Errorf("after turning it down: %+v / %d substitutes", d.Returns, len(d.Substitutes))
	}
}

func TestCancellingHereDoesNotProveTheSupplierStoppedIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job, req := jobWaitingFor(t, pool, "Wheel bearing", 1)
	order := orderOf(t, pool, workshop.OrderInput{Supplier: "Bildelsgrossisten", Lines: []workshop.OrderLineInput{{RequestID: req, Quantity: 1}}})
	if err := workshop.CancelOrder(ctx, pool, partsDesk(), order, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("cancelling with no reason = %v, want ErrInvalid", err)
	}
	if err := workshop.CancelOrder(ctx, pool, partsDesk(), order, "Customer changed their mind"); err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now())
	if len(d.Cancellations) != 1 || d.Cancellations[0].Open != 1 || len(d.Open) != 0 || len(d.Unordered) != 1 {
		t.Errorf("after cancelling: %d unconfirmed (%d open), %d open lines, %d unordered", len(d.Cancellations), d.Cancellations[0].Open, len(d.Open), len(d.Unordered))
	}
	// The job's request is still open: calling off an order does not call off
	// the need.
	if r := requestOf(t, pool, job); !r.Open() {
		t.Error("cancelling the order cancelled the request")
	}
	if err := workshop.ConfirmSupplierCancellation(ctx, pool, partsDesk(), order); err != nil {
		t.Fatal(err)
	}
	if d, _ = workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now()); len(d.Cancellations) != 0 {
		t.Error("a confirmed cancellation is still on the list")
	}
	other := orderOf(t, pool, workshop.OrderInput{Supplier: "X", Lines: []workshop.OrderLineInput{{Description: "Oil", Quantity: 1}}})
	if err := workshop.ConfirmSupplierCancellation(ctx, pool, partsDesk(), other); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("confirming a cancellation that never happened = %v, want ErrInvalid", err)
	}
}

func TestLateDeliveriesAndCostDifferencesAreListedAndEntriesAreChecked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	_, req := jobWaitingFor(t, pool, "Wheel bearing", 2)
	yesterday := time.Now().AddDate(0, 0, -1)
	expected := int64(30000)
	order := orderOf(t, pool, workshop.OrderInput{Supplier: "Lagerhuset", Reference: "LH-1", ETA: &yesterday,
		Lines: []workshop.OrderLineInput{{RequestID: req, Quantity: 2, ExpectedUnitCostMinor: &expected}}})
	line := orderView(t, pool, order).Lines[0].LineID
	d, _ := workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now())
	if len(d.Overdue) != 1 {
		t.Fatalf("%d overdue, want 1", len(d.Overdue))
	}
	tomorrow := time.Now().AddDate(0, 0, 1)
	if err := workshop.ChangeETA(ctx, pool, partsDesk(), line, tomorrow, "Supplier says Friday"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ChangeETA(ctx, pool, partsDesk(), line, tomorrow, ""); err != nil {
		t.Errorf("the same day again = %v, want quiet success", err)
	}
	d, _ = workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now())
	if len(d.Overdue) != 0 || len(d.Open) != 1 || d.Open[0].Slips != 1 {
		t.Errorf("after moving the day: %d overdue, slips %d", len(d.Overdue), d.Open[0].Slips)
	}
	if o := orderView(t, pool, order); len(o.Lines[0].ETAs) != 2 {
		t.Errorf("the line remembers %d days, want both", len(o.Lines[0].ETAs))
	}

	dearer := int64(33500)
	if _, _, err := workshop.ReceiveDelivery(ctx, pool, partsDesk(), workshop.DeliveryInput{LineID: line, Quantity: 2, Route: "direct", UnitCostMinor: &dearer}); err != nil {
		t.Fatal(err)
	}
	d, _ = workshop.SupplyDeskFor(ctx, pool, partsDesk(), time.Now())
	if len(d.CostDiffs) != 1 || d.CostDiffs[0].PerUnit() != 3500 || d.CostDiffs[0].Total() != 7000 {
		t.Errorf("cost differences: %+v", d.CostDiffs)
	}

	// The same reference from the same supplier is the same order.
	_, err := workshop.RecordOrder(ctx, pool, partsDesk(), workshop.OrderInput{Supplier: "LAGERHUSET", Reference: "lh-1",
		Lines: []workshop.OrderLineInput{{Description: "Oil", Quantity: 1}}})
	var dup *workshop.DuplicateOrder
	if !errors.As(err, &dup) || dup.ID != order || !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same order twice = %v, want a duplicate pointing at the first", err)
	}
	for name, in := range map[string]workshop.OrderInput{
		"no supplier":  {Lines: []workshop.OrderLineInput{{Description: "x", Quantity: 1}}},
		"no lines":     {Supplier: "A"},
		"no quantity":  {Supplier: "A", Lines: []workshop.OrderLineInput{{Description: "x"}}},
		"no words":     {Supplier: "A", Lines: []workshop.OrderLineInput{{Quantity: 1}}},
		"unknown part": {Supplier: "A", Lines: []workshop.OrderLineInput{{PartNumber: "NOPE-1", Quantity: 1}}},
	} {
		if _, err := workshop.RecordOrder(ctx, pool, partsDesk(), in); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestUncataloguedAndCustomerOwnedPartsNeverTouchTheShelf(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	stockShelf(t, pool, 5)
	job, req := jobWaitingFor(t, pool, "Mirror glass", 1)
	order := orderOf(t, pool, workshop.OrderInput{Supplier: "Specialisten", Lines: []workshop.OrderLineInput{{RequestID: req, Quantity: 1}}})
	line := orderView(t, pool, order).Lines[0].LineID
	if _, _, err := workshop.ReceiveDelivery(ctx, pool, partsDesk(), workshop.DeliveryInput{LineID: line, Quantity: 1, Route: "stock"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("shelving an uncatalogued part = %v, want ErrInvalid", err)
	}
	deliver(t, pool, line, 1, "customer", "")
	if r := requestOf(t, pool, job); r.Stage() != "arrived" {
		t.Errorf("the customer's own part left the request %q", r.Stage())
	}
	if got := shelf(t, pool).OnHand; got != 5 {
		t.Errorf("the shelf moved: %v", got)
	}
	for _, f := range []func() error{
		func() error {
			_, _, e := workshop.ReceiveDelivery(ctx, pool, technician(), workshop.DeliveryInput{LineID: line, Quantity: 1, Route: "direct"})
			return e
		},
		func() error {
			_, e := workshop.RecordOrder(ctx, pool, technician(), workshop.OrderInput{Supplier: "A"})
			return e
		},
		func() error { _, e := workshop.SupplyDeskFor(ctx, pool, technician(), time.Now()); return e },
		func() error { return workshop.ChangeETA(ctx, pool, technician(), line, time.Now(), "") },
		func() error { return workshop.CancelOrder(ctx, pool, technician(), order, "x") },
	} {
		if err := f(); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("a technician bought something: %v", err)
		}
	}
}
