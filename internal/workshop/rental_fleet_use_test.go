package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A fleet vehicle's regulatory lifecycle (#202).

func gapCodes(t *testing.T, gaps []workshop.HandoverGap) []string {
	t.Helper()
	var out []string
	for _, g := range gaps {
		out = append(out, g.Code)
	}
	return out
}

// A car not reported into rental use is not handed over, and one whose
// insurance ends before the rental does is not either; recording them
// clears the way. Reported out, it is unreported again.
func TestACarIsHandedOverOnlyReportedAndInsured(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id := handoverReady(t, pool) // reported, insured until further notice
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	u, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), b.UnitID)
	m := u.Memberships[0].ID
	if gaps, err := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id); err != nil || len(gaps) != 0 {
		t.Fatalf("reported and insured: %+v, %v", gaps, err)
	}
	if _, err := workshop.RecordFleetReport(ctx, pool, rentalDesk(), m, workshop.ReportedOut, day("2026-01-01"), "Av misstag"); err != nil {
		t.Fatal(err)
	}
	gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id)
	if !slices.Equal(gapCodes(t, gaps), []string{workshop.GapNotReported}) {
		t.Errorf("reported out: %v", gapCodes(t, gaps))
	}
	if _, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverOut); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unreported car checked out: %v", err)
	}
	if _, err := workshop.RecordFleetReport(ctx, pool, rentalDesk(), m, workshop.ReportedIn, day("2026-01-02"), ""); err != nil {
		t.Fatal(err)
	}
	// A new policy that ends the day before the rental does.
	pool.Exec(ctx, `DELETE FROM fleet_insurances`)
	end := day("2031-05-11")
	if _, err := workshop.RecordFleetInsurance(ctx, pool, rentalDesk(), m, workshop.FleetInsurance{Insurer: "Bolaget", PolicyRef: "P-2",
		ValidFrom: day("2026-01-01"), ValidTo: &end, CoversRental: "yes"}); err != nil {
		t.Fatal(err)
	}
	gaps, _ = workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id)
	if !slices.Equal(gapCodes(t, gaps), []string{workshop.GapInsurance}) {
		t.Errorf("insured until the day before the rental ends: %v", gapCodes(t, gaps))
	}
	// Renewed from the next day: the two between them cover it.
	if _, err := workshop.RecordFleetInsurance(ctx, pool, rentalDesk(), m, workshop.FleetInsurance{Insurer: "Bolaget", PolicyRef: "P-3",
		ValidFrom: day("2031-05-12"), CoversRental: "yes"}); err != nil {
		t.Fatal(err)
	}
	if gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id); len(gaps) != 0 {
		t.Errorf("renewed: %v", gapCodes(t, gaps))
	}
}

// A trailer whose profile does not require reporting is handed over
// unreported; it is a vehicle, so it is insured.
func TestATrailerNeedsNoReport(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	trailer := profileFor(t, pool, workshop.ProfileTrailer)
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "TRL 001", FleetNumber: "S1", Category: "Släp", ProfileID: trailer.ID})
	m, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().AddDate(-1, 0, 0), "bought", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordFleetInsurance(ctx, pool, rentalDesk(), m, workshop.FleetInsurance{Insurer: "Bolaget", PolicyRef: "S-1",
		ValidFrom: day("2024-01-01"), CoversRental: "yes"}); err != nil {
		t.Fatal(err)
	}
	dayPlan(t, pool, "Släp", 30000, "at_start")
	renter, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Tor", Role: workshop.PartyRenter})
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: unit, RenterID: renter, From: in2031(10, 9), To: in2031(11, 9)})
	if err != nil {
		t.Fatal(err)
	}
	gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id)
	if slices.Contains(gapCodes(t, gaps), workshop.GapNotReported) || slices.Contains(gapCodes(t, gaps), workshop.GapInsurance) {
		t.Errorf("an insured trailer, no report required: %v", gapCodes(t, gaps))
	}
}

// A missing report or insurance may be let go for one booking by whoever
// runs the shop, with a reason; the rental desk may not, and the permit
// and the licence check never are.
func TestAMissingReportIsLetGoOnlyByTheOwnerWithAReason(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool) // a car, no permit, no driver, not reported, not insured
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideHandoverGap(ctx, pool, rentalDesk(), id, workshop.GapNotReported, "Anmäls i morgon"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk letting a gap go: %v", err)
	}
	if err := workshop.OverrideHandoverGap(ctx, pool, rentalOwner(), id, workshop.GapNotReported, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("let go with no reason: %v", err)
	}
	if err := workshop.OverrideHandoverGap(ctx, pool, rentalOwner(), id, workshop.GapPermit, "Brådis"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the permit let go: %v", err)
	}
	if err := workshop.OverrideHandoverGap(ctx, pool, rentalOwner(), id, workshop.GapNotReported, "Anmäls i morgon"); err != nil {
		t.Fatal(err)
	}
	gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id)
	if c := gapCodes(t, gaps); slices.Contains(c, workshop.GapNotReported) || !slices.Contains(c, workshop.GapInsurance) || !slices.Contains(c, workshop.GapPermit) {
		t.Errorf("after letting the report go: %v", c)
	}
	let, err := workshop.RentalGapOverrides(ctx, pool, rentalDesk(), id)
	if err != nil || len(let) != 1 || let[0].Reason != "Anmäls i morgon" {
		t.Errorf("what was let go: %+v, %v", let, err)
	}
}

// The paperwork is on the rental desk's list before it is missed at the
// counter: a unit in the fleet not reported, an insurance ending within
// thirty days, a booking within them that would be refused today, and a
// unit out of the fleet still reported in.
func TestTheFleetsPaperworkIsOnTheRentalDesksList(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	kinds := func() []string {
		t.Helper()
		items, err := workshop.RentalDay(ctx, pool, rentalDesk())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Kind)
			if it.Says() == "" || it.Link() == "/jobs/"+it.SubjectID {
				t.Errorf("%s says %q and goes to %s", it.Kind, it.Says(), it.Link())
			}
		}
		return out
	}
	if k := kinds(); !slices.Contains(k, "rental_report_in") {
		t.Errorf("a car in the fleet not reported: %v", k)
	}
	u, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), v.unit)
	m := u.Memberships[0].ID
	soon := time.Now().AddDate(0, 0, 10)
	if _, err := workshop.RecordFleetReport(ctx, pool, rentalDesk(), m, workshop.ReportedIn, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordFleetInsurance(ctx, pool, rentalDesk(), m, workshop.FleetInsurance{Insurer: "Bolaget", PolicyRef: "P",
		ValidFrom: day("2024-01-01"), ValidTo: &soon, CoversRental: "yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.book(pool, time.Now().AddDate(0, 0, 5), time.Now().AddDate(0, 0, 6)); err != nil {
		t.Fatal(err)
	}
	k := kinds()
	if slices.Contains(k, "rental_report_in") || !slices.Contains(k, "rental_insurance_ends") || !slices.Contains(k, "rental_gaps") {
		t.Errorf("reported, insured for ten days more, booked in five with no permit: %v", k)
	}
}
