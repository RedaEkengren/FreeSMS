package workshop_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// An invoice has a source (#146). These tests issue the sources there are
// operations for -- the workshop's job and a sale entered by hand -- and
// write a vehicle sale's and a rental's as rows, since their modules do not
// issue yet, and hold every one to the same machine.

const customerA = "44444444-4444-4444-4444-444444444444"

func manual(t *testing.T, pool *pgxpool.Pool) workshop.Invoice {
	t.Helper()
	inv, err := workshop.IssueManual(context.Background(), pool, advisor(), workshop.ManualInvoice{
		CustomerID: customerA,
		Lines: []workshop.SourcedLine{
			{Description: "Vinterhjul, fyra", QuantityMilli: 1000, UnitPriceMinor: 480000, VATRateBasis: 2500},
			{Kind: "labour", Description: "Montering", QuantityMilli: 500, UnitPriceMinor: 80000, VATRateBasis: 2500},
		},
	})
	if err != nil {
		t.Fatalf("IssueManual: %v", err)
	}
	return inv
}

// sourceRow is what the database holds about where an invoice came from.
func sourceRow(t *testing.T, pool *pgxpool.Pool, id string) (source, job, customer string) {
	t.Helper()
	if err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT source, coalesce(work_order_id::text, ''), coalesce(customer_id::text, '')
			FROM invoices WHERE id = $1`, id).Scan(&source, &job, &customer)
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// A sale entered by hand is invoiced by whoever may invoice, from the same
// series, with its lines frozen as typed and its arithmetic done once.
func TestASaleEnteredByHandIsInvoicedFromTheSameSeries(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	first, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	inv := manual(t, pool)
	if inv.Series != first.Series || inv.Number != first.Number+1 || inv.Source != workshop.SourceManual {
		t.Errorf("the manual invoice is %s (%s), after %s; want the next number in the same series", inv.Reference(), inv.Source, first.Reference())
	}
	// 4 800 + 0,5 x 800 = 5 200 net, 1 300 VAT.
	if inv.NetMinor != 520000 || inv.VATMinor != 130000 || inv.GrossMinor != 650000 {
		t.Errorf("totals %d + %d = %d", inv.NetMinor, inv.VATMinor, inv.GrossMinor)
	}
	if source, job, customer := sourceRow(t, pool, inv.ID); source != "manual" || job != "" || customer != customerA {
		t.Errorf("stored as %s, job %q, customer %q", source, job, customer)
	}
	doc, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil || doc.Source != workshop.SourceManual || len(doc.Lines) != 2 || doc.CustomerName != "A Customer" {
		t.Errorf("the document reads %+v, %v", doc.Invoice, err)
	}
	if workSource, _, _ := sourceRow(t, pool, first.ID); workSource != "workshop" {
		t.Errorf("the workshop's invoice is stored as %s", workSource)
	}

	// Paid like any other, and listed with what is owed.
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: inv.IssuedAt}); err != nil {
		t.Errorf("paying the manual invoice: %v", err)
	}
	if _, err := workshop.Worklist(ctx, pool, advisor(), time.Now()); err != nil {
		t.Errorf("the worklist with a manual invoice in it: %v", err)
	}
	// A message is a job's until the outbox (#156): refused, and said why.
	if _, err := workshop.SendDocument(ctx, pool, advisor(), inv.ID, workshop.SendInput{Channel: "email",
		Address: "kund@example.test", RecipientName: "A Customer", Confirmed: true, Body: "{link}", LinkBase: "http://x"}); !errors.Is(err, workshop.ErrNotAJobsDocument) {
		t.Errorf("sending a manual invoice as a message: %v, want ErrNotAJobsDocument", err)
	}

	// Who may: the counter and the owner; refused for the others, with
	// nothing written.
	for _, s := range []access.Scope{technician(), partsDeskScope(), otherRoles(access.Of(access.RoleSales))} {
		if _, err := workshop.IssueManual(ctx, pool, s, workshop.ManualInvoice{CustomerID: customerA,
			Lines: []workshop.SourcedLine{{Description: "x", QuantityMilli: 1000, UnitPriceMinor: 100, VATRateBasis: 2500}}}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("IssueManual as %v: %v, want forbidden", s.Role, err)
		}
	}
	// Core: a shop that runs no module still invoices by hand.
	if _, err := workshop.IssueManual(ctx, pool, runningNothing(owner()), workshop.ManualInvoice{CustomerID: customerA,
		Lines: []workshop.SourcedLine{{Description: "Hyra av lokal", QuantityMilli: 1000, UnitPriceMinor: 100000, VATRateBasis: 2500}}}); err != nil {
		t.Errorf("a manual invoice with every module off: %v", err)
	}
	for name, lines := range map[string][]workshop.SourcedLine{
		"no lines":               nil,
		"no words":               {{QuantityMilli: 1000, UnitPriceMinor: 100, VATRateBasis: 2500}},
		"nothing":                {{Description: "x", QuantityMilli: 0, UnitPriceMinor: 100, VATRateBasis: 2500}},
		"a kind with no account": {{Kind: "goods", Description: "x", QuantityMilli: 1000, UnitPriceMinor: 100, VATRateBasis: 2500}},
	} {
		if _, err := workshop.IssueManual(ctx, pool, advisor(), workshop.ManualInvoice{CustomerID: customerA, Lines: lines}); err == nil {
			t.Errorf("a manual invoice with %s was issued", name)
		}
	}
}

// Whatever its source, an issued invoice is never changed or deleted, and a
// credit note keeps the source of what it credits -- the same job, sale or
// rental, the same customer -- by trigger, not by the code that writes it.
func TestEverySourcesInvoiceIsFrozenAndItsCreditNoteKeepsItsSource(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	work, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	hand := manual(t, pool)
	// A sale and a rental as their modules will write them.
	ids := map[string]string{"workshop": work.ID, "manual": hand.ID}
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for source, ref := range map[string]string{"vehicle_sale": "vehicle_sale_id", "rental": "rental_id"} {
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO invoices (shop_id, series, number, source, `+ref+`, customer_id, customer_name, net_minor, vat_minor, gross_minor)
				VALUES ($1, 'T', (SELECT coalesce(max(number), 0) + 1 FROM invoices WHERE series = 'T'), $2, gen_random_uuid(), $3,
				        'A Customer', 1000, 250, 1250) RETURNING id`, shopID, source, customerA).Scan(&id); err != nil {
				return err
			}
			ids[source] = id
		}
		return nil
	}); err != nil {
		t.Fatalf("writing a sale's and a rental's invoice: %v", err)
	}

	for source, id := range ids {
		for _, stmt := range []string{
			`UPDATE invoices SET customer_name = 'Someone else' WHERE id = $1`,
			`UPDATE invoices SET source = 'manual' WHERE id = $1`,
			`DELETE FROM invoices WHERE id = $1`,
		} {
			err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt, id)
				return err
			})
			if err == nil {
				t.Errorf("%s invoice: %q was allowed", source, stmt)
			}
		}
		// A credit note of some other source is refused by the database.
		other := `'manual', NULL`
		if source == "manual" {
			other = `'rental', gen_random_uuid()`
		}
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO invoices (shop_id, series, number, source, rental_id, customer_id, credit_of_id, customer_name, net_minor, vat_minor, gross_minor)
				VALUES ($1, 'X', 1, `+other+`, $2, $3, 'A Customer', -1, 0, -1)`, shopID, customerA, id)
			return err
		})
		if err == nil {
			t.Errorf("a credit note of another source than its %s invoice was written", source)
		}
	}

	// Credited through the code: the note says what the invoice said.
	for source, id := range map[string]string{"workshop": work.ID, "manual": hand.ID} {
		note, err := workshop.CreditNote(ctx, pool, advisor(), id)
		if err != nil {
			t.Fatalf("crediting the %s invoice: %v", source, err)
		}
		s, job, cust := sourceRow(t, pool, note.ID)
		os, ojob, ocust := sourceRow(t, pool, id)
		if s != os || job != ojob || cust != ocust {
			t.Errorf("the %s credit note is %s/%s/%s, its invoice %s/%s/%s", source, s, job, cust, os, ojob, ocust)
		}
	}
}

// A rebilled manual invoice keeps its source and names the new customer; a
// workshop one still goes through a correction job. The chain is the same.
func TestARebillKeepsItsSourceAndItsChain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	other, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "company", Name: "Bolaget AB"})
	if err != nil {
		t.Fatal(err)
	}
	hand := manual(t, pool)
	if _, err := workshop.CreditNote(ctx, pool, advisor(), hand.ID); err != nil {
		t.Fatal(err)
	}
	again, err := workshop.RebillTo(ctx, pool, advisor(), hand.ID, workshop.RebillRequest{CustomerID: other, Reason: "Fel köpare"})
	if err != nil {
		t.Fatalf("RebillTo: %v", err)
	}
	if s, job, cust := sourceRow(t, pool, again.ID); s != "manual" || job != "" || cust != other {
		t.Errorf("the replacement is %s, job %q, customer %q; want manual, no job, the new customer", s, job, cust)
	}
	corr, err := workshop.CorrectionsFor(ctx, pool, advisor(), hand.ID)
	if err != nil || len(corr) < 2 {
		t.Errorf("the chain from the original: %+v, %v", corr, err)
	}
}

// One series for every source: two people issuing at the same moment, one
// for a job and one by hand, never get the same number and never leave one
// out.
func TestConcurrentIssuesInDifferentSourcesShareTheSeriesWithoutAGap(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	const each = 4
	var jobs []string
	for i := 0; i < each; i++ {
		jobs = append(jobs, readyToInvoice(t, pool))
	}
	var mu sync.Mutex
	var numbers []int64
	var wg sync.WaitGroup
	for i := 0; i < each; i++ {
		wg.Add(2)
		go func(job string) {
			defer wg.Done()
			inv, err := workshop.Issue(ctx, pool, advisor(), job)
			if err != nil {
				t.Errorf("Issue: %v", err)
				return
			}
			mu.Lock()
			numbers = append(numbers, inv.Number)
			mu.Unlock()
		}(jobs[i])
		go func() {
			defer wg.Done()
			inv, err := workshop.IssueManual(ctx, pool, advisor(), workshop.ManualInvoice{CustomerID: customerA,
				Lines: []workshop.SourcedLine{{Description: "x", QuantityMilli: 1000, UnitPriceMinor: 100, VATRateBasis: 2500}}})
			if err != nil {
				t.Errorf("IssueManual: %v", err)
				return
			}
			mu.Lock()
			numbers = append(numbers, inv.Number)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	for i := range numbers {
		if i > 0 && numbers[i] != numbers[i-1]+1 {
			t.Fatalf("numbers %v share or skip one", numbers)
		}
	}
	if len(numbers) != 2*each {
		t.Errorf("%d invoices, want %d", len(numbers), 2*each)
	}
}
