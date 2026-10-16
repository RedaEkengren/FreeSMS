package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// The timeline through the pages: one list, notes written from a phone,
// a correction that keeps the old wording, and nothing of the counter's
// reaching the technician.
func TestTheTimelineAndNotesThroughThePages(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"TML 001"}, "complaint": {"Squeal"}})

	if status := postStatus(t, tech, job+"/notes", url.Values{"kind": {"note"}, "body": {"Seized bolt on the left disc"}}); status != http.StatusSeeOther {
		t.Fatalf("a technician's note answered %d", status)
	}
	if status := postStatus(t, desk, job+"/notes", url.Values{"kind": {"conversation"}, "channel": {"phone"}, "body": {"Rang about Friday"}}); status != http.StatusSeeOther {
		t.Fatalf("recording a conversation answered %d", status)
	}
	if status := postStatus(t, tech, job+"/notes", url.Values{"kind": {"conversation"}, "channel": {"phone"}, "body": {"x"}}); status != http.StatusForbidden {
		t.Errorf("a technician recorded a conversation: %d", status)
	}
	if status := postStatus(t, desk, job+"/notes", url.Values{"kind": {"note"}, "body": {""}}); status != http.StatusBadRequest {
		t.Errorf("an empty note answered %d", status)
	}

	_, theirs := page(t, desk, job+"/timeline")
	if !strings.Contains(theirs, "Seized bolt") || !strings.Contains(theirs, "Rang about Friday") || !strings.Contains(theirs, "Where it stands") {
		t.Fatalf("the counter's timeline is missing something: %s", theirs)
	}
	_, mine := page(t, tech, job+"/timeline")
	if !strings.Contains(mine, "Seized bolt") || strings.Contains(mine, "Rang about Friday") || strings.Contains(mine, "Record a conversation") {
		t.Errorf("the technician's timeline shows the counter's side")
	}
	if !strings.Contains(mine, "The job was opened") {
		t.Error("the technician's timeline does not begin with the job being opened")
	}

	noteID := regexp.MustCompile(`/notes/([0-9a-f-]{36})/correct`).FindStringSubmatch(mine)
	if noteID == nil {
		t.Fatal("the technician cannot correct their own note")
	}
	if status := postStatus(t, tech, job+"/notes/"+noteID[1]+"/correct", url.Values{"body": {"Seized bolt on the RIGHT disc"}}); status != http.StatusSeeOther {
		t.Fatalf("correcting answered %d", status)
	}
	// A second correction of the same version is told somebody got there first.
	if status := postStatus(t, desk, job+"/notes/"+noteID[1]+"/correct", url.Values{"body": {"Other"}}); status != http.StatusConflict {
		t.Errorf("a second correction of one version answered %d, want 409", status)
	}
	if _, after := page(t, tech, job+"/timeline"); !strings.Contains(after, "RIGHT disc") || !strings.Contains(after, "<s>Seized bolt on the left disc</s>") {
		t.Errorf("the corrected note does not show both wordings")
	}
	// The job page points at it.
	if _, body := page(t, tech, job); !strings.Contains(body, "/timeline") {
		t.Error("the job page does not link to the timeline")
	}
}
