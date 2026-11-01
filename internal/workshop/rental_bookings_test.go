package workshop_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rentable is a van in the fleet since long ago, priced 400 kr a day net,
// with a renter: what a booking needs.
type rentable struct {
	unit, resource, renter, plan string
}

func rentableVan(t *testing.T, pool *pgxpool.Pool) rentable {
	t.Helper()
	ctx := context.Background()
	unit := addUnit(t, pool, workshop.NewRentalUnit{Registration: "ABC 12D", FleetNumber: "V1", Category: "Small van"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	u, err := workshop.RentalUnitByID(ctx, pool, rentalDesk(), unit)
	if err != nil {
		t.Fatal(err)
	}
	renter, err := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Rita Hyrare", Role: workshop.PartyRenter})
	if err != nil {
		t.Fatal(err)
	}
	return rentable{unit: unit, resource: u.ResourceID, renter: renter, plan: dayPlan(t, pool, "Small van", 40000, "at_start")}
}

func (r rentable) book(pool *pgxpool.Pool, from, to time.Time) (string, error) {
	return workshop.BookRental(context.Background(), pool, rentalDesk(), workshop.NewRentalBooking{
		UnitID: r.unit, RenterID: r.renter, From: from, To: to})
}

// A confirmed booking freezes its price lines and blocks its unit, the
// cleaning buffer after it included; a later price changes nothing agreed.
func TestABookingFreezesItsPricesAndBlocksItsUnit(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	b, err := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	if err != nil || b.State != workshop.RentalConfirmed || b.Number != 1 || len(b.Lines) != 1 || b.Totals.GrossMinor != 2*50000 || b.RenterName != "Rita Hyrare" {
		t.Fatalf("the booking: %+v, %v", b, err)
	}
	blocks, _ := workshop.ResourceBlocks(ctx, pool, rentalDesk(), v.resource, in2031(1, 0), time.Time{})
	if len(blocks) != 1 || blocks[0].SourceID != id || !blocks[0].To.Equal(in2031(12, 10)) || blocks[0].After != time.Hour {
		t.Errorf("the block, an hour's cleaning after: %+v", blocks)
	}
	// The buffer is time the van is not free.
	if _, err := v.book(pool, in2031(12, 9).Add(30*time.Minute), in2031(13, 9)); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("a rental inside the cleaning buffer: %v", err)
	}
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), v.plan, workshop.RateSeason{ValidFrom: day("2030-01-01"), Day: kr(99900)}); err != nil {
		t.Fatal(err)
	}
	if again, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id); again.Totals.GrossMinor != b.Totals.GrossMinor || len(again.Lines) != 1 {
		t.Errorf("a new season changed an agreed price: %+v, was %+v", again.Totals, b.Totals)
	}
}

// Two bookings of the same van at the same moment: one stands, and the
// other is told it is taken by a rental.
func TestTwoBookingsAtOnceOneStands(t *testing.T) {
	pool := setup(t)
	v := rentableVan(t, pool)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = v.book(pool, in2031(10, 9), in2031(11, 9))
		}()
	}
	wg.Wait()
	ok, taken := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, workshop.ErrResourceTaken):
			taken++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || taken != 1 {
		t.Errorf("%d stood and %d were refused; want one of each", ok, taken)
	}
}

// The same request sent twice -- a phone that lost the answer -- is one
// booking, whether the second comes after the first or beside it.
func TestARepeatedBookingRequestIsOneBooking(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	n := workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter, From: in2031(10, 9), To: in2031(11, 9), Key: "phone-1"}
	var wg sync.WaitGroup
	ids, errs := make([]string, 2), make([]error, 2)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = workshop.BookRental(ctx, pool, rentalDesk(), n)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || ids[0] != ids[1] {
		t.Errorf("the same request twice at once: %v %v, %v %v", ids[0], errs[0], ids[1], errs[1])
	}
	again, err := workshop.BookRental(ctx, pool, rentalDesk(), n)
	if err != nil || again != ids[0] {
		t.Errorf("the same request later: %s, %v", again, err)
	}
	n.UnitID = addUnit(t, pool, workshop.NewRentalUnit{Name: "Släp", FleetNumber: "S1"})
	if _, err := workshop.BookRental(ctx, pool, rentalDesk(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the key used again for another unit: %v", err)
	}
}

// A hold blocks until it runs out; then it no longer does, the booking
// says so, and it cannot be confirmed.
func TestAHoldRunsOut(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	until := time.Now().Add(time.Hour)
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
		From: in2031(10, 9), To: in2031(11, 9), HoldUntil: &until})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id); b.State != workshop.RentalHeld || b.HeldUntil == nil {
		t.Errorf("held: %+v", b)
	}
	if _, err := v.book(pool, in2031(10, 12), in2031(10, 15)); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("over a hold: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE resource_blocks SET expires_at = now() - interval '1 minute' WHERE source_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id); b.State != workshop.RentalHoldExpired {
		t.Errorf("a hold run out reads as %s", b.State)
	}
	if err := workshop.ConfirmRentalBooking(ctx, pool, rentalDesk(), id); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("confirming a hold run out: %v", err)
	}
	if _, err := v.book(pool, in2031(10, 12), in2031(10, 15)); err != nil {
		t.Errorf("over a hold run out: %v", err)
	}
}

// A van in the workshop is not rented until whoever runs the shop has
// replanned the job's block, with a reason.
func TestAVanInTheWorkshopIsRentedOnlyAfterReplanning(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	newJob(t, pool)
	if _, err := v.book(pool, in2031(10, 9), in2031(11, 9)); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Fatalf("renting a van in the workshop: %v", err)
	}
	blocks, _ := workshop.ResourceBlocks(ctx, pool, rentalOwner(), v.resource, time.Now().Add(-time.Hour), time.Time{})
	if len(blocks) != 1 || blocks[0].Kind != workshop.BlockWorkshop {
		t.Fatalf("the job's block: %+v", blocks)
	}
	if err := workshop.OverrideBlock(ctx, pool, rentalDesk(), blocks[0].ID, time.Time{}, time.Time{}, "x"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk replanning the workshop: %v", err)
	}
	if err := workshop.OverrideBlock(ctx, pool, rentalOwner(), blocks[0].ID, time.Time{}, time.Time{}, "Kunden behöver bilen, jobbet tas efter"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.book(pool, in2031(10, 9), in2031(11, 9)); err != nil {
		t.Errorf("after replanning: %v", err)
	}
}

// Moving keeps the length and moves the block; extending adds lines of its
// own, priced now; both are refused over something else; cancelling lets
// the block go and keeps the booking.
func TestMovingExtendingAndCancellingABooking(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	id, err := v.book(pool, in2031(10, 9), in2031(11, 9))
	if err != nil {
		t.Fatal(err)
	}
	other, err := v.book(pool, in2031(20, 9), in2031(21, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveRentalBooking(ctx, pool, rentalDesk(), id, in2031(12, 9), in2031(14, 9), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a move that makes it longer: %v", err)
	}
	if err := workshop.MoveRentalBooking(ctx, pool, rentalDesk(), id, in2031(20, 12), in2031(21, 12), ""); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("a move onto another rental: %v", err)
	}
	if err := workshop.MoveRentalBooking(ctx, pool, rentalDesk(), id, in2031(15, 9), in2031(16, 9), "Kunden ville senare"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(20, 9), ""); !errors.Is(err, workshop.ErrResourceTaken) {
		t.Errorf("an extension into the next rental's buffer-free start: %v", err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(18, 9), "En vecka till"); err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	if !b.Starts.Equal(in2031(15, 9)) || !b.Ends.Equal(in2031(18, 9)) || len(b.Lines) != 2 || b.Lines[1].QuantityMilli != 2000 || b.Totals.NetMinor != 3*40000 {
		t.Errorf("moved and extended: %+v", b)
	}
	if len(b.Events) != 3 || b.Events[1].Kind != "moved" || b.Events[2].Kind != "extended" || !b.Events[2].OldEnds.Equal(in2031(16, 9)) {
		t.Errorf("what happened: %+v", b.Events)
	}
	if err := workshop.CancelRentalBooking(ctx, pool, rentalDesk(), other, "", false); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("cancelling without saying why: %v", err)
	}
	if err := workshop.CancelRentalBooking(ctx, pool, rentalDesk(), other, "Kunden avbokade", true); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a no-show before the rental started: %v", err)
	}
	if err := workshop.CancelRentalBooking(ctx, pool, rentalDesk(), other, "Kunden avbokade", false); err != nil {
		t.Fatal(err)
	}
	if _, err := v.book(pool, in2031(20, 9), in2031(21, 9)); err != nil {
		t.Errorf("the cancelled booking's time: %v", err)
	}
	if b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), other); b.State != workshop.RentalCancelled || len(b.Lines) != 1 {
		t.Errorf("the cancelled booking: %+v", b)
	}
}

// Outside the unit's time in the fleet, for a party that does not rent, or
// as a role or in a shop without Rental: refused.
func TestWhatABookingIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	if _, err := v.book(pool, time.Now().AddDate(-2, 0, 0), time.Now().AddDate(-2, 0, 1)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("before the van joined the fleet: %v", err)
	}
	all := otherRoles(access.Of(access.RoleOwner))
	seller, err := workshop.CreateParty(ctx, pool, all, workshop.NewParty{Kind: "private", Name: "Sam Säljare", Role: workshop.PartySeller})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.BookRental(ctx, pool, all, workshop.NewRentalBooking{UnitID: v.unit, RenterID: seller,
		From: in2031(10, 9), To: in2031(11, 9)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a party that is no renter: %v", err)
	}
	// The rental desk is not shown a party it does not deal with at all.
	if _, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: seller,
		From: in2031(10, 9), To: in2031(11, 9)}); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("the rental desk naming a seller: %v", err)
	}
	n := workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter, From: in2031(10, 9), To: in2031(11, 9)}
	for _, r := range []access.Role{access.RoleTechnician, access.RoleServiceAdvisor, access.RoleSales} {
		if _, err := workshop.BookRental(ctx, pool, renting(r), n); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s booking: %v", r, err)
		}
	}
	off := rentalDesk()
	off.Modules = workshopOnly
	if _, err := workshop.BookRental(ctx, pool, off, n); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("booking with Rental off: %v", err)
	}
}

// A van not back by its end keeps it from the next booking, which is at
// risk; nothing new is booked on it until it is back.
func TestAnOverdueReturnPutsTheNextBookingAtRisk(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	out, err := v.book(pool, time.Now().Add(-48*time.Hour), time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	next, err := v.book(pool, in2031(10, 9), in2031(11, 9))
	if err != nil {
		t.Fatal(err)
	}
	// Handed over: check-out (#194) writes this event.
	if err := asApp(ctx, pool, `INSERT INTO rental_booking_events (shop_id, booking_id, kind) VALUES ('`+shopID+`', '`+out+`', 'checked_out')`); err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), out)
	if !b.Overdue(time.Now()) {
		t.Errorf("the van out past its end: %+v", b)
	}
	if n, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), next); !n.AtRisk {
		t.Errorf("the next booking is not at risk: %+v", n)
	}
	if _, err := v.book(pool, in2031(12, 9), in2031(13, 9)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a new booking on an overdue van: %v", err)
	}
	list, err := workshop.RentalBookings(ctx, pool, rentalDesk(), time.Now(), time.Now().Add(24*time.Hour))
	if err != nil || len(list) != 1 || list[0].ID != out {
		t.Errorf("today's list has the overdue van: %+v, %v", list, err)
	}
}

// A price a person changes says from what, who and why, on the line and
// in the audit trail.
func TestAChangedPriceSaysWhoAndWhy(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	n := workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter, From: in2031(10, 9), To: in2031(11, 9),
		Overrides: []workshop.PriceOverride{{Position: 1, UnitPriceMinor: 30000}}}
	if _, err := workshop.BookRental(ctx, pool, rentalDesk(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a price changed with no reason: %v", err)
	}
	n.Overrides[0].Reason = "Stamkund"
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), n)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	if l := b.Lines[0]; l.UnitPriceMinor != 30000 || l.PricedMinor == nil || *l.PricedMinor != 40000 || l.OverrideReason != "Stamkund" || l.OverrideBy == "" {
		t.Errorf("the changed line: %+v", l)
	}
	trail, err := workshop.AuditTrail(ctx, pool, rentalDesk(), "rental_booking", id)
	if err != nil || len(trail) != 1 || trail[0].Event != "rental.price_overridden" || trail[0].Reason != "Stamkund" {
		t.Errorf("the trail: %+v, %v", trail, err)
	}
}

// rentalAudited makes Rental's audited transitions happen, for
// TestEveryAuditedTransitionLeavesAnEvent, and says what each was about.
func rentalAudited(t *testing.T, pool *pgxpool.Pool, made func(event, kind, id string)) {
	t.Helper()
	v := rentableVan(t, pool)
	id, err := workshop.BookRental(context.Background(), pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
		From: in2031(10, 9), To: in2031(11, 9), Overrides: []workshop.PriceOverride{{Position: 1, UnitPriceMinor: 1, Reason: "Prov"}}})
	if err != nil {
		t.Fatal(err)
	}
	made("rental.price_overridden", "rental_booking", id)
}

// 0303 widens audit_events' subjects by what the constraint lists, so a
// subject Core had and Rental's are both taken, the constraint still reads
// back in the form the next widening reads, and nothing else is let in.
func TestAuditSubjectsWidenedKeepWhatWasThere(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, kind := range []string{"shop", "work_order", "identity_record", "rental_booking"} {
		if err := asApp(ctx, pool, `INSERT INTO audit_events (shop_id, subject_kind, subject_id, event)
			VALUES ('`+shopID+`', '`+kind+`', '`+shopID+`', 'test.made')`); err != nil {
			t.Errorf("a %s event: %v", kind, err)
		}
	}
	if err := asApp(ctx, pool, `INSERT INTO audit_events (shop_id, subject_kind, subject_id, event)
		VALUES ('`+shopID+`', 'bakery', '`+shopID+`', 'test.made')`); err == nil {
		t.Error("a subject nobody added was let in")
	}
	var kinds []string
	if err := pool.QueryRow(ctx, `
		SELECT array_agg(m[1]) FROM pg_constraint c, regexp_matches(pg_get_constraintdef(c.oid), '''([a-z_]+)''::text', 'g') AS m
		WHERE c.conname = 'audit_events_subject_kind_check'`).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(kinds, "rental_booking") || !slices.Contains(kinds, "identity_record") || len(kinds) < 11 {
		t.Errorf("the constraint reads back as %v", kinds)
	}
}

// The rental desk's day is in the one queue and on the home page: what goes
// out today, what is out past its return, and the booking after it at
// risk. Nobody else's queue has them, and switching Rental off lists the
// bookings still open first.
func TestTheRentalDesksDayIsInTheQueueAndOnTheHomePage(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	today, err := v.book(pool, time.Now().Add(-time.Minute), time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	trailerUnit := addUnit(t, pool, workshop.NewRentalUnit{Name: "Släp", FleetNumber: "S1", Category: "Small van"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), trailerUnit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	late, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: trailerUnit, RenterID: v.renter,
		From: time.Now().Add(-48 * time.Hour), To: time.Now().Add(-3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := asApp(ctx, pool, `INSERT INTO rental_booking_events (shop_id, booking_id, kind) VALUES ('`+shopID+`', '`+late+`', 'checked_out')`); err != nil {
		t.Fatal(err)
	}
	items, err := workshop.WaitingFor(ctx, pool, rentalDesk())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, it := range items {
		kinds[it.Kind] = it.SubjectID
		if it.Link() != "/rental/bookings/"+it.SubjectID || it.Says() == "" {
			t.Errorf("an item that leads nowhere or says nothing: %+v", it)
		}
	}
	if kinds["rental_handover"] != today || kinds["rental_overdue"] != late {
		t.Errorf("the desk's queue: %+v", items)
	}
	home, errs := workshop.HomeFor(ctx, pool, rentalDesk())
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	titles := map[string]int{}
	for _, c := range home.Actions {
		titles[c.Title] = c.Count
	}
	if titles["%d rental to hand over today"] != 1 || titles["%d rental out past its return"] != 1 {
		t.Errorf("the desk's home page: %+v", home.Actions)
	}
	for _, s := range []access.Scope{technician(), advisor()} {
		items, _ := workshop.WaitingFor(ctx, pool, s)
		for _, it := range items {
			if it.Module == access.Rental {
				t.Errorf("%v was given the rental desk's %s", s.Role, it.Kind)
			}
		}
	}
	if _, err := workshop.SwitchModule(ctx, pool, rentalOwner(), access.Rental, true, "", false); err != nil {
		t.Fatal(err)
	}
	_, err = workshop.SwitchModule(ctx, pool, rentalOwner(), access.Rental, false, "", false)
	open, ok := workshop.AsOpenBusiness(err)
	if !ok || len(open.Items) != 2 || open.Items[0].Link != "/rental/bookings/"+late {
		t.Errorf("switching Rental off with two rentals open: %+v, %v", open, err)
	}
}

// The booking after an overdue rental is in the queue as at risk, whatever
// day it starts, and never as a handover today when it is not (review #8).
func TestABookingAtRiskIsQueuedAsAtRisk(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	out, err := v.book(pool, time.Now().Add(-48*time.Hour), time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	next, err := v.book(pool, in2031(10, 9), in2031(11, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := asApp(ctx, pool, `INSERT INTO rental_booking_events (shop_id, booking_id, kind) VALUES ('`+shopID+`', '`+out+`', 'checked_out')`); err != nil {
		t.Fatal(err)
	}
	items, err := workshop.WaitingFor(ctx, pool, rentalDesk())
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, it := range items {
		if it.SubjectID == next {
			kinds = append(kinds, it.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != "rental_at_risk" {
		t.Errorf("the booking after the overdue van is queued as %v", kinds)
	}
}
