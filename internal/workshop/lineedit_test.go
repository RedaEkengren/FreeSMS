package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A line on a job could be added and never corrected. These are the
// corrections, and what each one may not be allowed to do to the approval the
// customer gave, the shelf, or the other person editing at the same time.

func labourLine(t *testing.T, pool *pgxpool.Pool, job, text string, hoursMilli, priceMinor int64) {
	t.Helper()
	if err := workshop.AddLine(context.Background(), pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: text, QuantityMilli: hoursMilli, UnitPriceMinor: priceMinor, VATRateBasis: 2500,
	}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}
}

func lineOf(t *testing.T, pool *pgxpool.Pool, job string, position int) workshop.Line {
	t.Helper()
	_, lines, err := workshop.JobByID(context.Background(), pool, advisor(), job)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	for _, l := range lines {
		if l.Position == position {
			return l
		}
	}
	t.Fatalf("no line %d", position)
	return workshop.Line{}
}

func reservedFor(t *testing.T, pool *pgxpool.Pool, job string) float64 {
	t.Helper()
	var n float64
	if err := pool.QueryRow(context.Background(), `
		SELECT coalesce(sum(quantity), 0)::float8 FROM stock_movements
		WHERE work_order_id = $1 AND kind IN ('reserved', 'unreserved')`, job).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func asEdit(l workshop.Line) workshop.LineEdit {
	return workshop.LineEdit{
		Description: l.Description, QuantityMilli: l.QuantityMilli, UnitPriceMinor: l.UnitPriceMinor,
		VATRateBasis: l.VATRateBasis, CostBearer: l.CostBearer, PartID: l.PartID,
	}
}

func TestACorrectedQuantityResizesWhatIsPutAside(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	job := pricedPartJob(t, pool, 3000)
	if got := reservedFor(t, pool, job); got != 3 {
		t.Fatalf("reserved %v after pricing, want 3", got)
	}

	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.QuantityMilli = 1000
	c, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e)
	if err != nil {
		t.Fatalf("EditLine: %v", err)
	}
	if got := reservedFor(t, pool, job); got != 1 {
		t.Errorf("reserved %v after cutting to 1, want 1", got)
	}
	if len(c.Stock) != 1 || c.Stock[0].ReservedBeforeMilli != 3000 || c.Stock[0].ReservedAfterMilli != 1000 {
		t.Errorf("stock effect %+v", c.Stock)
	}

	l = lineOf(t, pool, job, 1)
	e.QuantityMilli = 5500
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); err != nil {
		t.Fatalf("EditLine up: %v", err)
	}
	if got := reservedFor(t, pool, job); got != 5.5 {
		t.Errorf("reserved %v after raising to 5.5, want 5.5", got)
	}
}

func TestSavingTheSameCorrectionTwiceChangesNothingTheSecondTime(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	job := pricedPartJob(t, pool, 3000)
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.QuantityMilli = 2000

	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); err != nil {
		t.Fatal(err)
	}
	moves := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE work_order_id = $1`, job)
	// The form is replayed from an offline queue, still carrying version 1.
	c, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e)
	if err != nil || !c.NoChange {
		t.Fatalf("replay: %+v %v, want a quiet success", c, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE work_order_id = $1`, job); n != moves {
		t.Errorf("the replay wrote %d more movements", n-moves)
	}
	h, _ := workshop.LineHistory(ctx, pool, advisor(), job, 1)
	if len(h) != 2 {
		t.Errorf("%d versions after one edit and a replay, want 2", len(h))
	}
}

func TestAHigherPriceOnAnApprovedLineNeedsAFreshYesAndKeepsTheOldOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Replace brake pads", 2000, 80000)
	labourLine(t, pool, job, "Check the fluid", 500, 80000)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)

	l := lineOf(t, pool, job, 1)
	if !l.Approved() {
		t.Fatal("the line should be approved")
	}
	var frozen int64
	if err := pool.QueryRow(ctx, `SELECT unit_price_minor FROM quote_lines WHERE description = 'Replace brake pads'`).Scan(&frozen); err != nil {
		t.Fatal(err)
	}

	// Wording and a lower price keep the yes.
	e := asEdit(l)
	e.Description = "Replace front brake pads"
	e.UnitPriceMinor = 70000
	c, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e)
	if err != nil || c.ApprovalCleared || !c.KeepsApproval() {
		t.Fatalf("a cheaper line: %+v %v, want the approval kept", c, err)
	}
	if !lineOf(t, pool, job, 1).Approved() {
		t.Error("the approval was lost on a cheaper line")
	}

	// A higher one does not.
	l = lineOf(t, pool, job, 1)
	e = asEdit(l)
	e.UnitPriceMinor = 90000
	c, err = workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e)
	if err != nil || !c.ApprovalCleared || !c.WasApproved {
		t.Fatalf("a dearer line: %+v %v, want the approval cleared", c, err)
	}
	l = lineOf(t, pool, job, 1)
	if l.Approved() {
		t.Error("the line is still approved at a price nobody agreed to")
	}
	if !lineOf(t, pool, job, 2).Approved() {
		t.Error("an untouched line lost its approval")
	}

	// The line goes back into what is to be put to the customer; the earlier
	// yes and the version it was a yes to are as they were.
	qv, err := workshop.QuoteFor(ctx, pool, advisor(), job)
	if err != nil || qv.Pending != 1 {
		t.Fatalf("QuoteFor: %+v %v, want one line waiting", qv, err)
	}
	var still int64
	if err := pool.QueryRow(ctx, `SELECT unit_price_minor FROM quote_lines WHERE description = 'Replace brake pads'`).Scan(&still); err != nil || still != frozen {
		t.Errorf("the quoted copy now says %d, want %d: %v", still, frozen, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM quote_decisions WHERE work_order_id = $1 AND decision = 'approved'`, job); n != 1 {
		t.Errorf("%d approvals kept, want 1", n)
	}
	h, _ := workshop.LineHistory(ctx, pool, advisor(), job, 1)
	if len(h) != 3 || !h[2].ApprovalCleared || h[2].Values.UnitPriceMinor != 90000 || h[0].Values.UnitPriceMinor != 80000 {
		t.Errorf("history %+v", h)
	}
}

func TestMoreOfAnApprovedLineClearsTheApprovalEvenAtACheaperPrice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Service", 1000, 90000)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved)
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.QuantityMilli = 2000
	e.UnitPriceMinor = 40000 // dearer in hours, cheaper in the total
	c, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e)
	if err != nil || !c.ApprovalCleared {
		t.Fatalf("%+v %v: twice the work is not what was agreed to", c, err)
	}
}

func TestAQuoteWhoseLineWasCorrectedCannotBeSaidYesTo(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Replace brake pads", 2000, 80000)
	q, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.UnitPriceMinor = 120000
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); err != nil {
		t.Fatal(err)
	}
	// The customer, or the counter on their behalf, answers the old number.
	err = workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, workshop.QuoteAnswer{
		Decision: "approved", Via: "counter", AgreedGrossMinor: q.GrossMinor})
	if !errors.Is(err, workshop.ErrQuoteSuperseded) {
		t.Fatalf("a yes to the old price = %v, want the quote to be out of date", err)
	}
	if lineOf(t, pool, job, 1).Approved() {
		t.Error("the line was approved at a price the customer never saw")
	}
}

func TestAWithdrawnLineIsGoneFromEverythingButTheHistory(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	job := pricedPartJob(t, pool, 2000)
	labourLine(t, pool, job, "Fit pads", 1000, 80000)
	before, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	_ = before
	gross := workshop.TotalsFor(lines).GrossMinor

	l := lineOf(t, pool, job, 1)
	c, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, l.Revision, "Wrong part, customer has their own")
	if err != nil {
		t.Fatalf("WithdrawLine: %v", err)
	}
	if c.JobGrossBefore != gross || c.JobGrossAfter != workshop.TotalsFor(lines[1:]).GrossMinor {
		t.Errorf("job gross %d -> %d", c.JobGrossBefore, c.JobGrossAfter)
	}
	if got := reservedFor(t, pool, job); got != 0 {
		t.Errorf("%v still put aside for a line that is gone", got)
	}
	_, lines, _ = workshop.JobByID(ctx, pool, advisor(), job)
	if len(lines) != 2 || lines[0].WithdrawnAt == nil || lines[0].WithdrawnReason == "" {
		t.Fatalf("lines %+v: the withdrawn line should stay, with its reason", lines)
	}
	if workshop.TotalsFor(lines).GrossMinor != workshop.TotalsFor(lines[1:]).GrossMinor {
		t.Error("a withdrawn line is in the total")
	}

	// It is not on the invoice either, and does not take the part off the
	// shelf a second time.
	invoice(t, pool, job)
	if n := countRows(t, pool, `SELECT count(*) FROM invoice_lines il JOIN invoices i ON i.id = il.invoice_id WHERE i.work_order_id = $1`, job); n != 1 {
		t.Errorf("%d lines on the invoice, want only the labour", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE work_order_id = $1 AND kind = 'consumed'`, job); n != 0 {
		t.Errorf("%d parts consumed for a withdrawn line", n)
	}
}

func TestWithdrawingTwiceReleasesOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	job := pricedPartJob(t, pool, 2000)
	l := lineOf(t, pool, job, 1)
	if _, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, l.Revision, "Entered twice"); err != nil {
		t.Fatal(err)
	}
	moves := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE work_order_id = $1`, job)
	c, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, l.Revision, "Entered twice")
	if err != nil || !c.NoChange {
		t.Fatalf("second withdrawal: %+v %v", c, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE work_order_id = $1`, job); n != moves {
		t.Errorf("a replayed withdrawal wrote %d movements", n-moves)
	}
	if got := reservedFor(t, pool, job); got != 0 {
		t.Errorf("reserved %v, want 0", got)
	}
}

func TestWithdrawingAPartThatIsAlreadyOnTheCarDoesNotSayItCameBack(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	job := pricedPartJob(t, pool, 2000)
	if err := workshop.TakeOut(ctx, pool, technician(), job, stockedPart, 2); err != nil {
		t.Fatal(err)
	}
	onShelf := shelf(t, pool).OnHand

	l := lineOf(t, pool, job, 1)
	c, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, l.Revision, "Priced to the wrong job")
	if err != nil {
		t.Fatalf("WithdrawLine: %v", err)
	}
	if len(c.Stock) != 1 || !c.Stock[0].NotPriced() || c.Stock[0].TakenOutMilli != 2000 {
		t.Errorf("effect %+v: the part is on the car and no longer on the bill", c.Stock)
	}
	if got := shelf(t, pool).OnHand; got != onShelf {
		t.Errorf("on hand %v -> %v: nothing came back", onShelf, got)
	}
	if got := reservedFor(t, pool, job); got != 0 {
		t.Errorf("reserved %v: a negative reservation means the shelf went wrong", got)
	}
	jp := jobPart(t, pool, job)
	if !jp.NotPriced() || jp.TakenOut != 2 {
		t.Errorf("the job should show it as taken out and not priced: %+v", jp)
	}
}

func TestCorrectingTheQuantityOfAPartAlreadyTakenOutNeverReservesLess_ThanNothing(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	job := pricedPartJob(t, pool, 3000)
	if err := workshop.TakeOut(ctx, pool, technician(), job, stockedPart, 3); err != nil {
		t.Fatal(err)
	}
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.QuantityMilli = 1000
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); err != nil {
		t.Fatal(err)
	}
	if got := reservedFor(t, pool, job); got != 0 {
		t.Errorf("reserved %v with all of it already taken out, want 0", got)
	}
}

func TestADiagnosisThatHappenedCannotBeWithdrawnButTheRepairAroundItCan(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Diagnose the noise", 1000, 90000)
	labourLine(t, pool, job, "Replace the bearing", 2000, 90000)
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 1, true); err != nil {
		t.Fatal(err)
	}
	d := lineOf(t, pool, job, 1)
	if _, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, d.Revision, "mistake"); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("withdrawing work that was done = %v, want it refused", err)
	}
	r := lineOf(t, pool, job, 2)
	if _, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 2, r.Revision, "The customer fixed it elsewhere"); err != nil {
		t.Fatalf("withdrawing the repair: %v", err)
	}
	if lineOf(t, pool, job, 1).WithdrawnAt != nil || lineOf(t, pool, job, 2).WithdrawnAt == nil {
		t.Error("the wrong line was withdrawn")
	}
	// A reason is required, and the correction of a done line stays possible.
	if _, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, d.Revision, "  "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("withdrawing without a reason = %v", err)
	}
	e := asEdit(d)
	e.Description = "Diagnose the noise from the rear axle"
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, d.Revision, e); err != nil {
		t.Errorf("correcting a line that was done: %v", err)
	}
}

func TestTwoPeopleCorrectingTheSameLineGetOneEachWinnerAndOneConflict(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Service", 1000, 90000)
	l := lineOf(t, pool, job, 1)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := asEdit(l)
			e.UnitPriceMinor = int64(91000 + i*1000)
			_, results[i] = workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e)
		}()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, workshop.ErrLineChanged):
			conflict++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("%d saved and %d conflicted, want one each", ok, conflict)
	}
	h, _ := workshop.LineHistory(ctx, pool, advisor(), job, 1)
	if len(h) != 2 {
		t.Errorf("%d versions, want the original and the winner's", len(h))
	}
}

func TestACorrectionMadeFromAnOlderVersionIsRefusedAndWritesNothing(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Service", 1000, 90000)
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.UnitPriceMinor = 95000
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); err != nil {
		t.Fatal(err)
	}
	e.UnitPriceMinor = 99000
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); !errors.Is(err, workshop.ErrLineChanged) {
		t.Fatalf("an edit against version 1 of version 2 = %v", err)
	}
	if _, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, l.Revision, "stale"); !errors.Is(err, workshop.ErrLineChanged) {
		t.Fatalf("a withdrawal against version 1 of version 2 = %v", err)
	}
	if got := lineOf(t, pool, job, 1); got.UnitPriceMinor != 95000 || got.WithdrawnAt != nil {
		t.Errorf("the line is %+v", got)
	}
}

func TestAPreviewSaysWhatWouldHappenAndChangesNothing(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	job := pricedPartJob(t, pool, 3000)
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.QuantityMilli = 1000
	c, err := workshop.PreviewLineEdit(ctx, pool, advisor(), job, 1, l.Revision, e)
	if err != nil {
		t.Fatal(err)
	}
	if c.JobGrossAfter >= c.JobGrossBefore || c.Stock[0].ReservedAfterMilli != 1000 {
		t.Errorf("preview %+v", c)
	}
	if got := lineOf(t, pool, job, 1); got.Revision != l.Revision || got.QuantityMilli != 3000 {
		t.Errorf("the preview changed the line: %+v", got)
	}
	if got := reservedFor(t, pool, job); got != 3 {
		t.Errorf("the preview moved the shelf: %v", got)
	}
	if _, err := workshop.PreviewWithdrawal(ctx, pool, advisor(), job, 1, l.Revision, "x"); err != nil {
		t.Fatal(err)
	}
	if lineOf(t, pool, job, 1).WithdrawnAt != nil {
		t.Error("the preview withdrew the line")
	}
}

func TestAnInvoicedJobsLinesAreNotEditedBehindTheInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Service", 1000, 90000)
	invoice(t, pool, job)
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.UnitPriceMinor = 1
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("editing under an invoice = %v", err)
	}
	if _, err := workshop.WithdrawLine(ctx, pool, advisor(), job, 1, l.Revision, "no"); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("withdrawing under an invoice = %v", err)
	}
}

func TestACorrectionKeepsAFreeLineFree(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Warranty repair", QuantityMilli: 1000, VATRateBasis: 2500, CostBearer: "supplier",
	}); err != nil {
		t.Fatal(err)
	}
	l := lineOf(t, pool, job, 1)
	e := asEdit(l)
	e.UnitPriceMinor = 5000
	if _, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("pricing a warranty line = %v, want it refused", err)
	}
	// Making it the customer's to pay is a price they have not agreed to.
	e = asEdit(l)
	e.CostBearer, e.UnitPriceMinor = "customer", 5000
	c, err := workshop.EditLine(ctx, pool, advisor(), job, 1, l.Revision, e)
	if err != nil || c.JobGrossAfter != 6250 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestOnlyThePricingRolesCorrectLines(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	job := newJob(t, pool)
	labourLine(t, pool, job, "Service", 1000, 90000)
	l := lineOf(t, pool, job, 1)
	for name, s := range map[string]access.Scope{"technician": technician(), "parts": partsDesk()} {
		if _, err := workshop.EditLine(ctx, pool, s, job, 1, l.Revision, asEdit(l)); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s edited a line: %v", name, err)
		}
		if _, err := workshop.WithdrawLine(ctx, pool, s, job, 1, l.Revision, "no"); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s withdrew a line: %v", name, err)
		}
		if _, err := workshop.LineHistory(ctx, pool, s, job, 1); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the history: %v", name, err)
		}
	}
}
