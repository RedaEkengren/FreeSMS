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
		// Today's page is today's handovers; the paperwork is in the
		// queue (review r5, #7).
		day, err := workshop.RentalDay(ctx, pool, rentalDesk())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range day {
			if it.SubjectKind != "rental_booking" || it.Kind == "rental_gaps" {
				t.Errorf("paperwork on today's page: %+v", it)
			}
		}
		items, err := workshop.WaitingFor(ctx, pool, rentalDesk())
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
	// Run out, the insurance stays on the list as ended (review r5, #6).
	if _, err := pool.Exec(ctx, `UPDATE fleet_insurances SET valid_from = '2024-01-01', valid_to = current_date - 2`); err != nil {
		t.Fatal(err)
	}
	if k := kinds(); !slices.Contains(k, "rental_insurance_ended") {
		t.Errorf("an insurance that has ended: %v", k)
	}
}

// A booking ahead with nothing missing is not on the list; the same one
// whose insurance is gone is (review r5, #3: the list asks every booking
// in one statement, and must agree with the counter).
func TestOnlyABookingAheadThatLacksSomethingIsListed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v, _ := handoverReady(t, pool)
	soon, err := v.book(pool, time.Now().AddDate(0, 0, 5), time.Now().AddDate(0, 0, 6))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.AddRentalDriver(ctx, pool, rentalDesk(), soon, v.renter, true)
	if _, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), d, workshop.LicenceShown{Shown: true, Categories: []string{"B"},
		ValidUntil: day("2035-01-01"), Method: workshop.LicenceCardInspected}); err != nil {
		t.Fatal(err)
	}
	listed := func() bool {
		t.Helper()
		items, err := workshop.WaitingFor(ctx, pool, rentalDesk())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Kind == "rental_gaps" && it.SubjectID == soon {
				return true
			}
		}
		return false
	}
	if gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), soon); len(gaps) != 0 || listed() {
		t.Errorf("a booking with nothing missing (%v) is listed: %v", gapCodes(t, gaps), listed())
	}
	if _, err := pool.Exec(ctx, `DELETE FROM fleet_insurances`); err != nil {
		t.Fatal(err)
	}
	if gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), soon); len(gaps) != 1 || !listed() {
		t.Errorf("a booking with its insurance gone (%v) is not listed", gapCodes(t, gaps))
	}
}

// Whether a unit must be insured for rental is its profile's rule, not
// whether it has a registration: a profile of vehicles may not ask it, and
// one of equipment may (review r5, #9).
func TestInsuranceIsAProfilesRule(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if car := profileFor(t, pool, workshop.ProfileCar); !car.Rules.InsuranceRequired {
		t.Error("the car profile does not require insurance by default")
	}
	equipment := profileFor(t, pool, workshop.ProfileEquipment)
	if equipment.Rules.InsuranceRequired {
		t.Error("the equipment profile requires insurance by default")
	}
	rules := equipment.Rules
	rules.InsuranceRequired = true
	if _, err := workshop.SaveRentalProfile(ctx, pool, rentalOwner(), equipment.ID, rules); err != nil {
		t.Fatal(err)
	}
	tent := addUnit(t, pool, workshop.NewRentalUnit{Name: "Tält", FleetNumber: "T1", Category: "Tent", ProfileID: equipment.ID})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), tent, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	dayPlan(t, pool, "Tent", 10000, "at_start")
	renter, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Tina", Role: workshop.PartyRenter})
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: tent, RenterID: renter, From: in2031(10, 9), To: in2031(11, 9)})
	if err != nil {
		t.Fatal(err)
	}
	if gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id); !slices.Equal(gapCodes(t, gaps), []string{workshop.GapInsurance}) {
		t.Errorf("a tent whose profile requires insurance: %v", gapCodes(t, gaps))
	}
}

// A gap let go is let go for the rental as it was then: moved or
// extended, it is asked again. And a rental out is not extended past its
// insurance, as it is not past a licence (review r5, #4).
func TestALetGoGapAndTheInsuranceAreAskedAgainForANewPeriod(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideHandoverGap(ctx, pool, rentalOwner(), id, workshop.GapNotReported, "Anmäls i morgon"); err != nil {
		t.Fatal(err)
	}
	if gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id); slices.Contains(gapCodes(t, gaps), workshop.GapNotReported) {
		t.Fatalf("let go, and still there: %v", gapCodes(t, gaps))
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(13, 9), ""); err != nil {
		t.Fatal(err)
	}
	if gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id); !slices.Contains(gapCodes(t, gaps), workshop.GapNotReported) {
		t.Errorf("a gap let go for the old period, after an extension: %v", gapCodes(t, gaps))
	}
	// Still shown, who and why, as let go for an earlier period (review r6, #8).
	if let, err := workshop.RentalGapOverrides(ctx, pool, rentalDesk(), id); err != nil || len(let) != 1 || let[0].Current || let[0].Reason != "Anmäls i morgon" {
		t.Errorf("what was let go, after an extension: %+v, %v", let, err)
	}

	pool2 := setup(t)
	_, out, _ := checkedOut(t, pool2)
	b, _ := workshop.RentalBookingByID(ctx, pool2, rentalDesk(), out)
	u, _ := workshop.RentalUnitByID(ctx, pool2, rentalDesk(), b.UnitID)
	pool2.Exec(ctx, `DELETE FROM fleet_insurances`)
	end := day("2031-05-12")
	if _, err := workshop.RecordFleetInsurance(ctx, pool2, rentalDesk(), u.Memberships[0].ID, workshop.FleetInsurance{Insurer: "Bolaget", PolicyRef: "P",
		ValidFrom: day("2024-01-01"), ValidTo: &end, CoversRental: "yes"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool2, rentalDesk(), out, in2031(14, 9), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a rental out extended past its insurance: %v", err)
	}
}

// Insurance let go for the rental as booked does not carry over to a
// longer one: extended while out, it is asked for the new period
// (review r6, #1).
func TestAnInsuranceLetGoDoesNotCoverAnExtension(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id := handoverReady(t, pool)
	if _, err := pool.Exec(ctx, `DELETE FROM fleet_insurances`); err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideHandoverGap(ctx, pool, rentalOwner(), id, workshop.GapInsurance, "Försäkring på väg"); err != nil {
		t.Fatal(err)
	}
	out, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverOut)
	if err != nil {
		t.Fatal(err)
	}
	recordFound(t, pool, out, nil)
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out, workshop.HandoverReading{OdometerKm: km64(12000), RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(20, 9), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("extended past an insurance let go only for the booked period: %v", err)
	}
}
