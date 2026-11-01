package workshop_test

import (
	"context"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Field-level rules are asserted on what the operation hands each role, not
// on the page that renders it (#144): a template that stops printing a field
// is not a permission, a value that arrives without it is.
func TestWhatEachRoleIsHandedIsReadFromTheValue(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	// The job a technician works from has no customer on it at all: not
	// left empty, not there. Whether it has one is not personal data.
	if fields := testsupport.FieldsLike(workshop.Job{}, `(?i)^customer(name|id)?$|name$|phone|email|address`); len(fields) > 0 {
		t.Errorf("a job carries %v; the technician's payload is the vehicle and the work", fields)
	}

	// Outsourced work: where it is, for everybody; the money, for the
	// counter and the parts desk only.
	job, line := turboJob(t, pool)
	id := sendOut(t, pool, job, line, time.Now().AddDate(0, 0, 2))
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "sent"})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "cost", AmountMinor: pointerTo(int64(470000))})
	record(t, pool, id, workshop.OutsourcedEventInput{Kind: "invoice", InvoiceRef: "F-1"})
	money := []string{"MoneyVisible", "CostMinor", "InvoiceRef"}
	desk, err := workshop.OutsourcedFor(ctx, pool, advisor(), job)
	if err != nil || len(desk) != 1 {
		t.Fatalf("OutsourcedFor as the counter: %v", err)
	}
	if got := testsupport.Handed(desk[0], money...); len(got) != len(money) {
		t.Errorf("the counter was handed %v of %v; the fixture has stopped setting them", got, money)
	}
	floor, err := workshop.OutsourcedFor(ctx, pool, technician(), job)
	if err != nil || len(floor) != 1 {
		t.Fatalf("OutsourcedFor as a technician: %v", err)
	}
	if got := testsupport.Handed(floor[0], append(money, "EstimateMinor", "InvoiceMinor", "ChargeMinor", "Reconciled")...); len(got) > 0 {
		t.Errorf("the technician was handed %v", got)
	}
}
