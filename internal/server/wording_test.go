package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// A refused move names the moves that are open, in the words on the buttons.
// It used to print the internal codes as a Go slice: [estimated cancelled].
func TestAMoveThatIsNotOpenNamesTheMovesThatAre(t *testing.T) {
	ts, _ := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	status, got := postBody(t, desk, ts.URL+"/jobs/"+jobA+"/state", url.Values{"state": {"ready"}})
	if status != http.StatusConflict {
		t.Fatalf("draft to ready answered %d, want 409", status)
	}
	if strings.Contains(got, "[") || strings.Contains(got, "estimated") {
		t.Errorf("the refusal shows a code: %s", got)
	}
	if !strings.Contains(got, "Price it") {
		t.Errorf("the refusal does not name the open move by its button: %s", got)
	}
}

// Money paid in beyond the invoice is owed back. The label says so, and the
// amount under it is the amount: not a minus sign saying it a second time.
func TestAnOverpaymentIsShownAsWhatIsOwedBack(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	var invoice string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, seller_name, seller_payment_terms_days)
			VALUES ($1, 'A', 77, $2, 'Test Kund', 100000, 25000, 125000, 'Shop A', 30) RETURNING id::text`, shopA, jobA).Scan(&invoice)
	}); err != nil {
		t.Fatal(err)
	}
	desk := signIn(t, ts, advisorEmail)
	day := time.Now().Format("2006-01-02")
	postStatus(t, desk, ts.URL+"/invoices/"+invoice+"/payments", url.Values{"amount": {"1500"}, "paid_on": {day}, "method": {"bank"}, "confirm_over": {"1"}})
	_, body := page(t, desk, ts.URL+"/invoices/"+invoice)
	owed := regexp.MustCompile(`Owed back to the customer</dt>\s*<dd[^>]*>\s*([^<]*)`).FindStringSubmatch(body)
	if owed == nil {
		t.Fatalf("the invoice does not say anything is owed back: %s", body)
	}
	if strings.ContainsAny(owed[1], "-−") {
		t.Errorf("the amount owed back carries a minus sign: %q", owed[1])
	}
}

// A payment dated before its invoice, or far above it, is asked about rather
// than recorded: the first moves money into a day with no such invoice, the
// second is usually an extra zero.
func TestAPaymentThatCannotBeRightIsNotRecordedUnasked(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	var invoice string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, seller_name, seller_payment_terms_days)
			VALUES ($1, 'A', 78, $2, 'Test Kund', 100000, 25000, 125000, 'Shop A', 30) RETURNING id::text`, shopA, jobA).Scan(&invoice)
	}); err != nil {
		t.Fatal(err)
	}
	desk := signIn(t, ts, advisorEmail)
	day := time.Now().Format("2006-01-02")
	pay := ts.URL + "/invoices/" + invoice + "/payments"

	if got := postStatus(t, desk, pay, url.Values{"amount": {"100"}, "paid_on": {"1999-01-01"}, "method": {"bank"}}); got != http.StatusBadRequest {
		t.Errorf("a payment dated before the invoice answered %d, want 400", got)
	}
	if got := postStatus(t, desk, pay, url.Values{"amount": {"12500"}, "paid_on": {day}, "method": {"bank"}}); got != http.StatusBadRequest {
		t.Errorf("a payment ten times the invoice answered %d, want 400", got)
	}
	// Half a krona over is cash rounding, and is not asked about.
	if got := postStatus(t, desk, pay, url.Values{"amount": {"1250,50"}, "paid_on": {day}, "method": {"cash"}}); got != http.StatusSeeOther {
		t.Errorf("fifty öre over answered %d, want 303", got)
	}
	// Meant, and said so: nothing is owed now, so any more is an overpayment.
	more := url.Values{"amount": {"100"}, "paid_on": {day}, "method": {"bank"}}
	if got := postStatus(t, desk, pay, more); got != http.StatusBadRequest {
		t.Errorf("a payment on a settled invoice answered %d, want 400", got)
	}
	more.Set("confirm_over", "1")
	if got := postStatus(t, desk, pay, more); got != http.StatusSeeOther {
		t.Errorf("a confirmed overpayment answered %d, want 303", got)
	}
}
