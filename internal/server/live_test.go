package server

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
)

// stream opens /events as a client and hands back its lines as they come.
func stream(t *testing.T, ts string, client *http.Client) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts+"/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("/events answered %d, %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	expect(t, lines, "retry: 5000")
	return lines
}

// expect reads lines until one is want, failing on a line containing any of
// never, or after a few seconds.
func expect(t *testing.T, lines <-chan string, want string, never ...string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, open := <-lines:
			if !open {
				t.Fatalf("the stream ended waiting for %q", want)
			}
			for _, n := range never {
				if strings.Contains(l, n) {
					t.Fatalf("the stream said %q", l)
				}
			}
			if l == want {
				return
			}
		case <-deadline:
			t.Fatalf("nothing said %q", want)
		}
	}
}

// The technician's screen hears that the front desk changed the job, and
// what it hears is only that: which job. Another workshop's change in the
// same database never reaches it.
func TestAChangeReachesTheOpenScreensOfItsShopOnly(t *testing.T) {
	ts, pool := testServer(t)
	tech, desk := signIn(t, ts, techEmail), signIn(t, ts, advisorEmail)
	lines := stream(t, ts.URL, tech)

	// Shop B, first: if it were going to arrive, it would arrive before
	// shop A's change below.
	if err := database.InShop(context.Background(), pool, shopB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO findings (shop_id, work_order_id, note, reported_by)
			VALUES ($1, $2, 'Shop B only', 'bbbbbbbb-0000-0000-0000-0000000000bb')`, shopB, jobB)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	post(t, desk, ts.URL+"/jobs/"+jobA+"/lines", url.Values{
		"description": {"Bromsvätska"}, "quantity": {"1"}, "unit_price": {"100"}, "kind": {"labour"}, "cost_bearer": {"customer"}})

	expect(t, lines, "data: job:"+jobA, jobB, "Bromsvätska")
	expect(t, lines, "event: change", jobB, "Bromsvätska")
}

// A stream does not outlive its session: signed out elsewhere, it says so
// and ends, within a heartbeat.
func TestAStreamEndsWithItsSession(t *testing.T) {
	ts, _ := testServer(t)
	tech := signIn(t, ts, techEmail)
	lines := stream(t, ts.URL, tech)
	post(t, tech, ts.URL+"/logout", nil)
	expect(t, lines, "event: signedout")
	select {
	case _, open := <-lines:
		for open {
			_, open = <-lines
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream carried on after signing out")
	}
}

// A board left open refreshes itself all day. If that counted as somebody
// using it, its session would never reach the idle limit, and the shared
// counter machine would stay signed in as whoever used it last.
func TestARefreshDoesNotKeepASessionAlive(t *testing.T) {
	ts, pool := testServer(t)
	desk := signIn(t, ts, advisorEmail)
	ctx := context.Background()
	lastSeen := func() time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT max(last_seen_at) FROM sessions`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	before := lastSeen()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/board", nil)
	req.Header.Set(LiveHeader, "1")
	resp, err := desk.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a refresh answered %d", resp.StatusCode)
	}
	lines := stream(t, ts.URL, desk)
	expect(t, lines, ": ping")
	if got := lastSeen(); !got.Equal(before) {
		t.Errorf("a refresh and the stream moved last seen from %v to %v", before, got)
	}

	resp, err = desk.Get(ts.URL + "/board")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := lastSeen(); !got.After(before) {
		t.Error("a person opening the board did not count as activity")
	}
}

// A stock count writes a row per part. The screens hear it once: Postgres
// delivers one notification per transaction for the same payload.
func TestManyChangesAtOnceAreOneRefresh(t *testing.T) {
	ts, pool := testServer(t)
	lines := stream(t, ts.URL, signIn(t, ts, advisorEmail))
	if err := database.InShop(context.Background(), pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO parts (shop_id, number, name, unit, price_minor)
			SELECT $1, 'LIVE-' || n, 'Del ' || n, 'each', 100 FROM generate_series(1, 50) n`, shopA); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO stock_movements (shop_id, part_id, kind, quantity)
			SELECT $1, id, 'counted', 3 FROM parts WHERE number LIKE 'LIVE-%'`, shopA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	parts := 0
	quiet := time.After(time.Second)
	for {
		select {
		case l := <-lines:
			if l == "data: parts" {
				parts++
			}
			continue
		case <-quiet:
		}
		break
	}
	if parts != 1 {
		t.Errorf("a hundred rows in one transaction were %d refreshes, want 1", parts)
	}
}
