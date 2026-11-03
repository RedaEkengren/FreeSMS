package workshop_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// TestTheRoleMatrix calls the workshop's and Core's operations as every
// role. This does the same for car sales and rental (#144), in a shop that
// runs every module, so that a refusal is the role's and not the module's:
// whoever sells reads leads and deals, whoever buys reads what a car cost,
// the rental desk reads rentals, the owner reads all of it, and the
// workshop's roles none. A refused call returns access.ErrForbidden and
// hands back nothing.
func TestTheDealerAndRentalRoleMatrix(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	unit := eligibleCar(t, pool)
	n := walkIn("Kim Köpare", "070-123 45 67")
	n.StockUnitID, n.Key = unit, "matrix-1"
	lead, err := workshop.CreateLead(ctx, pool, anna, n)
	if err != nil {
		t.Fatal(err)
	}
	deal := completedDealOn(t, pool, unit, workshop.TaxMarginScheme)
	van := rentableVanWith(t, pool, "VAN 01")
	booking, err := van.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}

	roles := map[string]access.Scope{
		"technician":  everything(technician()),
		"parts":       everything(partsDeskScope()),
		"front desk":  everything(advisor()),
		"owner":       everything(owner()),
		"sales":       sales(),
		"buyer":       buyerScope(),
		"rental desk": rentalDesk(),
	}
	selling := []string{"sales", "owner"}
	buying := []string{"buyer", "owner"}
	cars := []string{"sales", "buyer", "owner"}
	renting := []string{"rental desk", "owner"}
	one := func(id string, err error) (int, error) {
		if id != "" {
			return 1, err
		}
		return 0, err
	}

	ops := []struct {
		name    string
		allowed []string
		do      func(access.Scope) (int, error)
	}{
		{"Leads", selling, func(s access.Scope) (int, error) { v, err := workshop.Leads(ctx, pool, s, false); return len(v), err }},
		{"LeadByID", selling, func(s access.Scope) (int, error) {
			v, err := workshop.LeadByID(ctx, pool, s, lead.ID)
			return one(v.ID, err)
		}},
		{"Deals", selling, func(s access.Scope) (int, error) { v, err := workshop.Deals(ctx, pool, s, true); return len(v), err }},
		{"DealByID", selling, func(s access.Scope) (int, error) {
			v, err := workshop.DealByID(ctx, pool, s, deal.ID)
			return one(v.ID, err)
		}},
		{"StockUnitByID", cars, func(s access.Scope) (int, error) {
			v, err := workshop.StockUnitByID(ctx, pool, s, unit)
			return one(v.ID, err)
		}},
		{"VATEligibilityOfUnit", cars, func(s access.Scope) (int, error) {
			v, err := workshop.VATEligibilityOfUnit(ctx, pool, s, unit)
			return one(v.ID, err)
		}},
		{"PurchaseOfUnit", buying, func(s access.Scope) (int, error) {
			v, err := workshop.PurchaseOfUnit(ctx, pool, s, unit)
			return one(v.ID, err)
		}},
		{"SaleTaxOfDeal", buying, func(s access.Scope) (int, error) {
			v, err := workshop.SaleTaxOfDeal(ctx, pool, s, deal.ID)
			return int(v.VATMinor), err
		}},
		{"RentalBookingByID", renting, func(s access.Scope) (int, error) {
			v, err := workshop.RentalBookingByID(ctx, pool, s, booking)
			return one(v.ID, err)
		}},
		{"RentalBookings", renting, func(s access.Scope) (int, error) {
			v, err := workshop.RentalBookings(ctx, pool, s, in2031(1, 0), in2031(28, 0))
			return len(v), err
		}},
		{"QuoteRental", renting, func(s access.Scope) (int, error) {
			v, err := workshop.QuoteRental(ctx, pool, s, workshop.QuoteRequest{UnitID: van.unit, From: in2031(20, 9), To: in2031(21, 9)})
			return len(v.Lines), err
		}},
		{"RentalRegister", []string{"owner"}, func(s access.Scope) (int, error) {
			v, err := workshop.RentalRegister(ctx, pool, s, workshop.RegisterFilter{From: time.Now().AddDate(0, 0, -1), To: in2031(31, 0)}, "Tillsyn")
			return len(v), err
		}},
	}

	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, op := range ops {
		allowed := map[string]bool{}
		for _, r := range op.allowed {
			allowed[r] = true
		}
		for _, role := range names {
			payload, err := op.do(roles[role])
			switch {
			case allowed[role] && err != nil:
				t.Errorf("%s as %s: %v, and this role may", op.name, role, err)
			case !allowed[role] && !errors.Is(err, access.ErrForbidden):
				t.Errorf("%s as %s: %v, want forbidden", op.name, role, err)
			case !allowed[role] && payload != 0:
				t.Errorf("%s as %s: refused, and handed back something anyway", op.name, role)
			}
		}
	}
}

// A lead recorded and a rental booked tell every open home page and queue
// that something changed ('actions', #157, #158), as the workshop's and
// Core's changes do: another screen hears it without a reload.
func TestCarSalesAndRentalTellTheQueueTheyChanged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN freesms_changes`); err != nil {
		t.Fatal(err)
	}
	heard := func() bool {
		t.Helper()
		got := false
		for {
			wait, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			n, err := conn.Conn().WaitForNotification(wait)
			cancel()
			if err != nil {
				return got
			}
			got = got || n.Payload == shopID+" actions"
		}
	}
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	van := rentableVanWith(t, pool, "VAN 01")
	heard()
	n := walkIn("Kim Köpare", "070-123 45 67")
	n.Wishes, n.Key = "En kombi", "live-1"
	if _, err := workshop.CreateLead(ctx, pool, anna, n); err != nil {
		t.Fatal(err)
	}
	if !heard() {
		t.Error("a lead recorded, and the queue heard nothing")
	}
	if _, err := van.book(pool, in2031(10, 9), in2031(12, 9)); err != nil {
		t.Fatal(err)
	}
	if !heard() {
		t.Error("a rental booked, and the queue heard nothing")
	}
}
