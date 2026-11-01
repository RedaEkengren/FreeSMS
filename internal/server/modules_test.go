package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// postModuleForm posts a form as a signed-in client, from the page's own origin.
func postModuleForm(t *testing.T, client *http.Client, base, path string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Every route is registered as Core's or a module's, and the modules are
// ones there are (#143).
func TestEveryRouteBelongsToCoreOrAModule(t *testing.T) {
	routes := routeTable(t)
	perModule := map[access.Module]int{}
	for _, r := range routes {
		if r.Module != "" && !r.Module.Valid() {
			t.Errorf("%s is registered to %q, which is no module", r.Pattern, r.Module)
		}
		perModule[r.Module]++
	}
	if perModule[""] < 40 || perModule[access.Workshop] < 100 {
		t.Errorf("Core has %d routes and the workshop %d; the table has stopped seeing them", perModule[""], perModule[access.Workshop])
	}
}

// Switched off, a module's routes answer exactly as a path that does not
// exist -- signed in or not, whatever the method -- and Core's go on
// answering, its history with them (#143).
func TestASwitchedOffModulesRoutesAreNotThere(t *testing.T) {
	ts, pool := testServer(t)
	owner := signIn(t, ts, ownerEmail)
	tech := signIn(t, ts, techEmail)

	// An invoice issued while the workshop ran, to read afterwards. The job
	// it was for is still open, which switching off has to be told about.
	const job = "aaaaaaaa-0000-0000-0000-00000000000f"
	const invoice = "aaaaaaaa-0000-0000-0000-000000000020"
	seedShop(t, pool, shopA, []string{
		`INSERT INTO invoices (id, shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor)
		 VALUES ('` + invoice + `','` + shopA + `','A',1,'` + job + `','` + customerName + `', 100000, 25000, 125000)`,
	})
	inv := struct{ ID string }{invoice}

	missing := func(client *http.Client, method, path string) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp.StatusCode, readBody(t, resp)
	}
	_, nothing := missing(owner, http.MethodGet, "/there-is-no-such-page")

	// Off, with the open job confirmed over.
	resp := postModuleForm(t, owner, ts.URL, "/shop/modules", url.Values{"module": {"workshop"}, "on": {"no"},
		"reason": {"Closing the workshop"}})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "/jobs/") {
		t.Fatalf("switching off over an open job answered %d without listing it", resp.StatusCode)
	}
	// The reason typed is still there to confirm with, not lost to the
	// re-rendered page (which would record the change with no reason).
	if !strings.Contains(body, `value="Closing the workshop"`) {
		t.Error("the refused switch-off shows the form again without the reason that was typed")
	}
	resp = postModuleForm(t, owner, ts.URL, "/shop/modules", url.Values{"module": {"workshop"}, "on": {"no"},
		"confirm": {"yes"}, "reason": {"Closing the workshop"}})
	readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("switching off, confirmed, answered %d", resp.StatusCode)
	}

	anonymous := noRedirects()
	checked := 0
	for _, r := range routeTable(t) {
		if r.Module != access.Workshop {
			continue
		}
		method, path := fill(r.Pattern)
		for from, to := range map[string]string{
			"{token}": "a-token", "{secret}": "a-secret", "{kind}": "customers", "{step}": "vat", "{role}": "owner",
		} {
			path = strings.ReplaceAll(path, from, to)
		}
		for who, client := range map[string]*http.Client{"owner": owner, "technician": tech, "nobody": anonymous} {
			status, body := missing(client, method, path)
			if r.CustomerLink {
				// A link the customer was sent: a neutral page, nothing to
				// press (owner decision on #143).
				if status != http.StatusGone || !strings.Contains(body, "This service is no longer available") ||
					strings.Contains(body, "<form") {
					t.Errorf("%s %s as %s, workshop off: %d; want the neutral page with no actions", method, path, who, status)
				}
				continue
			}
			if status != http.StatusNotFound || body != nothing {
				t.Errorf("%s %s as %s, workshop off: %d %q; want the answer a path that does not exist gets",
					method, path, who, status, body)
			}
		}
		checked++
	}
	if checked < 100 {
		t.Errorf("only %d workshop routes checked", checked)
	}
	// Nor by a malformed identifier or a method nobody registered: the shape
	// check and the ServeMux's 405 used to answer ahead of the gate.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/jobs/not-a-uuid"}, {http.MethodPatch, "/jobs/" + job}, {http.MethodDelete, "/calendar"},
	} {
		for who, client := range map[string]*http.Client{"owner": owner, "nobody": anonymous} {
			if status, body := missing(client, probe.method, probe.path); status != http.StatusNotFound || body != nothing {
				t.Errorf("%s %s as %s, workshop off: %d %q; want the answer a path that does not exist gets",
					probe.method, probe.path, who, status, body)
			}
		}
	}

	// The invoice's link is Core: the customer reads their invoice for as
	// long as it is kept, whatever the shop runs.
	token := "an-invoice-link-token-for-the-test"
	sum := sha256.Sum256([]byte(token))
	seedShop(t, pool, shopA, []string{
		`INSERT INTO invoice_links (shop_id, invoice_id, token_sha256, created_by, expires_at) VALUES
		 ('` + shopA + `','` + invoice + `', decode('` + hex.EncodeToString(sum[:]) + `', 'hex'),
		  'aaaaaaaa-0000-0000-0000-000000000011', now() + interval '30 days')`,
	})
	if status, body := missing(anonymous, http.MethodGet, "/d/"+token); status != http.StatusOK || !strings.Contains(body, customerName) {
		t.Errorf("the customer's invoice link with the workshop off: %d", status)
	}

	// Core, and the workshop's history through it.
	for _, path := range []string{"/invoices/" + inv.ID, "/receivables", "/customers", "/customers/aaaaaaaa-0000-0000-0000-000000000004",
		"/vehicles/aaaaaaaa-0000-0000-0000-000000000005", "/shop", "/figures", "/accounting", "/mine", "/account", "/search"} {
		status, body := missing(owner, http.MethodGet, path)
		if status != http.StatusOK {
			t.Errorf("GET %s, workshop off: %d, want 200 -- Core and what the workshop recorded stay readable", path, status)
		}
		for _, link := range []string{`href="/calendar"`, `href="/enquiries"`, `href="/parts"`, `href="/labour"`, `href="/time"`, `href="/jobs/`} {
			if strings.Contains(body, link) {
				t.Errorf("GET %s, workshop off, still links to %s", path, link)
			}
		}
	}
	if status, body := missing(owner, http.MethodGet, "/invoices/"+inv.ID); status != http.StatusOK || !strings.Contains(body, customerName) {
		t.Errorf("the invoice the workshop issued is not readable after it was switched off: %d", status)
	}
	// Home, with no board to start at.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if resp, err := owner.Do(req); err != nil || resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/customers" {
		t.Errorf("home with the workshop off: %v %v", resp, err)
	}

	// On again, and it is all there.
	resp = postModuleForm(t, owner, ts.URL, "/shop/modules", url.Values{"module": {"workshop"}, "on": {"yes"}})
	readBody(t, resp)
	if status, _ := missing(owner, http.MethodGet, "/jobs/"+job); status != http.StatusOK {
		t.Errorf("the job after switching the workshop on again: %d", status)
	}
	if status, _ := missing(owner, http.MethodPatch, "/jobs/"+job); status != http.StatusMethodNotAllowed {
		t.Errorf("PATCH on a job with the workshop on: %d, want 405 as before", status)
	}
	if status, body := missing(owner, http.MethodGet, "/shop"); status != http.StatusOK ||
		!strings.Contains(body, "Closing the workshop") || !strings.Contains(body, "An Owner") {
		t.Errorf("the shop page does not show who switched the workshop off and why")
	}
}

// Only whoever runs the shop switches a module (#143).
func TestOnlyWhoeverRunsTheShopSwitchesAModule(t *testing.T) {
	ts, _ := testServer(t)
	for _, email := range []string{techEmail, advisorEmail} {
		client := signIn(t, ts, email)
		resp := postModuleForm(t, client, ts.URL, "/shop/modules", url.Values{"module": {"workshop"}, "on": {"no"}, "confirm": {"yes"}})
		readBody(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s switching the workshop off: %d, want 403", email, resp.StatusCode)
		}
	}
	// And nothing happened.
	client := signIn(t, ts, techEmail)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/jobs/aaaaaaaa-0000-0000-0000-00000000000f", nil)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("the job after refused switches: %v %v", resp, err)
	}
}

// Setup asks what the shop runs; the workshop is ticked, and a form with
// everything unticked is sent back (#143).
func TestSetupAsksWhatTheShopRuns(t *testing.T) {
	ts, pool := emptyServer(t)
	client := noRedirects()
	resp, err := client.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, resp); !strings.Contains(body, `name="module" value="workshop"  checked`) &&
		!strings.Contains(body, `name="module" value="workshop" checked`) {
		t.Errorf("the setup form does not offer the workshop, ticked")
	}
	form := url.Values{"shop_name": {"Verkstaden"}, "owner_name": {"Anna"}, "email": {"anna@example.test"},
		"password": {"a reasonable workshop password"}, "modules_asked": {"1"}}
	resp = postModuleForm(t, client, ts.URL, "/setup", form)
	if body := readBody(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "choose at least one") {
		t.Errorf("setup with nothing ticked: %d", resp.StatusCode)
	}
	form.Set("module", "dealer")
	resp = postModuleForm(t, client, ts.URL, "/setup", form)
	if readBody(t, resp); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("setup with a module this release does not have: %d, want 400", resp.StatusCode)
	}
	form.Set("module", "workshop")
	resp = postModuleForm(t, client, ts.URL, "/setup", form)
	readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	var modules []string
	var by *string
	if err := pool.QueryRow(context.Background(), `
		SELECT ARRAY(SELECT module FROM shop_module_changes WHERE enabled), (SELECT by_user::text FROM shop_module_changes LIMIT 1)`,
	).Scan(&modules, &by); err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules[0] != "workshop" || by == nil {
		t.Errorf("setup recorded %v by %v; want the workshop, by the owner it made", modules, by)
	}
	// The owner it signed in runs the workshop.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/board", nil)
	if resp, err := client.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("the board after setup: %v %v", resp, err)
	}
}

// A request with nobody signed in reads what the shop runs once for a
// moment, not once per request: a customer's link with twenty photographs
// is twenty-one requests. A switch clears it (the route test above asks as
// nobody straight after switching).
func TestWhatTheShopRunsIsReadOnceForABurstOfRequestsWithNoSession(t *testing.T) {
	_, pool := testServer(t)
	var c moduleCache
	ctx := context.Background()
	for i := 0; i < 21; i++ {
		on, err := c.runs(ctx, pool, shopA, access.Workshop)
		if err != nil || !on {
			t.Fatalf("request %d: %v, %v", i, on, err)
		}
	}
	if n := c.reads.Load(); n != 1 {
		t.Errorf("%d reads for 21 requests, want 1", n)
	}
	c.forget()
	if _, err := c.runs(ctx, pool, shopA, access.Workshop); err != nil || c.reads.Load() != 2 {
		t.Errorf("after forget: %v, %d reads; want it read again", err, c.reads.Load())
	}
}
