package money

import (
	"errors"
	"testing"
)

// The margin scheme, one car at a time (#176).
func TestMarginSchemeOneCarAtATime(t *testing.T) {
	for _, c := range []struct {
		name              string
		sale, purchase    int64
		rate              int
		margin, vat, base int64
	}{
		{"a margin", 12_500_000, 8_500_000, 2500, 4_000_000, 800_000, 3_200_000},
		{"sold for exactly what it cost", 8_500_000, 8_500_000, 2500, 0, 0, 0},
		{"sold at a loss", 8_000_000, 8_500_000, 2500, -500_000, 0, 0},
		{"one öre of margin", 8_500_001, 8_500_000, 2500, 1, 0, 1},
		{"two öre rounds to nothing", 8_500_002, 8_500_000, 2500, 2, 0, 2},
		{"three öre rounds to one", 8_500_003, 8_500_000, 2500, 3, 1, 2},
		// At 12 per cent the VAT is 3/28 of the margin: 14 öre is 1.5 öre of VAT.
		{"half an öre rounds away from zero", 14, 0, 1200, 14, 2, 12},
		{"at 12 per cent", 1_120_000, 0, 1200, 1_120_000, 120_000, 1_000_000},
		{"the most a car may cost", MaxVehicleMinor, 0, 2500, MaxVehicleMinor, MaxVehicleMinor / 5, MaxVehicleMinor - MaxVehicleMinor/5},
	} {
		got, err := MarginScheme(c.sale, c.purchase, c.rate)
		if err != nil || got.MarginMinor != c.margin || got.VATMinor != c.vat || got.BaseMinor != c.base {
			t.Errorf("%s: %+v, %v; want margin %d, VAT %d, base %d", c.name, got, err, c.margin, c.vat, c.base)
		}
		if err == nil && got.VATMinor+got.BaseMinor != max(got.MarginMinor, 0) {
			t.Errorf("%s: VAT and base do not add up to the margin: %+v", c.name, got)
		}
	}
	for _, bad := range [][2]int64{{MaxVehicleMinor + 1, 0}, {0, MaxVehicleMinor + 1}, {-1, 0}, {0, -1}} {
		if _, err := MarginScheme(bad[0], bad[1], 2500); !errors.Is(err, ErrVehicleAmount) {
			t.Errorf("sale %d, purchase %d: %v; want it refused clearly", bad[0], bad[1], err)
		}
	}
	if _, err := MarginScheme(1, 0, 10001); !errors.Is(err, ErrVehicleAmount) {
		t.Errorf("a rate over 100 per cent: %v", err)
	}
}

// A loss on one car is never set against another's margin: the VAT of two
// cars is the sum of each car's, each already floored at zero.
func TestALossIsNeverOffsetAgainstAnotherCar(t *testing.T) {
	a, _ := MarginScheme(11_000_000, 10_000_000, 2500) // +10 000 kr
	b, _ := MarginScheme(8_000_000, 10_000_000, 2500)  // -20 000 kr
	if total := a.VATMinor + b.VATMinor; total != 200_000 {
		t.Errorf("the two cars' VAT is %d; want 2 000 kr, the first car's alone", total)
	}
	if MarginSchemeVAT(11_000_000, 10_000_000, 2500) != a.VATMinor {
		t.Error("MarginSchemeVAT and MarginScheme disagree")
	}
}
