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
