package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The stock list (#168): every car in stock, a car bought today in stock 0
// days, and what each has cost so far -- purchase price and costs -- with
// the total, for whoever may see what a car cost. A salesperson is handed
// the same list with no figure at all, and the workshop's roles nothing.
func TestTheStockListShowsWhatACarCostOnlyToWhoMaySee(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, cost(workshop.CostTransport, 500_000)); err != nil {
		t.Fatal(err)
	}

	list, err := workshop.Stock(ctx, pool, buyerScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Rows) != 1 {
		t.Fatalf("the list: %+v", list.Rows)
	}
	row := list.Rows[0]
	if row.UnitID != p.StockUnitID || row.DaysInStock != 0 || row.Ageing() != "0-30" || row.Registration != "ABC 12D" {
		t.Errorf("the car: %+v", row)
	}
	if row.Figures == nil || row.Figures.PurchaseMinor == nil || *row.Figures.PurchaseMinor != 8_500_000 || row.Figures.CostsMinor != 500_000 {
		t.Errorf("the buyer's figures: %+v", row.Figures)
	}
	if v := row.ValueMinor(); v == nil || *v != 9_000_000 || list.TotalValueMinor == nil || *list.TotalValueMinor != 9_000_000 {
		t.Errorf("the value: %v, total %v", v, list.TotalValueMinor)
	}

	seen, err := workshop.Stock(ctx, pool, sales())
	if err != nil {
		t.Fatal(err)
	}
	if len(seen.Rows) != 1 || seen.Rows[0].Figures != nil || seen.Rows[0].ValueMinor() != nil || seen.TotalValueMinor != nil {
		t.Errorf("a salesperson was handed a figure: %+v, total %v", seen.Rows, seen.TotalValueMinor)
	}
	for _, s := range []access.Scope{everything(technician()), everything(advisor()), rentalDesk()} {
		if got, err := workshop.Stock(ctx, pool, s); !errors.Is(err, access.ErrForbidden) || len(got.Rows) != 0 {
			t.Errorf("%v read the stock: %v", s.Role, err)
		}
	}
}
