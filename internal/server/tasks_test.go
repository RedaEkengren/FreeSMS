package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// The work list through the pages: the technician keeps it on a phone with no
// price in sight, the counter sees what holds the car up, and "ready" is not
// offered while work is left.
func TestTheWorkListThroughThePages(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"WRK 001"}, "complaint": {"Squeal"}})

	if status := postStatus(t, tech, job+"/tasks", url.Values{"title": {"Road test"}}); status != http.StatusSeeOther {
		t.Fatalf("a technician adding a step answered %d", status)
	}
	if status := postStatus(t, desk, job+"/tasks", url.Values{"title": {""}}); status != http.StatusBadRequest {
		t.Errorf("an empty step answered %d", status)
	}
	_, mine := page(t, tech, job)
	if !strings.Contains(mine, "Road test") || !strings.Contains(mine, "1 of 1 left") {
		t.Fatalf("the technician's page does not show the list: %s", mine)
	}
	if strings.Contains(mine, "/tasks/from-lines") || strings.Contains(mine, `name="line"`) || strings.Contains(mine, `value="omitted"`) {
		t.Error("the technician is offered the counter's controls")
	}
	_, theirs := page(t, desk, job)
	if !strings.Contains(theirs, `value="omitted"`) {
		t.Error("the desk cannot leave a step out")
	}

	id := regexp.MustCompile(`/tasks/([0-9a-f-]{36})/move`).FindStringSubmatch(mine)
	if id == nil {
		t.Fatal("no step to move on the page")
	}
	move := job + "/tasks/" + id[1] + "/move"
	if status := postStatus(t, tech, move, url.Values{"to": {"blocked"}, "blocker": {"part"}}); status != http.StatusSeeOther {
		t.Fatalf("blocking answered %d", status)
	}
	if _, body := page(t, desk, job); !strings.Contains(body, "waiting for a part") {
		t.Error("the counter is not told what holds it up")
	}
	// A technician cannot leave a step out, whatever the form says.
	if status := postStatus(t, tech, move, url.Values{"to": {"omitted"}, "note": {"x"}}); status != http.StatusForbidden {
		t.Errorf("a technician left a step out: %d", status)
	}
	if status := postStatus(t, tech, move, url.Values{"to": {"finished"}}); status != http.StatusBadRequest {
		t.Errorf("a made-up status answered %d", status)
	}
	if status := postStatus(t, tech, move, url.Values{"to": {"done"}}); status != http.StatusSeeOther {
		t.Fatalf("finishing answered %d", status)
	}
	if _, body := page(t, desk, job); !strings.Contains(body, "All done") {
		t.Error("the list does not say everything is done")
	}
}
