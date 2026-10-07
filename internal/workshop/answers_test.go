package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A customer's yes on their link used to reach nobody: stored on the
// inspection, and nowhere else.

// answered is a job with one failed item the customer has answered.
func answered(t *testing.T, pool *pgxpool.Pool, decisions ...string) (jobID, itemID string) {
	t.Helper()
	ctx := context.Background()
	inspectionID, token, itemID := shared(t, pool)
	for _, d := range decisions {
		if err := workshop.RecordDecision(ctx, pool, shopID, token, itemID, d); err != nil {
			t.Fatalf("RecordDecision(%s): %v", d, err)
		}
	}
	insp, err := workshop.InspectionsForID(ctx, pool, advisor(), inspectionID)
	if err != nil {
		t.Fatalf("InspectionsForID: %v", err)
	}
	return insp.WorkOrderID, itemID
}

func boardCount(t *testing.T, pool *pgxpool.Pool, jobID string) int {
	t.Helper()
	board, err := workshop.Board(context.Background(), pool, advisor())
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, b := range board {
		if b.ID == jobID {
			return b.ApprovedUnpriced
		}
	}
	t.Fatalf("job %s is not on the board", jobID)
	return 0
}

var brakes = workshop.NewLine{Kind: "labour", Description: "Front brakes: Worn to the indicator",
	QuantityMilli: 1500, UnitPriceMinor: 89500, VATRateBasis: 2500}

func TestAnApprovalReachesTheJobAndTheBoardAndBecomesOneLine(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID, itemID := answered(t, pool, "approved")

	answers, err := workshop.CustomerAnswers(ctx, pool, advisor(), jobID)
	if err != nil {
		t.Fatalf("CustomerAnswers: %v", err)
	}
	if len(answers) != 1 || !answers[0].Priceable() || !answers[0].FromLink {
		t.Fatalf("answers = %+v, want one approved from the link and not yet priced", answers)
	}
	if got := answers[0].Description(); got != "Front brakes: Worn to the indicator" {
		t.Errorf("the line would say %q", got)
	}
	if n := boardCount(t, pool, jobID); n != 1 {
		t.Errorf("the board shows %d approved and unpriced, want 1", n)
	}

	if err := workshop.PriceAnswer(ctx, pool, advisor(), jobID, itemID, brakes); err != nil {
		t.Fatalf("PriceAnswer: %v", err)
	}

	// One line, approved as of the customer's answer, carrying which answer.
	var lines int
	var approvedAt, decidedAt time.Time
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_order_lines WHERE inspection_item_id = $1`, itemID).Scan(&lines); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT l.approved_at, d.decided_at FROM work_order_lines l
			JOIN inspection_decisions d ON d.id = l.customer_decision_id
			WHERE l.inspection_item_id = $1`, itemID).Scan(&approvedAt, &decidedAt)
	})
	if lines != 1 {
		t.Fatalf("%d lines, want 1", lines)
	}
	if !approvedAt.Equal(decidedAt) {
		t.Errorf("the line is approved at %v; the customer said yes at %v", approvedAt, decidedAt)
	}

	answers, _ = workshop.CustomerAnswers(ctx, pool, advisor(), jobID)
	if answers[0].Priceable() || answers[0].LineID == "" {
		t.Errorf("the approved item is still offered after it became a line: %+v", answers[0])
	}
	if n := boardCount(t, pool, jobID); n != 0 {
		t.Errorf("the board still shows %d to price", n)
	}
	if err := workshop.PriceAnswer(ctx, pool, advisor(), jobID, itemID, brakes); !errors.Is(err, workshop.ErrAlreadyALine) {
		t.Errorf("pricing it again: %v, want ErrAlreadyALine", err)
	}
}

// "They said no" is not "they have not answered", and it cannot be priced.
func TestADeclineIsShownAndCannotBePriced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID, itemID := answered(t, pool, "approved", "declined")

	answers, _ := workshop.CustomerAnswers(ctx, pool, advisor(), jobID)
	if len(answers) != 1 || answers[0].Decision != "declined" || answers[0].Priceable() {
		t.Fatalf("answers = %+v, want the latest answer, declined", answers)
	}
	if n := boardCount(t, pool, jobID); n != 0 {
		t.Errorf("the board counts a declined item: %d", n)
	}
	if err := workshop.PriceAnswer(ctx, pool, advisor(), jobID, itemID, brakes); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a declined item was priced: %v", err)
	}
}

// A line made from a yes stays when the customer changes their mind, and the
// item is not offered again: what to do now is a conversation, not a line
// quietly deleted.
func TestChangingTheirMindAfterPricingLeavesTheLine(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inspectionID, token, itemID := shared(t, pool)
	insp, _ := workshop.InspectionsForID(ctx, pool, advisor(), inspectionID)
	jobID := insp.WorkOrderID

	if err := workshop.RecordDecision(ctx, pool, shopID, token, itemID, "approved"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.PriceAnswer(ctx, pool, advisor(), jobID, itemID, brakes); err != nil {
		t.Fatalf("PriceAnswer: %v", err)
	}
	if err := workshop.RecordDecision(ctx, pool, shopID, token, itemID, "declined"); err != nil {
		t.Fatal(err)
	}

	answers, _ := workshop.CustomerAnswers(ctx, pool, advisor(), jobID)
	if answers[0].Decision != "declined" || answers[0].LineID == "" || answers[0].Priceable() {
		t.Errorf("after a change of mind: %+v, want declined with the line still there", answers[0])
	}
}

func TestTwoPeoplePricingTheSameYesMakeOneLine(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID, itemID := answered(t, pool, "approved")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- workshop.PriceAnswer(ctx, pool, advisor(), jobID, itemID, brakes)
		}()
	}
	wg.Wait()
	close(errs)
	var ok, already int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, workshop.ErrAlreadyALine):
			already++
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if ok != 1 || already != 1 {
		t.Errorf("%d priced and %d refused, want one of each", ok, already)
	}
}

// The customer's answers and the pricing are the front desk's.
func TestATechnicianNeitherSeesNorPricesAnAnswer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID, itemID := answered(t, pool, "approved")

	if _, err := workshop.CustomerAnswers(ctx, pool, technician(), jobID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the customer's answers: %v", err)
	}
	if err := workshop.PriceAnswer(ctx, pool, technician(), jobID, itemID, brakes); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician priced an answer: %v", err)
	}
}

// An item is priced on its own job, not on whichever job is named.
func TestAnAnswerIsPricedOnlyOnItsOwnJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, itemID := answered(t, pool, "approved")
	other := newJob2(t, pool)

	if err := workshop.PriceAnswer(ctx, pool, advisor(), other, itemID, brakes); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("an answer was priced onto another job: %v", err)
	}
}
