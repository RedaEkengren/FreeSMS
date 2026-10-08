package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	techUser    = "aaaaaaaa-0000-0000-0000-000000000002"
	advisorUser = "aaaaaaaa-0000-0000-0000-00000000000b"
)

func page(t *testing.T, c *http.Client, url string, header ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func postStatus(t *testing.T, c *http.Client, target string, form url.Values) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.URL.Host)
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Who is in when is for everybody; why somebody is away is for them and for
// whoever plans staff, and the counter can be given that -- not take it.
func TestTheRotaShowsEachPersonWhatTheyMayKnow(t *testing.T) {
	ts, _ := testServer(t)
	owner, desk, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	day := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	sick := url.Values{"action": {"away"}, "user_id": {techUser}, "date": {day}, "reason": {"sick"}}

	if status := postStatus(t, desk, ts.URL+"/rota", sick); status != http.StatusForbidden {
		t.Errorf("the front desk recording an absence answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/rota", sick); status != http.StatusSeeOther {
		t.Fatalf("the owner recording an absence answered %d", status)
	}
	week := "/rota?week=" + day
	if _, body := page(t, owner, ts.URL+week); !strings.Contains(body, "off sick") || !strings.Contains(body, `value="rota"`) {
		t.Error("whoever plans staff does not see why, or the forms")
	}
	if _, body := page(t, tech, ts.URL+week); !strings.Contains(body, "off sick") {
		t.Error("the person does not see their own reason")
	}
	_, body := page(t, desk, ts.URL+week)
	if strings.Contains(body, "off sick") || !strings.Contains(body, "away") {
		t.Error("a colleague reads why somebody is away, or not that they are")
	}
	if strings.Contains(body, `value="rota"`) {
		t.Error("a colleague is offered the planner's forms")
	}

	// Given the planning of people, the counter may.
	if status := postStatus(t, owner, ts.URL+"/staff/"+advisorUser+"/planner", url.Values{"on": {"yes"}}); status != http.StatusSeeOther {
		t.Fatalf("giving the planning of people answered %d", status)
	}
	if _, body := page(t, desk, ts.URL+week); !strings.Contains(body, "off sick") {
		t.Error("given the planning of people, the counter still cannot see why")
	}
	if status := postStatus(t, desk, ts.URL+"/rota", url.Values{"action": {"shift"}, "user_id": {techUser}, "date": {day},
		"starts": {"10:00"}, "ends": {"18:00"}}); status != http.StatusSeeOther {
		t.Errorf("a planner changing a day answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+"/staff/"+advisorUser+"/planner", url.Values{"on": {"no"}}); status == http.StatusSeeOther {
		t.Error("a technician took the planning of people away")
	}
}

// Looking at one's own schedule deals with "your schedule has changed"; the
// page refreshing itself does not -- nobody read it.
func TestOnlyAPersonReadingTheirScheduleCountsAsSeen(t *testing.T) {
	ts, pool := testServer(t)
	owner, tech := signIn(t, ts, ownerEmail), signIn(t, ts, techEmail)
	day := time.Now().AddDate(0, 0, 2).Format("2006-01-02")
	if status := postStatus(t, owner, ts.URL+"/rota", url.Values{"action": {"shift"}, "user_id": {techUser}, "date": {day},
		"starts": {"10:00"}, "ends": {"18:00"}}); status != http.StatusSeeOther {
		t.Fatalf("changing a day answered %d", status)
	}
	seen := func() bool {
		var at *time.Time
		if err := pool.QueryRow(context.Background(), `SELECT schedule_seen_at FROM users WHERE id = $1`, techUser).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at != nil
	}
	if _, body := page(t, tech, ts.URL+"/mine"); !strings.Contains(body, "Your schedule has changed") {
		t.Fatal("the technician is not told their schedule changed")
	}
	page(t, tech, ts.URL+"/schedule", LiveHeader, "1")
	if seen() {
		t.Error("a refresh counted as reading the schedule")
	}
	if _, body := page(t, tech, ts.URL+"/schedule"); !strings.Contains(body, "10:00") {
		t.Error("the changed day is not on the person's schedule")
	}
	if _, body := page(t, tech, ts.URL+"/mine"); strings.Contains(body, "Your schedule has changed") {
		t.Error("read, and still told")
	}
}
