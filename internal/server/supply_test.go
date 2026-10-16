package server

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Orders and deliveries through the pages: a request is ordered, the day
// moves, it arrives in two parts, a wrong one goes back, and the technician
// sees when it is expected without seeing who from or what it costs.
func TestOrdersAndDeliveriesThroughThePages(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"SUP 001"}, "complaint": {"Noise"}})

	if status := postStatus(t, tech, job+"/parts", url.Values{"description": {"Wheel bearing, left front"}, "quantity": {"2"}}); status != http.StatusSeeOther {
		t.Fatalf("asking for the part answered %d", status)
	}
	if status, _ := page(t, tech, base+"/supply"); status != http.StatusForbidden {
		t.Errorf("a technician opened the orders page: %d", status)
	}
	_, supply := page(t, desk, base+"/supply")
	req := regexp.MustCompile(`name="use_([0-9a-f-]{36})"`).FindStringSubmatch(supply)
	if req == nil || !strings.Contains(supply, "Wheel bearing, left front") {
		t.Fatalf("the request is not offered for ordering: %s", supply)
	}

	order := url.Values{"supplier": {"Lagerhuset"}, "reference": {"LH-77"}, "eta": {"2030-03-04"},
		"use_" + req[1]: {"1"}, "qty_" + req[1]: {"2"}, "cost_" + req[1]: {"310,50"}}
	req2, _ := http.NewRequest(http.MethodPost, base+"/supply/orders", strings.NewReader(order.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Origin", base)
	c := *desk
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("recording the order answered %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	if status := postStatus(t, desk, base+"/supply/orders", order); status != http.StatusBadRequest {
		t.Errorf("the same reference twice answered %d, want 400", status)
	}
	_, mine := page(t, tech, job)
	if !strings.Contains(mine, "ordered") || !strings.Contains(mine, "expected") || strings.Contains(mine, "Lagerhuset") || strings.Contains(mine, "310,50") || strings.Contains(mine, "310.50") {
		t.Errorf("the technician's job page: ordered/expected missing, or the supplier or a cost leaked: %v %v %v %v", strings.Contains(mine, "ordered"), strings.Contains(mine, "expected"), strings.Contains(mine, "Lagerhuset"), strings.Contains(mine, "310"))
	}

	_, orderPage := page(t, desk, base+loc)
	line := regexp.MustCompile(`/supply/lines/([0-9a-f-]{36})/receive`).FindStringSubmatch(orderPage)
	if line == nil {
		t.Fatal("no way to book a delivery on the order page")
	}
	rec := base + "/supply/lines/" + line[1] + "/receive"
	if status := postStatus(t, tech, rec, url.Values{"quantity": {"1"}, "route": {"direct"}}); status != http.StatusForbidden {
		t.Errorf("a technician booked a delivery: %d", status)
	}
	if status := postStatus(t, desk, rec, url.Values{"quantity": {"1"}, "route": {"direct"}, "delivery_ref": {"DN-1"}, "back": {loc}}); status != http.StatusSeeOther {
		t.Fatalf("booking the first delivery answered %d", status)
	}
	if status := postStatus(t, desk, rec, url.Values{"quantity": {"1"}, "route": {"direct"}, "delivery_ref": {"DN-1"}, "back": {loc}}); status != http.StatusSeeOther {
		t.Fatalf("booking the same note again answered %d", status)
	}
	_, after := page(t, desk, base+loc)
	if !strings.Contains(after, "1 still owed") {
		t.Errorf("after one of two the order should say one is owed: %s", after)
	}
	// An address outside the supply pages is not somewhere to be sent.
	if status := postStatus(t, desk, base+"/supply/lines/"+line[1]+"/eta", url.Values{"eta": {"2030-03-09"}, "back": {"//evil.example"}}); status != http.StatusSeeOther {
		t.Errorf("moving the day answered %d", status)
	}
	if _, orderPage = page(t, desk, base+loc); !strings.Contains(orderPage, "&rarr;") {
		t.Error("the order page does not show that the day moved")
	}
}
