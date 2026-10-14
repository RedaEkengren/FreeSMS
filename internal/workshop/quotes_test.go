package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func decisionsOn(t *testing.T, pool *pgxpool.Pool, job string) int {
	t.Helper()
	var n int
	if err := database.InShop(context.Background(), pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM quote_decisions WHERE work_order_id = $1`, job).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// The customer pressed "Yes" on a page that showed no price. Pricing it later
// does not make the price agreed to: until a quote with the price on it has
// been answered, the line is not approved and is not invoiced.
func TestAnUnpricedYesDoesNotApproveThePriceEnteredAfterwards(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job, item := answered(t, pool, "approved")
	if err := workshop.PriceAnswer(ctx, pool, advisor(), job, item, brakes); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, pool, job); len(got) != 1 || got[0] != "quoted" {
		t.Fatalf("a line priced from an unpriced yes is %v, want quoted", got)
	}
	move(t, pool, job, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Fatalf("invoicing a price the customer never saw = %v, want ErrNothingToInvoice", err)
	}

	q, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if q.GrossMinor != 167813 { // 1.5 h at 895.00 = 1342.50 + 25% VAT
		t.Errorf("the quote comes to %d, want 167813", q.GrossMinor)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, workshop.QuoteAnswer{
		Decision: "approved", Via: "phone", AgreedGrossMinor: q.GrossMinor}); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, pool, job); got[0] != "approved" {
		t.Errorf("after the yes to the priced quote the line is %v", got)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("invoicing what was agreed: %v", err)
	}
	if inv.NetMinor != 134250 {
		t.Errorf("the invoice is %d net", inv.NetMinor)
	}
}

// The yes is to the version shown. Lines added afterwards are not in it, and
// an answer sent for the old version -- from a link opened earlier, or from
// an offline queue -- is turned away rather than read as an answer to the new.
func TestAYesIsToTheVersionTheCustomerSaw(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	token, err := workshop.CreateJobLink(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	quote(t, pool, job, "Brake pads", 1000)
	v1, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	// Presenting the same lines again is the same version, not a second.
	again, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil || again.ID != v1.ID || again.Version != 1 {
		t.Fatalf("presenting twice: %+v, %v", again, err)
	}

	quote(t, pool, job, "Wheel alignment", 1000)
	v2, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || len(v2.Lines) != 2 || v2.GrossMinor <= v1.GrossMinor {
		t.Fatalf("the second version: %+v", v2)
	}
	// What the first showed is what it still shows.
	view, err := workshop.QuoteFor(ctx, pool, advisor(), job)
	if err != nil || view.Latest.ID != v2.ID {
		t.Fatalf("QuoteFor: %+v, %v", view, err)
	}

	if err := workshop.QuoteAnswerFromLink(ctx, pool, shopID, token, v1.ID, "approved"); !errors.Is(err, workshop.ErrQuoteSuperseded) {
		t.Errorf("a yes to the old version = %v, want ErrQuoteSuperseded", err)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, v1.ID, workshop.QuoteAnswer{
		Decision: "approved", Via: "phone", AgreedGrossMinor: v1.GrossMinor}); !errors.Is(err, workshop.ErrQuoteSuperseded) {
		t.Errorf("a telephone yes to the old version = %v, want ErrQuoteSuperseded", err)
	}
	if n := decisionsOn(t, pool, job); n != 0 {
		t.Errorf("%d answers recorded for a version nobody may answer", n)
	}
	if got := statuses(t, pool, job); got[0] != "quoted" || got[1] != "quoted" {
		t.Errorf("the lines moved without an answer: %v", got)
	}
}

// By telephone the amount is typed and has to be the quote's: an approval is
// of a number somebody said out loud.
func TestATelephoneYesNamesTheAmountAndWhoWroteItDown(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1000)
	q, _ := workshop.PresentQuote(ctx, pool, advisor(), job)

	wrong := workshop.QuoteAnswer{Decision: "approved", Via: "phone", AgreedGrossMinor: q.GrossMinor - 100}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, wrong); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a yes to the wrong amount = %v, want ErrInvalid", err)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, workshop.QuoteAnswer{
		Decision: "approved", Via: "link", AgreedGrossMinor: q.GrossMinor}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("writing down an answer as if it came from the link = %v, want ErrInvalid", err)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, technician(), job, q.ID, workshop.QuoteAnswer{
		Decision: "approved", Via: "phone", AgreedGrossMinor: q.GrossMinor}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician wrote down the customer's yes: %v", err)
	}
	if _, err := workshop.PresentQuote(ctx, pool, technician(), job); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician put a quote to the customer: %v", err)
	}
	if n := decisionsOn(t, pool, job); n != 0 {
		t.Fatalf("refused answers left %d rows", n)
	}

	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, workshop.QuoteAnswer{
		Decision: "approved", Via: "phone", AgreedGrossMinor: q.GrossMinor, Note: "Rang back at lunch"}); err != nil {
		t.Fatal(err)
	}
	view, _ := workshop.QuoteFor(ctx, pool, advisor(), job)
	if len(view.Decisions) != 1 {
		t.Fatalf("decisions: %+v", view.Decisions)
	}
	d := view.Decisions[0]
	if d.Via != "phone" || d.RecordedBy == "" || d.AgreedMinor() != q.GrossMinor || d.Note != "Rang back at lunch" || d.Version != 1 {
		t.Errorf("the record: %+v", d)
	}
}

// The customer on their own link: the amount is the one they were shown, the
// answer carries no member of staff, and pressing twice is one answer. A
// changed mind is a second row, not an edit of the first.
func TestTheCustomersOwnAnswerIsRecordedAsTheirs(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	token, _ := workshop.CreateJobLink(ctx, pool, advisor(), job)
	quote(t, pool, job, "Brake pads", 1000)
	q, _ := workshop.PresentQuote(ctx, pool, advisor(), job)

	for i := 0; i < 2; i++ {
		if err := workshop.QuoteAnswerFromLink(ctx, pool, shopID, token, q.ID, "approved"); err != nil {
			t.Fatal(err)
		}
	}
	if n := decisionsOn(t, pool, job); n != 1 {
		t.Errorf("pressing yes twice recorded %d answers", n)
	}
	st, err := workshop.JobStatus(ctx, pool, shopID, token)
	if err != nil || st.Quote == nil || st.Quote.Answer != "approved" || st.Quote.GrossMinor != q.GrossMinor {
		t.Fatalf("the customer's page: %+v, %v", st.Quote, err)
	}

	if err := workshop.QuoteAnswerFromLink(ctx, pool, shopID, token, q.ID, "declined"); err != nil {
		t.Fatal(err)
	}
	if n := decisionsOn(t, pool, job); n != 2 {
		t.Errorf("changing their mind left %d answers, want both", n)
	}
	if got := statuses(t, pool, job); got[0] != "declined" {
		t.Errorf("after the no the line is %v", got)
	}
	view, _ := workshop.QuoteFor(ctx, pool, advisor(), job)
	if view.Decisions[0].Via != "link" || view.Decisions[0].RecordedBy != "" || view.Decisions[0].AgreedMinor() != q.GrossMinor {
		t.Errorf("the first answer: %+v", view.Decisions[0])
	}

	// A wrong or closed link answers nothing.
	if err := workshop.QuoteAnswerFromLink(ctx, pool, shopID, "a-guess", q.ID, "approved"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("an answer on a link that does not exist = %v", err)
	}
}

// What the customer agreed to is not asked again when more work turns up: the
// new lines are a version of their own, and the old yes stands.
func TestExtraWorkIsItsOwnVersionAndTheOldYesStands(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1000)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval, workshop.StateApproved, workshop.StateInProgress)
	quote(t, pool, job, "Seized bolt, drilled out", 500)

	q2, err := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	if q2.Version != 2 || len(q2.Lines) != 1 || q2.Lines[0].Description != "Seized bolt, drilled out" {
		t.Fatalf("the second version holds %+v", q2)
	}
	if got := statuses(t, pool, job); got[0] != "approved" || got[1] != "quoted" {
		t.Errorf("lines: %v", got)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q2.ID, workshop.QuoteAnswer{
		Decision: "declined", Via: "phone"}); err != nil {
		t.Fatal(err)
	}
	got := statuses(t, pool, job)
	if got[0] != "approved" || got[1] != "declined" {
		t.Errorf("declining the extra work touched the original: %v", got)
	}
	if _, err := workshop.PresentQuote(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("nothing is left to ask, and PresentQuote = %v", err)
	}
}

// Eight people at the desk pressing the button give one version.
func TestPresentingAtOnceMakesOneVersion(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := workshop.PresentQuote(ctx, pool, advisor(), job); err != nil {
				t.Errorf("PresentQuote: %v", err)
			}
		}()
	}
	wg.Wait()
	var n int
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM quotes WHERE work_order_id = $1`, job).Scan(&n)
	})
	if n != 1 {
		t.Errorf("%d versions made at once, want 1", n)
	}
}

// A job is not approved unless what the customer pays for was agreed to; one
// with nothing for them to pay has nothing to agree.
func TestAJobIsNotApprovedWithoutTheCustomersAgreement(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	empty := newJob(t, pool)
	move(t, pool, empty, workshop.StateEstimated, workshop.StateAwaitingApproval)
	if err := workshop.SetState(ctx, pool, advisor(), empty, workshop.StateApproved); err != nil {
		t.Errorf("approving a job with nothing for the customer to pay: %v", err)
	}

	job := newJob(t, pool)
	quote(t, pool, job, "Brake pads", 1000)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval)
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateApproved); !errors.Is(err, workshop.ErrNeedsApproval) {
		t.Fatalf("approving with nothing recorded = %v, want ErrNeedsApproval", err)
	}
	view, _ := workshop.QuoteFor(ctx, pool, advisor(), job)
	if view.CanApprove || view.Pending != 1 {
		t.Errorf("the desk is told %+v", view)
	}
	q, _ := workshop.PresentQuote(ctx, pool, advisor(), job)
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateApproved); !errors.Is(err, workshop.ErrNeedsApproval) {
		t.Errorf("approving a quote nobody answered = %v", err)
	}
	if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, workshop.QuoteAnswer{Decision: "declined", Via: "phone"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateApproved); !errors.Is(err, workshop.ErrNeedsApproval) {
		t.Errorf("approving a job whose every line was declined = %v", err)
	}
}
