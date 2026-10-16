package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Correcting one wrongly billed line without refunding the rest: every credit
// bounded by what is left of the original under a lock, the last part of a line
// taking the öre the earlier parts rounded away, and a price correction never
// touching the shelf.

func creditedOf(t *testing.T, pool *pgxpool.Pool, inv string) (net, gross int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(sum(net_minor), 0), coalesce(sum(gross_minor), 0) FROM invoices WHERE credit_of_id = $1`, inv).Scan(&net, &gross); err != nil {
		t.Fatal(err)
	}
	return
}

func mixedInvoice(t *testing.T, pool *pgxpool.Pool) (job string, inv workshop.Invoice) {
	t.Helper()
	ctx := context.Background()
	job = newJob(t, pool)
	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Replace rear shocks", QuantityMilli: 3000, UnitPriceMinor: 33333, VATRateBasis: 1200},
		{Kind: "part", Description: "Shock absorber", QuantityMilli: 2000, UnitPriceMinor: 125049, VATRateBasis: 2500},
		{Kind: "labour", Description: "Alignment", QuantityMilli: 1250, UnitPriceMinor: 90000, VATRateBasis: 2500},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), job, l); err != nil {
			t.Fatal(err)
		}
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	return job, inv
}

func TestPartThenTheRestOfALineCreditsExactlyTheLine(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := mixedInvoice(t, pool)
	doc, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	var origNet, origVAT int64
	for _, l := range doc.Lines {
		if l.Position == 1 {
			origNet, origVAT = l.NetMinor, l.VATMinor
		}
	}

	// One hour of the three: the preview says what it comes to and what is left.
	req := workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 1, QuantityMilli: 1000}}, Reason: "Billed three hours, two were done", Key: "k1"}
	pv, err := workshop.PreviewCredit(ctx, pool, advisor(), inv.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if pv.GrossMinor >= 0 || len(pv.Lines) != 1 || pv.LeftAfter != pv.LeftBefore+pv.GrossMinor || pv.Everything {
		t.Errorf("preview = %+v", pv)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoices WHERE credit_of_id = $1`, inv.ID); n != 0 {
		t.Fatal("a preview wrote a credit note")
	}
	first, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.GrossMinor != pv.GrossMinor {
		t.Errorf("the note is %d, the preview said %d", first.GrossMinor, pv.GrossMinor)
	}

	// The rest of that line: it takes what the first part rounded away.
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{
		Lines: []workshop.CreditLine{{Position: 1, QuantityMilli: 2500}}, Reason: "x", Key: "k-too-many"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("crediting more than is left of a line = %v, want ErrInvalid", err)
	}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{
		Lines: []workshop.CreditLine{{Position: 1}}, Reason: "The other two hours as well", Key: "k2"}); err != nil {
		t.Fatal(err)
	}
	net, _ := creditedOf(t, pool, inv.ID)
	if net != -origNet {
		t.Errorf("the credits come to %d net, want exactly the line's %d", net, -origNet)
	}
	var vat int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(vat_minor), 0) FROM invoices WHERE credit_of_id = $1`, inv.ID).Scan(&vat); err != nil {
		t.Fatal(err)
	}
	if vat != -origVAT {
		t.Errorf("VAT credited %d, want %d", vat, -origVAT)
	}

	// Nothing named is what is left; and then there is nothing.
	rest, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, g := creditedOf(t, pool, inv.ID); g != -inv.GrossMinor {
		t.Errorf("after crediting the remainder %d is credited of %d", -g, inv.GrossMinor)
	}
	if len(rest.CustomerName) == 0 {
		t.Error("the remainder carries no buyer")
	}
	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); !errors.Is(err, workshop.ErrAlreadyCredited) {
		t.Errorf("a credit of nothing = %v, want ErrAlreadyCredited", err)
	}
	// A credit note is not credited.
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), rest.ID, workshop.CreditRequest{Reason: "x"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("crediting a credit note = %v, want ErrInvalid", err)
	}
}

func TestAnAdjustmentIsBoundedByWhatIsLeft(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := mixedInvoice(t, pool)
	adj := workshop.Adjustment{Description: "Goodwill for the delay", NetMinor: 20000, VATRateBasis: 2500}
	n, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Adjustments: []workshop.Adjustment{adj}, Reason: "Promised on the phone", Key: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	if n.NetMinor != -20000 || n.VATMinor != -5000 {
		t.Errorf("adjustment note = %d net %d VAT", n.NetMinor, n.VATMinor)
	}
	big := workshop.Adjustment{Description: "Everything and more", NetMinor: inv.GrossMinor, VATRateBasis: 2500}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Adjustments: []workshop.Adjustment{big}, Reason: "x", Key: "g2"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an adjustment above what is left = %v, want ErrInvalid", err)
	}
	for name, a := range map[string]workshop.Adjustment{
		"no words":  {NetMinor: 100, VATRateBasis: 2500},
		"no amount": {Description: "x", VATRateBasis: 2500},
		"bad rate":  {Description: "x", NetMinor: 100, VATRateBasis: 20000},
	} {
		if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Adjustments: []workshop.Adjustment{a}, Reason: "x"}); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 9}}, Reason: "x"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a line that is not there = %v, want ErrInvalid", err)
	}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 1}}}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a credit with no reason = %v, want ErrInvalid", err)
	}
}

func TestTwoPeopleCreditingTheSameLineCreditItOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := mixedInvoice(t, pool)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{
				Lines: []workshop.CreditLine{{Position: 2}}, Reason: "Wrong part billed", Key: "race-" + string(rune('a'+i))})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, workshop.ErrInvalid):
		default:
			t.Errorf("a racing credit failed oddly: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d of 8 credits of the same line went through, want exactly 1", ok)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoices WHERE credit_of_id = $1`, inv.ID); n != 1 {
		t.Errorf("%d credit notes, want 1", n)
	}
}

func TestTheSameFormSentTwiceMakesOneCreditNote(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := mixedInvoice(t, pool)
	req := workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 3, QuantityMilli: 500}}, Reason: "Half an hour too much", Key: "form-1"}
	a, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, req)
	if err != nil || b.ID != a.ID {
		t.Fatalf("the retry made %q (%v), want the first note %q", b.ID, err, a.ID)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoices WHERE credit_of_id = $1`, inv.ID); n != 1 {
		t.Errorf("%d credit notes after a retry, want 1", n)
	}
}

func TestACreditOnAPaidInvoiceIsMoneyOwedBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := mixedInvoice(t, pool)
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	req := workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 3}}, Reason: "Alignment was not done", Key: "p1"}
	pv, err := workshop.PreviewCredit(ctx, pool, advisor(), inv.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if pv.OwedBefore != 0 || pv.OwedAfter != pv.GrossMinor {
		t.Errorf("paid in full: owed %d -> %d, want 0 -> %d", pv.OwedBefore, pv.OwedAfter, pv.GrossMinor)
	}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, req); err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if b.OutstandingMinor() != pv.GrossMinor || b.OutstandingMinor() >= 0 {
		t.Errorf("the balance %d, want %d owed back", b.OutstandingMinor(), pv.GrossMinor)
	}
	// Reversing the payment is for a mistake; it does not refund anyone.
	rs, _ := workshop.Receivables(ctx, pool, advisor())
	var listed bool
	for _, r := range rs {
		if r.InvoiceID == inv.ID && r.OutstandingMinor() < 0 {
			listed = true
		}
	}
	if !listed {
		t.Error("money owed back is not on the list")
	}
}

func TestAPriceCorrectionNeverPutsAFittedPartBackOnTheShelf(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 6)
	job := pricedPartJob(t, pool, 2000)
	invoice(t, pool, job)
	invs, _ := workshop.InvoicesFor(ctx, pool, advisor(), job)
	before := countRows(t, pool, `SELECT count(*) FROM stock_movements`)
	onHand := shelf(t, pool).OnHand
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), invs[0].ID, workshop.CreditRequest{
		Lines: []workshop.CreditLine{{Position: 1, QuantityMilli: 1000}}, Reason: "Only one set was fitted", Key: "s1"}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM stock_movements`); got != before {
		t.Errorf("a price correction wrote %d stock movements", got-before)
	}
	if got := shelf(t, pool).OnHand; got != onHand {
		t.Errorf("on the shelf %v after a credit, want %v: a physical return is its own event", got, onHand)
	}
}

func TestTheWrongBuyerIsACreditAndAReplacementOnACorrectionOrder(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, inv := mixedInvoice(t, pool)
	company := companyWithContact(t, pool)
	req := workshop.RebillRequest{CustomerID: company, Reason: "The company is the payer", Key: "r1"}
	if _, err := workshop.RebillTo(ctx, pool, advisor(), inv.ID, req); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("rebilling an invoice nobody credited = %v, want ErrInvalid", err)
	}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 3}}, Reason: "x", Key: "c0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RebillTo(ctx, pool, advisor(), inv.ID, req); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("rebilling after only a part was credited = %v, want ErrInvalid", err)
	}
	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatal(err)
	}
	stockBefore := countRows(t, pool, `SELECT count(*) FROM stock_movements`)
	repl, err := workshop.RebillTo(ctx, pool, advisor(), inv.ID, req)
	if err != nil {
		t.Fatalf("RebillTo: %v", err)
	}
	if repl.CustomerName != "Andersson Åkeri AB" || repl.GrossMinor != inv.GrossMinor || repl.WorkOrderID == job {
		t.Errorf("replacement = %q gross %d on %q (original job %q)", repl.CustomerName, repl.GrossMinor, repl.WorkOrderID, job)
	}
	doc, _ := workshop.DocumentByID(ctx, pool, advisor(), repl.ID)
	if len(doc.Lines) != 3 || doc.CustomerName != "Andersson Åkeri AB" {
		t.Errorf("replacement document: %d lines, buyer %q", len(doc.Lines), doc.CustomerName)
	}
	// Retried, it is the same replacement; replaced twice, it is refused.
	again, err := workshop.RebillTo(ctx, pool, advisor(), inv.ID, req)
	if err != nil || again.ID != repl.ID {
		t.Errorf("the same form again = %q, %v; want the replacement already made", again.ID, err)
	}
	req.Key = "r2"
	if _, err := workshop.RebillTo(ctx, pool, advisor(), inv.ID, req); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("replacing an invoice twice = %v, want ErrInvalid", err)
	}
	// The original is untouched and still says who it was issued to.
	orig, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if orig.CustomerName == "Andersson Åkeri AB" || orig.GrossMinor != inv.GrossMinor {
		t.Errorf("the original changed: %q", orig.CustomerName)
	}
	// The job is still invoiced once; the new order says what it corrects; no
	// work is counted twice and nothing leaves the shelf again.
	if n := countRows(t, pool, `SELECT count(*) FROM invoices WHERE work_order_id = $1 AND credit_of_id IS NULL`, job); n != 1 {
		t.Errorf("the original order has %d invoices, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_orders WHERE corrects_work_order_id = $1`, job); n != 1 {
		t.Errorf("%d correction orders, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_order_lines WHERE work_order_id = $1`, repl.WorkOrderID); n != 0 {
		t.Errorf("the correction order carries %d lines, which would count the work twice", n)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM stock_movements`); got != stockBefore {
		t.Errorf("rebilling wrote %d stock movements", got-stockBefore)
	}
	// The trail from the original to everything done about it.
	cs, err := workshop.CorrectionsFor(ctx, pool, advisor(), inv.ID)
	if err != nil || len(cs) != 3 || cs[2].Kind != "rebill" || cs[0].Reason == "" || cs[0].By == "" {
		t.Errorf("corrections = %+v, %v", cs, err)
	}
	back, _ := workshop.CorrectionsFor(ctx, pool, advisor(), repl.ID)
	if len(back) != 1 || back[0].Kind != "replaces" {
		t.Errorf("the replacement does not point back: %+v", back)
	}
	// Money: the first is settled at nothing, the second is owed.
	b1, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	b2, _ := workshop.BalanceFor(ctx, pool, advisor(), repl.ID)
	if b1.OutstandingMinor() != 0 || b2.OutstandingMinor() != inv.GrossMinor {
		t.Errorf("balances %d and %d", b1.OutstandingMinor(), b2.OutstandingMinor())
	}
}

func TestOnlyTheCounterCorrectsAnInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	_, inv := mixedInvoice(t, pool)
	req := workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 1}}, Reason: "x"}
	if _, err := workshop.IssueCredit(ctx, pool, technician(), inv.ID, req); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician credited an invoice: %v", err)
	}
	if _, err := workshop.PreviewCredit(ctx, pool, technician(), inv.ID, req); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician previewed a credit: %v", err)
	}
	if _, err := workshop.EligibleLines(ctx, pool, technician(), inv.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the creditable lines: %v", err)
	}
	if _, err := workshop.RebillTo(ctx, pool, technician(), inv.ID, workshop.RebillRequest{CustomerID: fixtureCustomer, Reason: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician rebilled: %v", err)
	}
	if _, err := workshop.CorrectionsFor(ctx, pool, technician(), inv.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the corrections: %v", err)
	}
	el, err := workshop.EligibleLines(ctx, pool, advisor(), inv.ID)
	if err != nil || len(el) != 3 || el[0].RemainingMilli() != 3000 {
		t.Errorf("eligible = %+v, %v", el, err)
	}
}

func TestTheLastPartOfALineTakesTheOreTheEarlierPartsRoundedAway(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{Kind: "part", Description: "Gasket", QuantityMilli: 1000, UnitPriceMinor: 1001, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	// Half of 10,01 is 5,005: rounded up that is 5,01, twice is 10,02.
	for i, q := range []int64{500, 0} {
		if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{
			Lines: []workshop.CreditLine{{Position: 1, QuantityMilli: q}}, Reason: "half each", Key: "h" + string(rune('0'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if net, _ := creditedOf(t, pool, inv.ID); net != -1001 {
		t.Errorf("two halves credited %d net, want exactly 1001", -net)
	}
}
