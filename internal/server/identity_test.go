package server

import (
	"bytes"
	"context"
	"log/slog"
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

// The shop page's retention section (#150, #153): the owner sees how long
// documents and identity values are kept and changes it; a setting is a new
// row, so the page shows the latest; nobody else may.
func TestRetentionOnTheShopPage(t *testing.T) {
	ts, pool := testServer(t)
	dealerShop(t, pool)
	owner := signIn(t, ts, ownerEmail)

	page := get(t, owner, ts.URL+"/shop", http.StatusOK)
	if !strings.Contains(page, `id="retention"`) || !strings.Contains(page, `name="what" value="identity"`) {
		t.Fatalf("no retention section:\n%s", visibleText(page))
	}
	post := func(c *http.Client, what, name, value string) int {
		return postResponse(t, c, ts.URL+"/shop/retention", url.Values{"what": {what}, "name": {name}, "value": {value}}).StatusCode
	}
	if st := post(owner, "identity", "ownership_change", "45"); st != http.StatusSeeOther {
		t.Fatalf("identity retention: %d", st)
	}
	if st := post(owner, "document", workshop.DocPurchaseAgreement, "3"); st != http.StatusSeeOther {
		t.Fatalf("document retention: %d", st)
	}
	page = get(t, owner, ts.URL+"/shop", http.StatusOK)
	if !strings.Contains(page, `value="45"`) || !strings.Contains(page, `value="3"`) {
		t.Errorf("the page does not show the new settings:\n%s", visibleText(page))
	}
	for _, bad := range []struct{ what, name, value string }{
		{"identity", "ownership_change", "0"}, {"identity", "nonsense", "10"},
		{"document", workshop.DocPurchaseAgreement, "99"}, {"document", workshop.DocPurchaseAgreement, "x"},
	} {
		if st := post(owner, bad.what, bad.name, bad.value); st != http.StatusBadRequest && st != http.StatusNotFound {
			t.Errorf("%+v accepted: %d", bad, st)
		}
	}
	if st := post(signIn(t, ts, advisorEmail), "identity", "ownership_change", "10"); st != http.StatusForbidden {
		t.Errorf("the front desk set a retention: %d", st)
	}
}

// A stored identity value is in no log line and on no page that is not the
// identity page's own reading of it (#153): not while it is recorded, read
// masked, read in full, refused for want of a key or ended; not in the
// owner's queue, the home page or a search for it.
func TestAnIdentityValueIsInNoLogAndNoOtherPage(t *testing.T) {
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
	k, _ := identity.NewKey("k1")
	keys, err := identity.ParseKeys(k)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := testServers.Load(ts.URL)
	srv := v.(*Server)
	srv.identityKeys = keys
	var logged bytes.Buffer
	old := srv.log
	srv.log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defer func() { srv.log = old }()

	const digits = "198001014321"
	const number = "19800101-4321"
	base := ts.URL + "/parties/" + party + "/identity"
	seller := signIn(t, ts, salesEmail)
	if resp := postResponse(t, seller, base, url.Values{"purpose": {"aml_due_diligence"}, "kind": {workshop.IdentityNumber},
		"value": {number}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recording: %d", resp.StatusCode)
	}
	records, err := workshop.IdentityRecordsFor(ctx, pool, owner, party)
	if err != nil || len(records) != 1 {
		t.Fatalf("recorded: %+v, %v", records, err)
	}
	path := "/parties/" + party + "/identity/" + records[0].ID
	postModuleForm(t, seller, ts.URL, path+"/show", url.Values{})
	postModuleForm(t, seller, ts.URL, path+"/show", url.Values{"full": {"1"}})
	srv.identityKeys = nil
	postModuleForm(t, seller, ts.URL, path+"/show", url.Values{"full": {"1"}})
	srv.identityKeys = keys

	// Other pages, for the people who work with it and for the owner.
	for name, c := range map[string]*http.Client{"seller": seller, "owner": signIn(t, ts, ownerEmail)} {
		for _, p := range []string{"/", "/mine", "/search?q=4321", "/customers/aaaaaaaa-0000-0000-0000-000000000004"} {
			if _, body := getBody(t, c, ts.URL+p); strings.Contains(body, number) || strings.Contains(body, digits) {
				t.Errorf("%s: the value is on %s", name, p)
			}
		}
	}
	// A search for the value finds nothing: it is typed, so the page repeats
	// what was typed, but no record comes back.
	for _, q := range []string{number, digits} {
		if _, body := getBody(t, seller, ts.URL+"/search?q="+url.QueryEscape(q)); !strings.Contains(body, "Nothing matched") {
			t.Errorf("a search for the stored value found something:\n%s", visibleText(body))
		}
	}
	if postResponse(t, seller, base+"/"+records[0].ID+"/end", url.Values{"reason": {"The sale fell through"}}).StatusCode != http.StatusSeeOther {
		t.Fatal("ending")
	}
	if strings.Contains(logged.String(), number) || strings.Contains(logged.String(), digits) {
		t.Errorf("the value is in the log:\n%s", logged.String())
	}
}
