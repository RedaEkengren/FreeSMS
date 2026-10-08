package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/notify"
	"github.com/jackc/pgx/v5"
)

// fakeElks is 46elks as far as a test can see it: what it was sent.
type fakeElks struct {
	mu   sync.Mutex
	sent []url.Values
}

func (f *fakeElks) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.sent = append(f.sent, r.PostForm)
		n := len(f.sent)
		f.mu.Unlock()
		io.WriteString(w, `{"id":"s`+string(rune('0'+n))+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeElks) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

func boardSays(t *testing.T, c *http.Client, ts string) string {
	t.Helper()
	_, body := page(t, c, ts+"/board")
	return body
}

// From the job page to the customer's phone and back: the first text to a
// number is confirmed, the message carries the link and how to ring, the
// board says told -- and stops saying it when 46elks reports it never came.
func TestATextGoesToTheCustomerAndItsFateIsKept(t *testing.T) {
	ts, pool := testServer(t)
	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	elks := &fakeElks{}
	srv.sms = notify.Elks{URL: elks.server(t).URL, Username: "u", Password: "p", From: "Verkstaden"}
	srv.smsSecret = "the-callback-secret"
	ctx := context.Background()
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE work_orders SET state = 'ready', ready_at = now() WHERE id = $1`, jobA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	send := url.Values{"about": {"ready"}, "channel": {"sms"}}

	if status := postStatus(t, tech, ts.URL+"/jobs/"+jobA+"/message", send); status == http.StatusSeeOther {
		t.Error("a technician texted the customer")
	}
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/message", send); status != http.StatusBadRequest {
		t.Errorf("a first text to an unconfirmed number answered %d", status)
	}
	send.Set("confirm_sms", "yes")
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/message", send); status != http.StatusSeeOther {
		t.Fatalf("sending answered %d", status)
	}
	// In the evening it waits for the morning; the test is not in a hurry.
	srv.sendDue(ctx, time.Now().Add(24*time.Hour))
	if elks.count() != 1 {
		t.Fatalf("46elks was sent %d texts, want 1", elks.count())
	}
	got := elks.sent[0]
	if got.Get("to") != "+46701234567" || !strings.Contains(got.Get("message"), "/k/") || !strings.Contains(got.Get("message"), "not read") {
		t.Errorf("46elks was sent %v", got)
	}
	if body := boardSays(t, desk, ts.URL); !strings.Contains(body, "customer told") {
		t.Error("sent, and the board does not say told")
	}

	// Once is the rule; twice is a decision.
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/message", send); status != http.StatusBadRequest {
		t.Errorf("a second text without asking answered %d", status)
	}
	srv.sendDue(ctx, time.Now().Add(24*time.Hour))
	if elks.count() != 1 {
		t.Errorf("a refused second text went anyway")
	}

	// 46elks reports it never arrived. Only with the secret.
	report := url.Values{"id": {"s1"}, "status": {"failed"}}
	if status := postStatus(t, &http.Client{}, ts.URL+"/hooks/46elks/a-guess", report); status != http.StatusNotFound {
		t.Errorf("a report without the secret answered %d", status)
	}
	if body := boardSays(t, desk, ts.URL); !strings.Contains(body, "customer told") {
		t.Error("a forged report was believed")
	}
	if status := postStatus(t, &http.Client{}, ts.URL+"/hooks/46elks/the-callback-secret", report); status != http.StatusNoContent {
		t.Errorf("46elks's report answered %d", status)
	}
	if body := boardSays(t, desk, ts.URL); !strings.Contains(body, "customer not told") {
		t.Error("never arrived, and the board still says told")
	}
	if _, body := page(t, desk, ts.URL+"/jobs/"+jobA); !strings.Contains(body, "not delivered") {
		t.Error("the job page does not say the text never arrived")
	}
}

// Without a provider nothing pretends to send.
func TestWithoutAProviderTheJobPageDoesNotOfferToSend(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE work_orders SET state = 'ready', ready_at = now() WHERE id = $1`, jobA)
		return err
	})
	desk := signIn(t, ts, advisorEmail)
	if _, body := page(t, desk, ts.URL+"/jobs/"+jobA); strings.Contains(body, `/message"`) {
		t.Error("offered to send with no provider")
	}
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/message",
		url.Values{"about": {"ready"}, "channel": {"sms"}, "confirm_sms": {"yes"}}); status != http.StatusBadRequest {
		t.Errorf("sending with no provider answered %d", status)
	}
}

// Two senders at once -- the tick, and the send straight after queueing --
// hand a message over once.
func TestTwoSendersAtOnceSendOnce(t *testing.T) {
	ts, pool := testServer(t)
	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	elks := &fakeElks{}
	srv.sms = notify.Elks{URL: elks.server(t).URL, Username: "u", Password: "p", From: "Verkstaden"}
	ctx := context.Background()
	database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE work_orders SET state = 'ready', ready_at = now() WHERE id = $1`, jobA)
		return err
	})
	desk := signIn(t, ts, advisorEmail)
	if status := postStatus(t, desk, ts.URL+"/jobs/"+jobA+"/message",
		url.Values{"about": {"ready"}, "channel": {"sms"}, "confirm_sms": {"yes"}}); status != http.StatusSeeOther {
		t.Fatalf("sending answered %d", status)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); srv.sendDue(ctx, time.Now().Add(24*time.Hour)) }()
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond) // the handler's own send, if it ran
	if n := elks.count(); n != 1 {
		t.Errorf("one message was handed over %d times", n)
	}
}
