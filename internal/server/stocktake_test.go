package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The count form records what is on the shelf; an empty one records nothing;
// and the old signless "difference" is gone from the movement form.
func TestTheCountFormRecordsWhatIsOnTheShelf(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	const part = "aaaaaaaa-0000-0000-0000-0000000000a1"
	for _, q := range []string{
		`INSERT INTO parts (id, shop_id, number, name, unit) VALUES ($1, $2, 'OF-1', 'Oil filter', 'each')`,
		`INSERT INTO stock_movements (shop_id, part_id, kind, quantity, moved_by)
		 VALUES ($2, $1, 'received', 10, 'aaaaaaaa-0000-0000-0000-00000000000b')`,
	} {
		if _, err := pool.Exec(ctx, q, part, shopA); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	onHand := func() float64 {
		t.Helper()
		var n float64
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(sum(quantity), 0) FROM stock_movements
			WHERE part_id = $1 AND kind <> 'reserved' AND kind <> 'unreserved'`, part).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	desk := signIn(t, ts, advisorEmail)
	post := func(path string, form url.Values) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := desk.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Empty is not zero: it would have booked the whole shelf out.
	if got := post("/stock/count", url.Values{"part_id": {part}, "counted": {"  "}}); got != http.StatusBadRequest {
		t.Errorf("an empty count answered %d, want 400", got)
	}
	if onHand() != 10 {
		t.Fatalf("an empty count moved the stock to %v", onHand())
	}

	// The old way, a positive "difference", is refused rather than taken.
	if got := post("/stock/move", url.Values{"part_id": {part}, "kind": {"counted"}, "quantity": {"2"}}); got != http.StatusBadRequest {
		t.Errorf("a stocktake difference on the movement form answered %d, want 400", got)
	}

	if got := post("/stock/count", url.Values{"part_id": {part}, "counted": {"8"}}); got != http.StatusSeeOther {
		t.Fatalf("counting eight answered %d", got)
	}
	if onHand() != 8 {
		t.Errorf("on hand = %v after counting eight", onHand())
	}
}
