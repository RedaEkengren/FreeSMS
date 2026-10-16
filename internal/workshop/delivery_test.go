package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// What the customer is given back with the car: built from what was recorded
// as done, never from a quote; frozen when handed over; corrected by a new
// version rather than a rewrite.

func texts(items []workshop.SummaryItem) []string {
	var out []string
	for _, i := range items {
		out = append(out, i.Text)
	}
	return out
}

func has(items []workshop.SummaryItem, text string) bool {
	for _, i := range items {
		if i.Text == text {
			return true
		}
	}
	return false
}

func TestTheSummaryListsWhatWasDoneDeclinedAndLeftAndNeverAQuote(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	addTemplate(t, pool)
	job := inProgress(t, pool, "Diagnosis", "Replace gearbox", "Replace exhaust")
	// A fourth line is offered and not answered: it is a quote, not a repair.
	quote(t, pool, job, "Respray bonnet", 9000)

	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	tasks := tasksOf(t, pool, job)
	taskMove(t, pool, technician(), tasks[0].ID, workshop.TaskMove{To: workshop.TaskDone})
	taskMove(t, pool, technician(), tasks[2].ID, workshop.TaskMove{To: workshop.TaskBlocked, Blocker: workshop.BlockerPart})
	// The customer said no to the gearbox.
	q, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, workshop.QuoteAnswer{Decision: "declined", Via: "counter"}); err != nil {
		t.Fatal(err)
	}
	// An inspection with one thing to keep an eye on, and a finding left open.
	id, err := workshop.StartInspection(ctx, pool, technician(), job, templateID)
	if err != nil {
		t.Fatal(err)
	}
	insp, _ := workshop.InspectionsForID(ctx, pool, technician(), id)
	for i, it := range insp.Items {
		status := "pass"
		if i == 1 {
			status = "attention"
		}
		if err := workshop.SetItem(ctx, pool, technician(), it.ID, status, "Rear pads at 3 mm"); err != nil {
			t.Fatal(err)
		}
	}
	if err := workshop.CompleteInspection(ctx, pool, technician(), id); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReportFinding(ctx, pool, technician(), job, "Left driveshaft boot is split"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AddRecommendation(ctx, pool, technician(), job, "Replace rear pads", "within three months"); err != nil {
		t.Fatal(err)
	}

	d, err := workshop.DeliveryDraft(ctx, pool, technician(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !has(d.Completed, "Diagnosis") || len(d.Completed) != 1 {
		t.Errorf("completed = %v", texts(d.Completed))
	}
	if !has(d.Declined, "Respray bonnet") && !has(d.Declined, "Replace gearbox") {
		t.Errorf("declined = %v", texts(d.Declined))
	}
	if len(d.Unfinished) == 0 {
		t.Fatalf("nothing unfinished, but the exhaust waits for a part")
	}
	var why string
	for _, u := range d.Unfinished {
		if u.Text == "Replace exhaust" {
			why = u.Why
		}
	}
	if why != "waiting for a part" {
		t.Errorf("the exhaust is unfinished because %q", why)
	}
	if has(d.Completed, "Respray bonnet") || has(d.Unfinished, "Respray bonnet") {
		t.Error("a quote nobody answered is listed as work")
	}
	if len(d.Inspections) != 1 || d.Inspections[0].Attention != 1 || len(d.Inspections[0].Findings) != 1 {
		t.Errorf("inspections = %+v", d.Inspections)
	}
	if !has(d.Unresolved, "Left driveshaft boot is split") {
		t.Errorf("unresolved = %v", texts(d.Unresolved))
	}
	if !has(d.Recommendations, "Replace rear pads") {
		t.Errorf("recommendations = %v", texts(d.Recommendations))
	}
}

func TestHandingOverFreezesTheSummaryAndACorrectionIsTheNextVersion(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs", "Replace exhaust")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	tasks := tasksOf(t, pool, job)
	taskMove(t, pool, technician(), tasks[0].ID, workshop.TaskMove{To: workshop.TaskDone})

	if _, err := workshop.IssueSummary(ctx, pool, advisor(), job, workshop.SummaryInput{}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("handing over to nobody = %v, want ErrInvalid", err)
	}
	if _, err := workshop.IssueSummary(ctx, pool, technician(), job, workshop.SummaryInput{DeliveredTo: "Karl"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician handed a car over: %v", err)
	}
	fresh := newJob(t, pool)
	if _, err := workshop.IssueSummary(ctx, pool, advisor(), fresh, workshop.SummaryInput{DeliveredTo: "Karl"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a summary for a job nothing has happened on = %v, want ErrInvalid", err)
	}

	// Her brother collects it, partly done.
	in := workshop.SummaryInput{DeliveredTo: "Karl, her brother", Instructions: "Brake in the new discs gently for 200 km."}
	v1, err := workshop.IssueSummary(ctx, pool, advisor(), job, in)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || v1.DeliveredTo != "Karl, her brother" || v1.DeliveredBy == "" || !has(v1.Content.Completed, "Replace brake discs") || !has(v1.Content.Unfinished, "Replace exhaust") {
		t.Errorf("version 1 = %+v", v1)
	}
	// Pressed twice, it is the same one.
	again, err := workshop.IssueSummary(ctx, pool, advisor(), job, in)
	if err != nil || again.ID != v1.ID {
		t.Errorf("the same handover again = %q, %v; want the version already issued", again.ID, err)
	}

	// Frozen by the database, not by manners.
	for _, q := range []string{
		`UPDATE delivery_summaries SET instructions = 'x' WHERE id = $1`,
		`DELETE FROM delivery_summaries WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, v1.ID); err == nil || !strings.Contains(err.Error(), "cannot be") {
			t.Errorf("%q = %v, want the database to refuse", q, err)
		}
	}

	// The exhaust is finished and the car comes back for it; a correction is
	// the next version and the first is as it was.
	taskMove(t, pool, technician(), tasks[1].ID, workshop.TaskMove{To: workshop.TaskDone})
	v2, err := workshop.IssueSummary(ctx, pool, advisor(), job, workshop.SummaryInput{DeliveredTo: "Her", Instructions: in.Instructions})
	if err != nil || v2.Version != 2 || v2.Replaces != 1 {
		t.Fatalf("version 2 = %+v, %v", v2, err)
	}
	if !has(v2.Content.Completed, "Replace exhaust") || len(v2.Content.Unfinished) != 0 {
		t.Errorf("version 2 content = %+v", v2.Content)
	}
	list, _ := workshop.SummariesFor(ctx, pool, advisor(), job)
	if len(list) != 2 || list[1].Superseded != true || list[0].Superseded {
		t.Errorf("versions: %+v", list)
	}
	old, _ := workshop.SummaryVersion(ctx, pool, advisor(), job, 1)
	if !has(old.Content.Unfinished, "Replace exhaust") || len(old.Content.Completed) != 1 {
		t.Errorf("the first version changed after the correction: %+v", old.Content)
	}
}

func TestACustomerReadsTheLatestOnTheirLinkAndPaymentDoesNotChangeIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := readyToInvoice(t, pool)
	if err := workshop.MarkPerformed(ctx, pool, advisor(), job, 1, true); err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	half := inv.GrossMinor / 2
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: &half, Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A partly paid car is handed over like any other.
	v1, err := workshop.IssueSummary(ctx, pool, advisor(), job, workshop.SummaryInput{DeliveredTo: "A Customer"})
	if err != nil {
		t.Fatalf("a partly paid collection: %v", err)
	}
	token, err := workshop.CreateJobLink(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	got, shop, err := workshop.CustomerSummary(ctx, pool, shopID, token)
	if err != nil || got.ID != v1.ID || shop == "" {
		t.Fatalf("CustomerSummary = %q, %q, %v", got.ID, shop, err)
	}
	st, err := workshop.JobStatus(ctx, pool, shopID, token)
	if err != nil || !st.HasSummary || st.OutstandingMinor <= 0 {
		t.Errorf("the customer's page: summary %v, outstanding %d, %v", st.HasSummary, st.OutstandingMinor, err)
	}
	// A credit note changes the money; it does not rewrite what was done.
	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatalf("credit note: %v", err)
	}
	after, _, _ := workshop.CustomerSummary(ctx, pool, shopID, token)
	if len(after.Content.Completed) != len(v1.Content.Completed) || after.ID != v1.ID {
		t.Errorf("a credit note changed the summary: %+v", after.Content.Completed)
	}
	if _, _, err := workshop.CustomerSummary(ctx, pool, shopID, "not-a-token"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a wrong link = %v, want ErrShareNotUsable", err)
	}
}

func TestACarReturningAfterTheFirstHandoverGetsAVersionAndAnotherJobGetsItsOwn(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	taskMove(t, pool, technician(), tasksOf(t, pool, job)[0].ID, workshop.TaskMove{To: workshop.TaskDone})
	move(t, pool, job, workshop.StateReady)
	v1, err := workshop.IssueSummary(ctx, pool, advisor(), job, workshop.SummaryInput{DeliveredTo: "A Customer"})
	if err != nil {
		t.Fatal(err)
	}
	// It comes back with a squeal; the work is added, the car is ready again.
	if _, err := workshop.AddTask(ctx, pool, technician(), job, "Re-torque the wheels", 0, ""); err != nil {
		t.Fatal(err)
	}
	taskMove(t, pool, technician(), tasksOf(t, pool, job)[1].ID, workshop.TaskMove{To: workshop.TaskDone})
	v2, err := workshop.IssueSummary(ctx, pool, advisor(), job, workshop.SummaryInput{DeliveredTo: "A Customer"})
	if err != nil || v2.Version != 2 || !has(v2.Content.Completed, "Re-torque the wheels") || !has(v2.Content.Completed, "Replace brake discs") {
		t.Fatalf("the second handover = %+v, %v", v2.Content, err)
	}
	if len(v1.Content.Completed) != 1 {
		t.Errorf("the first handover changed: %+v", v1.Content.Completed)
	}
	// A different visit is a different job with its own summary.
	other := newJob(t, pool)
	d, _ := workshop.DeliveryDraft(ctx, pool, advisor(), other)
	if !d.Empty() {
		t.Errorf("a new job's summary carries the last visit's work: %+v", d)
	}
}

func TestARecommendationIsNotAnApprovalAndNobodyIsToldAboutIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs")
	if err := workshop.AddRecommendation(ctx, pool, technician(), job, "Replace rear pads", "within three months"); err != nil {
		t.Fatal(err)
	}
	recs, _ := workshop.RecommendationsFor(ctx, pool, technician(), job)
	if len(recs) != 1 || recs[0].When != "within three months" {
		t.Fatalf("recommendations = %+v", recs)
	}
	if _, err := workshop.RecommendationToEnquiry(ctx, pool, technician(), recs[0].ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician started an enquiry: %v", err)
	}
	before := countRows(t, pool, `SELECT count(*) FROM outbound_messages`)
	enq, err := workshop.RecommendationToEnquiry(ctx, pool, advisor(), recs[0].ID)
	if err != nil || enq == "" {
		t.Fatalf("RecommendationToEnquiry = %q, %v", enq, err)
	}
	if again, _ := workshop.RecommendationToEnquiry(ctx, pool, advisor(), recs[0].ID); again != enq {
		t.Error("doing it twice made two enquiries")
	}
	if countRows(t, pool, `SELECT count(*) FROM outbound_messages`) != before {
		t.Error("starting an enquiry sent a message")
	}
	if countRows(t, pool, `SELECT count(*) FROM quotes WHERE work_order_id = $1`, job) != 1 {
		t.Error("a recommendation changed the job's quotes")
	}
	e, err := workshop.EnquiryByID(ctx, pool, advisor(), enq)
	if err != nil || e.Subject != "Replace rear pads" || e.Registration == "" {
		t.Errorf("the enquiry = %+v, %v", e, err)
	}
	// Whoever made it takes it back; somebody else on the floor may not.
	if err := workshop.WithdrawRecommendation(ctx, pool, partsDesk(), recs[0].ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the parts desk withdrew a recommendation: %v", err)
	}
	if err := workshop.WithdrawRecommendation(ctx, pool, technician(), recs[0].ID); err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.DeliveryDraft(ctx, pool, advisor(), job)
	if len(d.Recommendations) != 0 {
		t.Error("a withdrawn recommendation is still in the summary")
	}
}
