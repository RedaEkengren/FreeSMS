package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The limit on the stock feed (#183).

// A bucket lets a burst through, refuses the next request and says when to
// come back, and fills again at its rate and no further than its burst.
func TestABucketRefillsAtItsRateAndNoFurtherThanItsBurst(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	b := newBucket(2, 3, func() time.Time { return at })
	for i := range 3 {
		if ok, _ := b.take(); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait := b.take()
	if ok || wait != 500*time.Millisecond {
		t.Errorf("past the burst: %v, wait %v; want refused, half a second at two a second", ok, wait)
	}
	at = at.Add(500 * time.Millisecond)
	if ok, _ := b.take(); !ok {
		t.Error("refused after the time it was told to wait")
	}
	if ok, _ := b.take(); ok {
		t.Error("a second request let through on one token's worth of time")
	}
	at = at.Add(time.Hour)
	for i := range 3 {
		if ok, _ := b.take(); !ok {
			t.Fatalf("request %d after a long wait refused", i+1)
		}
	}
	if ok, _ := b.take(); ok {
		t.Error("an hour of waiting stored more than the burst")
	}
	var none *bucket
	if ok, _ := none.take(); !ok {
		t.Error("no limit refused a request")
	}
}

// A caller that hammers the feed is refused with when to come back, whether
// or not the feed is published, and the photographs are not counted.
func TestTheStockFeedRefusesAHammeringCallerAndSaysWhenToComeBack(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	p, err := workshop.RecordPurchase(ctx, pool, owner, workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase,
		NewSeller: &workshop.NewParty{Kind: "private", Name: "Sam Säljare"}, PurchasedOn: time.Now(), PriceMinor: 8_543_210,
		SellerType: workshop.SellerPrivate})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, owner, p.StockUnitID, workshop.StockForSale, ""); err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, signIn(t, ts, ownerEmail), ts.URL+"/cars/feed", url.Values{"published": {"yes"}, "open": {"yes"}}); status != http.StatusSeeOther {
		t.Fatalf("publishing the feed answered %d", status)
	}
	anyone := noRedirects()
	refused, served := 0, 0
	var retry string
	for range feedBurst + 40 {
		resp, err := anyone.Get(ts.URL + "/feed/v1/stock.json")
		if err != nil {
			t.Fatal(err)
		}
		readBody(t, resp)
		switch resp.StatusCode {
		case http.StatusOK:
			served++
		case http.StatusTooManyRequests:
			refused++
			retry = resp.Header.Get("Retry-After")
		default:
			t.Fatalf("the feed answered %d", resp.StatusCode)
		}
	}
	if n, err := strconv.Atoi(retry); err != nil || n < 1 {
		t.Errorf("Retry-After on a refusal: %q", retry)
	}
	// Ten a second refills while the loop runs, so a few more than the burst
	// get through; what matters is that it stops.
	if served < feedBurst || refused == 0 {
		t.Errorf("%d served and %d refused of %d requests; want the burst served and the rest, or most of it, refused", served, refused, feedBurst+40)
	}
}
