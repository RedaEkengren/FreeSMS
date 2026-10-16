package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A car that comes back: the follow-up is an ordinary job, opening it blames
// nobody, and every position the shop takes is a row with a reason.

func backWith(t *testing.T, pool *pgxpool.Pool, original string, symptoms string) (job string, cb workshop.Comeback) {
	t.Helper()
	ctx := context.Background()
	km := int64(81500)
	job, err := workshop.OpenComeback(ctx, pool, advisor(), workshop.ComebackInput{
		OriginalJobID: original, Symptoms: symptoms, ReturnedOn: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), OdometerKm: &km,
	})
	if err != nil {
		t.Fatalf("OpenComeback: %v", err)
	}
	cb, err = workshop.ComebackFor(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("ComebackFor: %v", err)
	}
	return job, cb
}

func TestACarThatComesBackIsAJobAndNotAVerdict(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	original, inv := issued(t, pool)
	before, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)

	job, cb := backWith(t, pool, original, "Brakes squeal again after two weeks")
	if job == original || cb.OriginalJobID != original || cb.FollowUpJobID != job {
		t.Fatalf("the follow-up is not its own job: %+v", cb)
	}
	if cb.Kind != "reported" || cb.Settled() {
		t.Errorf("opening a comeback decided something: kind %q", cb.Kind)
	}
	if cb.OdometerKm == nil || *cb.OdometerKm != 81500 || cb.Symptoms == "" || cb.ReturnedOn.Day() != 12 {
		t.Errorf("what was reported was not kept: %+v", cb)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM odometer_readings WHERE km = 81500`); n != 1 {
		t.Errorf("%d odometer readings, want the one from the return", n)
	}
	// The same car, a fresh job with nothing on it, and the first invoice untouched.
	if n := countRows(t, pool, `SELECT count(*) FROM work_orders WHERE vehicle_id = (SELECT vehicle_id FROM work_orders WHERE id = $1)`, job); n != 2 {
		t.Errorf("%d jobs on the car, want 2", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_order_lines WHERE work_order_id = $1`, job); n != 0 {
		t.Errorf("the follow-up started with %d lines", n)
	}
	after, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if after.GrossMinor != before.GrossMinor || len(after.Lines) != len(before.Lines) {
		t.Error("the original invoice changed")
	}

	// The original knows it was come back to.
	list, err := workshop.ComebacksOf(ctx, pool, advisor(), original)
	if err != nil || len(list) != 1 || list[0].FollowUpNumber == 0 {
		t.Fatalf("ComebacksOf = %+v, %v", list, err)
	}
}

func TestTheShopMustSayWhyBeforeItTakesAPosition(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	original, _ := issued(t, pool)
	_, cb := backWith(t, pool, original, "Pulls to the left")

	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "investigating", ""); err != nil {
		t.Fatalf("looking into it needs no reason: %v", err)
	}
	got, _ := workshop.ComebackFor(ctx, pool, advisor(), cb.FollowUpJobID)
	if got.Kind != "investigating" || got.Settled() {
		t.Errorf("investigating confirmed something: %+v", got)
	}
	for _, kind := range []string{"warranty", "goodwill", "unrelated"} {
		if err := workshop.Classify(ctx, pool, advisor(), cb.ID, kind, "  "); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s with no reason = %v, want ErrInvalid", kind, err)
		}
	}
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "negligence", "x"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an invented position = %v", err)
	}
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "unrelated", "A different wheel bearing"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "unrelated", "again"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same position twice = %v", err)
	}
	// Changing its mind adds a row; the first opinion is still there.
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "warranty", "The bearing was ours after all"); err != nil {
		t.Fatal(err)
	}
	got, _ = workshop.ComebackFor(ctx, pool, advisor(), cb.FollowUpJobID)
	if got.Kind != "warranty" || got.KindNote == "" || !got.Settled() {
		t.Errorf("latest position = %+v", got)
	}
	var kinds []string
	for _, e := range got.Events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "reported,investigating,unrelated,warranty" {
		t.Errorf("the history reads %v", kinds)
	}
}

func TestSeveralReturnsForOneRepairAreSeveralCases(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	original, _ := issued(t, pool)
	first, a := backWith(t, pool, original, "Noise")
	second, b := backWith(t, pool, original, "Noise again")
	if first == second || a.ID == b.ID {
		t.Fatal("two returns became one")
	}
	if err := workshop.Classify(ctx, pool, advisor(), a.ID, "unrelated", "Loose heat shield"); err != nil {
		t.Fatal(err)
	}
	list, _ := workshop.ComebacksOf(ctx, pool, advisor(), original)
	if len(list) != 2 || list[0].Kind != "unrelated" || list[1].Kind != "reported" {
		t.Errorf("the cases do not stand apart: %+v", list)
	}
}

func TestAComebackNeedsAFinishedJobOnThisShopsLine(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	open := newJob(t, pool)
	in := workshop.ComebackInput{OriginalJobID: open, Symptoms: "x", ReturnedOn: time.Now()}
	if _, err := workshop.OpenComeback(ctx, pool, advisor(), in); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a job not finished = %v", err)
	}
	done, _ := issued(t, pool)
	in.OriginalJobID = done
	in.Symptoms = " "
	if _, err := workshop.OpenComeback(ctx, pool, advisor(), in); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("no symptoms = %v", err)
	}
	in.Symptoms = "x"
	in.LineID = "11111111-1111-1111-1111-111111111111"
	if _, err := workshop.OpenComeback(ctx, pool, advisor(), in); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a line from nowhere = %v", err)
	}
	in.LineID = ""
	in.OriginalJobID = "11111111-1111-1111-1111-111111111111"
	if _, err := workshop.OpenComeback(ctx, pool, advisor(), in); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a job that does not exist = %v", err)
	}
}

func TestACarSoldInBetweenIsFlagged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	original, _ := issued(t, pool)
	var vehicle string
	if err := pool.QueryRow(ctx, `SELECT vehicle_id::text FROM work_orders WHERE id = $1`, original).Scan(&vehicle); err != nil {
		t.Fatal(err)
	}
	buyer, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "Ny Ägare"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SellVehicle(ctx, pool, advisor(), vehicle, buyer, time.Now(), "Sold"); err != nil {
		t.Fatal(err)
	}
	job, cb := backWith(t, pool, original, "Squeal")
	if !cb.OwnerChanged {
		t.Error("the new owner is not flagged")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_orders WHERE id = $1 AND customer_id = $2`, job, buyer); n != 1 {
		t.Error("the follow-up is not for the person who has the car")
	}
}

func TestOnlyTheCounterSeesClaimsAndCosts(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	original, _ := issued(t, pool)
	job, cb := backWith(t, pool, original, "Squeal")
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "warranty", "Pad failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.FileClaim(ctx, pool, advisor(), cb.ID, workshop.ClaimInput{Supplier: "Bromsgrossen"}); err != nil {
		t.Fatal(err)
	}
	tech, err := workshop.ComebackFor(ctx, pool, technician(), job)
	if err != nil {
		t.Fatal(err)
	}
	if tech.Symptoms == "" || tech.Kind != "warranty" {
		t.Errorf("the technician cannot see what was reported: %+v", tech)
	}
	if len(tech.Claims) != 0 || len(tech.Costs) != 0 || tech.CostsVisible {
		t.Errorf("the technician was handed money: %+v", tech)
	}
	for _, e := range tech.Events {
		if strings.HasPrefix(e.Kind, "claim_") {
			t.Errorf("the technician sees %s", e.Kind)
		}
	}
	for name, err := range map[string]error{
		"open": func() error {
			_, e := workshop.OpenComeback(ctx, pool, technician(), workshop.ComebackInput{OriginalJobID: original, Symptoms: "x", ReturnedOn: time.Now()})
			return e
		}(),
		"classify": workshop.Classify(ctx, pool, technician(), cb.ID, "investigating", ""),
		"note":     workshop.AddComebackNote(ctx, pool, technician(), cb.ID, "x"),
		"answer":   workshop.AnswerClaim(ctx, pool, technician(), "11111111-1111-1111-1111-111111111111", "accepted", nil, ""),
	} {
		if !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s as a technician = %v, want forbidden", name, err)
		}
	}
	if _, err := workshop.FileClaim(ctx, pool, technician(), cb.ID, workshop.ClaimInput{Supplier: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("claim as a technician = %v", err)
	}
}

func TestASupplierClaimFollowsTheCaseAndMayBeRejectedThenArgued(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	original, _ := issued(t, pool)
	_, cb := backWith(t, pool, original, "Squeal")

	// Looking into it is not a ground for a claim.
	if _, err := workshop.FileClaim(ctx, pool, advisor(), cb.ID, workshop.ClaimInput{Supplier: "Bromsgrossen"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("a claim before the case is called warranty = %v", err)
	}
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "warranty", "Pad delaminated"); err != nil {
		t.Fatal(err)
	}
	amount := int64(90000)
	claim, err := workshop.FileClaim(ctx, pool, advisor(), cb.ID, workshop.ClaimInput{Supplier: "Bromsgrossen", Reference: "RMA-77", AmountMinor: &amount})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := workshop.ComebackFor(ctx, pool, advisor(), cb.FollowUpJobID)
	if !got.NeedsClaimAnswer() || len(got.Claims) != 1 || got.Claims[0].Status != "filed" {
		t.Fatalf("filed claim = %+v", got.Claims)
	}
	if err := workshop.AnswerClaim(ctx, pool, advisor(), claim, "paid", nil, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("paid before accepted = %v", err)
	}
	if err := workshop.AnswerClaim(ctx, pool, advisor(), claim, "rejected", nil, "Wear, they say"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AnswerClaim(ctx, pool, advisor(), claim, "rejected", nil, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("rejected twice = %v", err)
	}
	less := int64(60000)
	if err := workshop.AnswerClaim(ctx, pool, advisor(), claim, "accepted", &less, "Argued, half"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AnswerClaim(ctx, pool, advisor(), claim, "paid", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AnswerClaim(ctx, pool, advisor(), claim, "rejected", nil, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a paid claim reopened = %v", err)
	}
	got, _ = workshop.ComebackFor(ctx, pool, advisor(), cb.FollowUpJobID)
	if c := got.Claims[0]; c.Status != "paid" || c.AmountMinor == nil || *c.AmountMinor != 60000 {
		t.Errorf("the claim ended as %+v", c)
	}
	var claimEvents []string
	for _, e := range got.Events {
		if strings.HasPrefix(e.Kind, "claim_") {
			claimEvents = append(claimEvents, e.Kind)
		}
	}
	if strings.Join(claimEvents, ",") != "claim_filed,claim_rejected,claim_accepted,claim_paid" {
		t.Errorf("claim history = %v", claimEvents)
	}
}

func clock(t *testing.T, pool *pgxpool.Pool, job, line string, minutes int) {
	t.Helper()
	err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO time_entries (shop_id, work_order_id, line_id, user_id, started_at, ended_at)
			VALUES ($1, $2, nullif($3, '')::uuid, $4, now() - make_interval(mins => $5 + 5), now() - interval '5 minutes')`,
			shopID, job, line, userID, minutes)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func lineID(t *testing.T, pool *pgxpool.Pool, job, description string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM work_order_lines WHERE work_order_id = $1 AND description = $2`, job, description).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func shareOf(cb workshop.Comeback, bearer string) workshop.CostShare {
	for _, c := range cb.Costs {
		if c.Bearer == bearer {
			return c
		}
	}
	return workshop.CostShare{Bearer: bearer}
}

// Free to the customer is a statement about the invoice. What the repair cost,
// and whose it was, is read from the lines and the clock.
func TestAShopPaidRepairAndMixedWorkAreAccountedForSeparately(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	stockShelf(t, pool, 10)
	original, _ := issued(t, pool)
	job, cb := backWith(t, pool, original, "Squeal")
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "warranty", "Pad failed"); err != nil {
		t.Fatal(err)
	}

	// Free to the customer is free: a price on a line the customer is not
	// paying for is refused, whatever it is called.
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Sneaky", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500, CostBearer: "shop"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a priced warranty line = %v, want ErrInvalid", err)
	}

	for _, l := range []workshop.NewLine{
		{Kind: "part", Description: "Pad set (redo)", QuantityMilli: 2000, VATRateBasis: 2500, CostBearer: "supplier", PartID: stockedPart},
		{Kind: "labour", Description: "Refit pads", QuantityMilli: 1500, VATRateBasis: 2500, CostBearer: "shop"},
		{Kind: "labour", Description: "Replace wiper blades", QuantityMilli: 500, UnitPriceMinor: 89500, VATRateBasis: 2500},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), job, l); err != nil {
			t.Fatalf("AddLine %s: %v", l.Description, err)
		}
	}
	clock(t, pool, job, lineID(t, pool, job, "Refit pads"), 90)
	clock(t, pool, job, lineID(t, pool, job, "Replace wiper blades"), 30)
	clock(t, pool, job, "", 15)

	got, err := workshop.ComebackFor(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if s := shareOf(got, "supplier"); s.PartsCost != 90000 || s.PartsUnpriced != 0 {
		t.Errorf("supplier-borne parts = %+v, want 2 x 450.00", s)
	}
	if s := shareOf(got, "shop"); s.Minutes() != 90 || s.PartsCost != 0 {
		t.Errorf("shop-borne = %+v, want 90 minutes", s)
	}
	if s := shareOf(got, "customer"); s.Minutes() != 30 {
		t.Errorf("customer-borne = %+v", s)
	}
	if s := shareOf(got, "unassigned"); s.Minutes() != 15 {
		t.Errorf("unassigned time = %+v, want it listed", s)
	}

	// The invoice carries the one thing the customer pays for, and only that.
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	doc, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if len(doc.Lines) != 1 || !strings.Contains(doc.Lines[0].Description, "wiper") {
		t.Errorf("the follow-up invoice has %d lines: %+v", len(doc.Lines), doc.Lines)
	}
}

func TestAFullyShopPaidRepairHasNothingToInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	original, _ := issued(t, pool)
	job, _ := backWith(t, pool, original, "Squeal")
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "labour", Description: "Refit pads", QuantityMilli: 1000, VATRateBasis: 2500, CostBearer: "shop"}); err != nil {
		t.Fatal(err)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if n := countRows(t, pool, `SELECT count(*) FROM invoices WHERE work_order_id = $1`, job); n != 0 {
		t.Fatalf("%d invoices before anyone issued one", n)
	}
	// Whatever Issue does with a job nobody pays for, it does not invent a charge.
	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("Issue on a shop-paid job = %v, want ErrNothingToInvoice", err)
	}
}

func TestTheOriginalInvoiceIsCorrectedByTheNormalRouteNotByTheCase(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	original, inv := issued(t, pool)
	_, cb := backWith(t, pool, original, "Squeal")
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "goodwill", "Customer of ten years"); err != nil {
		t.Fatal(err)
	}
	// Nothing in the case touches the issued document; the trigger would stop it.
	if _, err := pool.Exec(ctx, `UPDATE invoices SET gross_minor = 0 WHERE id = $1`, inv.ID); err == nil {
		t.Error("an issued invoice could be rewritten")
	}
}

func TestTheRelationshipShowsInHistoryTimelineAndSummary(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	original, _ := issued(t, pool)
	job, cb := backWith(t, pool, original, "Squeal after two weeks")
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "unrelated", "Different wheel"); err != nil {
		t.Fatal(err)
	}

	var vehicle string
	if err := pool.QueryRow(ctx, `SELECT vehicle_id::text FROM work_orders WHERE id = $1`, job).Scan(&vehicle); err != nil {
		t.Fatal(err)
	}
	v, err := workshop.VehicleByID(ctx, pool, technician(), vehicle)
	if err != nil {
		t.Fatal(err)
	}
	var sawOriginal, sawReturn bool
	for _, h := range v.History {
		switch {
		case h.WorkOrderID == original && h.ComebackCount == 1:
			sawOriginal = true
		case h.WorkOrderID == job && h.ComebackOf != 0 && h.ComebackKind == "unrelated":
			sawReturn = true
		}
	}
	if !sawOriginal || !sawReturn {
		t.Errorf("the vehicle's history does not relate the two jobs: %+v", v.History)
	}

	for _, who := range []access.Scope{advisor(), technician()} {
		for _, id := range []string{job, original} {
			evs, err := workshop.TimelineFor(ctx, pool, who, id)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range evs {
				found = found || e.Kind == "comeback"
			}
			if !found {
				t.Errorf("%s: no return visit on the timeline of %s", who.Role, id)
			}
		}
	}

	// The customer is told what they reported. An unrelated finding is not
	// announced as a ruling, and a warranty one is said to be without charge.
	d, err := workshop.DeliveryDraft(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if d.Return == nil || d.Return.Reported == "" || d.Return.Covered != "" || d.Return.OriginalNumber == 0 {
		t.Errorf("summary after 'unrelated' = %+v", d.Return)
	}
	if err := workshop.Classify(ctx, pool, advisor(), cb.ID, "goodwill", "We will cover it"); err != nil {
		t.Fatal(err)
	}
	d, _ = workshop.DeliveryDraft(ctx, pool, advisor(), job)
	if d.Return == nil || d.Return.Covered != "goodwill" {
		t.Errorf("summary after goodwill = %+v", d.Return)
	}
	if d, _ := workshop.DeliveryDraft(ctx, pool, advisor(), original); d.Return != nil {
		t.Error("the original repair's summary claims to be a return visit")
	}
}
