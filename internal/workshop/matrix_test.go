package workshop_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

const customerPerson = "22222222-2222-2222-2222-222222222223"

// Who may call what, by calling it: the business operations that price,
// read documents, keep the time library and the stock prices, and handle a
// person's data, each called directly -- no handler in front -- as every
// role. A refused call returns access.ErrForbidden, hands back nothing, and
// changes nothing.
func TestTheRoleMatrix(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	job := newJob(t, pool)
	invoiced := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), invoiced)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "OF-1", Name: "Oil filter", Unit: "each", CostMinor: price(4500)})
	now := time.Now()

	roles := map[string]access.Scope{
		"technician": technician(), "parts": partsDeskScope(), "front desk": advisor(), "owner": owner(),
	}
	frontDesk := []string{"front desk", "owner"}
	stock := []string{"parts", "front desk", "owner"}

	// Each call reports what it handed back, so a refusal can be checked to
	// have handed back nothing.
	type call func(access.Scope) (payload int, err error)
	n := func(k int, err error) (int, error) { return k, err }
	ops := []struct {
		name    string
		allowed []string
		do      call
	}{
		// Pricing.
		{"AddLine", frontDesk, func(s access.Scope) (int, error) {
			return 0, workshop.AddLine(ctx, pool, s, job, workshop.NewLine{Kind: "labour", Description: "x",
				QuantityMilli: 1000, UnitPriceMinor: 100, VATRateBasis: 2500, CostBearer: "customer"})
		}},
		{"LabourTimes", frontDesk, func(s access.Scope) (int, error) { v, err := workshop.LabourTimes(ctx, pool, s); return n(len(v), err) }},
		{"LabourRate", frontDesk, func(s access.Scope) (int, error) { v, err := workshop.LabourRate(ctx, pool, s); return n(int(v), err) }},
		{"SuggestFor", frontDesk, func(s access.Scope) (int, error) {
			v, err := workshop.SuggestFor(ctx, pool, s, vehicleA)
			return n(len(v), err)
		}},
		{"SetLabourRate", frontDesk, func(s access.Scope) (int, error) { return 0, workshop.SetLabourRate(ctx, pool, s, 95000) }},
		{"SavePriceBand", frontDesk, func(s access.Scope) (int, error) { return 0, workshop.SavePriceBand(ctx, pool, s, nil, 4500) }},
		{"PriceFromCost", stock, func(s access.Scope) (int, error) {
			v, err := workshop.PriceFromCost(ctx, pool, s, 4500)
			return n(int(v), err)
		}},
		{"PartByCode", stock, func(s access.Scope) (int, error) {
			v, err := workshop.PartByCode(ctx, pool, s, "OF-1")
			if v.ID != "" {
				return 1, err
			}
			return 0, err
		}},

		// Documents.
		{"InvoicesFor", frontDesk, func(s access.Scope) (int, error) {
			v, err := workshop.InvoicesFor(ctx, pool, s, invoiced)
			return n(len(v), err)
		}},
		{"DocumentByID", frontDesk, func(s access.Scope) (int, error) {
			v, err := workshop.DocumentByID(ctx, pool, s, inv.ID)
			return n(len(v.Lines), err)
		}},

		// A person's data, and the accounts.
		{"ExportPerson", frontDesk, func(s access.Scope) (int, error) {
			v, err := workshop.ExportPerson(ctx, pool, s, customerPerson)
			if v.Person.Name != "" {
				return 1, err
			}
			return 0, err
		}},
		{"Erasures", frontDesk, func(s access.Scope) (int, error) { v, err := workshop.Erasures(ctx, pool, s); return n(len(v), err) }},
		{"ExportAccounting", frontDesk, func(s access.Scope) (int, error) {
			v, _, err := workshop.ExportAccounting(ctx, pool, s, now.AddDate(0, -1, 0), now.Add(time.Hour), true)
			return n(len(v), err)
		}},
		{"Summary", frontDesk, func(s access.Scope) (int, error) {
			v, err := workshop.Summary(ctx, pool, s, now.AddDate(0, -1, 0), now.Add(time.Hour))
			return n(len(v.Technicians)+int(v.HoursBilled), err)
		}},
	}

	for _, op := range ops {
		allowed := map[string]bool{}
		for _, r := range op.allowed {
			allowed[r] = true
		}
		names := make([]string, 0, len(roles))
		for r := range roles {
			names = append(names, r)
		}
		sort.Strings(names)
		for _, role := range names {
			payload, err := op.do(roles[role])
			switch {
			case allowed[role] && errors.Is(err, access.ErrForbidden):
				t.Errorf("%s as %s: refused, and this role may", op.name, role)
			case !allowed[role] && !errors.Is(err, access.ErrForbidden):
				t.Errorf("%s as %s: %v, want forbidden", op.name, role, err)
			case !allowed[role] && payload != 0:
				t.Errorf("%s as %s: refused, and handed back something anyway", op.name, role)
			}
		}
	}

	// The refused writes changed nothing. The allowed ones above added one
	// line each for the front desk and the owner, and nothing else.
	_, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	if len(lines) != 2 {
		t.Errorf("the job has %d lines, want the 2 the allowed roles added", len(lines))
	}
}

// Erasing a person is the front desk's; any other role is refused and the
// person is untouched.
func TestOnlyTheFrontDeskErasesAPerson(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	for _, s := range []access.Scope{technician(), partsDeskScope()} {
		if err := workshop.ErasePerson(ctx, pool, s, customerPerson, "asked"); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("ErasePerson as %s: %v, want forbidden", s.Role, err)
		}
	}
	v, err := workshop.ExportPerson(ctx, pool, advisor(), customerPerson)
	if err != nil || !strings.Contains(v.Person.Name, "Customer") {
		t.Errorf("a refused erasure touched the person: %+v, %v", v.Person, err)
	}
}
