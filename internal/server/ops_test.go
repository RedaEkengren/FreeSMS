package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// A monitor can ask the running service whether it is doing its job. Off until
// a token is set, refused without it, and 503 -- an alert to the simplest
// possible check -- when messages are not going or photographs cannot be saved.
func TestTheServiceTellsAMonitorWhatHealthzDoesNot(t *testing.T) {
	ts, pool := testServer(t)
	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	srv.attachmentsDir = t.TempDir()
	get := func(token string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ops/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Off.
	if status, _ := get("whatever"); status != http.StatusNotFound {
		t.Errorf("with no token configured it answered %d, want 404", status)
	}

	srv.opsToken = "a-very-long-and-random-token-for-ops"
	if status, _ := get(""); status != http.StatusUnauthorized {
		t.Errorf("without a token it answered %d", status)
	}
	if status, _ := get("not-the-token"); status != http.StatusUnauthorized {
		t.Errorf("with the wrong token it answered %d", status)
	}
	status, body := get(srv.opsToken)
	if status != http.StatusOK {
		t.Fatalf("a quiet service answered %d: %s", status, body)
	}
	var st struct {
		OK       bool     `json:"ok"`
		Problems []string `json:"problems"`
		Storage  struct {
			Writable bool `json:"writable"`
		} `json:"storage"`
		Queue struct {
			DueNotSent int `json:"due_not_sent"`
		} `json:"queue"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || !st.OK || !st.Storage.Writable || len(st.Problems) != 0 {
		t.Errorf("a quiet service says %s (%v)", body, err)
	}
	if strings.Contains(body, "@") || strings.Contains(body, "Greta") {
		t.Error("the status carries a customer's details")
	}

	// A message that should have gone an hour ago and has not.
	desk := signIn(t, ts, advisorEmail)
	job := intake(t, ts.URL, desk, url.Values{"registration": {"OPS 001"}, "complaint": {"Test"}})
	jobID := job[strings.LastIndex(job, "/")+1:]
	ctx := context.Background()
	var msg string
	if err := pool.QueryRow(ctx, `
		INSERT INTO outbound_messages (shop_id, work_order_id, about, channel, recipient, subject, body, not_before, created_by)
		SELECT $1, $2, 'ready', 'email', 'x@example.test', 's', 'b', now() - interval '1 hour', id FROM users WHERE shop_id = $1 LIMIT 1
		RETURNING id::text`, shopA, jobID).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO message_events (shop_id, message_id, event, at) VALUES ($1, $2, 'queued', now() - interval '1 hour')`, shopA, msg); err != nil {
		t.Fatal(err)
	}
	status, body = get(srv.opsToken)
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "past their time") || !strings.Contains(body, `"due_not_sent":1`) {
		t.Errorf("a message stuck for an hour answered %d: %s", status, body)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO message_events (shop_id, message_id, event, detail) VALUES ($1, $2, 'failed', 'rejected')`, shopA, msg); err != nil {
		t.Fatal(err)
	}
	if status, body = get(srv.opsToken); status != http.StatusServiceUnavailable || !strings.Contains(body, "given up on") {
		t.Errorf("a message given up on answered %d: %s", status, body)
	}

	// A disk photographs cannot be written to.
	if os.Geteuid() != 0 {
		if _, err := pool.Exec(ctx, `DELETE FROM message_events WHERE message_id = $1`, msg); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM outbound_messages WHERE id = $1`, msg); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		srv.attachmentsDir = dir
		if status, body = get(srv.opsToken); status != http.StatusServiceUnavailable || !strings.Contains(body, "photographs cannot be written") {
			t.Errorf("a read-only disk answered %d: %s", status, body)
		}
	}
	_ = time.Now
}
