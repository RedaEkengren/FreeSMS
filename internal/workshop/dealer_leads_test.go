package workshop_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Leads (#172).

// salesperson is a person who sells cars here, signed in.
func salesperson(t *testing.T, pool *pgxpool.Pool, name, email string) access.Scope {
	t.Helper()
	id, err := workshop.AddStaff(context.Background(), pool, everything(owner()), name, email, access.RoleSales, "a long enough password")
	if err != nil {
		t.Fatalf("AddStaff: %v", err)
	}
	s := sales()
	s.UserID = id
	return s
}

func walkIn(name, phone string) workshop.NewLead {
	return workshop.NewLead{Name: name, Phone: phone, Channel: "walk_in", Wishes: "Kombi, automat, under 150 000 kr"}
}

// An enquiry is a lead; the same person asking again is offered the lead
// they have, however the number is written, and asking about another car
// is another lead for the same person. The same request sent twice is one
// lead.
func TestAnEnquiryIsALeadAndTheSamePersonIsRecognised(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	p := buy(t, pool, workshop.StockFromPurchase)

	first := walkIn("Kim Köpare", "070-123 45 67")
	first.StockUnitID, first.Wishes, first.Note, first.Key = p.StockUnitID, "", "Vill provköra på lördag", "form-1"
	lead, err := workshop.CreateLead(ctx, pool, anna, first)
	if err != nil {
		t.Fatal(err)
	}
	if lead.Status != workshop.LeadNew || lead.Salesperson != "" || lead.Car != "ABC 12D" || len(lead.Contacts) != 1 {
		t.Errorf("the lead: %+v", lead)
	}
	again, err := workshop.CreateLead(ctx, pool, anna, first)
	if err != nil || again.ID != lead.ID {
		t.Errorf("the same request again: %+v, %v", again, err)
	}

	second := walkIn("Kim Köpare", "+46 70 123 45 67")
	_, err = workshop.CreateLead(ctx, pool, anna, second)
	var dup *workshop.DuplicateLeadError
	if !errors.As(err, &dup) || len(dup.Leads) != 1 || dup.Leads[0].ID != lead.ID || !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("the same person again: %v", err)
	}
	second.Alongside = true
	other, err := workshop.CreateLead(ctx, pool, anna, second)
	if err != nil || other.ID == lead.ID || other.Wishes == "" {
		t.Errorf("another car for the same person: %+v, %v", other, err)
	}

	for name, n := range map[string]workshop.NewLead{
		"no way to reach them": {Name: "Kim", Channel: "phone", Wishes: "Något"},
		"no car or wish":       {Name: "Kim", Phone: "0701234567", Channel: "phone"},
		"no channel":           {Name: "Kim", Phone: "0701234567", Wishes: "Något"},
		"a short phone number": {Name: "Kim", Phone: "12", Channel: "phone", Wishes: "Något"},
		"not an email":         {Name: "Kim", Email: "kim", Channel: "email", Wishes: "Något"},
	} {
		if _, err := workshop.CreateLead(ctx, pool, anna, n); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A salesperson sees their own leads and the ones nobody has; another's
// are not found, asked for directly, and nothing of them is handed over.
// Whoever runs the shop sees every lead. The roles that do not sell cars
// see none.
func TestASalespersonSeesTheirOwnLeadsAndTheUnassigned(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	bo := salesperson(t, pool, "Bo Säljare", "bo.s@example.test")
	mine := walkIn("Kim Köpare", "0701234567")
	mine.Salesperson = anna.UserID
	annas, err := workshop.CreateLead(ctx, pool, anna, mine)
	if err != nil {
		t.Fatal(err)
	}
	nobodys, err := workshop.CreateLead(ctx, pool, bo, walkIn("Lo Letare", "0731234567"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CreateLead(ctx, pool, bo, workshop.NewLead{Name: "X", Phone: "0761234567", Channel: "phone",
		Wishes: "Y", Salesperson: anna.UserID}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson gave a lead to another: %v", err)
	}

	got, err := workshop.LeadByID(ctx, pool, bo, annas.ID)
	if !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another's lead, asked for directly: %v", err)
	}
	if handed := testsupport.Handed(got, "Name", "Phone", "Email", "Contacts"); len(handed) != 0 {
		t.Errorf("handed %v", handed)
	}
	if err := workshop.RecordLeadContact(ctx, pool, bo, annas.ID, "phone", "Ringde"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another's lead, contacted: %v", err)
	}
	list, err := workshop.Leads(ctx, pool, bo, false)
	if err != nil || len(list) != 1 || list[0].ID != nobodys.ID {
		t.Errorf("Bo's list: %+v, %v", list, err)
	}
	if list, _ := workshop.Leads(ctx, pool, everything(owner()), false); len(list) != 2 {
		t.Errorf("the owner's list has %d leads; want both", len(list))
	}
	for name, s := range map[string]access.Scope{"the front desk": everything(advisor()), "a buyer": buyerScope(),
		"a technician": everything(technician())} {
		if _, err := workshop.Leads(ctx, pool, s, true); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s listed leads: %v", name, err)
		}
		if _, err := workshop.CreateLead(ctx, pool, s, walkIn("Z", "0709999999")); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s recorded a lead: %v", name, err)
		}
	}

	// Taken by Bo, it is Bo's; Anna leaves and hers go to Bo.
	if err := workshop.AssignLead(ctx, pool, bo, nobodys.ID, bo.UserID); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AssignLead(ctx, pool, anna, nobodys.ID, anna.UserID); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("Anna took Bo's lead: %v", err)
	}
	if _, err := workshop.ReassignLeads(ctx, pool, bo, anna.UserID, bo.UserID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson reassigned another's leads: %v", err)
	}
	moved, err := workshop.ReassignLeads(ctx, pool, everything(owner()), anna.UserID, bo.UserID)
	if err != nil || moved != 1 {
		t.Fatalf("reassigned %d (%v)", moved, err)
	}
	if l, err := workshop.LeadByID(ctx, pool, bo, annas.ID); err != nil || l.Salesperson != bo.UserID {
		t.Errorf("Anna's lead after she left: %+v, %v", l, err)
	}

	// With car sales off, a lead is still read, and none is recorded.
	off := bo
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.LeadByID(ctx, pool, off, annas.ID); err != nil {
		t.Errorf("read with car sales off: %v", err)
	}
	if _, err := workshop.CreateLead(ctx, pool, off, walkIn("Z", "0709999998")); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("recorded with car sales off: %v", err)
	}
	for _, stmt := range []string{`DELETE FROM lead_events`, `UPDATE lead_events SET status = 'won'`, `DELETE FROM leads`,
		`UPDATE leads SET channel = 'phone'`, `DELETE FROM lead_contacts`, `UPDATE lead_contacts SET how = 'sms'`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}

// A lead moves as it is worked: contacted once somebody reaches the
// person, lost only with why, taken up again, never won by a tap.
func TestALeadMovesAsItIsWorked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	l, err := workshop.CreateLead(ctx, pool, anna, walkIn("Kim Köpare", "0701234567"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordLeadContact(ctx, pool, anna, l.ID, "phone", "Ringer tillbaka i morgon"); err != nil {
		t.Fatal(err)
	}
	got, _ := workshop.LeadByID(ctx, pool, anna, l.ID)
	if got.Status != workshop.LeadContacted || len(got.Contacts) != 1 || got.Contacts[0].By != "Anna Säljare" {
		t.Errorf("after the call: %+v", got)
	}
	if err := workshop.MoveLead(ctx, pool, anna, l.ID, workshop.LeadWon, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("won by a tap: %v", err)
	}
	if err := workshop.MoveLead(ctx, pool, anna, l.ID, workshop.LeadLost, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("lost without why: %v", err)
	}
	if err := workshop.MoveLead(ctx, pool, anna, l.ID, workshop.LeadLost, "other", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("lost for another reason, unsaid: %v", err)
	}
	if err := workshop.MoveLead(ctx, pool, anna, l.ID, workshop.LeadLost, "bought_elsewhere", ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetLeadNextStep(ctx, pool, anna, l.ID, refTime(time.Now()), "Ring"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a next step on a lost lead: %v", err)
	}
	if err := workshop.MoveLead(ctx, pool, anna, l.ID, workshop.LeadContacted, "", ""); err != nil {
		t.Fatalf("taken up again: %v", err)
	}
	got, _ = workshop.LeadByID(ctx, pool, anna, l.ID)
	if got.Status != workshop.LeadContacted || got.LostReason != "" {
		t.Errorf("taken up again: %+v", got)
	}
	var history int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lead_events WHERE lead_id = $1 AND kind = 'status'`, l.ID).Scan(&history); err != nil || history != 4 {
		t.Errorf("%d status events (%v); want new, contacted, lost and contacted again kept", history, err)
	}
}

func refTime(t time.Time) *time.Time { return &t }

// What waits for whom: a new lead nobody has, for everyone who sells cars;
// a next step that is due, for the lead's salesperson; and a lead whose
// car has been reserved, for its salesperson, flagged on the lead.
func TestLeadsWaitForTheirSalesperson(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	bo := salesperson(t, pool, "Bo Säljare", "bo.s@example.test")
	p := buy(t, pool, workshop.StockFromPurchase)
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	unassigned, err := workshop.CreateLead(ctx, pool, anna, walkIn("Kim Köpare", "0701234567"))
	if err != nil {
		t.Fatal(err)
	}
	car := walkIn("Lo Letare", "0731234567")
	car.StockUnitID, car.Wishes, car.Salesperson = p.StockUnitID, "", anna.UserID
	annas, err := workshop.CreateLead(ctx, pool, anna, car)
	if err != nil {
		t.Fatal(err)
	}
	kinds := func(s access.Scope) map[string]string {
		t.Helper()
		items, err := workshop.WaitingFor(ctx, pool, s)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, it := range items {
			if it.SubjectKind == "lead" {
				out[it.SubjectID] = it.Kind
			}
		}
		return out
	}
	if got := kinds(bo); got[unassigned.ID] != "lead_unassigned_any" || len(got) != 1 {
		t.Errorf("Bo is waited for by %v; want the lead nobody has", got)
	}
	if got := kinds(anna); len(got) != 1 {
		t.Errorf("Anna is waited for by %v before her lead's step is due", got)
	}
	if err := workshop.SetLeadNextStep(ctx, pool, anna, annas.ID, refTime(time.Now()), "Ring om provkörning"); err != nil {
		t.Fatal(err)
	}
	if got := kinds(anna); got[annas.ID] != "lead_follow_up" {
		t.Errorf("Anna is waited for by %v; want her lead's step", got)
	}
	if got := kinds(bo); got[annas.ID] != "" {
		t.Errorf("Bo is waited for by Anna's lead: %v", got)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockReserved)
	if got := kinds(anna); got[annas.ID] != "lead_car_gone" {
		t.Errorf("Anna is waited for by %v; want her lead's car reserved", got)
	}
	if l, _ := workshop.LeadByID(ctx, pool, anna, annas.ID); !l.CarGone() {
		t.Errorf("the lead is not flagged with its car reserved: %+v", l)
	}
	if got := kinds(everything(advisor())); len(got) != 0 {
		t.Errorf("the front desk is waited for by leads: %v", got)
	}
}

// A lead that never became a deal loses the person after the shop's
// period, and still counts: where it came from, which car and how it
// ended stay.
func TestALeadThatNeverBecameADealForgetsThePerson(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	if months, err := workshop.LeadErasureMonths(ctx, pool, anna); err != nil || months != 12 {
		t.Errorf("the default period: %d, %v", months, err)
	}
	if err := workshop.SetLeadErasure(ctx, pool, anna, 6); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson set the period: %v", err)
	}
	if err := workshop.SetLeadErasure(ctx, pool, everything(owner()), 6); err != nil {
		t.Fatal(err)
	}
	old, err := workshop.CreateLead(ctx, pool, anna, walkIn("Kim Köpare", "0701234567"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordLeadContact(ctx, pool, anna, old.ID, "phone", "Kim bor i Täby"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveLead(ctx, pool, anna, old.ID, workshop.LeadLost, "price", ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := workshop.CreateLead(ctx, pool, anna, walkIn("Lo Letare", "0731234567"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE leads SET created_at = created_at - interval '7 months' WHERE id = $1`,
		`UPDATE lead_events SET at = at - interval '7 months' WHERE lead_id = $1`,
		`UPDATE lead_contacts SET at = at - interval '7 months' WHERE lead_id = $1`} {
		if _, err := pool.Exec(ctx, q, old.ID); err != nil {
			t.Fatal(err)
		}
	}
	r, err := workshop.Sweep(ctx, pool, shopID)
	if err != nil || r.Leads != 1 {
		t.Fatalf("the sweep: %+v, %v", r, err)
	}
	got, err := workshop.LeadByID(ctx, pool, anna, old.ID)
	if err != nil || !got.Erased || got.Name != "" || got.Phone != "" || got.Wishes != "" || got.Contacts[0].Note != "" ||
		got.Channel != "walk_in" || got.Status != workshop.LeadLost || got.LostReason != "price" {
		t.Errorf("the old lead after the sweep: %+v, %v", got, err)
	}
	if err := workshop.RecordLeadContact(ctx, pool, anna, old.ID, "phone", "Igen"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an erased lead took a contact: %v", err)
	}
	if got, _ := workshop.LeadByID(ctx, pool, anna, fresh.ID); got.Erased || got.Name == "" {
		t.Errorf("a fresh lead was erased: %+v", got)
	}
}

// The same enquiry sent several times at once -- a double tap, a form
// resent by a phone -- is one lead, and every sending is told it.
func TestTheSameEnquirySentAtOnceIsOneLead(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	for _, alongside := range []bool{false, true} {
		n := walkIn("Kim Köpare", "0701234567")
		n.Alongside = alongside
		n.Key = fmt.Sprintf("form-%v", alongside)
		n.Email = fmt.Sprintf("kim-%v@example.test", alongside)
		n.Phone = map[bool]string{false: "0701234567", true: "0731234567"}[alongside]
		var wg sync.WaitGroup
		ids := make([]string, 6)
		errs := make([]error, 6)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, err := workshop.CreateLead(ctx, pool, anna, n)
				ids[i], errs[i] = l.ID, err
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil || ids[i] != ids[0] {
				t.Errorf("alongside %v, sending %d: %s, %v; want the one lead", alongside, i, ids[i], err)
			}
		}
	}
}

// A car known only by its chassis number is named by it, on a lead and in
// what waits: a lead whose car has gone is never a sentence with nothing
// in it.
func TestACarKnownByItsChassisNumberIsNamedByIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	var vehicle string
	if err := pool.QueryRow(ctx, `INSERT INTO vehicles (shop_id, vin) VALUES ($1, 'YS3EH5G13Y1234567') RETURNING id`, shopID).Scan(&vehicle); err != nil {
		t.Fatal(err)
	}
	p, err := workshop.RecordPurchase(ctx, pool, buyerScope(), workshop.NewPurchase{VehicleID: vehicle, Reason: workshop.StockFromPurchase,
		SellerPartyID: theSeller(t, pool), PurchasedOn: time.Now(), PriceMinor: 5_000_000, SellerType: workshop.SellerPrivate})
	if err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	n := walkIn("Kim Köpare", "0701234567")
	n.StockUnitID, n.Wishes, n.Salesperson = p.StockUnitID, "", anna.UserID
	l, err := workshop.CreateLead(ctx, pool, anna, n)
	if err != nil || l.Car != "YS3EH5G13Y1234567" {
		t.Fatalf("the lead's car: %q, %v", l.Car, err)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockReserved)
	items, err := workshop.WaitingFor(ctx, pool, anna)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.SubjectID == l.ID && (it.Kind != "lead_car_gone" || it.Text != "YS3EH5G13Y1234567" || it.Says() == "") {
			t.Errorf("the item: %+v, saying %q", it, it.Says())
		}
	}
}
