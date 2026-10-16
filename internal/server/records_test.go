package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// The registers through the pages: the desk finds a customer and corrects
// them, a stale form is told so, a vehicle is sold and re-plated, and the
// technician is kept out of the customer register altogether.
func TestTheCustomerAndVehicleRegistersThroughThePages(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"REG 001"}, "complaint": {"Squeal"}})

	if status := postStatus(t, desk, job+"/customer", url.Values{"kind": {"private"}, "name": {"Greta Gren"}, "phone": {"070-123 45 67"}, "also_owner": {"yes"}}); status != http.StatusSeeOther {
		t.Fatalf("creating the customer answered %d", status)
	}
	if status, _ := page(t, tech, base+"/customers"); status != http.StatusForbidden {
		t.Errorf("a technician opened the customer register: %d", status)
	}
	status, list := page(t, desk, base+"/customers?q=1234567")
	if status != http.StatusOK || !strings.Contains(list, "Greta Gren") {
		t.Fatalf("searching by the end of the number answered %d: %s", status, list)
	}
	id := regexp.MustCompile(`/customers/([0-9a-f-]{36})`).FindStringSubmatch(list)
	if id == nil {
		t.Fatal("no customer link in the register")
	}
	card := base + "/customers/" + id[1]
	_, body := page(t, desk, card)
	if !strings.Contains(body, "REG 001") || !regexp.MustCompile(`#\d+ &middot;`).MatchString(body) {
		t.Errorf("the card does not link the car and the job: %s", body)
	}

	edit := url.Values{"revision": {"1"}, "name": {"Greta Gren"}, "phone": {"070-765 43 21"}, "reason": {"New number"}}
	if status := postStatus(t, desk, card+"/edit", edit); status != http.StatusSeeOther {
		t.Fatalf("editing answered %d", status)
	}
	if _, body = page(t, desk, card); !strings.Contains(body, "070-765 43 21") || !strings.Contains(body, "070-123 45 67") || !strings.Contains(body, "New number") {
		t.Errorf("the card does not show the new number and the change: %s", body)
	}
	edit.Set("phone", "070-000 00 00")
	if status, p := postBody(t, desk, card+"/edit", edit); status != http.StatusConflict || !strings.Contains(p, "Somebody else got there first") {
		t.Errorf("a stale form answered %d", status)
	}
	if status := postStatus(t, tech, card+"/edit", edit); status != http.StatusForbidden {
		t.Errorf("a technician edited a customer: %d", status)
	}

	// The car: the technician sees no forms, the desk can re-plate and sell.
	vehicle := regexp.MustCompile(`/vehicles/([0-9a-f-]{36})`).FindStringSubmatch(body)
	if vehicle == nil {
		t.Fatal("no vehicle link on the card")
	}
	v := base + "/vehicles/" + vehicle[1]
	if _, mine := page(t, tech, v); strings.Contains(mine, "/registration") || strings.Contains(mine, "Greta") {
		t.Error("the technician is offered record corrections or the owner's name")
	}
	if status := postStatus(t, tech, v+"/registration", url.Values{"mode": {"correct"}, "registration": {"ZZZ 111"}}); status != http.StatusForbidden {
		t.Errorf("a technician re-plated a car: %d", status)
	}
	if status := postStatus(t, desk, v+"/registration", url.Values{"mode": {"correct"}, "registration": {"REG 002"}}); status != http.StatusSeeOther {
		t.Fatalf("correcting the plate answered %d", status)
	}
	if _, p := page(t, desk, v); !strings.Contains(p, "REG 002") || !strings.Contains(p, "registration (corrected)") {
		t.Errorf("the corrected plate and the log are not on the vehicle page")
	}
	if status, form := page(t, desk, v+"/sell?q=Greta"); status != http.StatusOK || !strings.Contains(form, "Greta Gren") {
		t.Fatalf("the sale form answered %d", status)
	}
	if status := postStatus(t, desk, v+"/sell", url.Values{"from": {""}, "reason": {"Sold"}}); status != http.StatusSeeOther {
		t.Fatalf("ending the ownership answered %d", status)
	}
	if _, p := page(t, desk, v); strings.Contains(p, "<dt>Owner</dt>") {
		t.Error("the vehicle still has an owner after the sale")
	}
}

// A second copy of somebody is put in front of the desk before it is made, and
// two records that are one are joined after a page that says what moves.
func TestDuplicatesAreAskedAboutAndMergedThroughThePages(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	first := base + intake(t, base, desk, url.Values{"registration": {"DUP 001"}, "complaint": {"Squeal"}})
	second := base + intake(t, base, desk, url.Values{"registration": {"DUP 002"}, "complaint": {"Squeal"}})

	if status := postStatus(t, desk, first+"/customer", url.Values{"kind": {"private"}, "name": {"Greta Gren"}, "phone": {"070-123 45 67"}}); status != http.StatusSeeOther {
		t.Fatalf("the first customer answered %d", status)
	}
	// The same number again: asked, not made.
	form := url.Values{"kind": {"private"}, "name": {"G. Gren"}, "phone": {"0701234567"}}
	status, ask := postBody(t, desk, second+"/customer", form)
	if status != http.StatusOK || !strings.Contains(ask, "Is this somebody we already have?") || !strings.Contains(ask, "Greta Gren") || !strings.Contains(ask, "the same telephone number") {
		t.Fatalf("the duplicate warning answered %d: %s", status, ask)
	}
	if _, body := page(t, desk, base+"/customers?q=0701234567"); strings.Contains(body, "G. Gren") {
		t.Error("asking about a duplicate made the customer")
	}
	// Same name and nothing else is not a duplicate.
	if status := postStatus(t, desk, second+"/customer", url.Values{"kind": {"private"}, "name": {"Greta Gren"}, "phone": {"070-999 00 11"}}); status != http.StatusSeeOther {
		t.Fatalf("a different Greta answered %d", status)
	}

	// Going on after looking makes the second record.
	third := base + intake(t, base, desk, url.Values{"registration": {"DUP 003"}, "complaint": {"Squeal"}})
	form.Set("not_duplicate", "1")
	if status := postStatus(t, desk, third+"/customer", form); status != http.StatusSeeOther {
		t.Fatalf("a confirmed create answered %d", status)
	}
	_, list := page(t, desk, base+"/customers?q=Gren")
	id := regexp.MustCompile(`/customers/([0-9a-f-]{36})`).FindStringSubmatch(list)
	if id == nil {
		t.Fatal("the confirmed customer is not in the register")
	}
	_, card := page(t, desk, base+"/customers/"+id[1])
	other := regexp.MustCompile(`/customers/([0-9a-f-]{36})/merge`).FindStringSubmatch(card)
	if other == nil {
		t.Fatal("the card offers no merge")
	}
	m := base + "/customers/" + id[1] + "/merge"
	_, picker := page(t, desk, m)
	with := regexp.MustCompile(`merge\?with=([0-9a-f-]{36})`).FindStringSubmatch(picker)
	if with == nil || !strings.Contains(picker, "the same telephone number") {
		t.Fatalf("the merge page lists no likely duplicate: %s", picker)
	}
	_, review := page(t, desk, m+"?with="+with[1])
	if !strings.Contains(review, "moves to the kept customer") || !strings.Contains(review, "Yes, they are the same: merge") {
		t.Fatalf("the review does not say what moves: %s", review)
	}
	if status, _ := page(t, tech, m+"?with="+with[1]); status != http.StatusForbidden {
		t.Errorf("a technician reached the merge review: %d", status)
	}
	rev := regexp.MustCompile(`name="keep_revision" value="(\d+)"`).FindStringSubmatch(review)
	drev := regexp.MustCompile(`name="drop_revision" value="(\d+)"`).FindStringSubmatch(review)
	post := url.Values{"drop": {with[1]}, "keep_revision": {rev[1]}, "drop_revision": {"99"}}
	if status := postStatus(t, desk, m, post); status != http.StatusConflict {
		t.Errorf("a stale merge answered %d, want 409", status)
	}
	post.Set("drop_revision", drev[1])
	if status := postStatus(t, desk, m, post); status != http.StatusSeeOther {
		t.Fatalf("merging answered %d", status)
	}
	if _, body := page(t, desk, base+"/customers/"+with[1]); !strings.Contains(body, "merged") {
		t.Error("the merged-away record does not say so")
	}
	if status := postStatus(t, tech, m, post); status != http.StatusForbidden {
		t.Errorf("a technician merged: %d", status)
	}
}
