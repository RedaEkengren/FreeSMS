package workshop_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// After every migration, audit_events takes a subject Core had and
// Rental's (0103, through widen_text_check), and refuses one nobody added:
// a list widened into nothing would let every value in.
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

// A booking and the end of the unit's time in the fleet, made at the same
// moment, never both stand: a rental on a unit sold or returned from lease.
// The cleaning buffer after a rental is time in the fleet too (review #3).
func TestABookingAndTheFleetsEndDoNotBothStand(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	u, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), v.unit)
	membership := u.Memberships[0].ID
	for i := 0; i < 8; i++ {
		from := in2031(1+2*i, 9)
		var wg sync.WaitGroup
		var booked, ended error
		wg.Add(2)
		go func() { defer wg.Done(); _, booked = v.book(pool, from, from.Add(24*time.Hour)) }()
		go func() {
			defer wg.Done()
			ended = workshop.EndFleetMembership(ctx, pool, rentalDesk(), membership, from.Add(-time.Hour), "sold", "")
		}()
		wg.Wait()
		if booked == nil && ended == nil {
			t.Fatalf("round %d: a rental and the van's sale both stood", i)
		}
		if ended == nil {
			return // sold: the rounds are over
		}
	}
}

// A rental whose cleaning buffer runs past the unit's last day in the
// fleet is refused.
func TestABuffersTimeIsInTheFleet(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	u, _ := workshop.RentalUnitByID(ctx, pool, rentalDesk(), v.unit)
	if err := workshop.EndFleetMembership(ctx, pool, rentalDesk(), u.Memberships[0].ID, in2031(11, 9).Add(30*time.Minute), "sold", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.book(pool, in2031(10, 9), in2031(11, 9)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an hour's cleaning after the van was sold: %v", err)
	}
}

// A booking for somebody new, sent twice with its key -- one after the
// other, or both at once -- is one booking and one new renter (review #4).
func TestARepeatedBookingForSomebodyNewMakesThemOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	n := workshop.NewRentalBooking{UnitID: v.unit, From: in2031(10, 9), To: in2031(11, 9), Key: "phone-2",
		NewRenter: &workshop.NewParty{Kind: "private", Name: "Nina Ny", Phone: "070-1"}}
	var wg sync.WaitGroup
	ids, errs := make([]string, 2), make([]error, 2)
	for i := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); ids[i], errs[i] = workshop.BookRental(ctx, pool, rentalDesk(), n) }()
	}
	wg.Wait()
	again, err := workshop.BookRental(ctx, pool, rentalDesk(), n)
	if errs[0] != nil || errs[1] != nil || err != nil || ids[0] != ids[1] || again != ids[0] {
		t.Fatalf("three sends: %v %v %v, %v %v %v", ids, again, "", errs[0], errs[1], err)
	}
	var made int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM people WHERE display_name = 'Nina Ny'`).Scan(&made); err != nil || made != 1 {
		t.Errorf("Nina was made %d times, %v", made, err)
	}
}

// An extension charges a per-day extra for the days it adds, and adds the
// kilometres the plan includes for them (review #1).
func TestAnExtensionCarriesItsExtrasAndKilometres(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
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
	seat, _ := workshop.AddRentalExtra(ctx, pool, rentalOwner(), workshop.NewRentalExtra{Kind: "child_seat", Name: "Barnstol", Per: "day",
		PriceMinor: 5000, VATRateBp: 2500, VATCategory: "S"})
	waiver, _ := workshop.AddRentalExtra(ctx, pool, rentalOwner(), workshop.NewRentalExtra{Kind: "delivery", Name: "Leverans", Per: "rental",
		PriceMinor: 20000, VATRateBp: 2500, VATCategory: "S"})
	renter, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "R", Role: workshop.PartyRenter})
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: unit, RenterID: renter,
		From: in2031(10, 9), To: in2031(11, 9), Extras: []workshop.ExtraChoice{{ExtraID: seat, Count: 2}, {ExtraID: waiver, Count: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(17, 9), "En vecka"); err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	var seats, deliveries int64
	for _, l := range b.Lines {
		switch l.ExtraID {
		case seat:
			seats += l.QuantityMilli
		case waiver:
			deliveries += l.QuantityMilli
		}
	}
	if seats != 2*7*1000 || deliveries != 1000 {
		t.Errorf("two seats for seven days and one delivery: %d seat-days, %d deliveries (%+v)", seats/1000, deliveries/1000, b.Lines)
	}
	if b.IncludedKm == nil || *b.IncludedKm != 700 {
		t.Errorf("seven days at 100 km: %v", b.IncludedKm)
	}
}

// A hold is not moved to start before it runs out: the renter would have
// the van before saying yes (review).
func TestAHoldIsNotMovedToStartBeforeItRunsOut(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	until := time.Now().Add(48 * time.Hour)
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
		From: in2031(10, 9), To: in2031(11, 9), HoldUntil: &until})
	if err != nil {
		t.Fatal(err)
	}
	from := time.Now().Add(24 * time.Hour)
	if err := workshop.MoveRentalBooking(ctx, pool, rentalDesk(), id, from, from.Add(24*time.Hour), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a hold moved to start before it runs out: %v", err)
	}
}

// Cancelling a hold that has run out while somebody books over its time
// never deadlocks: both take the unit first (review 2 #1).
func TestCancellingARunOutHoldBesideABookingDoesNotDeadlock(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	for i := 0; i < 6; i++ {
		from := in2031(1+3*i, 9)
		until := time.Now().Add(time.Hour)
		held, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
			From: from, To: from.Add(24 * time.Hour), HoldUntil: &until})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE resource_blocks SET expires_at = now() - interval '1 minute' WHERE source_id = $1`, held); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var cancelled, booked error
		wg.Add(2)
		go func() {
			defer wg.Done()
			cancelled = workshop.CancelRentalBooking(ctx, pool, rentalDesk(), held, "Inget svar", false)
		}()
		go func() { defer wg.Done(); _, booked = v.book(pool, from.Add(time.Hour), from.Add(5*time.Hour)) }()
		wg.Wait()
		for _, err := range []error{cancelled, booked} {
			if err != nil && strings.Contains(err.Error(), "deadlock") {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		if booked != nil {
			t.Errorf("round %d: booking over a run-out hold: %v", i, booked)
		}
	}
}

// Sent twice at once with a new renter's email, the second finds the first
// booking rather than being told the email is taken (review 2 #2).
func TestARepeatedBookingWithAnEmailFindsTheFirst(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	for i := 0; i < 4; i++ {
		from := in2031(1+3*i, 9)
		n := workshop.NewRentalBooking{UnitID: v.unit, From: from, To: from.Add(24 * time.Hour), Key: "mail-" + fmt.Sprint(i),
			NewRenter: &workshop.NewParty{Kind: "private", Name: "Ella", Email: fmt.Sprintf("ella%d@example.test", i)}}
		var wg sync.WaitGroup
		ids, errs := make([]string, 2), make([]error, 2)
		for j := range ids {
			wg.Add(1)
			go func() { defer wg.Done(); ids[j], errs[j] = workshop.BookRental(ctx, pool, rentalDesk(), n) }()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || ids[0] != ids[1] {
			t.Fatalf("round %d: %v %v, %v %v", i, ids[0], errs[0], ids[1], errs[1])
		}
	}
}

// A booking agreed with no kilometre limit stays unlimited when it is
// extended into a season that has one (review 2 #4).
func TestAnUnlimitedBookingStaysUnlimitedWhenExtended(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	km := 100
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), v.plan, workshop.RateSeason{ValidFrom: day("2031-05-11"), Day: kr(40000),
		IncludedKmPerDay: &km}); err != nil {
		t.Fatal(err)
	}
	id, err := v.book(pool, in2031(9, 9), in2031(11, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(13, 9), ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id); b.IncludedKm != nil {
		t.Errorf("an unlimited booking became one of %d km", *b.IncludedKm)
	}
}

// A daily extra is charged on an extension for as many as were taken,
// whatever the wall clock did to the days in between: a booking moved
// across the night the clocks go forward still has its one seat (review 2
// #6).
func TestADailyExtraKeepsItsCountAcrossTheChangeOfHour(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	seat, _ := workshop.AddRentalExtra(ctx, pool, rentalOwner(), workshop.NewRentalExtra{Kind: "child_seat", Name: "Barnstol", Per: "day",
		PriceMinor: 5000, VATRateBp: 2500, VATCategory: "S"})
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
		From: wallAt("2031-03-20 12:00"), To: wallAt("2031-03-22 12:00"), Extras: []workshop.ExtraChoice{{ExtraID: seat, Count: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	// Forty-eight hours from noon on 29 March end at one on the 31st: three
	// days begun on the wall clock.
	if err := workshop.MoveRentalBooking(ctx, pool, rentalDesk(), id, wallAt("2031-03-29 12:00"), wallAt("2031-03-31 13:00"), ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, wallAt("2031-04-02 13:00"), ""); err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	var seats int64
	for _, l := range b.Lines {
		if l.ExtraID == seat {
			seats += l.QuantityMilli
		}
	}
	if seats != 4000 {
		t.Errorf("one seat for two days and two more: %d seat-days", seats/1000)
	}
}

// A daily extra taken on two lines -- a seat, and another seat -- is
// charged for both on an extension, and a second extension charges the
// same two again rather than the lines of the first counted on top
// (review 3 #4).
func TestADailyExtraOnTwoLinesIsChargedForBothWhenExtended(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	seat, _ := workshop.AddRentalExtra(ctx, pool, rentalOwner(), workshop.NewRentalExtra{Kind: "child_seat", Name: "Barnstol", Per: "day",
		PriceMinor: 5000, VATRateBp: 2500, VATCategory: "S"})
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: v.unit, RenterID: v.renter,
		From: in2031(10, 9), To: in2031(11, 9), Extras: []workshop.ExtraChoice{{ExtraID: seat, Count: 1}, {ExtraID: seat, Count: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	seatDays := func() int64 {
		t.Helper()
		b, err := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, l := range b.Lines {
			if l.ExtraID == seat {
				n += l.QuantityMilli
			}
		}
		return n / 1000
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(13, 9), ""); err != nil {
		t.Fatal(err)
	}
	if got := seatDays(); got != 2*3 {
		t.Errorf("two seats for three days: %d seat-days", got)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(14, 9), ""); err != nil {
		t.Fatal(err)
	}
	if got := seatDays(); got != 2*4 {
		t.Errorf("two seats for four days: %d seat-days", got)
	}
}
