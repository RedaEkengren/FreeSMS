package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Work sent to a supplier, through the pages: the counter sends it out and
// records what happens; the technician can see where it is and nothing about
// money; a job is not ready while it is out.
func TestWorkAtASupplierThroughThePages(t *testing.T) {
	ts, pool := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"SUP 001"}, "complaint": {"Turbo"}})
	jobID := job[strings.LastIndex(job, "/")+1:]

	if status := postStatus(t, tech, job+"/outsourced", url.Values{"action": {"request"}, "supplier": {"X"}, "scope": {"x"}, "item": {"x"}, "item_kind": {"component"}}); status != http.StatusForbidden {
		t.Errorf("a technician sent work out: %d", status)
	}
	if status, _ := page(t, tech, base+"/outsourced"); status != http.StatusForbidden {
		t.Errorf("a technician opened the counter's list: %d", status)
	}
	if status := postStatus(t, desk, job+"/outsourced", url.Values{"action": {"request"}, "supplier": {"Turbo-Verkstan"}, "scope": {"Overhaul"}, "item": {"Turbocharger"}, "item_kind": {"component"}, "expected_back": {"nonsense"}}); status != http.StatusBadRequest {
		t.Errorf("a bad date answered %d", status)
	}
	if status := postStatus(t, desk, job+"/outsourced", url.Values{"action": {"request"}, "supplier": {"Turbo-Verkstan"}, "contact": {"Lars"}, "scope": {"Overhaul and balance"}, "item": {"Turbocharger"}, "item_kind": {"component"}, "estimate": {"4500"}, "expected_back": {"2020-03-01"}}); status != http.StatusSeeOther {
		t.Fatalf("sending work out answered %d", status)
	}
	var item string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM outsourced_work`).Scan(&item); err != nil {
		t.Fatal(err)
	}
	record := func(v url.Values) int {
		v.Set("action", "record")
		v.Set("item", item)
		return postStatus(t, desk, job+"/outsourced", v)
	}
	if status := record(url.Values{"kind": {"returned"}, "outcome": {"repaired"}, "note": {"x"}}); status != http.StatusBadRequest {
		t.Errorf("returned before it was sent answered %d", status)
	}
	if status := record(url.Values{"kind": {"sent"}}); status != http.StatusSeeOther {
		t.Fatalf("sent answered %d", status)
	}
	if status := record(url.Values{"kind": {"cost"}, "amount": {"4700"}}); status != http.StatusSeeOther {
		t.Fatalf("cost answered %d", status)
	}

	_, body := page(t, desk, job+"/outsourced")
	for _, want := range []string{"Turbo-Verkstan", "Turbocharger", "at the supplier", "4,500.00", "4,700.00", "Send it out"} {
		if !strings.Contains(strings.ReplaceAll(body, " ", " "), want) {
			t.Errorf("the counter's page does not say %q: %s", want, body)
		}
	}
	_, list := page(t, desk, base+"/outsourced")
	if !strings.Contains(list, "Turbo-Verkstan") || !strings.Contains(list, "overdue") {
		t.Errorf("the counter's list does not show the overdue item: %s", list)
	}

	_, tb := page(t, tech, job+"/outsourced")
	if !strings.Contains(tb, "Turbocharger") || !strings.Contains(tb, "at the supplier") {
		t.Error("the technician cannot see where the part is")
	}
	for _, leak := range []string{"4,500", "4,700", "4500", "4700", "Send it out", "cost recorded"} {
		if strings.Contains(strings.ReplaceAll(tb, " ", " "), leak) {
			t.Errorf("the technician's page carries %q", leak)
		}
	}

	// A job with work at the supplier is not ready; set it up to be, and see.
	if _, err := pool.Exec(context.Background(), `UPDATE work_orders SET state = 'in_progress' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, desk, job+"/state", url.Values{"to": {"ready"}}); status != http.StatusConflict && status != http.StatusBadRequest {
		t.Errorf("ready with the work at the supplier answered %d", status)
	}
}

// plain turns the non-breaking spaces a formatted amount carries into ordinary
// ones, so a test can look for "4 500".
func plain(s string) string {
	return strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(s)
}
