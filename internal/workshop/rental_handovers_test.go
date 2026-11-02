package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Check-out and check-in (#194).

// handoverReady is a van booked for two days in 2031 with a permit, a
// checked driver and a checklist for both ends on its profile: nothing
// missing for the handover. 100 km a day are included, at 3 kr net over.
func handoverReady(t *testing.T, pool *pgxpool.Pool) (rentable, string) {
	t.Helper()
	ctx := context.Background()
	tpl, err := workshop.SaveChecklistTemplate(ctx, pool, owner(), "", "Utlämning och återlämning",
		[]string{"Lack och plåt", "Vänster dörr", "Fälgar"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetTemplateSubjects(ctx, pool, everything(owner()), tpl,
		[]string{workshop.SubjectCheckOut, workshop.SubjectCheckIn}); err != nil {
		t.Fatal(err)
	}
	car := profileFor(t, pool, workshop.ProfileCar)
	rules := car.Rules
	rules.CheckOutTemplateID, rules.CheckInTemplateID = tpl, tpl
	if _, err := workshop.SaveRentalProfile(ctx, pool, rentalOwner(), car.ID, rules); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), workshop.NewRentalPermit{
		Reference: "TSV 2024-1234", DecidedOn: day("2024-01-15"), Responsible: "Anna Andersson"}); err != nil {
		t.Fatal(err)
	}
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Bil"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	plan, _ := workshop.CreateRatePlan(ctx, pool, rentalOwner(), workshop.NewRatePlan{Name: "Bil", Category: "Bil", VATRateBp: 2500, SeasonRule: "at_start"})
	km := 100
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), plan, workshop.RateSeason{ValidFrom: day("2020-01-01"), Day: kr(40000),
		IncludedKmPerDay: &km, ExtraKmMinor: kr(300)}); err != nil {
		t.Fatal(err)
	}
	renter, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Rita Hyrare", Role: workshop.PartyRenter})
	v := rentable{unit: unit, renter: renter, plan: plan}
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, renter, true)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, workshop.LicenceShown{Shown: true, Categories: []string{"B"},
		ValidUntil: day("2035-01-01"), Method: workshop.LicenceCardInspected}); err != nil || !ok {
		t.Fatalf("the licence check: %v, %v", ok, err)
	}
	return v, id
}

// recordFound sets each checkpoint of a protocol, by label, to a status.
func recordFound(t *testing.T, pool *pgxpool.Pool, protocol string, found map[string]string) workshop.Protocol {
	t.Helper()
	ctx := context.Background()
	p, err := workshop.ProtocolByID(ctx, pool, rentalDesk(), protocol)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		status := found[it.Label]
		if status == "" {
			status = "pass"
		}
		if err := workshop.RecordProtocolItem(ctx, pool, rentalDesk(), it.ID, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	p, _ = workshop.ProtocolByID(ctx, pool, rentalDesk(), protocol)
	return p
}

func itemID(p workshop.Protocol, label string) string {
	for _, it := range p.Items {
		if it.Label == label {
			return it.ID
		}
	}
	return ""
}

// The acceptance test: a van goes out with a dent the desk records, comes
// back with that dent and a new scratch, and only the scratch becomes a
// damage case -- observed, and no charge. The handover times are stored,
// the odometer is read twice, and the kilometres over the allowance and
// the fuel short are proposed, not charged.
func TestOnlyANewFindingAtReturnBecomesADamageCase(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v, id := handoverReady(t, pool)

	out, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverOut)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverOut); err != nil || again != out {
		t.Errorf("a second tap on check-out: %s, %v; want the protocol under way %s", again, err, out)
	}
	recordFound(t, pool, out, map[string]string{"Vänster dörr": "attention"}) // the known dent
	if _, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a check-in before the check-out: %v", err)
	}
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out, workshop.HandoverReading{FuelEighths: ptr(8), RenterPresent: true}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a car handed over with no odometer reading: %v", err)
	}
	reading := workshop.HandoverReading{OdometerKm: km64(12000), FuelEighths: ptr(8), RenterPresent: true, RenterAccepted: true}
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out, reading); err != nil {
		t.Fatal(err)
	}
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out, reading); err != nil {
		t.Errorf("the same check-out sent again: %v", err)
	}
	if b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id); b.State != workshop.RentalCheckedOut {
		t.Fatalf("after check-out the booking is %s", b.State)
	}

	in, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
	if err != nil {
		t.Fatal(err)
	}
	back := recordFound(t, pool, in, map[string]string{"Vänster dörr": "attention", "Lack och plåt": "attention"})
	view, p, err := workshop.RentalHandoverOfProtocol(ctx, pool, rentalDesk(), in)
	if err != nil || p.ID != in || view.Comparison == nil {
		t.Fatalf("the check-in beside the check-out: %+v, %v", view, err)
	}
	for _, c := range view.Comparison.Items {
		if want := c.Label == "Lack och plåt"; c.New != want || c.Out == nil {
			t.Errorf("%s: new %v beside %+v", c.Label, c.New, c.Out)
		}
	}
	// The dent was there when it went out: it is not a new finding, and
	// only the scratch becomes a case.
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), in, workshop.HandoverReading{OdometerKm: km64(12350), FuelEighths: ptr(5),
		Note: "Nyckeln i lådan"}); err != nil {
		t.Fatal(err)
	}

	view, err = workshop.RentalHandovers(ctx, pool, rentalDesk(), id)
	if err != nil || view.Out == nil || view.In == nil {
		t.Fatalf("the handovers: %+v, %v", view, err)
	}
	if view.In.RenterPresent || *view.Out.OdometerKm != 12000 || *view.In.OdometerKm != 12350 || view.In.At.Before(view.Out.At) {
		t.Errorf("the two ends: %+v and %+v", view.Out, view.In)
	}
	if len(view.Damage) != 1 || view.Damage[0].State != "observed" || view.Damage[0].BookingID != id {
		t.Errorf("the damage register after return: %+v", view.Damage)
	}
	// Two days, 200 km included: 150 over at 3 kr; three eighths short.
	if len(view.Proposed) != 2 || view.Proposed[0].Kind != workshop.ChargeExtraKm || view.Proposed[0].Quantity != 150 ||
		*view.Proposed[0].UnitPriceMinor != *kr(300) || view.Proposed[1].Kind != workshop.ChargeFuel || view.Proposed[1].Quantity != 3 {
		t.Errorf("proposed: %+v", view.Proposed)
	}
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	if b.State != workshop.RentalReturned || b.Totals.GrossMinor != 2*50000 {
		t.Errorf("returned, and nothing charged by the return: %s, %d", b.State, b.Totals.GrossMinor)
	}
	// Back early, the time it was booked for is free again.
	if _, err := v.book(pool, in2031(11, 9), in2031(11, 18)); err != nil {
		t.Errorf("the unit booked after an early return: %v", err)
	}
	if p, err := workshop.ProtocolByID(ctx, pool, rentalDesk(), in); err != nil || p.CompletedAt == nil {
		t.Errorf("the check-in protocol is finished and frozen: %+v, %v", p, err)
	}
	if err := workshop.RecordProtocolItem(ctx, pool, rentalDesk(), itemID(back, "Fälgar"), "fail", ""); !errors.Is(err, workshop.ErrProtocolFinished) {
		t.Errorf("a finished check-in changed: %v", err)
	}
}

// A check-out is refused while anything is missing for the handover, and
// by anyone but the rental desk and whoever runs the shop.
func TestACheckOutIsRefusedWhileSomethingIsMissing(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool) // a car with no permit and no driver
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	_, err = workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverOut)
	var gaps *workshop.HandoverGapsError
	if !errors.As(err, &gaps) || len(gaps.Gaps) == 0 || !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a check-out with things missing: %v", err)
	}
	for _, s := range []access.Scope{renting(access.RoleTechnician), advisor()} {
		if _, err := workshop.StartHandover(ctx, pool, s, id, workshop.HandoverOut); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%v checking out: %v", s.Role, err)
		}
	}
}

// Equipment has no odometer and no tank: its handover reads neither, and
// its protocol is of the unit, with no vehicle.
func TestEquipmentIsHandedOverWithNoOdometer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl, _ := workshop.SaveChecklistTemplate(ctx, pool, owner(), "", "Tält", []string{"Duk", "Pinnar"})
	if err := workshop.SetTemplateSubjects(ctx, pool, everything(owner()), tpl,
		[]string{workshop.SubjectCheckOut, workshop.SubjectCheckIn}); err != nil {
		t.Fatal(err)
	}
	equipment := profileFor(t, pool, workshop.ProfileEquipment)
	rules := equipment.Rules
	rules.CheckOutTemplateID, rules.CheckInTemplateID = tpl, tpl
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
	for _, kind := range []string{workshop.HandoverOut, workshop.HandoverIn} {
		p, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		recordFound(t, pool, p, nil)
		if err := workshop.FinishHandover(ctx, pool, rentalDesk(), p, workshop.HandoverReading{RenterPresent: true}); err != nil {
			t.Fatalf("finish %s: %v", kind, err)
		}
	}
	view, err := workshop.RentalHandovers(ctx, pool, rentalDesk(), id)
	if err != nil || view.HasOdometer || view.In == nil || view.In.OdometerKm != nil || len(view.Proposed) != 0 {
		t.Errorf("a tent's handovers: %+v, %v", view, err)
	}
}

func km64(n int64) *int64 { return &n }

// checkedOut is handoverReady handed over: the check-out finished.
func checkedOut(t *testing.T, pool *pgxpool.Pool) (rentable, string, string) {
	t.Helper()
	ctx := context.Background()
	v, id := handoverReady(t, pool)
	out, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverOut)
	if err != nil {
		t.Fatal(err)
	}
	recordFound(t, pool, out, nil)
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), out, workshop.HandoverReading{OdometerKm: km64(12000), FuelEighths: ptr(8), RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	return v, id, out
}

// A unit that went out always comes back: with no check-in checklist on
// its profile, or the one there taken out of use since, the check-in
// walks the check-out's own checkpoints (review of R6, #1).
func TestACheckInNeedsNoChecklistOfItsOwn(t *testing.T) {
	ctx := context.Background()
	for name, take := range map[string]string{
		"none on the profile":    `UPDATE rental_profile_versions SET check_in_template_id = NULL`,
		"taken out of use since": `UPDATE inspection_templates SET active = false`,
	} {
		t.Run(name, func(t *testing.T) {
			pool := setup(t)
			_, id, out := checkedOut(t, pool)
			if _, err := pool.Exec(ctx, take); err != nil {
				t.Fatal(err)
			}
			in, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
			if err != nil {
				t.Fatalf("checking in: %v", err)
			}
			was, _ := workshop.ProtocolByID(ctx, pool, rentalDesk(), out)
			p := recordFound(t, pool, in, nil)
			if len(p.Items) != len(was.Items) || p.Items[0].Key != was.Items[0].Key {
				t.Errorf("the check-in's checkpoints %+v, the check-out's %+v", p.Items, was.Items)
			}
			if err := workshop.FinishHandover(ctx, pool, rentalDesk(), in, workshop.HandoverReading{OdometerKm: km64(12100), RenterPresent: true}); err != nil {
				t.Errorf("finishing the check-in: %v", err)
			}
		})
	}
}

// After the return, its comparison still says what was new at it: the
// damage case the check-in opened is its own finding, not damage known
// before it (review of R6, #5).
func TestAReturnsOwnDamageIsNewToIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id, _ := checkedOut(t, pool)
	left, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.AbandonHandover(ctx, pool, rentalDesk(), left, "Fel bil"); err != nil {
		t.Fatal(err)
	}
	in, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
	if err != nil || in == left {
		t.Fatalf("a new check-in after one left: %s, %v", in, err)
	}
	recordFound(t, pool, in, map[string]string{"Lack och plåt": "fail"})
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), in, workshop.HandoverReading{OdometerKm: km64(12100), RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	view, err := workshop.RentalHandovers(ctx, pool, rentalDesk(), id)
	if err != nil || view.Comparison == nil {
		t.Fatalf("%+v, %v", view, err)
	}
	for _, c := range view.Comparison.Items {
		if c.Label == "Lack och plåt" && (!c.New || c.Known) {
			t.Errorf("the scratch found at this return: new %v, known %v", c.New, c.Known)
		}
	}
}

// A check-in left unfinished is compared as itself, not as the one
// finished after it (review of R6, #4).
func TestACheckInLeftIsComparedAsItself(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id, _ := checkedOut(t, pool)
	left, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.AbandonHandover(ctx, pool, rentalDesk(), left, "Fel bil"); err != nil {
		t.Fatal(err)
	}
	in, _ := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
	recordFound(t, pool, in, nil)
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), in, workshop.HandoverReading{OdometerKm: km64(12100), RenterPresent: true}); err != nil {
		t.Fatal(err)
	}
	lv, p, err := workshop.RentalHandoverOfProtocol(ctx, pool, rentalDesk(), left)
	if err != nil || p.ID != left || lv.Comparison == nil {
		t.Fatalf("the check-in left: %+v, %v", lv, err)
	}
	for _, c := range lv.Comparison.Items {
		if itemID(p, c.Label) != c.In.ID {
			t.Errorf("the check-in left is shown with another protocol's checkpoint %s", c.Label)
		}
	}
}

// The finish of a check-in may be sent from a phone that was offline,
// after answers it queued: what the page showed as new when it was
// filled in is not what is new when the finish arrives. Every finding
// new at the finish becomes a damage case unless the person let it go as
// not damage; one let go that is no longer new is passed over. Nothing
// is refused for it, so the queue behind it keeps going (review of R6, #2).
func TestAFinishSentAfterQueuedAnswersMissesNoNewFinding(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, id, _ := checkedOut(t, pool)
	in, err := workshop.StartHandover(ctx, pool, rentalDesk(), id, workshop.HandoverIn)
	if err != nil {
		t.Fatal(err)
	}
	// The page showed the rims as new, and the person let them go as dirt;
	// the queued answers then put the rims right and found the door torn.
	back := recordFound(t, pool, in, map[string]string{"Fälgar": "attention"})
	rims := itemID(back, "Fälgar")
	recordFound(t, pool, in, map[string]string{"Vänster dörr": "fail"})
	if err := workshop.FinishHandover(ctx, pool, rentalDesk(), in, workshop.HandoverReading{OdometerKm: km64(12100), RenterPresent: true,
		Dismissed: []string{rims, "99999999-0000-0000-0000-000000000009"}}); err != nil {
		t.Fatalf("the finish behind the queued answers: %v", err)
	}
	view, _ := workshop.RentalHandovers(ctx, pool, rentalDesk(), id)
	if len(view.Damage) != 1 || view.Damage[0].Description != "Vänster dörr" {
		t.Errorf("the damage register: %+v; want the torn door alone", view.Damage)
	}
}
