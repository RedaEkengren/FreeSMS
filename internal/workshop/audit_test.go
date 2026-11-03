package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Audit events (#151). Every transition on the list leaves an event that
// one place reads back, in the transition's own transaction; the list is
// the code's, and a transition on it that this test does not make fails.
func TestEveryAuditedTransitionLeavesAnEvent(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	everything := everything(owner())
	type subject struct{ kind, id string }
	made := map[string]subject{}

	// A module switched off and on.
	if _, err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, false, "Semester", true); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.SwitchModule(ctx, pool, owner(), access.Workshop, true, "Tillbaka", false); err != nil {
		t.Fatal(err)
	}
	made["module.enabled"], made["module.disabled"] = subject{"shop", shopID}, subject{"shop", shopID}

	// Somebody's roles.
	staff, err := workshop.AddStaff(ctx, pool, owner(), "Ny Person", "ny@example.test", access.RoleTechnician, "a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetStaffRoles(ctx, pool, owner(), staff, access.RoleTechnician, []access.Role{access.RoleParts}); err != nil {
		t.Fatal(err)
	}
	made["roles.changed"] = subject{"user", staff}

	// A booking overridden.
	res := trailer(t, pool)
	block, err := rent(pool, rentalDesk(), res, in2031(15, 9), in2031(15, 17))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideBlock(ctx, pool, everything, block, time.Time{}, time.Time{}, "Bilen behövs i verkstaden"); err != nil {
		t.Fatal(err)
	}
	made["block.overridden"] = subject{"resource_block", block}

	// A contract signed, and corrected.
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), doc, d.Parties[0].ID, workshop.SignatureEvidence{
		Method: workshop.SignedOnPaper, Hash: d.Hash, WitnessedBy: "Bo"}); err != nil {
		t.Fatal(err)
	}
	purchaseAgreement(t, pool, tpl, doc)
	made["document.signed"], made["document.superseded"] = subject{"document", doc}, subject{"document", doc}

	// An invoice with a line nobody agreed to, paid far over, and a
	// payment reversed.
	job := readyToInvoice(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "fee", Description: "Extra", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job, workshop.InvoiceUnagreedLines("")); !errors.Is(err, workshop.ErrUnagreedNoReason) {
		t.Errorf("unagreed lines invoiced with no reason: %v", err)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job, workshop.InvoiceUnagreedLines("Kunden godkände per telefon"))
	if err != nil {
		t.Fatal(err)
	}
	made["invoice.unagreed_lines_invoiced"] = subject{"invoice", inv.ID}
	ten := inv.GrossMinor * 10
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &ten, Method: "bank", PaidOn: time.Now(), ConfirmedOverpayment: true}); !errors.Is(err, workshop.ErrOverpaymentNoReason) {
		t.Errorf("an overpayment confirmed with no reason: %v", err)
	}
	over, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &ten, Method: "bank", PaidOn: time.Now(), ConfirmedOverpayment: true, OverpaymentReason: "Betalade för hela flottan"})
	if err != nil {
		t.Fatal(err)
	}
	made["payment.overpayment_confirmed"] = subject{"payment", over.ID}
	if err := workshop.ReversePayment(ctx, pool, advisor(), over.ID, "Registrerad av misstag"); err != nil {
		t.Fatal(err)
	}
	made["payment.recorded"], made["payment.reversed"] = subject{"invoice", inv.ID}, subject{"invoice", inv.ID}

	// A deposit received, applied and partly given back; a seller paid
	// out; a payment matched to the bank's statement and the match undone.
	dep, err := workshop.CreateDeposit(ctx, pool, everything, workshop.NewHolding{PartyID: partyOf(t, pool, fixtureCustomer), Purpose: "other"})
	if err != nil {
		t.Fatal(err)
	}
	held, err := workshop.RecordDepositPayment(ctx, pool, everything, dep, workshop.NewPayment{AmountMinor: minor(300000), Method: "swish", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	other, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ApplyDeposit(ctx, pool, everything, dep, other.ID, 1000, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RefundPayment(ctx, pool, everything, held.ID, workshop.NewPayment{AmountMinor: minor(1000), Method: "bank", PaidOn: time.Now()}, "Delvis tillbaka"); err != nil {
		t.Fatal(err)
	}
	made["payment.applied"], made["payment.refunded"] = subject{"deposit", dep}, subject{"deposit", dep}
	payout := purchasePayout(t, pool, everything, theSeller(t, pool))
	if _, err := workshop.RecordPayout(ctx, pool, everything, payout, workshop.NewPayment{AmountMinor: minor(5000000), Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	made["payment.paid_out"] = subject{"payout", payout}
	if _, err := workshop.AddStatementLines(ctx, pool, owner(), "typed", []workshop.StatementLine{{BookedOn: time.Now(), AmountMinor: 300000}}); err != nil {
		t.Fatal(err)
	}
	r, err := workshop.Unreconciled(ctx, pool, owner())
	if err != nil || len(r.Lines) != 1 {
		t.Fatalf("unreconciled: %+v, %v", r, err)
	}
	if err := workshop.MatchPayment(ctx, pool, owner(), held.ID, r.Lines[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := workshop.UndoMatch(ctx, pool, owner(), held.ID); err != nil {
		t.Fatal(err)
	}
	made["payment.matched"], made["payment.match_undone"] = subject{"payment", held.ID}, subject{"payment", held.ID}
	made["job.state_changed"] = subject{"work_order", job}

	// An identity number read under its purpose.
	keys := identityKeys(t, "k1")
	buyer := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything, buyer, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	record, err := workshop.RecordIdentity(ctx, pool, sales(), keys, workshop.NewIdentityRecord{PartyID: buyer,
		Kind: workshop.IdentityNumber, Purpose: "ownership_change", Value: seededNumber})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, record, false); err != nil {
		t.Fatal(err)
	}
	made["identity.read"] = subject{"identity_record", record}

	// A customer's details put right.
	c, err := workshop.CustomerByID(ctx, pool, advisor(), fixtureCustomer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), fixtureCustomer, c.Revision, workshop.CustomerEdit{
		Name: c.Name, Phone: "070-999 88 77", Reason: "Nytt nummer"}); err != nil {
		t.Fatal(err)
	}
	made["record.corrected"] = subject{"customer", fixtureCustomer}

	// What a technician sees (#225): the profile, and an exception given
	// and ended.
	if err := workshop.SetTechnicianField(ctx, pool, owner(), access.FieldLinePrices, true, "Ackordslön"); err != nil {
		t.Fatal(err)
	}
	made["technician_view.changed"] = subject{"shop", shopID}
	if err := workshop.GrantTechnicianField(ctx, pool, owner(), staff, access.FieldHourlyRate, "Arbetsledare", nil); err != nil {
		t.Fatal(err)
	}
	if err := workshop.EndTechnicianGrant(ctx, pool, owner(), staff, access.FieldHourlyRate, "Tillbaka på golvet"); err != nil {
		t.Fatal(err)
	}
	made["technician_view.granted"], made["technician_view.ended"] = subject{"user", staff}, subject{"user", staff}

	// Dealer
	// A car's cycle through stock (#160).
	unit := takeIn(t, pool, workshop.StockFromDefleet)
	moveStock(t, pool, unit.ID, workshop.StockForSale)
	made["stock_unit.opened"], made["stock_unit.moved"] = subject{"stock_unit", unit.ID}, subject{"stock_unit", unit.ID}
	// A job on it posted its cost when ready, then went back to work (#164).
	jobTakenBackToWork(t, pool)
	made["stock_cost.reversed"] = subject{"stock_unit", unit.ID}
	// A car bought (#161), its eligibility corrected.
	bought := boughtCar(t, pool)
	if _, err := workshop.CorrectVATEligibility(ctx, pool, buyerScope(), bought.ID, workshop.SellerCompanyChargedVAT, nil, "Säljaren var ett företag"); err != nil {
		t.Fatal(err)
	}
	made["purchase.recorded"], made["vat_eligibility.corrected"] = subject{"purchase", bought.ID}, subject{"purchase", bought.ID}
	// Money let leave for it before its checks passed (#162).
	if err := workshop.OverrideStockChecks(ctx, pool, everything, bought.StockUnitID, "Kontrollerat per telefon"); err != nil {
		t.Fatal(err)
	}
	made["stock_checks.overridden"] = subject{"stock_unit", bought.StockUnitID}
	// A test drive let go before its agreement was signed (#173).
	made["test_drive.started_unsigned"] = subject{"test_drive", unsignedDrive(t, pool)}
	// A reservation ended without a sale, the deposit decided (#174).
	{
		moveStock(t, pool, bought.StockUnitID, workshop.StockForSale)
		r, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: bought.StockUnitID, PartyID: driver(t, pool),
			ExpiresAt: time.Now().Add(time.Hour), Refundable: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := workshop.EndReservation(ctx, pool, sales(), r.ID, workshop.ReservationCancelled, workshop.DepositNone, "Ångrade sig"); err != nil {
			t.Fatal(err)
		}
		made["reservation.ended"] = subject{"stock_unit", bought.StockUnitID}
	}
	// A sold car handed over unpaid, on the owner's word (#179).
	{
		// The car the reservation above let go, for sale again; its
		// purchase was reassessed as not eligible, so ordinary VAT.
		d := completedDealOn(t, pool, bought.StockUnitID, workshop.TaxOrdinary)
		if _, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := workshop.DeliverCar(ctx, pool, func() access.Scope { s := owner(); s.Modules = access.Modules; return s }(), d.ID, workshop.NewDelivery{At: time.Now(), Override: "Banken betalar"}); err != nil {
			t.Fatal(err)
		}
		made["vehicle_sale.delivered_early"] = subject{"vehicle_sale", d.ID}
		// Its ownership confirmed (#180).
		f, err := workshop.FilingOf(ctx, pool, sales(), "sale", d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := workshop.RecordFilingFiled(ctx, pool, sales(), f.ID, workshop.FilingChecklist{ControlNumberInHand: true}); err != nil {
			t.Fatal(err)
		}
		if err := workshop.ConfirmFiling(ctx, pool, sales(), f.ID, "TS-1"); err != nil {
			t.Fatal(err)
		}
		made["ownership.confirmed"] = subject{"ownership_filing", f.ID}
	}
	// Rental
	rentalAudited(t, pool, func(event, kind, id string) { made[event] = subject{kind, id} })
	// A disputed damage set against the deposit on the owner's word (#197).
	{
		c := newDamageOn(t, pool, "DMG 197", "V197")
		amount := int64(100_000)
		for _, s := range []workshop.DamageStep{
			{Step: "amount_settled", AmountMinor: &amount, Reason: "Självrisken"},
			{Step: "customer_informed", Channel: "sms", Reason: "Skickat"},
			{Step: "customer_response", Response: "disputed", Reason: "Bestrids"},
		} {
			if err := workshop.TakeDamageStep(ctx, pool, rentalDesk(), c, s); err != nil {
				t.Fatal(err)
			}
		}
		if err := workshop.TakeDamageStep(ctx, pool, rentalOwner(), c, workshop.DamageStep{Step: "outcome", Outcome: "set_against_deposit",
			Reason: "Bilderna"}); err != nil {
			t.Fatal(err)
		}
		made["damage.set_against_disputed"] = subject{"damage_case", c}
	}

	for _, tr := range workshop.AuditedTransitions {
		s, ok := made[tr.Event]
		if !ok {
			t.Errorf("%s is on the list and this test does not make it", tr.Event)
			continue
		}
		if s.kind != tr.Subject {
			t.Errorf("%s is about a %s on the list, made here about a %s", tr.Event, tr.Subject, s.kind)
		}
		events, err := workshop.AuditTrail(ctx, pool, everything, s.kind, s.id)
		if err != nil {
			t.Fatalf("AuditTrail(%s %s): %v", s.kind, s.id, err)
		}
		var found *workshop.AuditEvent
		for i := range events {
			if events[i].Event == tr.Event {
				found = &events[i]
			}
		}
		switch {
		case found == nil:
			t.Errorf("%s left no event on its %s: %+v", tr.Event, s.kind, events)
		case found.KeptIn != tr.KeptIn:
			t.Errorf("%s was read from %s, the list says %s", tr.Event, found.KeptIn, tr.KeptIn)
		case tr.NeedsReason && found.Reason == "":
			t.Errorf("%s has no reason", tr.Event)
		case found.KeptIn == "audit_events" && found.Actor == "":
			t.Errorf("%s does not say who", tr.Event)
		}
	}

	// The decision about unagreed lines says which lines.
	events, _ := workshop.AuditTrail(ctx, pool, everything, "invoice", inv.ID)
	for _, e := range events {
		if e.Event == "invoice.unagreed_lines_invoiced" {
			if lines, _ := e.Detail["lines"].([]any); len(lines) != 1 || e.Detail["count"] != float64(1) {
				t.Errorf("the decision names lines %v", e.Detail)
			}
		}
	}
	// Two transitions in one transaction keep their order: the job's
	// state changes come in the order they were made.
	jobs, _ := workshop.AuditTrail(ctx, pool, everything, "work_order", job)
	if len(jobs) < 2 || jobs[len(jobs)-1].To != string(workshop.StateInvoiced) && jobs[len(jobs)-1].To != string(workshop.StateClosed) {
		t.Errorf("the job's events end %+v", jobs[len(jobs)-1])
	}
}

// An event is never changed or deleted, whatever writes; the application
// may take only its reason away, which only an erasure does.
func TestAnAuditEventStaysAsItWasWritten(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res := trailer(t, pool)
	block, err := rent(pool, rentalDesk(), res, in2031(16, 9), in2031(16, 17))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideBlock(ctx, pool, everything(owner()), block, time.Time{}, time.Time{}, "Kunden ångrade sig"); err != nil {
		t.Fatal(err)
	}
	for stmt, code := range map[string]string{
		`DELETE FROM audit_events`:                       "42501",
		`UPDATE audit_events SET event = 'x.y'`:          "42501",
		`UPDATE audit_events SET reason = 'Något annat'`: "23001",
	} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != code {
			t.Errorf("%q as the application: %v, want %s", stmt, err, code)
		}
	}
	// As the database's owner, the trigger refuses it as well.
	if _, err := pool.Exec(ctx, `DELETE FROM audit_events`); err == nil {
		t.Error("the owner deleted an event")
	}
}

// What a role may not read about a subject it is not handed: the rental
// desk reads a booking's trail without why it was overridden, a technician
// reads no money's, and a salesperson is not handed a purchase price in
// an event's detail.
func TestAnAuditTrailIsReadAsItsSubjectIs(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res := trailer(t, pool)
	block, err := rent(pool, rentalDesk(), res, in2031(17, 9), in2031(17, 17))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.OverrideBlock(ctx, pool, everything(owner()), block, time.Time{}, time.Time{}, "Privat skäl"); err != nil {
		t.Fatal(err)
	}
	desk, err := workshop.AuditTrail(ctx, pool, rentalDesk(), "resource_block", block)
	if err != nil || len(desk) != 1 || desk[0].Reason != "" {
		t.Errorf("the rental desk read %+v, %v; want the override without its reason", desk, err)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AuditTrail(ctx, pool, technician(), "invoice", inv.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read an invoice's trail: %v", err)
	}
	if _, err := workshop.AuditTrail(ctx, pool, owner(), "shop", "99999999-0000-0000-0000-000000000000"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another shop's trail: %v", err)
	}
	// A detail with a purchase price, as Dealer's purchase will write one.
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_events (shop_id, subject_kind, subject_id, event, detail)
		VALUES ($1, 'resource_block', $2, 'block.overridden', '{"purchase_price_minor": 8500000, "change": "ended"}')`, shopID, block); err == nil {
		t.Fatal("an override with no reason was written")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_events (shop_id, subject_kind, subject_id, event, reason, detail)
		VALUES ($1, 'resource_block', $2, 'block.overridden', 'Inköp', '{"purchase_price_minor": 8500000, "change": "ended"}')`, shopID, block); err != nil {
		t.Fatal(err)
	}
	sales := otherRoles(access.Of(access.RoleSales))
	seen, err := workshop.AuditTrail(ctx, pool, sales, "resource_block", block)
	if err != nil || len(seen) != 2 {
		t.Fatalf("the salesperson read %+v, %v", seen, err)
	}
	if _, there := seen[1].Detail["purchase_price_minor"]; there || seen[1].Detail["change"] != "ended" {
		t.Errorf("the salesperson was handed %v", seen[1].Detail)
	}
	if all, _ := workshop.AuditTrail(ctx, pool, everything(owner()), "resource_block", block); all[1].Detail["purchase_price_minor"] == nil {
		t.Errorf("the owner was not handed the purchase price: %v", all[1].Detail)
	}
}

// Erasing a person takes away the reasons given for what was done about
// them, which can name them; that it was done stays.
func TestErasureTakesTheReasonsOfEventsAboutThePerson(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_events (shop_id, subject_kind, subject_id, event, actor, reason)
		VALUES ($1, 'customer', $2, 'record.corrected', $3, 'Anna ringde från sitt nya nummer')`, shopID, fixtureCustomer, userID); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), &recordingRemover{}, customerPersonID, "Begäran"); err != nil {
		t.Fatal(err)
	}
	events, err := workshop.AuditTrail(ctx, pool, advisor(), "customer", fixtureCustomer)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.KeptIn == "audit_events" {
			found = true
			if e.Reason != "" || !e.ReasonErased || e.Actor == "" {
				t.Errorf("after the erasure: %+v", e)
			}
		}
	}
	if !found {
		t.Error("the erasure took the event away")
	}
}
