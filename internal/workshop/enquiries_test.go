package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// enquired makes an enquiry with two lines on the draft: 2 hours at 800 and a
// part at 450, 25 % VAT -- 2 500,00 net, 3 125,00 with VAT on the whole.
func enquired(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	id, err := workshop.CreateEnquiry(ctx, pool, advisor(), workshop.NewEnquiry{Subject: "Bromsar fram"})
	if err != nil {
		t.Fatalf("CreateEnquiry: %v", err)
	}
	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Byte av bromsskivor", QuantityMilli: 2000, UnitPriceMinor: 80000, VATRateBasis: 2500},
		{Kind: "part", Description: "Bromsskivor, par", QuantityMilli: 1000, UnitPriceMinor: 90000, VATRateBasis: 2500},
	} {
		if err := workshop.AddEnquiryLine(ctx, pool, advisor(), id, l); err != nil {
			t.Fatalf("AddEnquiryLine: %v", err)
		}
	}
	return id
}

var fixed = workshop.Terms{ValidDays: 14, PriceKind: "fixed"}

// accept writes down a customer's yes to the latest version for what it
// comes to.
func accept(t *testing.T, pool *pgxpool.Pool, id string, declined ...int) {
	t.Helper()
	e, err := workshop.EnquiryByID(context.Background(), pool, advisor(), id)
	if err != nil {
		t.Fatal(err)
	}
	l := e.Latest()
	gross := l.GrossMinor
	if len(declined) > 0 {
		gross = 0
		for _, line := range l.Lines {
			keep := true
			for _, d := range declined {
				keep = keep && line.Position != d
			}
			if keep {
				gross += line.NetMinor + line.VATMinor
			}
		}
	}
	if err := workshop.RecordEnquiryAnswer(context.Background(), pool, advisor(), id, l.ID, workshop.EnquiryAnswerInput{
		Decision: "approved", Via: "phone", AgreedGrossMinor: gross, DeclinedPositions: declined}); err != nil {
		t.Fatalf("RecordEnquiryAnswer: %v", err)
	}
}

// Asking what something costs is not a car being here, work being done, parts
// taken out or anything being owed -- whether it is sent, or accepted.
func TestAnEnquiryIsNotACarOrAJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := enquired(t, pool)
	q, err := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	if err != nil {
		t.Fatalf("PresentEnquiry: %v", err)
	}
	if q.Version != 1 || q.GrossMinor != 312500 || q.PriceKind != "fixed" || !q.ValidUntil.After(time.Now().Add(13*24*time.Hour)) {
		t.Errorf("version %d for %d, %s, until %v", q.Version, q.GrossMinor, q.PriceKind, q.ValidUntil)
	}
	accept(t, pool, id)

	var jobs, presence, movements, invoices, lines int
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM work_orders), (SELECT count(*) FROM vehicle_presence),
			(SELECT count(*) FROM stock_movements), (SELECT count(*) FROM invoices), (SELECT count(*) FROM work_order_lines)`).
			Scan(&jobs, &presence, &movements, &invoices, &lines)
	})
	if jobs+presence+movements+invoices+lines != 0 {
		t.Errorf("sending and accepting wrote %d jobs, %d presence, %d movements, %d invoices, %d lines", jobs, presence, movements, invoices, lines)
	}
	e, _ := workshop.EnquiryByID(ctx, pool, advisor(), id)
	if e.Status() != "Accepted" || e.Registration != "" || e.CustomerName != "" {
		t.Errorf("status %q; registration %q, customer %q: unknown things were made up", e.Status(), e.Registration, e.CustomerName)
	}
}

// The versions are distinct: a changed draft is a new one, the old is
// superseded, and an answer to the old one is refused rather than taken for
// the new.
func TestAnAnswerToASupersededProposalIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := enquired(t, pool)
	v1, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	again, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	if again.ID != v1.ID {
		t.Error("putting the same draft forward twice made two versions")
	}
	// A part that is not available is taken off, and the rest re-priced.
	if err := workshop.RemoveEnquiryLine(ctx, pool, advisor(), id, 2); err != nil {
		t.Fatal(err)
	}
	v2, err := workshop.PresentEnquiry(ctx, pool, advisor(), id, workshop.Terms{ValidDays: 7, PriceKind: "estimate", Limits: "Diagnos upp till en timme"})
	if err != nil || v2.Version != 2 || v2.GrossMinor != 200000 || v2.PriceKind != "estimate" || v2.Limits == "" {
		t.Fatalf("version 2: %+v, %v", v2, err)
	}
	err = workshop.RecordEnquiryAnswer(ctx, pool, advisor(), id, v1.ID, workshop.EnquiryAnswerInput{
		Decision: "approved", Via: "phone", AgreedGrossMinor: v1.GrossMinor})
	if !errors.Is(err, workshop.ErrQuoteSuperseded) {
		t.Errorf("a yes to version 1 after version 2: %v", err)
	}
	e, _ := workshop.EnquiryByID(ctx, pool, advisor(), id)
	if len(e.Answers) != 0 || !e.Versions[1].Superseded || e.Versions[0].Superseded {
		t.Errorf("the answer was kept or the versions are not told apart: %+v", e.Answers)
	}
	// The first version still reads as it was shown.
	if len(e.Versions[1].Lines) != 2 || e.Versions[1].GrossMinor != 312500 {
		t.Errorf("version 1 changed: %+v", e.Versions[1])
	}
}

// A yes needs the amount, as said aloud, and it has to be right.
func TestAYesNeedsTheRightAmount(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := enquired(t, pool)
	v, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	for name, in := range map[string]workshop.EnquiryAnswerInput{
		"the wrong amount":    {Decision: "approved", Via: "phone", AgreedGrossMinor: 1},
		"no way of answering": {Decision: "approved", Via: "", AgreedGrossMinor: v.GrossMinor},
		"a yes to nothing":    {Decision: "approved", Via: "phone", AgreedGrossMinor: 0, DeclinedPositions: []int{1, 2}},
		"a line not there":    {Decision: "approved", Via: "phone", AgreedGrossMinor: v.GrossMinor, DeclinedPositions: []int{9}},
	} {
		if err := workshop.RecordEnquiryAnswer(ctx, pool, advisor(), id, v.ID, in); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A proposal stops standing, and a late answer is refused.
func TestAnExpiredProposalCannotBeAnswered(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := enquired(t, pool)
	v, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	// Time passes. The workshop's own role cannot rewrite a version, so the
	// test does it as the database's owner.
	if _, err := pool.Exec(ctx, `UPDATE quotes SET valid_until = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	err := workshop.RecordEnquiryAnswer(ctx, pool, advisor(), id, v.ID, workshop.EnquiryAnswerInput{
		Decision: "approved", Via: "phone", AgreedGrossMinor: v.GrossMinor})
	if !errors.Is(err, workshop.ErrQuoteExpired) {
		t.Errorf("a yes to an expired proposal: %v", err)
	}
	e, _ := workshop.EnquiryByID(ctx, pool, advisor(), id)
	if e.Status() != "Expired" {
		t.Errorf("status %q", e.Status())
	}
	// A new version stands again.
	v2, err := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	if err != nil || v2.Version != 2 || v2.Expired {
		t.Errorf("a fresh version after expiry: %+v, %v", v2, err)
	}
}

// The customer's own answer, on their link, goes to the version they saw; a
// partial yes keeps only the lines they ticked, and the amount follows.
func TestTheCustomerAnswersOnTheirLink(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := enquired(t, pool)
	v1, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	token, err := workshop.CreateEnquiryLink(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatal(err)
	}
	o, err := workshop.OfferFor(ctx, pool, shopID, token)
	if err != nil || o.Quote == nil || o.Quote.GrossMinor != 312500 || o.Subject != "Bromsar fram" {
		t.Fatalf("what the link shows: %+v, %v", o, err)
	}
	if _, err := workshop.OfferFor(ctx, pool, shopID, "nonsense"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a made-up token: %v", err)
	}
	// The workshop replaces the version; the page the customer has open is stale.
	workshop.RemoveEnquiryLine(ctx, pool, advisor(), id, 2)
	v2, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	if err := workshop.OfferAnswer(ctx, pool, shopID, token, v1.ID, "approved", nil); !errors.Is(err, workshop.ErrQuoteSuperseded) {
		t.Errorf("a stale link accepting version 1: %v", err)
	}
	// A partial yes on a version with two lines.
	workshop.AddEnquiryLine(ctx, pool, advisor(), id, workshop.NewLine{Kind: "fee", Description: "Miljöavgift", QuantityMilli: 1000, UnitPriceMinor: 5000, VATRateBasis: 2500})
	v3, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	if err := workshop.OfferAnswer(ctx, pool, shopID, token, v3.ID, "approved", []int{2}); err != nil {
		t.Fatalf("a partial yes: %v", err)
	}
	if err := workshop.OfferAnswer(ctx, pool, shopID, token, v3.ID, "approved", []int{2}); err != nil {
		t.Errorf("the same answer twice: %v", err)
	}
	e, _ := workshop.EnquiryByID(ctx, pool, advisor(), id)
	a := e.Answer()
	if len(e.Answers) != 1 || a == nil || !a.Partly() || a.AgreedGrossMinor != v2.GrossMinor || a.Via != "link" || e.Status() != "Partly accepted" {
		t.Errorf("answers %+v, status %q", e.Answers, e.Status())
	}
	if err := workshop.OfferAnswer(ctx, pool, shopID, token, v3.ID, "approved", []int{1, 2}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a yes to no lines: %v", err)
	}
}

// What was accepted is booked and then taken in without being typed again,
// at the price the customer was shown, and the job points back at the
// answer, the version and the enquiry.
func TestAnAcceptedProposalBecomesABookingAndThenAJobAtTheSamePrice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := enquired(t, pool)
	if _, err := workshop.BookEnquiry(ctx, pool, advisor(), id, workshop.NewBooking{
		Starts: at("2026-10-20", 8, 0), Ends: at("2026-10-20", 9, 0)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("booking before anything was put forward: %v", err)
	}
	v, _ := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed)
	if _, err := workshop.BookEnquiry(ctx, pool, advisor(), id, workshop.NewBooking{
		Starts: at("2026-10-20", 8, 0), Ends: at("2026-10-20", 9, 0)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("booking before the customer said yes: %v", err)
	}
	accept(t, pool, id, 2) // the part is bought elsewhere

	b1, err := workshop.BookEnquiry(ctx, pool, advisor(), id, workshop.NewBooking{
		Starts: at("2026-10-20", 8, 0), Ends: at("2026-10-20", 9, 0)})
	if err != nil {
		t.Fatalf("BookEnquiry: %v", err)
	}
	b2, _ := workshop.BookEnquiry(ctx, pool, advisor(), id, workshop.NewBooking{
		Starts: at("2026-10-20", 8, 0), Ends: at("2026-10-20", 9, 0)})
	if b1 != b2 {
		t.Error("booking twice made two bookings")
	}
	booking, _, _ := workshop.BookingByID(ctx, pool, advisor(), b1)
	if booking.What != "Bromsar fram" {
		t.Errorf("the booking is for %q: retyped or lost", booking.What)
	}
	if jobs, presence, _ := counts(t, pool); jobs != 0 || presence != 0 {
		t.Errorf("booking made %d jobs and %d presences: the car is not here", jobs, presence)
	}

	job, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{
		BookingID: b1, Registration: "ENQ 111", Complaint: booking.What})
	if err != nil {
		t.Fatalf("TakeInBooking: %v", err)
	}
	_, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	if len(lines) != 1 || lines[0].Description != "Byte av bromsskivor" || lines[0].UnitPriceMinor != 80000 ||
		lines[0].QuantityMilli != 2000 || lines[0].ApprovedAt == nil {
		t.Fatalf("the job's lines: %+v", lines)
	}
	var enquiry, quote, decision string
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT d.enquiry_id::text, d.quote_id::text, d.id::text FROM work_order_lines l
			JOIN quote_decisions d ON d.id = l.quote_decision_id WHERE l.work_order_id = $1`, job).Scan(&enquiry, &quote, &decision)
	})
	if enquiry != id || quote != v.ID {
		t.Errorf("the line points at enquiry %s, version %s; want %s, %s", enquiry, quote, id, v.ID)
	}
	j, _, _ := workshop.JobByID(ctx, pool, advisor(), job)
	if j.EnquiryID != id {
		t.Errorf("the job does not point back at its enquiry: %q", j.EnquiryID)
	}
	// Agreed already: nothing is asked again to go ahead.
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateEstimated); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateApproved); err != nil {
		t.Errorf("approving a job whose lines were agreed on the enquiry: %v", err)
	}
	// And the proposal can no longer be reworked once it is booked.
	if _, err := workshop.PresentEnquiry(ctx, pool, advisor(), id, fixed); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("re-quoting a booked enquiry: %v", err)
	}
}

// Everything about an enquiry is the front desk's.
func TestEnquiriesAreTheFrontDesksAlone(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := enquired(t, pool)
	tech := access.Scope{ShopID: shopID, UserID: techUserID, Role: access.RoleTechnician}
	if _, err := workshop.CreateEnquiry(ctx, pool, tech, workshop.NewEnquiry{Subject: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician creating one: %v", err)
	}
	if _, err := workshop.EnquiryByID(ctx, pool, tech, id); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician reading one: %v", err)
	}
	if _, err := workshop.PresentEnquiry(ctx, pool, tech, id, fixed); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician putting one forward: %v", err)
	}
	// Another shop's enquiry is not there.
	if _, err := workshop.EnquiryByID(ctx, pool, access.Scope{ShopID: "99999999-9999-9999-9999-999999999999", UserID: userID, Role: access.RoleServiceAdvisor}, id); err == nil {
		t.Error("another shop read the enquiry")
	}
}
