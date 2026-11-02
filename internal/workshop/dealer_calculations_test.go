package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Purchase calculations (#163).

func ref[T any](v T) *T { return &v }

// A car expected to sell for 125 000 kr, with 10 000 kr of costs, 30 days
// at 100 kr a day and 15 000 kr of margin wanted: 28 000 kr to keep.
// Under ordinary VAT the sale carries 25 000 kr of VAT and the most to pay
// is 72 000 kr before any VAT the seller charges. Under the margin scheme
// every krona off the price is a krona more margin less a fifth of it in
// VAT: 125 000 - 1,25 x 28 000 = 90 000 kr, and the sale carries 7 000 kr
// of VAT. At that price the margin is exactly what was wanted, and a öre
// more is less.
func TestABidIsWorkedOutUnderBothWaysOfTaxingTheSale(t *testing.T) {
	in := workshop.CalculationInput{
		ExpectedSaleGrossMinor: ref(int64(12_500_000)),
		Costs: []workshop.CalculationCost{{Kind: workshop.CostReconditioning, NetMinor: 600_000},
			{Kind: workshop.CostTransport, NetMinor: 400_000}},
		ExpectedStockDays: 30, HoldingPerDayMinor: 10_000,
		TargetMarginMinor: ref(int64(1_500_000)),
		SellerType:        workshop.SellerPrivate,
	}
	r := in.Result()
	if r.CostsMinor != 1_000_000 || r.HoldingMinor != 300_000 || r.TargetMarginMinor != 1_500_000 || r.LossLeader || r.NoSalePrice {
		t.Errorf("the parts: %+v", r)
	}
	if r.Ordinary.MaxBidMinor != 7_200_000 || r.Ordinary.SaleVATMinor != 2_500_000 {
		t.Errorf("under ordinary VAT: %+v", r.Ordinary)
	}
	if r.MarginScheme == nil || r.MarginScheme.MaxBidMinor != 9_000_000 || r.MarginScheme.SaleVATMinor != 700_000 {
		t.Fatalf("under the margin scheme: %+v", r.MarginScheme)
	}
	margin := func(p int64) int64 { return 12_500_000 - money.MarginSchemeVAT(12_500_000, p, 2500) - p - 1_300_000 }
	if margin(9_000_000) < 1_500_000 || margin(9_000_001) >= 1_500_000 {
		t.Errorf("the bid is not the highest that leaves the margin: %d at it, %d an öre over", margin(9_000_000), margin(9_000_001))
	}

	// A margin as a share of the sale price: 12 per cent of 125 000 kr.
	in.TargetMarginMinor, in.TargetMarginBasis = nil, ref(1200)
	if r := in.Result(); r.TargetMarginMinor != 1_500_000 {
		t.Errorf("12 per cent: %d", r.TargetMarginMinor)
	}
	// An uneven one, made exact against the scheme's own VAT.
	in.TargetMarginBasis = ref(1234)
	r = in.Result()
	keep := r.CostsMinor + r.HoldingMinor + r.TargetMarginMinor
	p := r.MarginScheme.MaxBidMinor
	left := func(p int64) int64 { return 12_500_000 - money.MarginSchemeVAT(12_500_000, p, 2500) - p }
	if left(p) < keep || left(p+1) >= keep {
		t.Errorf("uneven: bid %d leaves %d, an öre more leaves %d, want %d", p, left(p), left(p+1), keep)
	}
}

// The edge cases: no sale price yet, a loss on purpose, a seller who would
// not make the car eligible or is not known, and prices too large to fit.
func TestACalculationsEdges(t *testing.T) {
	if r := (workshop.CalculationInput{SellerType: workshop.SellerPrivate}).Result(); !r.NoSalePrice || r.MarginScheme != nil {
		t.Errorf("no sale price: %+v", r)
	}
	loss := workshop.CalculationInput{ExpectedSaleGrossMinor: ref(int64(10_000_000)), TargetMarginMinor: ref(int64(-1_000_000)),
		SellerType: workshop.SellerPrivate}
	r := loss.Result()
	if !r.LossLeader || r.MarginScheme == nil || r.MarginScheme.MaxBidMinor != 11_000_000 || r.MarginScheme.SaleVATMinor != 0 {
		t.Errorf("a loss on purpose: %+v %+v", r, r.MarginScheme)
	}
	for _, seller := range []string{"", workshop.SellerCompanyChargedVAT, workshop.SellerEUDealerMargin} {
		in := workshop.CalculationInput{ExpectedSaleGrossMinor: ref(int64(10_000_000)), SellerType: seller}
		if r := in.Result(); r.MarginScheme != nil {
			t.Errorf("seller %q: a margin-scheme bid was shown", seller)
		}
	}
	huge := workshop.CalculationInput{ExpectedSaleGrossMinor: ref(int64(100_000_000_000)),
		Costs:             []workshop.CalculationCost{{Kind: workshop.CostOther, NetMinor: 10_000_000_000}},
		ExpectedStockDays: 3650, HoldingPerDayMinor: 100_000_000, TargetMarginBasis: ref(10000), SellerType: workshop.SellerPrivate}
	if r := huge.Result(); r.Ordinary.MaxBidMinor >= 0 || r.MarginScheme.MaxBidMinor >= 0 || r.Ordinary.Possible() {
		t.Errorf("the largest there can be: %+v %+v", r.Ordinary, r.MarginScheme)
	}
}

// Saved, a calculation is a version; edited, the next; an edit from an old
// version is refused rather than lost. A bid is recorded on the version it
// was made on, once. All of it is the buyer's and the owner's: a
// salesperson calling directly is refused and handed nothing.
func TestACalculationIsSavedInVersionsAndOnlyThoseWhoSeeCostsReadIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	in := workshop.CalculationInput{ExpectedSaleGrossMinor: ref(int64(12_500_000)), SellerType: workshop.SellerPrivate,
		Costs: []workshop.CalculationCost{{Kind: workshop.CostReconditioning, NetMinor: 600_000}}, Note: "Visning lördag"}
	v1, err := workshop.SaveCalculation(ctx, pool, buyerScope(), vehicleA, "", 0, in)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || v1.CalculationID != v1.ID || len(v1.Input.Costs) != 1 || v1.Input.Note != "Visning lördag" || v1.LatestVersion != 1 {
		t.Errorf("the first version: %+v", v1)
	}
	in.Costs = append(in.Costs, workshop.CalculationCost{Kind: workshop.CostTransport, NetMinor: 300_000})
	v2, err := workshop.SaveCalculation(ctx, pool, buyerScope(), vehicleA, v1.CalculationID, 1, in)
	if err != nil || v2.Version != 2 || v2.CalculationID != v1.CalculationID || len(v2.Input.Costs) != 2 {
		t.Fatalf("the second version: %+v, %v", v2, err)
	}
	if _, err := workshop.SaveCalculation(ctx, pool, buyerScope(), vehicleA, v1.CalculationID, 1, in); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("edited from a version that is not the newest: %v", err)
	}
	accepted, err := workshop.AcceptCalculation(ctx, pool, buyerScope(), v2.ID, 9_000_000)
	if err != nil || !accepted.Accepted() || accepted.AcceptedBidMinor != 9_000_000 {
		t.Fatalf("the bid on version 2: %+v, %v", accepted, err)
	}
	if _, err := workshop.AcceptCalculation(ctx, pool, buyerScope(), v1.ID, 1); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second bid on the same calculation: %v", err)
	}
	v3, err := workshop.SaveCalculation(ctx, pool, buyerScope(), vehicleA, v1.CalculationID, 2, in)
	if err != nil || v3.Accepted() || v3.AcceptedVersionID != v2.ID {
		t.Errorf("an edit after the bid: %+v, %v", v3, err)
	}
	again, _ := workshop.CalculationByID(ctx, pool, buyerScope(), v2.ID)
	if !again.Accepted() || len(again.Input.Costs) != 2 || again.LatestVersion != 3 {
		t.Errorf("the version bid on, after an edit: %+v", again)
	}
	list, err := workshop.CalculationsForVehicle(ctx, pool, everything(owner()), vehicleA)
	if err != nil || len(list) != 1 || list[0].Version != 3 {
		t.Errorf("the car's calculations: %+v, %v", list, err)
	}

	for name, s := range map[string]access.Scope{"a salesperson": sales(), "the front desk": everything(advisor()), "the rental desk": rentalDesk()} {
		got, err := workshop.CalculationByID(ctx, pool, s, v2.ID)
		if !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read a calculation: %v", name, err)
		}
		if handed := testsupport.Handed(got, "ID", "Input", "AcceptedBidMinor"); len(handed) != 0 {
			t.Errorf("%s was handed %v", name, handed)
		}
		if _, err := workshop.CalculationsForVehicle(ctx, pool, s, vehicleA); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s listed calculations: %v", name, err)
		}
		if _, err := workshop.SaveCalculation(ctx, pool, s, vehicleA, "", 0, in); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s saved a calculation: %v", name, err)
		}
	}
	off := buyerScope()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.SaveCalculation(ctx, pool, off, vehicleA, "", 0, in); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("saved with car sales off: %v", err)
	}
	if _, err := workshop.CalculationByID(ctx, pool, off, v2.ID); err != nil {
		t.Errorf("read with car sales off: %v", err)
	}
	for _, stmt := range []string{`UPDATE purchase_calculations SET note = 'x'`, `DELETE FROM purchase_calculation_costs`,
		`UPDATE purchase_calculation_acceptances SET bid_minor = 1`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
	bad := in
	bad.TargetMarginMinor, bad.TargetMarginBasis = ref(int64(1)), ref(1)
	if _, err := workshop.SaveCalculation(ctx, pool, buyerScope(), vehicleA, "", 0, bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a margin as both an amount and a share: %v", err)
	}
}

// The car a buyer means is found by its plate or chassis number, or added;
// what the workshop knows of it is not written over.
func TestABuyerFindsOrAddsTheCar(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if id, err := workshop.CarForPurchase(ctx, pool, buyerScope(), workshop.CarToBuy{Registration: "abc 12d", Make: "Saab"}); err != nil || id != vehicleA {
		t.Errorf("by the fixture's plate: %q, %v", id, err)
	}
	id, err := workshop.CarForPurchase(ctx, pool, buyerScope(), workshop.CarToBuy{Registration: "XYZ 789", VIN: "wba12345678901234", Make: "BMW"})
	if err != nil || id == vehicleA {
		t.Fatalf("a car not known: %q, %v", id, err)
	}
	if again, _ := workshop.CarForPurchase(ctx, pool, buyerScope(), workshop.CarToBuy{VIN: "WBA12345678901234"}); again != id {
		t.Errorf("by its chassis number: %q, want %q", again, id)
	}
	v, _ := workshop.VehicleByID(ctx, pool, owner(), vehicleA)
	if v.Make != "Volvo" {
		t.Errorf("the workshop's car was written over: %+v", v)
	}
	if _, err := workshop.CarForPurchase(ctx, pool, buyerScope(), workshop.CarToBuy{}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("neither plate nor chassis number: %v", err)
	}
	if _, err := workshop.CarForPurchase(ctx, pool, sales(), workshop.CarToBuy{Registration: "ABC12D"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson: %v", err)
	}
}
