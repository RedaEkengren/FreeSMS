package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
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

// A salesperson, a buyer and the rental desk have no part in the workshop's
// work (#145). The operations the workshop opens to every role mean every
// role the workshop has: these are refused, in a shop running every module,
// and hand back nothing -- a part's cost included, which a salesperson must
// never be handed by any route. A person who is also a workshop role has
// the workshop through it.
func TestRolesWithNoPartInTheWorkshopAreHandedNothingOfIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	if _, err := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "OF-1", Name: "Oil filter", Unit: "each", CostMinor: price(4500)}); err != nil {
		t.Fatal(err)
	}
	for _, roles := range []access.Roles{
		access.Of(access.RoleSales), access.Of(access.RoleBuyer), access.Of(access.RoleRentalAgent),
		access.Of(access.RoleSales, access.RoleBuyer),
	} {
		s := otherRoles(roles)
		refused := map[string]error{}
		var j workshop.Job
		j, _, refused["JobByID"] = workshop.JobByID(ctx, pool, s, job)
		var open []workshop.Job
		open, refused["OpenJobs"] = workshop.OpenJobs(ctx, pool, s)
		refused["ClockIn"] = workshop.ClockIn(ctx, pool, s, job)
		var tpl []workshop.Template
		tpl, refused["Templates"] = workshop.Templates(ctx, pool, s)
		var part workshop.Part
		part, refused["PartByCode"] = workshop.PartByCode(ctx, pool, s, "OF-1")
		for name, err := range refused {
			if !errors.Is(err, access.ErrForbidden) {
				t.Errorf("%s as %v: %v, want forbidden", name, roles, err)
			}
		}
		if j.ID != "" || len(open) > 0 || len(tpl) > 0 {
			t.Errorf("as %v: refused, and handed a job, jobs or checklists anyway", roles)
		}
		if got := testsupport.Handed(part, "ID", "CostMinor", "PriceMinor"); len(got) > 0 {
			t.Errorf("as %v: handed the part's %v", roles, got)
		}
		if items, err := workshop.WaitingFor(ctx, pool, s); err != nil || len(items) > 0 {
			t.Errorf("waiting for %v: %v, %d workshop items", roles, err, len(items))
		}
	}
	if _, _, err := workshop.JobByID(ctx, pool, otherRoles(access.Of(access.RoleSales, access.RoleTechnician)), job); err != nil {
		t.Errorf("a salesperson who is also a technician cannot open the job: %v", err)
	}
}
