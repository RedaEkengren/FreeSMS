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
)

// On a UTC server serving a Stockholm workshop, the correction form shows the
// shop's wall clock, and sending it back unchanged moves nothing.
//
// It showed the server's wall clock and read the answer as the shop's, so
// saving a correction without touching the times moved them by an hour in
// winter and two in summer.
func TestACorrectionSentBackUnchangedMovesNothing(t *testing.T) {
	// The container runs in UTC; a developer's machine often does not, and
	// would hide this. Not parallel, and put back after.
	was := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = was })

	ts, pool := testServer(t)
	ctx := context.Background()
	const entry = "aaaaaaaa-0000-0000-0000-0000000000e1"
	started := time.Date(2026, 7, 1, 5, 30, 0, 0, time.UTC) // 07:30 in Stockholm
	ended := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)   // 16:00
	if _, err := pool.Exec(ctx, `
		INSERT INTO time_entries (id, shop_id, work_order_id, user_id, started_at, ended_at, flagged)
		VALUES ($1, $2, $3, 'aaaaaaaa-0000-0000-0000-000000000002', $4, $5, true)`,
		entry, shopA, jobA, started, ended); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	advisor := signIn(t, ts, advisorEmail)
	resp, err := advisor.Get(ts.URL + "/time")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	field := func(name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindSubmatch(page)
		if m == nil {
			t.Fatalf("no %s on the page", name)
		}
		return string(m[1])
	}
	if got := field("started_at"); got != "2026-07-01T07:30" {
		t.Errorf("the form shows the start as %s, want the shop's 07:30", got)
	}

	form := url.Values{
		"entry_id":    {entry},
		"started_at":  {field("started_at")},
		"ended_at":    {field("ended_at")},
		"started_was": {field("started_was")},
		"ended_was":   {field("ended_was")},
		"note":        {"Checked, it was right"},
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/time/correct", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err = advisor.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		m := regexp.MustCompile(`(?s)<main.*?</main>`).Find(body)
		t.Fatalf("correcting answered %d: %s", resp.StatusCode, regexp.MustCompile(`\s+`).ReplaceAll(m, []byte(" ")))
	}

	var gotStart, gotEnd time.Time
	if err := pool.QueryRow(ctx, `SELECT started_at, ended_at FROM time_entries WHERE id = $1`, entry).
		Scan(&gotStart, &gotEnd); err != nil {
		t.Fatal(err)
	}
	if !gotStart.Equal(started) || !gotEnd.Equal(ended) {
		t.Errorf("unchanged fields moved the entry to %s–%s, want %s–%s",
			gotStart.UTC(), gotEnd.UTC(), started, ended)
	}
}
