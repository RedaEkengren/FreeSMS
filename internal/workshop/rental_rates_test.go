package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

func kr(v int64) *int64 { return &v }

// dayPlan prices a category by the day, from a day long ago.
func dayPlan(t *testing.T, pool *pgxpool.Pool, category string, perDay int64, rule string) string {
	t.Helper()
	ctx := context.Background()
	plan, err := workshop.CreateRatePlan(ctx, pool, rentalOwner(), workshop.NewRatePlan{
		Name: "Standard", Category: category, VATRateBp: 2500, SeasonRule: rule})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), plan, workshop.RateSeason{
		ValidFrom: day("2020-01-01"), Day: kr(perDay), Hour: kr(perDay / 4)}); err != nil {
		t.Fatal(err)
	}
	return plan
}

// A rental is priced by its unit's category's plan, extras beside it,
// each line with its own VAT, and the total including VAT is what the
// lines come to.
func TestARentalIsQuotedFromItsPlanAndExtras(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	van := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Small van"})
	if _, err := workshop.QuoteRental(ctx, pool, rentalDesk(), workshop.QuoteRequest{UnitID: van,
		From: wallAt("2026-11-10 09:00"), To: wallAt("2026-11-13 09:00")}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a quote with no plan: %v", err)
	}
	dayPlan(t, pool, "Small van", 40000, "at_start")
	seat, err := workshop.AddRentalExtra(ctx, pool, rentalOwner(), workshop.NewRentalExtra{Kind: "child_seat",
		Name: "Bilbarnstol", Per: "day", PriceMinor: 5000, VATRateBp: 2500, VATCategory: "S"})
	if err != nil {
		t.Fatal(err)
	}
	waiver, err := workshop.AddRentalExtra(ctx, pool, rentalOwner(), workshop.NewRentalExtra{Kind: "excess_reduction",
		Name: "Självriskreducering", Per: "rental", PriceMinor: 20000, VATRateBp: 2500, VATCategory: "S"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := workshop.QuoteRental(ctx, pool, rentalDesk(), workshop.QuoteRequest{UnitID: van,
		From: wallAt("2026-11-10 09:00"), To: wallAt("2026-11-13 09:00"),
		Extras: []workshop.ExtraChoice{{ExtraID: seat, Count: 1}, {ExtraID: waiver, Count: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Lines) != 3 || q.Lines[0].Unit != "day" || q.Lines[0].QuantityMilli != 3000 || q.Lines[1].QuantityMilli != 3000 {
		t.Fatalf("three days and a seat for each: %+v", q.Lines)
	}
	if !q.Lines[2].Unconfirmed || q.Lines[1].Unconfirmed {
		t.Errorf("only the excess reduction's VAT treatment is unconfirmed: %+v", q.Lines)
	}
	var sum int64
	for _, l := range q.Lines {
		sum += l.Net()
	}
	if q.Totals.NetMinor != sum || q.Totals.GrossMinor != 3*40000*5/4+3*5000*5/4+20000*5/4 {
		t.Errorf("totals %+v from lines worth %d", q.Totals, sum)
	}

	// The shop's own plan for this unit comes before the category's.
	own, err := workshop.CreateRatePlan(ctx, pool, rentalOwner(), workshop.NewRatePlan{Name: "Lånebil", UnitID: van, VATRateBp: 2500, SeasonRule: "at_start"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), own, workshop.RateSeason{ValidFrom: day("2020-01-01"), Day: kr(0)}); err != nil {
		t.Fatal(err)
	}
	q, err = workshop.QuoteRental(ctx, pool, rentalDesk(), workshop.QuoteRequest{UnitID: van, From: wallAt("2026-11-10 09:00"), To: wallAt("2026-11-12 09:00")})
	if err != nil || q.PlanID != own || q.Totals.GrossMinor != 0 {
		t.Errorf("a courtesy plan of the unit's own: %+v, %v", q, err)
	}
	if err := workshop.ArchiveRatePlan(ctx, pool, rentalOwner(), own); err != nil {
		t.Fatal(err)
	}
	if q, _ := workshop.QuoteRental(ctx, pool, rentalDesk(), workshop.QuoteRequest{UnitID: van, From: wallAt("2026-11-10 09:00"), To: wallAt("2026-11-12 09:00")}); q.PlanID == own {
		t.Error("an archived plan was used")
	}
}

// A season is a row from a day: a new one ends the open one before it, an
// overlapping one is refused, and a rental across the change is priced by
// the plan's rule.
func TestSeasonsAreRowsAndARentalAcrossThemFollowsThePlan(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	van := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Small van"})
	plan := dayPlan(t, pool, "Small van", 40000, "split")
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), plan, workshop.RateSeason{ValidFrom: day("2026-06-01"), Day: kr(70000)}); err != nil {
		t.Fatal(err)
	}
	until := day("2026-08-01")
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), plan, workshop.RateSeason{ValidFrom: day("2026-07-01"), ValidUntil: &until, Day: kr(1)}); err != nil {
		t.Fatalf("a season inside the open one ends it: %v", err)
	}
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), plan, workshop.RateSeason{ValidFrom: day("2026-07-15"), Day: kr(1)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an overlapping season: %v", err)
	}
	q, err := workshop.QuoteRental(ctx, pool, rentalDesk(), workshop.QuoteRequest{UnitID: van, From: wallAt("2026-05-30 10:00"), To: wallAt("2026-06-02 10:00")})
	if err != nil || q.Totals.NetMinor != 2*40000+2*70000 || len(q.Lines) != 2 || q.Lines[0].SeasonID == q.Lines[1].SeasonID {
		t.Errorf("split across the start of summer: %+v, %v", q, err)
	}
	plans, _ := workshop.RatePlans(ctx, pool, rentalDesk())
	if len(plans) != 1 || len(plans[0].Seasons) != 3 || plans[0].Seasons[1].ValidUntil == nil {
		t.Errorf("three seasons, the summer one ended by July's: %+v", plans)
	}
	if err := asApp(ctx, pool, `UPDATE rate_plan_seasons SET day_minor = 1`); err == nil {
		t.Error("the application could change a season's price")
	}
	if err := asApp(ctx, pool, `UPDATE rate_plan_seasons SET valid_until = '2030-01-01' WHERE valid_until IS NOT NULL`); err == nil {
		t.Error("the application could move a season's end")
	}
}

// Whoever runs the shop sets prices; the rental desk applies them; nobody
// else does either.
func TestPricesByRole(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := workshop.CreateRatePlan(ctx, pool, rentalDesk(), workshop.NewRatePlan{Name: "x", Category: "x", VATRateBp: 2500, SeasonRule: "at_start"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk making a plan: %v", err)
	}
	if _, err := workshop.AddRentalExtra(ctx, pool, rentalDesk(), workshop.NewRentalExtra{Kind: "other", Name: "x", Per: "rental", VATRateBp: 2500, VATCategory: "S"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk adding an extra: %v", err)
	}
	plan := dayPlan(t, pool, "Släp", 20000, "at_start")
	if _, err := workshop.AddRateSeason(ctx, pool, rentalDesk(), plan, workshop.RateSeason{ValidFrom: day("2027-01-01"), Day: kr(1)}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk adding a season: %v", err)
	}
	for _, r := range []access.Role{access.RoleTechnician, access.RoleServiceAdvisor, access.RoleSales} {
		if _, err := workshop.RatePlans(ctx, pool, renting(r)); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s reading the plans: %v", r, err)
		}
	}
	if _, err := workshop.AddRentalExtra(ctx, pool, rentalOwner(), workshop.NewRentalExtra{Kind: "other", Name: "Skadestånd", Per: "rental",
		VATRateBp: 2500, VATCategory: "O"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("outside the scope of VAT at a rate: %v", err)
	}
}
