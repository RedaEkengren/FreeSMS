package server

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// registeredRoutes reads the patterns out of server.go.
//
// http.ServeMux does not report what has been registered with it, so the
// source is the only list there is. Reading it is the point: a route added to
// the mux and not to the table below becomes a failure here, which is the
// difference between a sweep and a handful of examples.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ [^"]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	if len(out) < 40 {
		t.Fatalf("found only %d routes; the pattern has stopped matching", len(out))
	}
	return out
}

// swept says what the sweep does with each route.
//
// Every registered pattern needs an entry. An exclusion carries its reason in
// the same place, because a list of skips with no reasons is a list of
// failures somebody hid.
var swept = map[string]string{
	// Nothing to substitute: these take no identifier at all.
	"GET /healthz": "", "GET /static/": "", "GET /setup": "", "POST /setup": "",
	"GET /login": "", "POST /login": "", "POST /logout": "", "GET /{$}": "",
	"GET /board": "", "GET /jobs/new": "", "POST /jobs/new": "",
	"GET /jobs/new/lookup": "", "GET /figures": "", "GET /accounting": "",
	"POST /accounting/export": "", "GET /shop": "", "POST /shop": "", "POST /shop/charges": "",
	"GET /privacy": "", "GET /labour": "", "POST /labour": "",
	"POST /labour/rate": "", "GET /time": "", "POST /time/correct": "",
	"POST /drafts": "", "GET /drafts": "", "DELETE /drafts": "",
	"GET /search": "", "GET /scan": "", "GET /labels": "", "GET /stock": "",
	"POST /stock/move": "", "POST /stock/count": "", "GET /stock/parts/new": "", "POST /stock/parts": "",
	"GET /staff": "", "GET /receivables": "", "POST /staff": "", "GET /account": "", "POST /account/password": "", "POST /account/language": "", "POST /stock/bands": "", "GET /parts": "",
	"POST /parts/arrived": "", "GET /checklists": "", "POST /checklists": "",

	// The customer's link. Not a shop identifier: it is an unguessable token,
	// and asking for it with a uuid is a different test -- the share tests
	// cover an unknown, an expired and a revoked one.
	"GET /i/{token}":                 "token, not an identifier",
	"POST /i/{token}/items/{itemID}": "token, not an identifier",
	"GET /i/{token}/photos/{key}":    "token, not an identifier",

	// Everything below takes an identifier and gets swept.
	"GET /jobs/{id}":                              "sweep",
	"POST /jobs/{id}/state":                       "sweep",
	"POST /jobs/{id}/lines":                       "sweep",
	"POST /jobs/{id}/invoice":                     "sweep",
	"POST /jobs/{id}/credit":                      "sweep",
	"POST /jobs/{id}/inspect":                     "sweep",
	"POST /jobs/{id}/promise":                     "sweep",
	"POST /jobs/{id}/answers/{itemID}":            "sweep",
	"POST /jobs/{id}/car":                         "sweep",
	"POST /jobs/{id}/takeout":                     "sweep",
	"POST /jobs/{id}/told":                        "sweep",
	"GET /jobs/{id}/parts":                        "sweep",
	"POST /jobs/{id}/parts":                       "sweep",
	"GET /jobs/{id}/customer":                     "sweep",
	"POST /jobs/{id}/customer":                    "sweep",
	"POST /jobs/{id}/findings":                    "sweep",
	"POST /jobs/{id}/findings/handled":            "sweep",
	"POST /jobs/{id}/clock-in":                    "sweep",
	"POST /jobs/{id}/clock-out":                   "sweep",
	"GET /invoices/{id}":                          "sweep",
	"POST /invoices/{id}/payments":                "sweep",
	"POST /payments/{id}/reverse":                 "sweep",
	"GET /inspections/{id}":                       "sweep",
	"POST /inspections/{id}/items/{itemID}":       "sweep",
	"POST /inspections/{id}/items/{itemID}/photo": "sweep",
	"POST /inspections/{id}/complete":             "sweep",
	"POST /inspections/{id}/share":                "sweep",
	"POST /inspections/{id}/revoke":               "sweep",
	"GET /photos/{key}":                           "sweep",
	"GET /people/{id}/export":                     "sweep",
	"POST /people/{id}/erase":                     "sweep",
	"GET /vehicles/{id}":                          "sweep",
	"GET /checklists/{id}":                        "sweep",
	"GET /stock/parts/{id}":                       "sweep",
	"POST /staff/{id}/active":                     "sweep",
	"POST /staff/{id}/password":                   "sweep",
	"POST /stock/parts/{id}":                      "sweep",
	"POST /stock/parts/{id}/active":               "sweep",
	"POST /checklists/{id}":                       "sweep",
	"POST /checklists/{id}/active":                "sweep",
}

// fill substitutes shop B's identifiers into a pattern.
func fill(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	for from, to := range map[string]string{
		"/jobs/{id}":        "/jobs/" + jobB,
		"/invoices/{id}":    "/invoices/" + invoiceB,
		"/payments/{id}":    "/payments/" + paymentB,
		"/inspections/{id}": "/inspections/" + inspectionB,
		"/people/{id}":      "/people/" + personB,
		"/vehicles/{id}":    "/vehicles/" + vehicleB,
		"/checklists/{id}":  "/checklists/" + checklistB,
		"/stock/parts/{id}": "/stock/parts/" + partB,
		"/staff/{id}":       "/staff/bbbbbbbb-0000-0000-0000-0000000000bb",
		"{itemID}":          itemB,
		"{key}":             photoB,
	} {
		path = strings.ReplaceAll(path, from, to)
	}
	return method, path
}

// Every route that takes an identifier, asked for with shop B's, as each of
// shop A's roles. None of them may answer 200, and none may put shop B's data
// in the body -- checking the status alone would pass a page that renders the
// name and then sets the code.
//
// 403 is allowed as well as 404: a role that may not reach the route at all is
// refused before the identifier is looked at, and that is a stronger answer,
// not a weaker one. What is not allowed is 200, and what is never allowed is
// the name.
func TestNoRouteServesAnotherShopsRow(t *testing.T) {
	ts, _ := testServer(t)

	clients := map[string]*http.Client{
		"technician": signIn(t, ts, techEmail),
		"advisor":    signIn(t, ts, advisorEmail),
		"owner":      signIn(t, ts, ownerEmail),
	}

	var sweptCount int
	for _, pattern := range registeredRoutes(t) {
		what, known := swept[pattern]
		if !known {
			t.Errorf("route %q is registered and not in the sweep table; decide what it does and say so there", pattern)
			continue
		}
		if what != "sweep" {
			continue
		}
		sweptCount++

		method, path := fill(pattern)
		if strings.ContainsAny(path, "{}") {
			t.Errorf("%s: no substitution for every placeholder, so the sweep would ask for a literal brace", pattern)
			continue
		}

		for role, client := range clients {
			req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(""))
			if err != nil {
				t.Fatalf("%s: %v", pattern, err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", ts.URL)

			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("%s as %s: %v", pattern, role, err)
				continue
			}
			body := make([]byte, 64*1024)
			n, _ := resp.Body.Read(body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				t.Errorf("%s as %s answered 200 for shop B's row", pattern, role)
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				// A redirect to a page that then shows the row would pass a
				// status check and leak anyway.
				t.Errorf("%s as %s answered %d; a redirect hides where this ends up", pattern, role, resp.StatusCode)
			}
			if strings.Contains(string(body[:n]), secretB) {
				t.Errorf("%s as %s leaked shop B's customer into the body", pattern, role)
			}
		}
	}

	// A sweep that silently stopped sweeping is worse than no sweep.
	if sweptCount < 20 {
		t.Errorf("only %d routes were swept; the table has drifted from the mux", sweptCount)
	}
	t.Logf("swept %d routes carrying an identifier, as %d roles", sweptCount, len(clients))
}
