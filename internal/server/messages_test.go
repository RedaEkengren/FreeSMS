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

// A booking message through the pages: previewed, confirmed, queued with the
// time as the shop's clock shows it, a reminder the day before at the same
// hour on the wall across the night the clocks change, and never sent for a
// time the booking no longer has.
func TestABookingIsConfirmedPreviewedAndNotRemindedAboutAMovedTime(t *testing.T) {
	ts, pool := testServer(t)
	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	elks := &fakeElks{}
	srv.sms = notify.Elks{URL: elks.server(t).URL, Username: "u", Password: "p", From: "Verkstaden"}
	ctx := context.Background()
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL

	// A Sunday booking at 10:00 on the day the clocks go forward.
	loc := srv.zone()
	starts := time.Date(time.Now().Year()+1, 3, 31, 10, 0, 0, 0, loc)
	for starts.Weekday() != time.Sunday || starts.Month() != 3 {
		starts = starts.AddDate(0, 0, 1)
	}
	var id string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO bookings (shop_id, starts_at, ends_at, what, customer_name, customer_phone, created_by)
			SELECT $1, $2, $3, 'Service', 'Greta Gren', '070-123 45 67', u.id FROM users u WHERE u.role = 'service_advisor' LIMIT 1 RETURNING id::text`,
			shopA, starts, starts.Add(time.Hour)).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"kind": {"reminder"}, "channel": {"sms"}, "address": {"070-123 45 67"}, "recipient": {"Greta Gren"}, "arrival": {"Park in the yard."}}
	if status := postStatus(t, tech, base+"/bookings/"+id+"/message", form); status != http.StatusForbidden {
		t.Errorf("a technician messaged a booking's customer: %d", status)
	}
	status, preview := postBody(t, desk, base+"/bookings/"+id+"/message", form)
	if status != http.StatusOK || !strings.Contains(preview, "This is what will be sent:") || !strings.Contains(preview, starts.Format("2006-01-02 15:04")) ||
		!strings.Contains(preview, "Park in the yard.") || !strings.Contains(preview, "Queue it") {
		t.Fatalf("the preview answered %d: %s", status, preview)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM outbound_messages WHERE booking_id = $1`, id).Scan(&n)
	if n != 0 {
		t.Fatal("a preview queued a message")
	}
	form.Set("send", "1")
	if status := postStatus(t, desk, base+"/bookings/"+id+"/message", form); status != http.StatusBadRequest {
		t.Errorf("queueing without confirming answered %d", status)
	}
	form.Set("confirm", "yes")
	if status := postStatus(t, desk, base+"/bookings/"+id+"/message", form); status != http.StatusSeeOther {
		t.Fatalf("queueing a reminder answered %d", status)
	}
	var notBefore time.Time
	if err := pool.QueryRow(ctx, `SELECT not_before FROM outbound_messages WHERE booking_id = $1`, id).Scan(&notBefore); err != nil {
		t.Fatal(err)
	}
	// The day before at the same hour on the wall, not twenty-four hours back.
	want := time.Date(starts.Year(), starts.Month(), starts.Day()-1, 10, 0, 0, 0, loc)
	if !notBefore.Equal(want) {
		t.Errorf("the reminder is set for %v, want %v", notBefore.In(loc), want)
	}

	// Moved before it goes: it is not sent.
	if status := postStatus(t, desk, base+"/bookings/"+id, url.Values{"action": {"move"}, "date": {starts.AddDate(0, 0, 7).Format("2006-01-02")},
		"time": {"10:00"}, "minutes": {"60"}, "what": {"Service"}}); status != http.StatusSeeOther {
		t.Fatalf("moving answered %d", status)
	}
	srv.sendDue(ctx, starts.Add(-time.Hour))
	if elks.count() != 0 {
		t.Errorf("a reminder for a moved booking reached the provider: %d texts", elks.count())
	}
	_, page2 := page(t, desk, base+"/bookings/"+id)
	if !strings.Contains(page2, "cancelled") || !strings.Contains(page2, "moved before this went") && !strings.Contains(page2, "booking was moved") {
		t.Errorf("the booking page does not say what became of the reminder")
	}
	// A confirmation for the new time goes.
	form2 := url.Values{"kind": {"confirm"}, "channel": {"sms"}, "address": {"070-123 45 67"}, "recipient": {"Greta Gren"}, "send": {"1"}, "confirm": {"yes"}}
	if status := postStatus(t, desk, base+"/bookings/"+id+"/message", form2); status != http.StatusSeeOther {
		t.Fatalf("confirming the new time answered %d", status)
	}
	srv.sendDue(ctx, time.Now().Add(24*time.Hour))
	if elks.count() != 1 || !strings.Contains(elks.sent[0].Get("message"), starts.AddDate(0, 0, 7).Format("2006-01-02")+" 10:00") {
		t.Fatalf("what was sent: %v", elks.sent)
	}
	// And by hand when nothing could be sent.
	if status := postStatus(t, desk, base+"/bookings/"+id+"/contact", url.Values{"by_hand": {"1"}, "how": {"Rang and she confirmed"}}); status != http.StatusSeeOther {
		t.Errorf("writing it down answered %d", status)
	}
	if _, body := page(t, desk, base+"/bookings/"+id); !strings.Contains(body, "Rang and she confirmed") {
		t.Error("the hand confirmation is not on the booking")
	}
}
