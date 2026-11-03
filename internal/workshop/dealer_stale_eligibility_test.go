package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A car contracted under the margin scheme, whose purchase is assessed
// again before it is invoiced -- the seller turned out to have charged VAT
// -- is not invoiced under the scheme on the assessment that no longer
// stands.
func TestACarNoLongerEligibleIsNotInvoicedUnderTheMarginScheme(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	d := completedDealOn(t, pool, unit, workshop.TaxMarginScheme)
	var purchase string
	if err := pool.QueryRow(ctx, `SELECT id FROM vehicle_purchases WHERE stock_unit_id = $1`, unit).Scan(&purchase); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CorrectVATEligibility(ctx, pool, buyerScope(), purchase, workshop.SellerCompanyChargedVAT,
		[]string{contractFor(t, pool, purchase)}, "Säljaren debiterade moms"); err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("invoiced under the margin scheme on a superseded assessment: %+v, %v", inv, err)
	}
}
