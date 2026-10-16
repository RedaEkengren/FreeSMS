package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/notify"
)

// An invoice to the customer through the pages: confirmed, queued, sent, opened
// on a link that opens that document only, closed by a resend, and handed over
// another way when messaging is not the answer.
func TestAnInvoiceReachesTheCustomerOnALinkThatOpensOnlyThatDocument(t *testing.T) {
	ts, pool := testServer(t)
	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	elks := &fakeElks{}
	srv.sms = notify.Elks{URL: elks.server(t).URL, Username: "u", Password: "p", From: "Verkstaden"}
	ctx := context.Background()
	var invoice string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, seller_name)
			VALUES ($1, 'A', 77, $2, 'Test Kund', 100000, 25000, 125000, 'Shop A') RETURNING id::text`, shopA, jobA).Scan(&invoice)
	}); err != nil {
		t.Fatal(err)
	}
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	doc := base + "/invoices/" + invoice

	if status, _ := page(t, tech, doc); status != http.StatusForbidden {
		t.Errorf("a technician opened an invoice: %d", status)
	}
	if _, body := page(t, desk, doc); !strings.Contains(body, "not delivered yet") {
		t.Error("an invoice nobody sent does not say so")
	}
	form := url.Values{"channel": {"sms"}, "address": {"070-123 45 67"}, "recipient": {"Bolagets ekonomi"}}
	if status := postStatus(t, desk, doc+"/send", form); status != http.StatusBadRequest {
		t.Errorf("sending without confirming answered %d, want 400", status)
	}
	if status := postStatus(t, tech, doc+"/send", form); status != http.StatusForbidden {
		t.Errorf("a technician sent an invoice: %d", status)
	}
	form.Set("confirm", "yes")
	if status := postStatus(t, desk, doc+"/send", form); status != http.StatusSeeOther {
		t.Fatalf("sending answered %d", status)
	}
	srv.sendDue(ctx, time.Now().Add(24*time.Hour))
	if elks.count() != 1 {
		t.Fatalf("46elks was sent %d texts, want 1", elks.count())
	}
	msg := elks.sent[0].Get("message")
	m := regexp.MustCompile(`/d/([A-Za-z0-9_-]{20,})`).FindStringSubmatch(msg)
	if m == nil || !strings.Contains(msg, "A-77") {
		t.Fatalf("the text does not carry the invoice and its link: %q", msg)
	}
	if _, body := page(t, desk, doc); !strings.Contains(body, "handed to the provider") || strings.Contains(body, "opened by the customer") {
		t.Error("a text the provider took is reported as read")
	}

	anon := &http.Client{}
	get := func(path string) (int, string) {
		resp, err := anon.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	status, theirs := get("/d/" + m[1])
	if status != http.StatusOK || !strings.Contains(theirs, "A-77") || !strings.Contains(theirs, "Test Kund") || !strings.Contains(theirs, "Where it stands today") {
		t.Fatalf("the customer's copy answered %d: %s", status, theirs)
	}
	if strings.Contains(theirs, "/jobs/") || strings.Contains(theirs, "/vehicles/") || strings.Contains(theirs, "Record the payment") {
		t.Error("the customer's copy carries the staff's pages or controls")
	}
	if _, body := page(t, desk, doc); !strings.Contains(body, "opened by the customer") {
		t.Error("opening the link did not show on the invoice")
	}

	// Sending again closes the first link.
	if status := postStatus(t, desk, doc+"/send", form); status != http.StatusSeeOther {
		t.Fatalf("resending answered %d", status)
	}
	if status, _ := get("/d/" + m[1]); status != http.StatusNotFound {
		t.Errorf("the first link still opens after a resend: %d", status)
	}
	srv.sendDue(ctx, time.Now().Add(48*time.Hour))
	second := regexp.MustCompile(`/d/([A-Za-z0-9_-]{20,})`).FindStringSubmatch(elks.sent[len(elks.sent)-1].Get("message"))
	if second == nil {
		t.Fatal("the resend carries no link")
	}
	if status, _ := get("/d/" + second[1]); status != http.StatusOK {
		t.Errorf("the new link answered %d", status)
	}
	// Revoked, it is gone.
	if status := postStatus(t, desk, doc+"/revoke", url.Values{}); status != http.StatusSeeOther {
		t.Fatalf("revoking answered %d", status)
	}
	if status, _ := get("/d/" + second[1]); status != http.StatusNotFound {
		t.Errorf("a revoked link answered %d", status)
	}
	if status, _ := get("/d/not-a-token"); status != http.StatusNotFound {
		t.Errorf("a made-up link answered %d", status)
	}
	// And a print at the counter is a delivery too.
	if status := postStatus(t, desk, doc+"/handover", url.Values{"recipient": {"Test Kund"}, "how": {"printed"}}); status != http.StatusSeeOther {
		t.Fatalf("recording a handover answered %d", status)
	}
	if _, body := page(t, desk, doc); !strings.Contains(body, "printed at the counter") {
		t.Error("the handover is not on the invoice")
	}
}

// Crediting part of an invoice through the pages: the form, the preview that
// writes nothing, the credit that follows, and a retry that is the same credit.
func TestCreditingPartOfAnInvoiceThroughThePages(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	var invoice string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, seller_name)
			VALUES ($1, 'A', 88, $2, 'Test Kund', 200000, 50000, 250000, 'Shop A') RETURNING id::text`, shopA, jobA).Scan(&invoice); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO invoice_lines (shop_id, invoice_id, position, kind, description, quantity, unit_price_minor, vat_rate_bp, net_minor, vat_minor)
			VALUES ($1, $2, 1, 'labour', 'Brakes', 2, 80000, 2500, 160000, 40000),
			       ($1, $2, 2, 'labour', 'Test drive', 1, 40000, 2500, 40000, 10000)`, shopA, invoice)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL + "/invoices/" + invoice

	if status, _ := page(t, tech, base+"/credit"); status != http.StatusForbidden {
		t.Errorf("a technician opened the credit form: %d", status)
	}
	status, form := page(t, desk, base+"/credit")
	key := regexp.MustCompile(`name="key" value="([0-9a-f]+)"`).FindStringSubmatch(form)
	if status != http.StatusOK || key == nil || !strings.Contains(form, "qty_1") || !strings.Contains(form, "qty_2") {
		t.Fatalf("the credit form answered %d: %s", status, form)
	}
	post := url.Values{"key": {key[1]}, "qty_1": {"1"}, "reason": {"Only one fitted"}}
	if status, _ := postBody(t, desk, base+"/credit", url.Values{"key": {key[1]}, "reason": {"nothing picked"}}); status != http.StatusBadRequest {
		t.Errorf("a credit that names nothing answered %d, want 400", status)
	}
	status, preview := postBody(t, desk, base+"/credit", post)
	if status != http.StatusOK || !strings.Contains(preview, "Check the credit") || !strings.Contains(preview, "Issue the credit note") {
		t.Fatalf("the preview answered %d: %s", status, preview)
	}
	if n := func() int {
		var n int
		pool.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE credit_of_id = $1`, invoice).Scan(&n)
		return n
	}(); n != 0 {
		t.Fatalf("a preview wrote %d credit notes", n)
	}
	post.Set("confirm", "1")
	if status := postStatus(t, desk, base+"/credit", post); status != http.StatusSeeOther {
		t.Fatalf("issuing answered %d", status)
	}
	if status := postStatus(t, desk, base+"/credit", post); status != http.StatusSeeOther {
		t.Fatalf("the same form again answered %d", status)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE credit_of_id = $1`, invoice).Scan(&n)
	if n != 1 {
		t.Errorf("%d credit notes after sending the form twice, want 1", n)
	}
	// More than is left is told why.
	again := url.Values{"qty_1": {"5"}, "reason": {"x"}}
	if status, body := postBody(t, desk, base+"/credit", again); status != http.StatusBadRequest || !strings.Contains(body, "is left to credit") {
		t.Errorf("crediting too much answered %d", status)
	}
	if _, body := page(t, desk, base); !strings.Contains(body, "Only one fitted") || !strings.Contains(body, "Credit note A-") {
		t.Errorf("the correction is not on the invoice")
	}
	if status, _ := page(t, desk, base+"/rebill?q=Greta"); status != http.StatusOK {
		t.Errorf("the rebill form answered %d", status)
	}
}
