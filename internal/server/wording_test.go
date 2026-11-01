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

// #134: every page that shows money owed back shows it as an amount under a
// label that says which way it runs, in English and in Swedish. The
// receivables list printed it with a minus sign beside "owed back to the
// customer"; the job page and the customer card show no balance at all, and
// are read to make sure they do not start to with the sign on.
func TestMoneyOwedBackHasNoMinusSignOnAnyPage(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	var invoice, customer string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT customer_id::text FROM work_orders WHERE id = $1`, jobA).Scan(&customer); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, seller_name, seller_payment_terms_days)
			VALUES ($1, 'A', 79, $2, 'Test Kund', 100000, 25000, 125000, 'Shop A', 30) RETURNING id::text`, shopA, jobA).Scan(&invoice)
	}); err != nil {
		t.Fatal(err)
	}
	desk := signIn(t, ts, advisorEmail)
	day := time.Now().Format("2006-01-02")
	if got := postStatus(t, desk, ts.URL+"/invoices/"+invoice+"/payments", url.Values{
		"amount": {"1500"}, "paid_on": {day}, "method": {"bank"}, "confirm_over": {"1"}}); got != http.StatusSeeOther {
		t.Fatalf("the overpayment answered %d", got)
	}
	minus := regexp.MustCompile(`[-−]\s*(SEK\s*)?250[.,]00`)

	for _, lang := range []string{"en", "sv"} {
		if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE shops SET locale = $2 WHERE id = $1`, shopA, lang)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, p := range []struct {
			path        string
			showsTheSum bool
		}{
			{"/invoices/" + invoice, true},
			{"/invoices/" + invoice + "/followup", true},
			{"/receivables", true},
			{"/jobs/" + jobA, false},
			{"/customers/" + customer, false},
		} {
			status, body := page(t, desk, ts.URL+p.path)
			if status != http.StatusOK {
				t.Errorf("%s %s answered %d", lang, p.path, status)
				continue
			}
			if m := minus.FindString(body); m != "" {
				t.Errorf("%s %s shows the amount owed back as %q", lang, p.path, m)
			}
			if p.showsTheSum && !regexp.MustCompile(`250[.,]00`).MatchString(body) {
				t.Errorf("%s %s does not show the 250.00 owed back", lang, p.path)
			}
		}
	}
}

// #131: the question about a line nobody agreed to, and the decision to
// invoice it anyway, are the front desk's. The technician's page of the same
// job does not ask it, and a technician who posts the tick is refused.
//
// The technician's page does show the line table with its prices, for every
// line, agreed or not; that is how the job page has always been built and is
// not changed here.
func TestATechnicianIsNotAskedAboutALineNobodyAgreedTo(t *testing.T) {
	ts, pool := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	job := ts.URL + "/jobs/" + jobA

	postStatus(t, desk, job+"/lines", url.Values{"kind": {"labour"}, "description": {"Service"}, "quantity": {"1"}, "unit_price": {"1000"}})
	for _, to := range []string{"estimated", "awaiting_approval", "approved", "in_progress"} {
		if to == "approved" {
			agreeAtTheCounter(t, ts.URL, jobA, desk, pool)
		}
		if status := postStatus(t, desk, job+"/state", url.Values{"state": {to}}); status != http.StatusSeeOther {
			t.Fatalf("moving to %s answered %d", to, status)
		}
	}
	if status := postStatus(t, desk, job+"/lines", url.Values{"kind": {"labour"}, "description": {"Extra wiper blades"},
		"quantity": {"1"}, "unit_price": {"4321"}}); status != http.StatusSeeOther {
		t.Fatalf("adding the extra line answered %d", status)
	}
	if status := postStatus(t, tech, job+"/state", url.Values{"state": {"ready"}}); status != http.StatusSeeOther {
		t.Fatalf("marking it ready answered %d", status)
	}

	_, counter := page(t, desk, job)
	if !strings.Contains(counter, "Invoice it with the lines nobody agreed to") {
		t.Fatalf("the front desk is not asked about the unagreed line:\n%s", firstLines(counter, 60))
	}
	_, spanner := page(t, tech, job)
	if !strings.Contains(spanner, "Extra wiper blades") {
		t.Error("the technician does not see the work")
	}
	for _, hidden := range []string{"Invoice it with the lines nobody agreed to", "has no agreement", "confirm_unagreed"} {
		if strings.Contains(spanner, hidden) {
			t.Errorf("the technician's page shows %q", hidden)
		}
	}
	if status := postStatus(t, tech, job+"/invoice", url.Values{"confirm_unagreed": {"1"}}); status != http.StatusForbidden {
		t.Errorf("a technician invoicing with the tick answered %d, want 403", status)
	}
}

// #137: a well-formed id that is no job is not found, on the two routes that
// used to answer 500 for it.
func TestStartingAnInspectionOrClockingOutOfNoJobIsNotFound(t *testing.T) {
	ts, pool := testServer(t)
	tech := signIn(t, ts, techEmail)
	var template string
	if err := database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO inspection_templates (shop_id, name) VALUES ($1, 'Service check') RETURNING id::text`, shopA).Scan(&template)
	}); err != nil {
		t.Fatalf("a template to start with: %v", err)
	}
	const nothing = "aaaaaaaa-0000-0000-0000-0000000000ff"
	for _, path := range []string{"/jobs/" + nothing + "/inspect", "/jobs/" + nothing + "/clock-out"} {
		if got := postStatus(t, tech, ts.URL+path, url.Values{"template_id": {template}}); got != http.StatusNotFound {
			t.Errorf("POST %s answered %d, want 404", path, got)
		}
	}
}
