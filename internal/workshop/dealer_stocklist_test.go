package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

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

// The list is one read however long it is (#168): the same round trips for
// one car as for three hundred, for the buyer who is handed the figures and
// for the salesperson who is not.
func TestTheStockListOf300CarsIsReadInTheRoundTripsOfOne(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 300 cars")
	}
	pool := setup(t)
	ctx := context.Background()
	cfg := pool.Config()
	counter := &countingTracer{}
	cfg.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer counted.Close()
	read := func(scope access.Scope, want int) int64 {
		t.Helper()
		before := counter.n.Load()
		list, err := workshop.Stock(ctx, counted, scope)
		if err != nil || len(list.Rows) != want {
			t.Fatalf("the list as %s: %d cars, %v; want %d", scope.Role, len(list.Rows), err, want)
		}
		return counter.n.Load() - before
	}
	p := boughtCar(t, pool)
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, cost(workshop.CostTransport, 1_000)); err != nil {
		t.Fatal(err)
	}
	buyerOne, salesOne := read(buyerScope(), 1), read(sales(), 1)
	for range 299 {
		q := boughtCar(t, pool)
		if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), q.StockUnitID, cost(workshop.CostTransport, 1_000)); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(buyerScope(), 300); got != buyerOne {
		t.Errorf("the buyer's list: %d round trips for 300 cars, %d for one", got, buyerOne)
	}
	if got := read(sales(), 300); got != salesOne {
		t.Errorf("the salesperson's list: %d round trips for 300 cars, %d for one", got, salesOne)
	}
}

// A shop draws its own ageing lines (#168), narrows the list by bucket and
// status and orders it; what the shop holds, in total, does not shrink
// because a filter is on.
func TestAShopDrawsItsOwnAgeingLinesAndNarrowsTheList(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	ages := []int{3, 15, 25, 40}
	var units []string
	for _, d := range ages {
		p := boughtCar(t, pool)
		units = append(units, p.StockUnitID)
		if _, err := pool.Exec(ctx, `UPDATE stock_units SET opened_at = clock_timestamp() - make_interval(days => $2) WHERE id = $1`, p.StockUnitID, d); err != nil {
			t.Fatal(err)
		}
	}
	moveStock(t, pool, units[0], workshop.StockForSale)

	list, err := workshop.Stock(ctx, pool, buyerScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Rows) != 4 || list.Buckets[0] != "0-30" || list.Rows[0].DaysInStock != 40 || list.Rows[0].Ageing() != "31-60" {
		t.Errorf("the default lines: %v, first %+v", list.Buckets, list.Rows[0])
	}
	total := *list.TotalValueMinor

	for _, bad := range [][]int{nil, {}, {0}, {10, 10}, {20, 10}, {1, 2, 3, 4, 5, 6, 7}, {4000}} {
		if err := workshop.SetStockAgeing(ctx, pool, everything(owner()), bad); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("lines %v accepted: %v", bad, err)
		}
	}
	for _, s := range []access.Scope{buyerScope(), sales(), everything(technician())} {
		if err := workshop.SetStockAgeing(ctx, pool, s, []int{10, 20}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s set the lines: %v", s.Role, err)
		}
	}
	if err := workshop.SetStockAgeing(ctx, pool, everything(owner()), []int{10, 20}); err != nil {
		t.Fatal(err)
	}
	got, err := workshop.Stock(ctx, pool, buyerScope())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{3: "0-10", 15: "11-20", 25: "20+", 40: "20+"}
	for _, r := range got.Rows {
		if r.Ageing() != want[r.DaysInStock] {
			t.Errorf("a car %d days in stock is in %q, want %q", r.DaysInStock, r.Ageing(), want[r.DaysInStock])
		}
	}
	if len(got.Buckets) != 3 || got.Buckets[0] != "0-10" || got.Buckets[2] != "20+" {
		t.Errorf("the buckets: %v", got.Buckets)
	}

	old, err := workshop.StockFiltered(ctx, pool, buyerScope(), workshop.StockFilter{Bucket: "20+"})
	if err != nil || len(old.Rows) != 2 || old.Rows[0].DaysInStock != 40 {
		t.Fatalf("the oldest bucket: %+v, %v", old.Rows, err)
	}
	if *old.TotalValueMinor != total {
		t.Errorf("the total of what is held shrank to %d from %d with a filter on", *old.TotalValueMinor, total)
	}
	forSale, err := workshop.StockFiltered(ctx, pool, buyerScope(), workshop.StockFilter{Status: workshop.StockForSale})
	if err != nil || len(forSale.Rows) != 1 || forSale.Rows[0].UnitID != units[0] {
		t.Errorf("cars for sale: %+v, %v", forSale.Rows, err)
	}
	both, _ := workshop.StockFiltered(ctx, pool, buyerScope(), workshop.StockFilter{Bucket: "20+", Status: workshop.StockForSale})
	if len(both.Rows) != 0 {
		t.Errorf("an old car for sale: %+v", both.Rows)
	}
	byOffer, _ := workshop.StockFiltered(ctx, pool, buyerScope(), workshop.StockFilter{SortBy: "days_for_sale"})
	if len(byOffer.Rows) != 4 || byOffer.Rows[0].UnitID != units[0] || byOffer.Rows[0].DaysForSale == nil || byOffer.Rows[3].DaysForSale != nil {
		t.Errorf("by days on offer, the car for sale first and the rest after: %+v", byOffer.Rows)
	}
	// A salesperson narrows it the same and is handed no figure.
	seen, err := workshop.StockFiltered(ctx, pool, sales(), workshop.StockFilter{Bucket: "20+"})
	if err != nil || len(seen.Rows) != 2 || seen.TotalValueMinor != nil || seen.Rows[0].Figures != nil {
		t.Errorf("the salesperson's narrowed list: %+v, %v", seen, err)
	}
}

// Cars in stock longer than the shop's oldest ageing line are one item in
// the shared queue (#168), counting them, for whoever buys or sells cars
// and nobody else: it carries a count, never a car's cost.
func TestCarsOlderThanTheOldestAgeingLineAreOneItemInTheQueue(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	waiting := func(s access.Scope) (n int) {
		t.Helper()
		items, err := workshop.WaitingFor(ctx, pool, s)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Kind == "stock_ageing" {
				if it.SubjectKind != "stock_list" || it.Text != "" {
					t.Errorf("the item: %+v", it)
				}
				n += it.Count
			}
		}
		return n
	}
	fresh, old, older := boughtCar(t, pool), boughtCar(t, pool), boughtCar(t, pool)
	for unit, days := range map[string]int{old.StockUnitID: 100, older.StockUnitID: 200} {
		if _, err := pool.Exec(ctx, `UPDATE stock_units SET opened_at = clock_timestamp() - make_interval(days => $2) WHERE id = $1`, unit, days); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []access.Scope{buyerScope(), sales(), everything(owner())} {
		if n := waiting(s); n != 2 {
			t.Errorf("%s sees %d ageing cars, want the two past 90 days", s.Role, n)
		}
	}
	for _, s := range []access.Scope{everything(technician()), everything(advisor()), rentalDesk()} {
		if n := waiting(s); n != 0 {
			t.Errorf("%s sees %d ageing cars in the queue", s.Role, n)
		}
	}
	// A car sold and waiting to be delivered is not ageing stock.
	moveStock(t, pool, older.StockUnitID, workshop.StockForSale, workshop.StockSold)
	if n := waiting(buyerScope()); n != 1 {
		t.Errorf("after one of them was sold: %d", n)
	}
	// The shop's own lines move what counts.
	if err := workshop.SetStockAgeing(ctx, pool, everything(owner()), []int{150}); err != nil {
		t.Fatal(err)
	}
	if n := waiting(buyerScope()); n != 0 {
		t.Errorf("with the line at 150 days: %d", n)
	}
	_ = fresh
}
