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

// Consignment (#166).

func consignmentTerms(model string) workshop.ConsignmentTerms {
	minimum, basis := int64(10_000_000), 800
	return workshop.ConsignmentTerms{Model: model, MinimumPriceMinor: &minimum, CommissionBasis: &basis,
		PrincipalCosts: "Tvätt och besiktning", InsuredBy: "principal", SellerType: workshop.SellerPrivate,
		StartsOn: time.Now(), EndsOn: time.Now().AddDate(0, 2, 0), Terms: "Bilen visas på vår gård"}
}

func consign(t *testing.T, pool *pgxpool.Pool, model, key string) workshop.Consignment {
	t.Helper()
	c, err := workshop.ConsignCar(context.Background(), pool, buyerScope(), workshop.NewConsignment{VehicleID: vehicleA,
		NewPrincipal:     &workshop.NewParty{Kind: "private", Name: "Pia Principal", Phone: "070-555 66 77"},
		ConsignmentTerms: consignmentTerms(model), Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A car taken in on consignment is a stock unit the dealer does not own:
// on the stock list, marked, and out of the stock value. Replayed, the
// same request makes nothing more; a salesperson reads in whose name it is
// sold and may not take one in.
func TestAConsignedCarIsInStockAndNotTheDealers(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := consign(t, pool, workshop.ConsignOwnName, "consign-1")
	if again := consign(t, pool, workshop.ConsignOwnName, "consign-1"); again.ID != c.ID {
		t.Errorf("the same request again made %s, want %s", again.ID, c.ID)
	}
	if c.Version != 1 || c.RootID != c.ID || c.PrincipalName != "Pia Principal" || c.Model != workshop.ConsignOwnName {
		t.Errorf("the agreement: %+v", c)
	}
	u, err := workshop.StockUnitByID(ctx, pool, sales(), c.StockUnitID)
	if err != nil || u.Owned() || u.Reason != workshop.StockFromConsignment {
		t.Errorf("the consigned car: %+v, %v", u, err)
	}
	list, err := workshop.Stock(ctx, pool, buyerScope())
	if err != nil || len(list.Rows) != 1 || !list.Rows[0].Consigned() || list.Rows[0].ValueMinor() != nil ||
		list.TotalValueMinor == nil || *list.TotalValueMinor != 0 || list.MissingValue != 0 {
		t.Errorf("the stock list: %+v, %v", list, err)
	}
	if read, err := workshop.ConsignmentsOfUnit(ctx, pool, sales(), c.StockUnitID); err != nil || len(read) != 1 || read[0].Model != workshop.ConsignOwnName {
		t.Errorf("the salesperson's read: %+v, %v", read, err)
	}
	if _, err := workshop.ConsignCar(ctx, pool, sales(), workshop.NewConsignment{VehicleID: vehicleA,
		NewPrincipal: &workshop.NewParty{Kind: "private", Name: "X"}, ConsignmentTerms: consignmentTerms(workshop.ConsignOwnName)}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson took a car in on consignment: %v", err)
	}
}

// A version is never changed: a lower minimum the principal approved is
// the next version, superseding the last. Withdrawn by the principal, the
// agreement ends with why and the car's cycle is closed.
func TestAConsignmentIsRevisedAndEnded(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := consign(t, pool, workshop.ConsignPrincipalName, "")
	lower := consignmentTerms(workshop.ConsignPrincipalName)
	m := int64(9_000_000)
	lower.MinimumPriceMinor = &m
	next, err := workshop.ReviseConsignment(ctx, pool, buyerScope(), c.ID, lower)
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != 2 || next.SupersedesID != c.ID || next.RootID != c.ID || *next.MinimumPriceMinor != m {
		t.Errorf("the revision: %+v", next)
	}
	if _, err := workshop.ReviseConsignment(ctx, pool, buyerScope(), c.ID, lower); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a replaced version revised again: %v", err)
	}
	bad := lower
	bad.CommissionMinor = new(int64)
	if _, err := workshop.ReviseConsignment(ctx, pool, buyerScope(), next.ID, bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a commission both as an amount and a share: %v", err)
	}
	if err := workshop.EndConsignment(ctx, pool, buyerScope(), next.ID, "withdrawn", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("ended without a reason: %v", err)
	}
	if err := workshop.EndConsignment(ctx, pool, buyerScope(), next.ID, "withdrawn", "Sålde den själv"); err != nil {
		t.Fatal(err)
	}
	all, _ := workshop.ConsignmentsOfUnit(ctx, pool, buyerScope(), c.StockUnitID)
	u, _ := workshop.StockUnitByID(ctx, pool, buyerScope(), c.StockUnitID)
	if len(all) != 2 || all[0].EndKind != "withdrawn" || all[1].EndKind != "replaced" || u.Status != workshop.StockClosed {
		t.Errorf("after the principal withdrew it: %+v, car %s", all, u.Status)
	}
}

// Sold, the agreement ends as sold with the car.
func TestAConsignedCarSoldEndsItsAgreement(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := consign(t, pool, workshop.ConsignOwnName, "")
	moveStock(t, pool, c.StockUnitID, workshop.StockForSale)
	terms := consumerTerms(driver(t, pool))
	terms.TaxTreatment = workshop.TaxOrdinary
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: c.StockUnitID, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	if _, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := workshop.ConsignmentsOfUnit(ctx, pool, sales(), c.StockUnitID)
	if len(all) != 1 || all[0].EndKind != "sold" {
		t.Errorf("the agreement after the sale: %+v", all)
	}
}
