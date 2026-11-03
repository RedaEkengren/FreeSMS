package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// What each car sold earned (#182): net of every VAT, less what it was
// bought for and every cost, a cost after delivery counted now and not at
// delivery, and totals by salesperson and month built from the same rows.
// A salesperson may not read margins at all.
func TestWhatACarEarnedCountsEveryCost(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), unit, cost(workshop.CostReconditioning, 1_000_000)); err != nil {
		t.Fatal(err)
	}
	d := deliveredOn(t, pool, unit, time.Now())
	after := cost(workshop.CostWorkshop, 300_000)
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), unit, after); err != nil {
		t.Fatal(err)
	}
	tax, err := workshop.SaleTaxOfDeal(ctx, pool, buyerScope(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := workshop.SaleResults(ctx, pool, buyerScope(), time.Now().AddDate(0, -1, 0), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("the cars sold: %+v", res.Rows)
	}
	r := res.Rows[0]
	// 139 480 kr in all: the car at 125 000 under the scheme, winter
	// tyres at 10 000 with 2 000 of VAT and a tow bar at 4 480 with 480.
	wantNet := int64(13_948_000) - tax.VATMinor - 248_000
	if r.NetRevenueMinor != wantNet || r.PurchaseMinor == nil || *r.PurchaseMinor != 8_500_000 {
		t.Fatalf("the result: %+v, want net %d", r, wantNet)
	}
	if r.MarginAtDeliveryMinor() != wantNet-8_500_000-1_000_000 || r.MarginMinor() != wantNet-8_500_000-1_300_000 {
		t.Errorf("margins: at delivery %d, now %d", r.MarginAtDeliveryMinor(), r.MarginMinor())
	}
	if r.MarginMinor() == tax.MarginMinor {
		t.Error("the business margin came out as the tax margin")
	}
	if len(res.ByMonth) != 1 || res.ByMonth[0].MarginMinor != r.MarginMinor() || len(res.BySalesperson) != 1 {
		t.Errorf("the totals: %+v, %+v", res.ByMonth, res.BySalesperson)
	}
	for _, s := range []access.Scope{sales(), everything(advisor()), rentalDesk()} {
		if got, err := workshop.SaleResults(ctx, pool, s, time.Now().AddDate(0, -1, 0), time.Now()); !errors.Is(err, access.ErrForbidden) || len(got.Rows) != 0 {
			t.Errorf("%v read the results: %v", s.Role, err)
		}
	}
}
