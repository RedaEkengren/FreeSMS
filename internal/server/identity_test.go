package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/identity"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A party's identity checks on their page (#153): the salesperson records
// a number for due diligence and reads it masked, then in full, each read
// an event; the workshop's front desk sees that a check was made and
// nothing else; without the key nothing is shown in the clear; ending the
// purpose removes the value and keeps the check.
func TestIdentityChecksOnTheirPage(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	party, err := workshop.PartyOfCustomer(ctx, pool, owner, "aaaaaaaa-0000-0000-0000-000000000004")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartPartyRole(ctx, pool, owner, party, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	k, err := identity.NewKey("k1")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := identity.ParseKeys(k)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	srv.identityKeys = keys

	const number = "19800101-4321"
	base := ts.URL + "/parties/" + party + "/identity"
	seller := signIn(t, ts, salesEmail)
	if form := get(t, seller, base, http.StatusOK); !strings.Contains(form, `value="aml_due_diligence"`) || strings.Contains(form, "Licence check before a rental") {
		t.Fatalf("the seller's form offers the wrong purposes:\n%s", visibleText(form))
	}
	if resp := postResponse(t, seller, base, url.Values{"purpose": {"aml_due_diligence"}, "kind": {workshop.IdentityNumber},
		"value": {number}, "reference": {"x"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a value and a reference both: %d", resp.StatusCode)
	}
	if resp := postResponse(t, seller, base, url.Values{"purpose": {"aml_due_diligence"}, "kind": {workshop.IdentityNumber},
		"value": {number}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recording: %d %s", resp.StatusCode, readBody(t, resp))
	}
	records, err := workshop.IdentityRecordsFor(ctx, pool, owner, party)
	if err != nil || len(records) != 1 {
		t.Fatalf("recorded: %+v, %v", records, err)
	}
	path := "/parties/" + party + "/identity/" + records[0].ID
	show := base + "/" + records[0].ID + "/show"

	page := get(t, seller, base, http.StatusOK)
	if strings.Contains(page, "4321") || !strings.Contains(page, "Show in full") {
		t.Errorf("the list shows the value, or no way to read it:\n%s", visibleText(page))
	}
	resp := postModuleForm(t, seller, ts.URL, path+"/show", url.Values{})
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "4321") || strings.Contains(body, "19800101") {
		t.Errorf("masked: %d %s", resp.StatusCode, visibleText(body))
	}
	resp = postModuleForm(t, seller, ts.URL, path+"/show", url.Values{"full": {"1"}})
	if body := readBody(t, resp); !strings.Contains(body, number) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("in full: %q %s", resp.Header.Get("Cache-Control"), visibleText(body))
	}
	if events, err := workshop.AuditTrail(ctx, pool, owner, "identity_record", records[0].ID); err != nil || len(events) != 2 {
		t.Errorf("the reads' events: %+v, %v", events, err)
	}

	// Another shop's party, real and with a buyer's role: not found.
	var other string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM parties WHERE customer_id = $1`, customerB).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO party_roles (shop_id, party_id, role) VALUES ($1, $2, 'buyer')`, shopB, other); err != nil {
		t.Fatal(err)
	}
	if status, body := getBody(t, seller, ts.URL+"/parties/"+other+"/identity"); status != http.StatusNotFound || strings.Contains(body, secretB) {
		t.Errorf("another shop's party: %d", status)
	}

	// The workshop's front desk: that a check was made, no way to read it.
	desk := signIn(t, ts, advisorEmail)
	page = get(t, desk, base, http.StatusOK)
	if !strings.Contains(page, "Personal identity number") || strings.Contains(page, "Show in full") || strings.Contains(page, "Record the check") {
		t.Errorf("the front desk's page:\n%s", visibleText(page))
	}
	if resp := postResponse(t, desk, show, url.Values{"full": {"1"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the front desk read it: %d", resp.StatusCode)
	}
	if card := get(t, desk, ts.URL+"/customers/aaaaaaaa-0000-0000-0000-000000000004", http.StatusOK); !strings.Contains(card, "/parties/"+party+"/identity") {
		t.Error("the customer card has no way to the checks")
	}

	// No key: refused, nothing in the clear.
	srv.identityKeys = nil
	resp = postModuleForm(t, seller, ts.URL, path+"/show", url.Values{"full": {"1"}})
	if body := readBody(t, resp); resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(body, "4321") {
		t.Errorf("without the key: %d %s", resp.StatusCode, visibleText(body))
	}
	srv.identityKeys = keys

	if resp := postResponse(t, seller, base+"/"+records[0].ID+"/end", url.Values{"reason": {""}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("ended with no reason: %d", resp.StatusCode)
	}
	if resp := postResponse(t, seller, base+"/"+records[0].ID+"/end", url.Values{"reason": {"The sale fell through"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ending: %d", resp.StatusCode)
	}
	if page := get(t, seller, base, http.StatusOK); strings.Contains(page, "Show in full") || !strings.Contains(page, "The purpose ended") {
		t.Errorf("after the end:\n%s", visibleText(page))
	}
}
