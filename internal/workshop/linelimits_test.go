package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A quantity and a price that can each be typed multiplied to a number that
// wraps around an int64 and was shown as a total: 1 000 000 pieces at
// 9 999 999 999 999 came out as 25 trillion.
func TestALineTooLargeToAddUpIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	for _, c := range []struct {
		name        string
		qty, price  int64
		wantRefusal bool
	}{
		{"both huge", 1_000_000_000, 999_999_999_999_900, true},
		{"price over the limit", 1000, money.MaxUnitPriceMinor + 1, true},
		{"quantity over the limit", money.MaxQuantityMilli + 1, 100, true},
		{"both at the limit", money.MaxQuantityMilli, money.MaxUnitPriceMinor, false},
	} {
		err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
			Kind: "part", Description: c.name, QuantityMilli: c.qty, UnitPriceMinor: c.price, VATRateBasis: 2500,
		})
		if got := errors.Is(err, workshop.ErrInvalid); got != c.wantRefusal {
			t.Errorf("%s: refused = %v (%v), want %v", c.name, got, err, c.wantRefusal)
		}
	}
}
