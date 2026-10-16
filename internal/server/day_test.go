package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Closing the day through the pages: the figures, a comparison that needs a
// reason when it differs, the owner's sign-off, and the day saying so when it
// changes after it was signed.
func TestClosingTheDayThroughThePages(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	var invoice string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, seller_name, seller_payment_terms_days)
			VALUES ($1, 'A', 55, $2, 'Test Kund', 100000, 25000, 125000, 'Shop A', 30) RETURNING id::text`, shopA, jobA).Scan(&invoice)
	}); err != nil {
		t.Fatal(err)
	}
	desk, owner, tech := signIn(t, ts, advisorEmail), signIn(t, ts, ownerEmail), signIn(t, ts, techEmail)
	base := ts.URL
	day := time.Now().Format("2006-01-02")

	if status, _ := page(t, tech, base+"/day"); status != http.StatusForbidden {
		t.Errorf("a technician opened the day: %d", status)
	}
	status, body := page(t, desk, base+"/day")
	if status != http.StatusOK || !strings.Contains(body, "A-55") || !strings.Contains(body, "Net sales") || !strings.Contains(body, "not the bank") && !strings.Contains(body, "what was entered") {
		t.Fatalf("the day answered %d: %s", status, body)
	}
	if status := postStatus(t, desk, base+"/invoices/"+invoice+"/payments", url.Values{"amount": {"500"}, "paid_on": {day}, "method": {"bank"}}); status != http.StatusSeeOther {
		t.Fatalf("recording a payment answered %d", status)
	}
	if _, body = page(t, desk, base+"/day?d="+day); !strings.Contains(body, "bank") || !strings.Contains(body, "500") {
		t.Errorf("the day does not show the payment: %s", body)
	}

	// A difference needs a reason; an agreement does not.
	cmp := url.Values{"d": {day}, "method": {"bank"}, "external": {"480"}}
	if status := postStatus(t, desk, base+"/day/settlement", cmp); status != http.StatusBadRequest {
		t.Errorf("a difference with no reason answered %d, want 400", status)
	}
	cmp.Set("explanation", "Fee taken first")
	if status := postStatus(t, desk, base+"/day/settlement", cmp); status != http.StatusSeeOther {
		t.Fatalf("a difference with a reason answered %d", status)
	}
	if _, body = page(t, desk, base+"/day?d="+day); !strings.Contains(body, "Fee taken first") || !strings.Contains(body, "difference") {
		t.Error("the comparison and its reason are not on the day")
	}

	// Only the owner signs it off, and the day says so when it changes after.
	if status := postStatus(t, desk, base+"/day/review", url.Values{"d": {day}}); status != http.StatusForbidden {
		t.Errorf("the counter signed the day off: %d", status)
	}
	if status := postStatus(t, owner, base+"/day/review", url.Values{"d": {day}, "note": {"All good"}}); status != http.StatusSeeOther {
		t.Fatalf("the owner's sign-off answered %d", status)
	}
	if _, body = page(t, owner, base+"/day?d="+day); !strings.Contains(body, "reviewed") || strings.Contains(body, "has changed since") {
		t.Error("the signed-off day does not say reviewed")
	}
	if status := postStatus(t, desk, base+"/invoices/"+invoice+"/payments", url.Values{"amount": {"300"}, "paid_on": {day}, "method": {"cash"}}); status != http.StatusSeeOther {
		t.Fatalf("a later payment answered %d", status)
	}
	if _, body = page(t, owner, base+"/day?d="+day); !strings.Contains(body, "has changed since it was reviewed") {
		t.Error("a payment added after the sign-off did not make the day stale")
	}
	if status, _ := page(t, desk, base+"/day?d=not-a-day"); status != http.StatusBadRequest {
		t.Errorf("a made-up day answered %d, want 400", status)
	}
}
