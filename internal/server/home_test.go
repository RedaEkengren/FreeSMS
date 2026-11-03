package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// One home page (#157): what to do now before how the business is doing,
// each card from the modules the shop runs and only for a role that may
// see it -- the owner the work ready to invoice, the counter the cars
// ready, a technician neither a money card nor anything the counter's --
// above the screen the role works from.
func TestTheHomePageIsCardsByModuleAndRole(t *testing.T) {
	ts, pool := testServer(t)
	desk, tech, owner := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail), signIn(t, ts, ownerEmail)
	post(t, desk, ts.URL+"/jobs/"+jobA+"/lines", url.Values{
		"kind": {"labour"}, "description": {"Replace front pads"},
		"quantity": {"2.5"}, "unit_price": {"895.00"}, "vat_rate": {"25"},
	})
	for _, state := range []string{"estimated", "awaiting_approval", "approved", "in_progress", "ready"} {
		if state == "approved" {
			agreeAtTheCounter(t, ts.URL, jobA, desk, pool)
		}
		post(t, desk, ts.URL+"/jobs/"+jobA+"/state", url.Values{"state": {state}})
	}

	home := visibleText(get(t, owner, ts.URL+"/", http.StatusOK))
	now, business := strings.Index(home, "What to do now"), strings.Index(home, "How the business is doing")
	if now < 0 || business < now || !strings.Contains(home, "1 car ready") || !strings.Contains(home, "Work ready to invoice, before VAT: ") {
		t.Errorf("the owner's home page: what to do now, then the business, with the car ready and the work to invoice:\n%s", home)
	}
	if !strings.Contains(get(t, owner, ts.URL+"/", http.StatusOK), `id="live-board"`) {
		t.Error("the owner's home page lost the board below its cards")
	}
	deskHome := visibleText(get(t, desk, ts.URL+"/", http.StatusOK))
	if !strings.Contains(deskHome, "1 car ready") || strings.Contains(deskHome, "How the business is doing") {
		t.Errorf("the counter's home page: the car ready and no figures of the business:\n%s", deskHome)
	}
	techHome := visibleText(get(t, tech, ts.URL+"/", http.StatusOK))
	for _, never := range []string{"car ready", "How the business is doing", "invoice", customerName} {
		if strings.Contains(techHome, never) {
			t.Errorf("a technician's home page shows %q", never)
		}
	}
}
