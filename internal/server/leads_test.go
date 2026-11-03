package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Leads through the pages (#172): a salesperson on the floor records an
// enquiry from a phone, finds it on their list, and the same person asking
// again is offered their lead. The workshop's roles are refused, and a
// shop that does not sell cars has no such page.
func TestASalespersonRecordsALeadOnThePage(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	unit, err := workshop.TakeIntoStock(ctx, pool, owner, vehicleA, workshop.StockFromDefleet, "")
	if err != nil {
		t.Fatal(err)
	}
	seller := signIn(t, ts, salesEmail)
	form := url.Values{"name": {"Kim Köpare"}, "phone": {"070-123 45 67"}, "unit": {unit.ID}, "channel": {"walk_in"},
		"salesperson": {"aaaaaaaa-0000-0000-0000-0000000000a2"}, "note": {"Vill provköra"}, "key": {"k-1"}}
	resp := postResponse(t, seller, ts.URL+"/cars/leads", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/cars/leads/") {
		t.Fatalf("recording the lead answered %d, %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	lead := get(t, seller, ts.URL+resp.Header.Get("Location"), http.StatusOK)
	if !strings.Contains(lead, "Kim Köpare") || !strings.Contains(lead, "Vill provköra") || !strings.Contains(lead, "A Salesperson") {
		t.Errorf("the lead's page:\n%s", visibleText(lead))
	}
	if list := get(t, seller, ts.URL+"/cars/leads", http.StatusOK); !strings.Contains(list, "Kim Köpare") {
		t.Errorf("the list:\n%s", visibleText(list))
	}

	form.Set("key", "k-2")
	form.Set("phone", "+46 70 123 45 67")
	again := postResponse(t, seller, ts.URL+"/cars/leads", form)
	defer again.Body.Close()
	if again.StatusCode != http.StatusConflict {
		t.Errorf("the same person again answered %d; want their lead offered", again.StatusCode)
	}

	if page := get(t, signIn(t, ts, techEmail), ts.URL+"/cars/leads", http.StatusForbidden); strings.Contains(page, "Kim Köpare") {
		t.Error("a technician was shown a lead")
	}
	// The lead is open business: switching off asks first (#143), and the
	// owner, meaning it, switches off with it open.
	if _, err := workshop.SwitchModule(ctx, pool, owner, access.Dealer, false, "Slutar sälja bilar", false); err == nil {
		t.Error("car sales switched off with a lead open, unasked")
	}
	if _, err := workshop.SwitchModule(ctx, pool, owner, access.Dealer, false, "Slutar sälja bilar", true); err != nil {
		t.Fatal(err)
	}
	get(t, signIn(t, ts, salesEmail), ts.URL+"/cars/leads", http.StatusNotFound)
}
