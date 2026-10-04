package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The shelf's Add button, as the browser sends it, puts a costed part on the
// job at its cost with the shop's markup -- not at nothing.
//
// The form used to carry the part's price in a hidden field. A part priced
// from its cost has no price of its own, so the field was empty, empty was
// read as zero, and the part went onto the job for free.
func TestAPartFromTheShelfIsNotFree(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	const part = "aaaaaaaa-0000-0000-0000-0000000000a2"
	if _, err := pool.Exec(ctx, `INSERT INTO parts (id, shop_id, number, name, unit, cost_minor)
		VALUES ($1, $2, 'BP-1', 'Brake pads', 'each', 40000)`, part, shopA); err != nil {
		t.Fatalf("seed part: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO price_bands (shop_id, up_to_minor, markup_basis)
		VALUES ($1, NULL, 5000)`, shopA); err != nil {
		t.Fatalf("seed band: %v", err)
	}

	desk := signIn(t, ts, advisorEmail)
	form := url.Values{"kind": {"part"}, "cost_bearer": {"customer"}, "part_id": {part},
		"description": {"Brake pads (BP-1)"}, "quantity": {"1"}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/"+jobA+"/lines", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := desk.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("adding from the shelf answered %d", resp.StatusCode)
	}

	var got int64
	if err := pool.QueryRow(ctx, `SELECT unit_price_minor FROM work_order_lines WHERE part_id = $1`, part).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 60000 {
		t.Errorf("the line was priced at %d, want 600.00: cost 400.00 and half again", got)
	}
}
