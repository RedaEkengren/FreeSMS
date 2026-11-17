package workshop_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The stock feed's webhook (#183).

// pgxRow keeps stockCalls' signature short.
type pgxRow interface{ Scan(...any) error }

func countStockCalls(t *testing.T, q func(context.Context, string, ...any) pgxRow) int {
	t.Helper()
	var n int
	if err := q(context.Background(), `SELECT count(*) FROM outbox WHERE kind = 'stock_webhook'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitForAFreshWindow sleeps until the ten-second window is young, so a test
// that makes a few changes does not straddle two of them.
func waitForAFreshWindow() {
	for time.Now().UTC().Truncate(10 * time.Second).Add(5 * time.Second).Before(time.Now()) {
		time.Sleep(100 * time.Millisecond)
	}
}

func TestAShopWithoutAWebhookQueuesNothingAndOneWithOneQueuesOneCallPerWindow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	count := func() int {
		return countStockCalls(t, func(ctx context.Context, q string, a ...any) pgxRow { return pool.QueryRow(ctx, q, a...) })
	}
	p := buy(t, pool, workshop.StockFromPurchase)
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	if n := count(); n != 0 {
		t.Fatalf("%d calls queued by a shop that gave no address", n)
	}
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, true, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetFeedWebhook(ctx, pool, everything(owner()), "https://site.example/hook"); err != nil {
		t.Fatal(err)
	}
	// Setting the feed again keeps the address.
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, true, true, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := workshop.FeedWebhookOf(ctx, pool, everything(owner())); got != "https://site.example/hook" {
		t.Fatalf("the address after the feed was set again: %q", got)
	}
	waitForAFreshWindow()
	price := int64(100_000)
	if err := workshop.SetListing(ctx, pool, sales(), p.StockUnitID, workshop.Listing{AskingPriceMinor: &price}); err != nil {
		t.Fatal(err)
	}
	a, err := workshop.AttachStockPhoto(ctx, pool, sales(), p.StockUnitID, "cccccccccccccccccccccccccccccccc", "image/jpeg", 10, "c.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MakeCover(ctx, pool, sales(), a.ID); err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockReserved)
	if n := count(); n != 1 {
		t.Errorf("%d calls for four changes in one window, want 1", n)
	}
	var notBefore, now time.Time
	if err := pool.QueryRow(ctx, `SELECT max(not_before), clock_timestamp() FROM outbox WHERE kind = 'stock_webhook'`).Scan(&notBefore, &now); err != nil {
		t.Fatal(err)
	}
	if !notBefore.After(now) {
		t.Errorf("the call is due %s, before the window has closed (%s)", notBefore, now)
	}
	// Removed again, the address stops the calls.
	if err := workshop.SetFeedWebhook(ctx, pool, everything(owner()), ""); err != nil {
		t.Fatal(err)
	}
	before := count()
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	if count() != before {
		t.Errorf("a call queued after the address was taken away")
	}
}

func TestAWebhookAddressIsHttpsAndTheOwnersToGive(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, bad := range []string{"http://site.example/hook", "ftp://x/y", "https://", "https://user:pw@site.example/", "not a url", "https://site.example/" + strings.Repeat("a", 600)} {
		if err := workshop.SetFeedWebhook(ctx, pool, everything(owner()), bad); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%.40q accepted: %v", bad, err)
		}
	}
	for _, sc := range []access.Scope{everything(sales()), everything(advisor()), everything(technician())} {
		if err := workshop.SetFeedWebhook(ctx, pool, sc, "https://site.example/hook"); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s set the webhook: %v", sc.Role, err)
		}
		if _, err := workshop.FeedWebhookOf(ctx, pool, sc); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the webhook: %v", sc.Role, err)
		}
	}
}

// The call itself: what is posted, what is retried and what is not, and
// that an address in the server's own network is never called.
func TestTheWebhookCallSaysOnlyThatSomethingChangedAndKnowsWhatToRetry(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	var mu sync.Mutex
	var bodies []string
	var headers []http.Header
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		headers = append(headers, r.Header.Clone())
		code := status
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer srv.Close()
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, true, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetFeedWebhook(ctx, pool, everything(owner()), srv.URL+"/secret-path"); err != nil {
		t.Fatal(err)
	}
	adapter := workshop.StockWebhookAdapter{Pool: pool, Client: srv.Client()}
	item := workshop.OutboxItem{ShopID: shopID, Kind: "stock_webhook", Key: "stock@2026-11-03T12:00:00Z",
		Payload: map[string]any{"window_start": "2026-11-03T12:00:00Z"}}
	if err := adapter.Send(ctx, item); err != nil {
		t.Fatalf("a site that answers 200: %v", err)
	}
	if len(bodies) != 1 || bodies[0] != `{"event":"stock.changed","since":"2026-11-03T12:00:00Z"}` {
		t.Errorf("the call: %q", bodies)
	}
	if headers[0].Get("Idempotency-Key") != item.Key {
		t.Errorf("the call's key: %q", headers[0].Get("Idempotency-Key"))
	}
	for code, permanent := range map[int]bool{500: false, 503: false, 429: false, 408: false, 404: true, 410: true, 401: true, 301: true} {
		mu.Lock()
		status = code
		mu.Unlock()
		err := adapter.Send(ctx, item)
		if err == nil || errors.Is(err, workshop.ErrPermanent) != permanent {
			t.Errorf("a site answering %d: %v (permanent %v wanted)", code, err, permanent)
		}
		if err != nil && strings.Contains(err.Error(), "secret-path") {
			t.Errorf("the error carries the address: %v", err)
		}
	}
	// A site that is not there at all: retried, and the address not in what is said.
	gone := httptest.NewTLSServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	if err := workshop.SetFeedWebhook(ctx, pool, everything(owner()), goneURL+"/secret-path"); err != nil {
		t.Fatal(err)
	}
	err := adapter.Send(ctx, item)
	if err == nil || errors.Is(err, workshop.ErrPermanent) || strings.Contains(err.Error(), "secret-path") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("a site that is down: %v", err)
	}
	// The shop's own network is never called, by the adapter the server uses.
	if err := workshop.SetFeedWebhook(ctx, pool, everything(owner()), srv.URL+"/secret-path"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	status = http.StatusOK
	seen := len(bodies)
	mu.Unlock()
	err = workshop.NewStockWebhookAdapter(pool).Send(ctx, item)
	if !errors.Is(err, workshop.ErrPermanent) {
		t.Errorf("a loopback address: %v", err)
	}
	if len(bodies) != seen {
		t.Errorf("the server's own network was called")
	}
	// No address any more: nothing to call, and not worth trying again.
	if err := workshop.SetFeedWebhook(ctx, pool, everything(owner()), ""); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Send(ctx, item); !errors.Is(err, workshop.ErrPermanent) || len(bodies) != seen {
		t.Errorf("a call after the address was removed: %v", err)
	}
}
