package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The coming season is charged on the change job as a line, once. The same set
// for the same period is refused, whether the first charge was made on a job or
// on an invoice, and another year is a new period.
func TestAStorageFeeIsChargedOnTheChangeJobOncePerPeriod(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 10)
	winter := aStoredSet(t, pool, customerA, vehicleA, workshop.SeasonWinterFriction, place)
	fee, err := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{
		Name: "Winter storage", Per: "set", LineKind: "fee", UnitPriceMinor: 40000, VATRateBasis: 2500, StoragePeriod: "season"})
	if err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, winter, fee, 2027); err != nil {
		t.Fatal(err)
	}
	_, lines, err := workshop.JobByID(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	var got *workshop.Line
	for i := range lines {
		if lines[i].Kind == "fee" {
			got = &lines[i]
		}
	}
	if got == nil || got.UnitPriceMinor != 40000 || got.QuantityMilli != 1000 || got.VATRateBasis != 2500 ||
		got.Description != "Winter storage – Michelin Primacy 4, 2027-winter" {
		t.Fatalf("the line: %+v", got)
	}
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, winter, fee, 2027); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same period twice: %v", err)
	}
	if _, err := workshop.InvoiceStorage(ctx, pool, advisor(), customerA, fee, 2027); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("invoicing a period the job charged: %v", err)
	}
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, winter, fee, 2028); err != nil {
		t.Errorf("the next year: %v", err)
	}
}

// A customer with two sets gets one invoice with a line for each, at the VAT
// of the price list, and it is an unpaid invoice like any other. A second run,
// and two at once, invoice nothing again.
func TestAYearlyStorageInvoiceHasOneLinePerSetAndIsOnlyIssuedOnce(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 10)
	aStoredSet(t, pool, customerA, vehicleA, workshop.SeasonSummer, place)
	aStoredSet(t, pool, customerA, anotherCar(t, pool, customerA), workshop.SeasonWinterFriction, place)
	other := aContact(t, pool, "Bea", "+46702222222", "")
	aStoredSet(t, pool, other, "", workshop.SeasonSummer, place)
	fee, err := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{
		Name: "Storage", Per: "set", LineKind: "fee", UnitPriceMinor: 50000, VATRateBasis: 2500, StoragePeriod: "year"})
	if err != nil {
		t.Fatal(err)
	}
	due, err := workshop.StorageDueFor(ctx, pool, advisor(), fee, 2027)
	if err != nil || len(due) != 2 {
		t.Fatalf("who is due: %+v, %v", due, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var issued []workshop.Invoice
	nothing := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv, err := workshop.InvoiceStorage(ctx, pool, advisor(), customerA, fee, 2027)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				issued = append(issued, inv)
			case errors.Is(err, workshop.ErrNothingToInvoice):
				nothing++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(issued) != 1 || nothing != 1 {
		t.Fatalf("two at once issued %d invoices and refused %d", len(issued), nothing)
	}
	inv := issued[0]
	if inv.NetMinor != 100000 || inv.VATMinor != 25000 || inv.GrossMinor != 125000 {
		t.Errorf("two sets at 500 plus 25 per cent: %d net, %d VAT, %d gross", inv.NetMinor, inv.VATMinor, inv.GrossMinor)
	}
	var lines int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoice_lines WHERE invoice_id = $1`, inv.ID).Scan(&lines); err != nil || lines != 2 {
		t.Errorf("lines: %d, %v", lines, err)
	}
	// It is owed like any invoice, so the follow-up finds it.
	owed, err := workshop.Receivables(ctx, pool, advisor())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range owed {
		if b.InvoiceID == inv.ID && b.OutstandingMinor() == 125000 {
			found = true
		}
	}
	if !found {
		t.Errorf("an unpaid storage invoice is not among the receivables: %+v", owed)
	}
	// Bea is still due, and customerA no longer is.
	due, _ = workshop.StorageDueFor(ctx, pool, advisor(), fee, 2027)
	if len(due) != 1 || due[0].CustomerID != other {
		t.Errorf("who is due after: %+v", due)
	}
	// A set taken out in the middle of the period stays charged: no refund.
	sets, _ := workshop.TyreSetsOf(ctx, pool, advisor(), workshop.TyreFilter{CustomerID: customerA})
	if err := workshop.TakeTyreSetOutOfStorage(ctx, pool, technician(), sets[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.InvoiceStorage(ctx, pool, advisor(), customerA, fee, 2027); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("a set taken out was charged again: %v", err)
	}
}

// A fee charged per tyre is charged for the tyres standing in the set, and a
// storage fee cannot be added to a job as a plain extra, which would charge it
// outside the period's record.
func TestAFeePerTyreCountsTheTyresAndAFeeIsNotAnOrdinaryExtra(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 10)
	addTechnician(t, pool)
	set := aStoredSet(t, pool, customerA, vehicleA, workshop.SeasonSummer, place)
	fee, err := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{
		Name: "Per tyre", Per: "tyre", LineKind: "fee", UnitPriceMinor: 10000, VATRateBasis: 2500, StoragePeriod: "year"})
	if err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)
	if err := workshop.AddTyreExtraToJob(ctx, pool, advisor(), job, fee, 1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a storage fee added as an extra: %v", err)
	}
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, set, fee, 2027); err != nil {
		t.Fatal(err)
	}
	_, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	ok := false
	for _, l := range lines {
		if l.Kind == "fee" && l.QuantityMilli == 4000 && l.UnitPriceMinor == 10000 {
			ok = true
		}
	}
	if !ok {
		t.Errorf("four tyres at 100: %+v", lines)
	}
	plain, _ := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{Name: "Balancing", Per: "tyre", LineKind: "labour", UnitPriceMinor: 1, VATRateBasis: 2500})
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, set, plain, 2027); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an ordinary extra charged as storage: %v", err)
	}
}

func TestStorageIsChargedToTheOwnerOfAStoredSetByWhoMay(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 10)
	other := aContact(t, pool, "Bea", "+46702222222", "")
	foreign := aStoredSet(t, pool, other, "", workshop.SeasonSummer, place)
	loose, _ := workshop.CreateTyreSet(ctx, pool, advisor(), summerSet(customerA, ""))
	fee, _ := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{Name: "Storage", Per: "set", LineKind: "fee", UnitPriceMinor: 100, VATRateBasis: 2500, StoragePeriod: "year"})
	job := newJob(t, pool)
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, foreign, fee, 2027); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("another customer's set charged on this job: %v", err)
	}
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, loose, fee, 2027); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set not in storage charged for storing: %v", err)
	}
	if err := workshop.ChargeStorageOnJob(ctx, pool, advisor(), job, loose, fee, 1999); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a year long gone: %v", err)
	}
	if err := workshop.ChargeStorageOnJob(ctx, pool, technician(), job, loose, fee, 2027); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician charged storage: %v", err)
	}
	if _, err := workshop.InvoiceStorage(ctx, pool, technician(), customerA, fee, 2027); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician invoiced storage: %v", err)
	}
	if err := workshop.SetStorageBilling(ctx, pool, advisor(), "invoice"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("an advisor chose the billing: %v", err)
	}
	if got, _ := workshop.StorageBillingOf(ctx, pool, advisor()); got != "job" {
		t.Errorf("the default billing: %q", got)
	}
	if err := workshop.SetStorageBilling(ctx, pool, owner(), "invoice"); err != nil {
		t.Fatal(err)
	}
	if got, _ := workshop.StorageBillingOf(ctx, pool, advisor()); got != "invoice" {
		t.Errorf("the chosen billing: %q", got)
	}
	if err := workshop.SetStorageBilling(ctx, pool, owner(), "yearly"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a billing that does not exist: %v", err)
	}
}
