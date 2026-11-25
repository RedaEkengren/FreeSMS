package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// An extra goes onto a job as a line at the price it has that day. Changing
// the price afterwards leaves the line as it was, and a new line takes the new
// price: what the customer was quoted is not rewritten by the price list.
func TestAnExtraOnAJobKeepsThePriceItWasAddedAt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	balancing, err := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{
		Name: "Balancing", Per: "tyre", LineKind: "labour", UnitPriceMinor: 15000, VATRateBasis: 2500})
	if err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)
	if err := workshop.AddTyreExtraToJob(ctx, pool, advisor(), job, balancing, 4); err != nil {
		t.Fatal(err)
	}
	if err := workshop.PriceTyreExtra(ctx, pool, owner(), balancing, 20000, 2500); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AddTyreExtraToJob(ctx, pool, advisor(), job, balancing, 1); err != nil {
		t.Fatal(err)
	}
	_, lines, err := workshop.JobByID(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	var got []workshop.Line
	for _, l := range lines {
		if l.Description == "Balancing" {
			got = append(got, l)
		}
	}
	if len(got) != 2 || got[0].QuantityMilli != 4000 || got[0].UnitPriceMinor != 15000 || got[0].Kind != "labour" ||
		got[1].QuantityMilli != 1000 || got[1].UnitPriceMinor != 20000 || got[1].VATRateBasis != 2500 {
		t.Fatalf("the lines: %+v", got)
	}
}

func TestARetiredExtraIsNotAddedAndCanBeOfferedAgain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	valves, err := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{
		Name: "New valves", Per: "set", LineKind: "part", UnitPriceMinor: 12000, VATRateBasis: 2500})
	if err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)
	if err := workshop.SetTyreExtraInUse(ctx, pool, owner(), valves, false); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AddTyreExtraToJob(ctx, pool, advisor(), job, valves, 1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a retired extra was added: %v", err)
	}
	if err := workshop.SetTyreExtraInUse(ctx, pool, owner(), valves, true); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AddTyreExtraToJob(ctx, pool, advisor(), job, valves, 1); err != nil {
		t.Errorf("an extra offered again: %v", err)
	}
	list, err := workshop.TyreExtras(ctx, pool, advisor())
	if err != nil || len(list) != 1 || !list[0].Active || list[0].UnitPriceMinor != 12000 {
		t.Errorf("the list: %+v, %v", list, err)
	}
}

func TestTheExtrasPriceListRefusesWhatCannotBeRight(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	good := workshop.NewTyreExtra{Name: "Washing", Per: "set", LineKind: "labour", UnitPriceMinor: 10000, VATRateBasis: 2500}
	if _, err := workshop.CreateTyreExtra(ctx, pool, owner(), good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(n *workshop.NewTyreExtra){
		"the same name twice":    func(n *workshop.NewTyreExtra) {},
		"no name":                func(n *workshop.NewTyreExtra) { n.Name = "  " },
		"neither tyre nor set":   func(n *workshop.NewTyreExtra) { n.Name, n.Per = "A", "car" },
		"a line kind nobody has": func(n *workshop.NewTyreExtra) { n.Name, n.LineKind = "B", "gift" },
		"a negative price":       func(n *workshop.NewTyreExtra) { n.Name, n.UnitPriceMinor = "C", -1 },
		"a VAT rate over 100":    func(n *workshop.NewTyreExtra) { n.Name, n.VATRateBasis = "D", 10001 },
	} {
		n := good
		mutate(&n)
		if _, err := workshop.CreateTyreExtra(ctx, pool, owner(), n); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	job := newJob(t, pool)
	list, _ := workshop.TyreExtras(ctx, pool, owner())
	for _, count := range []int{0, -1, 101} {
		if err := workshop.AddTyreExtraToJob(ctx, pool, advisor(), job, list[0].ID, count); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("count %d: %v", count, err)
		}
	}
}

// The price list is the shop's prices and a conversation with the customer:
// the owner sets it, the front desk uses it, and a technician sees none of it.
func TestWhoMayPriceAndUseTheExtras(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	n := workshop.NewTyreExtra{Name: "Bolts", Per: "set", LineKind: "part", UnitPriceMinor: 5000, VATRateBasis: 2500}
	for _, who := range []struct {
		name  string
		scope access.Scope
	}{{"advisor", advisor()}, {"technician", technician()}} {
		if _, err := workshop.CreateTyreExtra(ctx, pool, who.scope, n); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s created an extra: %v", who.name, err)
		}
	}
	id, err := workshop.CreateTyreExtra(ctx, pool, owner(), n)
	if err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)
	if err := workshop.PriceTyreExtra(ctx, pool, advisor(), id, 1, 2500); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("an advisor priced an extra: %v", err)
	}
	if err := workshop.SetTyreExtraInUse(ctx, pool, technician(), id, false); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician retired an extra: %v", err)
	}
	if _, err := workshop.TyreExtras(ctx, pool, technician()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the price list: %v", err)
	}
	if err := workshop.AddTyreExtraToJob(ctx, pool, technician(), job, id, 1); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician priced a job: %v", err)
	}
}
