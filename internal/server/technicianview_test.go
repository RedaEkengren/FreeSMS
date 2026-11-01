package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// What a technician sees (#225), read off the pages: nothing of the six
// fields by default, a field the owner opens for every technician, and
// one person's exception on a job of their own, with the sentence saying
// what the customer's details may be used for. The counter's page of the
// same job is unchanged, and the settings are the owner's.
func TestATechniciansJobPageShowsOnlyWhatTheShopOpened(t *testing.T) {
	ts, pool := testServer(t)
	tech, desk, owner := signIn(t, ts, techEmail), signIn(t, ts, advisorEmail), signIn(t, ts, ownerEmail)
	post(t, desk, ts.URL+"/jobs/"+jobA+"/lines", url.Values{
		"kind": {"labour"}, "description": {"Replace front pads"},
		"quantity": {"2.5"}, "unit_price": {"895.00"}, "vat_rate": {"25"},
	})
	job := ts.URL + "/jobs/" + jobA
	price := "895"
	sentence := "may be used only for it"
	if page := visibleText(get(t, tech, job, http.StatusOK)); strings.Contains(page, price) || strings.Contains(page, "Total") ||
		strings.Contains(page, customerName) || strings.Contains(page, customerPhoneDigits) {
		t.Errorf("a technician's job page by default shows a price or the customer:\n%s", page)
	}
	if !strings.Contains(visibleText(get(t, tech, job, http.StatusOK)), "Ask the front desk to contact the customer") {
		t.Error("a technician cannot ask the desk to contact the customer")
	}
	if page := visibleText(get(t, desk, job, http.StatusOK)); !strings.Contains(page, price) || !strings.Contains(page, customerName) {
		t.Errorf("the counter's job page lost the price or the customer:\n%s", page)
	}
	for _, c := range []*http.Client{tech, desk} {
		if status := postStatus(t, c, ts.URL+"/technicians/fields", url.Values{"field": {"line_prices"}, "shown": {"yes"}, "reason": {"x"}}); status < 400 {
			t.Errorf("somebody who does not run the shop changed the profile: %d", status)
		}
	}

	if status := postStatus(t, owner, ts.URL+"/technicians/fields", url.Values{"field": {"line_prices"}, "shown": {"yes"}, "reason": {"Ackord"}}); status != http.StatusSeeOther {
		t.Fatalf("opening the prices answered %d", status)
	}
	if page := visibleText(get(t, tech, job, http.StatusOK)); !strings.Contains(page, price) || strings.Contains(page, customerName) {
		t.Errorf("the prices opened: the price and not the customer, got:\n%s", page)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE work_orders SET assigned_to = 'aaaaaaaa-0000-0000-0000-000000000002' WHERE id = $1`, jobA); err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, owner, ts.URL+"/technicians/exceptions", url.Values{"user_id": {"aaaaaaaa-0000-0000-0000-000000000002"},
		"field": {"customer_phone"}, "reason": {"Ringer själv om delar"}}); status != http.StatusSeeOther {
		t.Fatalf("an exception answered %d", status)
	}
	page := visibleText(get(t, tech, job, http.StatusOK))
	if !strings.Contains(page, customerPhoneDigits) || strings.Contains(page, customerName) || !strings.Contains(page, sentence) {
		t.Errorf("the number opened for them: the number, the sentence, not the name; got:\n%s", page)
	}
	if settings := visibleText(get(t, owner, ts.URL+"/technicians", http.StatusOK)); !strings.Contains(settings, "Ringer själv om delar") || !strings.Contains(settings, "Ackord") {
		t.Errorf("the settings page does not say who opened what and why:\n%s", settings)
	}
}
