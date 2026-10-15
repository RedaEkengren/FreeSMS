package server

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// From a question about a price to a booking, through the pages: the desk
// builds the proposal, the customer reads every amount on their own link and
// answers a version they were shown, and what they accepted is booked.
func TestAnEnquiryFromQuestionToBooking(t *testing.T) {
	ts, _ := testServer(t)
	desk, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL

	// Nothing but what they asked about.
	location := intake2(t, base+"/enquiries", desk, url.Values{"subject": {"Bromsar fram"}})
	enquiry := base + location
	for _, line := range []url.Values{
		{"kind": {"labour"}, "description": {"Byte av skivor"}, "quantity": {"2"}, "unit_price": {"800"}},
		{"kind": {"part"}, "description": {"Skivor, par"}, "quantity": {"1"}, "unit_price": {"900"}},
	} {
		if status := postStatus(t, desk, enquiry+"/lines", line); status != http.StatusSeeOther {
			t.Fatalf("adding a line answered %d", status)
		}
	}
	if status := postStatus(t, desk, enquiry+"/quote", url.Values{"valid_days": {"10"}, "price_kind": {"estimate"},
		"limits": {"Diagnos upp till en timme"}}); status != http.StatusSeeOther {
		t.Fatalf("putting it forward answered %d", status)
	}
	_, body := page(t, desk, enquiry)
	for _, want := range []string{"Version 1", "3,125.00", "An estimate", "Diagnos upp till en timme", "Sent", "not known yet", "Print this version"} {
		if !strings.Contains(body, want) {
			t.Errorf("the enquiry page lacks %q", want)
		}
	}
	if strings.Contains(body, "Book what they accepted") {
		t.Error("booking is offered before the customer has said yes")
	}

	// The print page carries the frozen figures.
	_, print := page(t, desk, enquiry+"/quote/1/print")
	for _, want := range []string{"Byte av skivor", "2,500.00", "625.00", "3,125.00", "Diagnos upp till en timme", "stands until"} {
		if !strings.Contains(print, want) {
			t.Errorf("the printable proposal lacks %q", want)
		}
	}

	// The customer's link.
	resp := post(t, desk, enquiry+"/link", url.Values{})
	m := regexp.MustCompile(`/o/([A-Za-z0-9_-]{20,})`).FindStringSubmatch(resp)
	if m == nil {
		t.Fatal("the page gave no link")
	}
	token := m[1]
	customer := &http.Client{}
	_, offer := page(t, customer, base+"/o/"+token)
	for _, want := range []string{"Bromsar fram", "Byte av skivor", "1,600.00", "3,125.00", "Yes, do it", "An estimate", "Untick"} {
		if !strings.Contains(offer, want) {
			t.Errorf("the customer's page lacks %q", want)
		}
	}
	quoteID := regexp.MustCompile(`/o/` + token + `/quote/([0-9a-f-]{36})`).FindStringSubmatch(offer)
	if quoteID == nil {
		t.Fatal("the customer's page has no form for the version")
	}

	// The workshop replaces the version while the page is open: the old form
	// is sent to read the new one and answers nothing.
	postStatus(t, desk, enquiry+"/lines", url.Values{"kind": {"fee"}, "description": {"Miljöavgift"}, "quantity": {"1"}, "unit_price": {"100"}})
	postStatus(t, desk, enquiry+"/quote", url.Values{"valid_days": {"10"}, "price_kind": {"fixed"}})
	if status := postStatus(t, customer, base+"/o/"+token+"/quote/"+quoteID[1], url.Values{"decision": {"approved"}, "keep": {"1", "2"}}); status != http.StatusSeeOther {
		t.Fatalf("the old form answered %d", status)
	}
	_, fresh := page(t, customer, base+"/o/"+token+"?changed=1")
	if !strings.Contains(fresh, "Miljöavgift") || !strings.Contains(fresh, "changed the proposal") {
		t.Error("the customer is not shown the new version")
	}
	_, body = page(t, desk, enquiry)
	if strings.Contains(body, "Book what they accepted") {
		t.Error("a stale yes was taken")
	}

	// A partial yes to the version they are now shown: the part is bought elsewhere.
	newID := regexp.MustCompile(`/o/` + token + `/quote/([0-9a-f-]{36})`).FindStringSubmatch(fresh)
	if status := postStatus(t, customer, base+"/o/"+token+"/quote/"+newID[1], url.Values{"decision": {"approved"}, "keep": {"1", "3"}}); status != http.StatusSeeOther {
		t.Fatalf("the customer's answer answered %d", status)
	}
	_, body = page(t, desk, enquiry)
	for _, want := range []string{"Partly accepted", "yes to part", "on their link", "Book what they accepted"} {
		if !strings.Contains(body, want) {
			t.Errorf("after a partial yes the page lacks %q", want)
		}
	}

	// Booked, once, however many times it is sent.
	var booking string
	for i := 0; i < 2; i++ {
		got := intake2(t, enquiry+"/book", desk, url.Values{"date": {"2027-03-02"}, "time": {"09:00"}, "minutes": {"90"}})
		if booking != "" && got != booking {
			t.Errorf("booking twice made a second booking: %s and %s", booking, got)
		}
		booking = got
	}
	_, b := page(t, desk, base+booking)
	if !strings.Contains(b, "Bromsar fram") {
		t.Error("the booking does not say what it is for")
	}

	// Taken in, the job carries what they accepted and says where it came from.
	var job string
	{
		bookingID := strings.TrimPrefix(booking, "/bookings/")
		job = intake2(t, base+"/jobs/new", desk, url.Values{"registration": {"ENQ 900"}, "complaint": {"Bromsar fram"}, "booking_id": {bookingID}})
	}
	_, jobPage := page(t, desk, base+job)
	for _, want := range []string{"Byte av skivor", "Miljöavgift", "The enquiry and the price they accepted", "/enquiries/"} {
		if !strings.Contains(jobPage, want) {
			t.Errorf("the job lacks %q", want)
		}
	}
	if strings.Contains(jobPage, "Skivor, par") {
		t.Error("a line the customer turned down came with the job")
	}

	// The front desk's alone.
	if resp, err := tech.Get(enquiry); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a technician opening an enquiry: %v %v", resp, err)
	}
	// A link that is not one.
	if resp, err := customer.Get(base + "/o/not-a-link"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("a made-up link: %v %v", resp, err)
	}
}

// intake2 posts a form and returns where it was sent.
func intake2(t *testing.T, target string, c *http.Client, form url.Values) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", strings.SplitN(target, "/", 4)[0]+"//"+strings.SplitN(target, "/", 4)[2])
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%s answered %d", target, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}
