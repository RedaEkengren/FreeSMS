package server

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/config"
)

// registeredRoutes is every pattern the server registers.
//
// http.ServeMux does not report what has been registered with it; the
// server's own router writes each registration down, with its module (#143).
// A route added to the mux and not to the table below becomes a failure here,
// which is the difference between a sweep and a handful of examples. It used
// to read server.go with a pattern, which stopped seeing a route the moment
// it was registered through anything but a variable named mux.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range routeTable(t) {
		out = append(out, r.Pattern)
	}
	if len(out) < 40 {
		t.Fatalf("found only %d routes; the router has stopped writing them down", len(out))
	}
	return out
}

// routeTable is every route with the module it belongs to, from a server
// that has not been given a database: registering touches none.
func routeTable(t *testing.T) []registeredRoute {
	t.Helper()
	srv, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), &config.Config{
		BaseURL: "http://localhost", DefaultLocale: "en", Release: "test",
		AttachmentsDir: t.TempDir(), ExportDir: t.TempDir(),
	}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := srv.routes(); err != nil {
		t.Fatalf("routes: %v", err)
	}
	return srv.routeTable
}

// swept says what the sweep does with each route.
//
// Every registered pattern needs an entry. An exclusion carries its reason in
// the same place, because a list of skips with no reasons is a list of
// failures somebody hid.
var swept = map[string]string{
	// Nothing to substitute: these take no identifier at all.
	"GET /healthz": "", "GET /ops/status": "token, not an identifier", "GET /static/": "", "GET /setup": "", "POST /setup": "",
	"GET /login": "", "POST /login": "", "POST /logout": "", "GET /{$}": "",
	"GET /board": "", "GET /jobs/new": "", "POST /jobs/new": "",
	"GET /jobs/new/lookup": "", "GET /jobs/new/customers": "", "GET /figures": "", "GET /accounting": "",
	"POST /accounting/export": "", "GET /shop": "", "POST /shop": "", "POST /shop/modules": "", "POST /shop/charges": "", "POST /shop/final-check": "",
	"GET /payment-flows": "", "POST /payment-flows": "", "POST /payment-flows/threshold": "",
	// An invoice by its payment reference (#234), read under the shop's row level security.
	"POST /payments/by-reference": "",
	// What technicians see (#225): a person is a form value, read under the
	// shop's row level security like any other.
	"GET /technicians": "", "POST /technicians/fields": "", "POST /technicians/exceptions": "", "POST /technicians/exceptions/end": "",
	"GET /privacy": "", "GET /outbox": "", "GET /day": "", "POST /day/settlement": "", "POST /day/review": "", "POST /privacy/export": "", "GET /events": "", "GET /mine": "", "GET /rota": "", "POST /hooks/46elks/{secret}": "", "POST /rota": "", "GET /schedule": "", "GET /mine/count": "", "POST /account/alert": "", "POST /account/theme": "", "GET /calendar": "", "POST /calendar": "", "GET /enquiries": "", "POST /enquiries": "", "POST /calendar/capacity": "", "GET /labour": "", "POST /labour": "",
	"POST /labour/rate": "", "GET /time": "", "POST /time/correct": "",
	"POST /drafts": "", "GET /drafts": "", "DELETE /drafts": "",
	"GET /search": "", "GET /scan": "", "GET /labels": "", "GET /stock": "",
	"POST /stock/move": "", "POST /stock/count": "", "GET /stock/parts/new": "", "POST /stock/parts": "",
	"GET /staff": "", "GET /receivables": "", "POST /staff": "", "GET /account": "", "POST /account/password": "", "POST /account/language": "", "POST /stock/bands": "", "GET /parts": "",
	"POST /parts/arrived": "", "GET /checklists": "", "POST /checklists": "",
	// Rental
	"GET /rental/settings":                             "",
	"POST /rental/permits":                             "",
	"POST /rental/profiles":                            "",
	"POST /rental/permits/{id}/revoke":                 "sweep",
	"POST /rental/profiles/{id}":                       "sweep",
	"GET /rental":                                      "",
	"POST /rental/units":                               "",
	"GET /rental/units/{id}":                           "sweep",
	"POST /rental/units/{id}/fleet":                    "sweep",
	"POST /rental/memberships/{id}/end":                "sweep",
	"GET /rental/prices":                               "",
	"POST /rental/plans":                               "",
	"POST /rental/extras":                              "",
	"POST /rental/plans/{id}/seasons":                  "sweep",
	"POST /rental/plans/{id}/archive":                  "sweep",
	"POST /rental/extras/{id}/archive":                 "sweep",
	"POST /rental/bookings":                            "",
	"GET /rental/bookings/{id}":                        "sweep",
	"POST /rental/bookings/{id}/{action}":              "sweep",
	"POST /rental/drivers/{id}/check":                  "sweep",
	"POST /rental/drivers/{id}/remove":                 "sweep",
	"POST /rental/agreements/{id}/accepted":            "sweep",
	"GET /rental/handovers/{id}":                       "sweep",
	"POST /rental/handovers/{id}/items/{itemID}":       "sweep",
	"POST /rental/handovers/{id}/items/{itemID}/photo": "sweep",
	"POST /rental/handovers/{id}/finish":               "sweep",
	"POST /rental/handovers/{id}/abandon":              "sweep",
	"GET /rental/photos/{key}":                         "sweep",

	// The customer's link. Not a shop identifier: it is an unguessable token,
	// and asking for it with a uuid is a different test -- the share tests
	// cover an unknown, an expired and a revoked one.
	"GET /i/{token}":                  "token, not an identifier",
	"GET /k/{token}":                  "token, not an identifier",
	"POST /k/{token}/quote/{quoteID}": "token, not an identifier",
	"GET /o/{token}":                  "token, not an identifier",
	"POST /o/{token}/quote/{quoteID}": "token, not an identifier",
	"POST /i/{token}/items/{itemID}":  "token, not an identifier",
	"GET /i/{token}/photos/{key}":     "token, not an identifier",

	// Everything below takes an identifier and gets swept.
	"GET /enquiries/{id}":                            "sweep",
	"POST /enquiries/{id}/lines":                     "sweep",
	"POST /enquiries/{id}/lines/{position}/remove":   "sweep",
	"POST /enquiries/{id}/quote":                     "sweep",
	"POST /enquiries/{id}/quote/{quoteID}/answer":    "sweep",
	"POST /enquiries/{id}/link":                      "sweep",
	"POST /enquiries/{id}/book":                      "sweep",
	"GET /enquiries/{id}/quote/{version}/print":      "sweep",
	"GET /jobs/{id}":                                 "sweep",
	"POST /jobs/{id}/state":                          "sweep",
	"POST /jobs/{id}/lines":                          "sweep",
	"POST /jobs/{id}/quote":                          "sweep",
	"POST /jobs/{id}/quote/{quoteID}/answer":         "sweep",
	"POST /jobs/{id}/lines/{position}/performed":     "sweep",
	"GET /jobs/{id}/lines/{position}/edit":           "sweep",
	"POST /jobs/{id}/lines/{position}/edit":          "sweep",
	"POST /jobs/{id}/lines/{position}/withdraw":      "sweep",
	"POST /jobs/{id}/invoice":                        "sweep",
	"POST /jobs/{id}/credit":                         "sweep",
	"POST /jobs/{id}/inspect":                        "sweep",
	"POST /jobs/{id}/promise":                        "sweep",
	"POST /jobs/{id}/staff":                          "sweep",
	"POST /jobs/{id}/plan":                           "sweep",
	"POST /jobs/{id}/plan/carry":                     "sweep",
	"POST /jobs/{id}/answers/{itemID}":               "sweep",
	"POST /jobs/{id}/car":                            "sweep",
	"POST /jobs/{id}/takeout":                        "sweep",
	"POST /jobs/{id}/told":                           "sweep",
	"POST /jobs/{id}/link":                           "sweep",
	"GET /jobs/{id}/parts":                           "sweep",
	"POST /jobs/{id}/parts":                          "sweep",
	"GET /jobs/{id}/customer":                        "sweep",
	"POST /jobs/{id}/customer":                       "sweep",
	"POST /jobs/{id}/findings":                       "sweep",
	"POST /jobs/{id}/contact-request":                "sweep",
	"POST /jobs/{id}/findings/handled":               "sweep",
	"GET /customers":                                 "",
	"GET /customers/{id}":                            "sweep",
	"POST /customers/{id}/edit":                      "sweep",
	"POST /customers/{id}/people":                    "sweep",
	"POST /customers/{id}/people/{row}/remove":       "sweep",
	"POST /vehicles/{id}/edit":                       "sweep",
	"POST /vehicles/{id}/registration":               "sweep",
	"GET /vehicles/{id}/sell":                        "sweep",
	"GET /customers/{id}/merge":                      "sweep",
	"POST /customers/{id}/merge":                     "sweep",
	"GET /vehicles/{id}/merge":                       "sweep",
	"POST /vehicles/{id}/merge":                      "sweep",
	"POST /vehicles/{id}/sell":                       "sweep",
	"GET /supply":                                    "",
	"POST /supply/orders":                            "",
	"GET /supply/orders/{id}":                        "sweep",
	"POST /supply/orders/{id}/cancel":                "sweep",
	"POST /supply/lines/{line}/eta":                  "sweep",
	"POST /supply/lines/{line}/receive":              "sweep",
	"POST /supply/receipts/{receipt}/accept":         "sweep",
	"POST /supply/receipts/{receipt}/reject":         "sweep",
	"POST /supply/receipts/{receipt}/return":         "sweep",
	"POST /supply/returns/{ret}/returned":            "sweep",
	"POST /supply/returns/{ret}/credit":              "sweep",
	"GET /jobs/{id}/summary":                         "sweep",
	"POST /jobs/{id}/summary":                        "sweep",
	"GET /jobs/{id}/summary/{version}":               "sweep",
	"POST /jobs/{id}/recommendations":                "sweep",
	"POST /jobs/{id}/recommendations/{rec}/withdraw": "sweep",
	"POST /jobs/{id}/recommendations/{rec}/enquiry":  "sweep",
	"GET /k/{token}/summary":                         "token, not an identifier",
	"GET /d/{token}":                                 "token, not an identifier",
	"GET /contracts/{id}":                            "sweep",
	"POST /contracts/{id}/link":                      "sweep",
	"GET /c/{token}":                                 "token, not an identifier",
	"GET /invoices/{id}/followup":                    "sweep",
	"GET /invoices/{id}/einvoice.xml":                "sweep",
	"GET /jobs/{id}/comeback":                        "sweep",
	"POST /jobs/{id}/comeback":                       "sweep",
	"GET /jobs/{id}/outsourced":                      "sweep",
	"POST /jobs/{id}/outsourced":                     "sweep",
	"GET /outsourced":                                "",
	"GET /import":                                    "", "POST /import": "", "GET /import/template/{kind}": "",
	"GET /start": "", "POST /start/step/{step}": "", "GET /start/invoice": "", "POST /start/test-message": "", "GET /help": "", "GET /help/{role}": "",
	"GET /import/{id}": "sweep", "POST /import/{id}": "sweep",
	"POST /invoices/{id}/followup":                "sweep",
	"POST /invoices/{id}/reminder":                "sweep",
	"GET /invoices/{id}/credit":                   "sweep",
	"POST /invoices/{id}/credit":                  "sweep",
	"GET /invoices/{id}/rebill":                   "sweep",
	"POST /invoices/{id}/rebill":                  "sweep",
	"POST /invoices/{id}/send":                    "sweep",
	"POST /invoices/{id}/handover":                "sweep",
	"POST /invoices/{id}/revoke":                  "sweep",
	"GET /jobs/{id}/timeline":                     "sweep",
	"POST /jobs/{id}/notes":                       "sweep",
	"POST /jobs/{id}/notes/{note}/correct":        "sweep",
	"POST /jobs/{id}/tasks":                       "sweep",
	"POST /jobs/{id}/tasks/from-lines":            "sweep",
	"POST /jobs/{id}/tasks/{task}/move":           "sweep",
	"POST /jobs/{id}/tasks/{task}/decision":       "sweep",
	"POST /jobs/{id}/clock-in":                    "sweep",
	"POST /jobs/{id}/clock-out":                   "sweep",
	"GET /invoices/{id}":                          "sweep",
	"GET /exports/{id}":                           "sweep",
	"GET /bookings/{id}":                          "sweep",
	"POST /bookings/{id}":                         "sweep",
	"POST /bookings/{id}/message":                 "sweep",
	"POST /bookings/{id}/contact":                 "sweep",
	"POST /invoices/{id}/payments":                "sweep",
	"POST /payments/{id}/reverse":                 "sweep",
	"POST /payment-flows/{id}/check":              "sweep",
	"POST /payment-flows/{id}/withdraw":           "sweep",
	"POST /outbox/{id}/retry":                     "sweep",
	"POST /outbox/{id}/done":                      "sweep",
	"POST /outbox/{id}/cancel":                    "sweep",
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
	"POST /staff/{id}/planner":                    "sweep",
	"POST /jobs/{id}/message":                     "sweep",
	"POST /staff/{id}/password":                   "sweep",
	"POST /staff/{id}/roles":                      "sweep",
	"POST /stock/parts/{id}":                      "sweep",
	"POST /stock/parts/{id}/active":               "sweep",
	"POST /checklists/{id}":                       "sweep",
	"POST /checklists/{id}/active":                "sweep",
	"POST /checklists/{id}/subjects":              "sweep",

	// Dealer
	"GET /cars/calculations/new": "", "POST /cars/calculations": "",
	"GET /cars/calculations/{id}": "sweep", "POST /cars/calculations/{id}/accept": "sweep",
}

// fill substitutes shop B's identifiers into a pattern.
func fill(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	for from, to := range map[string]string{
		"/jobs/{id}":          "/jobs/" + jobB,
		"/invoices/{id}":      "/invoices/" + invoiceB,
		"/payments/{id}":      "/payments/" + paymentB,
		"/exports/{id}":       "/exports/" + exportB,
		"/bookings/{id}":      "/bookings/" + bookingB,
		"/enquiries/{id}":     "/enquiries/" + enquiryB,
		"/inspections/{id}":   "/inspections/" + inspectionB,
		"/people/{id}":        "/people/" + personB,
		"/vehicles/{id}":      "/vehicles/" + vehicleB,
		"/customers/{id}":     "/customers/" + customerB,
		"/checklists/{id}":    "/checklists/" + checklistB,
		"/stock/parts/{id}":   "/stock/parts/" + partB,
		"/staff/{id}":         "/staff/bbbbbbbb-0000-0000-0000-0000000000bb",
		"{itemID}":            itemB,
		"{position}":          "1",
		"{task}":              taskB,
		"{note}":              noteB,
		"{rec}":               recB,
		"{line}":              lineB,
		"{receipt}":           receiptB,
		"{ret}":               returnB,
		"/supply/orders/{id}": "/supply/orders/" + orderB,
		"/import/{id}":        "/import/" + batchB,
		"/contracts/{id}":     "/contracts/" + contractB,
		// No other shop's call is seeded: an id that is nobody's.
		"/outbox/{id}":        "/outbox/bbbbbbbb-0000-0000-0000-0000000000ee",
		"/payment-flows/{id}": "/payment-flows/bbbbbbbb-0000-0000-0000-0000000000ed",
		"{row}":               "bbbbbbbb-0000-0000-0000-0000000000ff",
		"{version}":           "1",
		"{quoteID}":           "cccccccc-0000-0000-0000-0000000000cc",
		"{key}":               photoB,
		// Dealer: shop A runs no car sales here, so these are not found
		// whoever asks; TestNoCarSalesRouteServesAnotherShopsRow asks them
		// in a shop that does.
		"/cars/calculations/{id}": "/cars/calculations/bbbbbbbb-0000-0000-0000-0000000000ca",
		// Rental: ids that are nobody's.
		"/rental/permits/{id}":     "/rental/permits/bbbbbbbb-0000-0000-0000-0000000000e1",
		"/rental/profiles/{id}":    "/rental/profiles/bbbbbbbb-0000-0000-0000-0000000000e2",
		"/rental/units/{id}":       "/rental/units/bbbbbbbb-0000-0000-0000-0000000000e3",
		"/rental/memberships/{id}": "/rental/memberships/bbbbbbbb-0000-0000-0000-0000000000e4",
		"/rental/plans/{id}":       "/rental/plans/bbbbbbbb-0000-0000-0000-0000000000e5",
		"/rental/extras/{id}":      "/rental/extras/bbbbbbbb-0000-0000-0000-0000000000e6",
		"/rental/bookings/{id}":    "/rental/bookings/bbbbbbbb-0000-0000-0000-0000000000e7",
		"{action}":                 "confirm",
		"/rental/drivers/{id}":     "/rental/drivers/bbbbbbbb-0000-0000-0000-0000000000e8",
		"/rental/agreements/{id}":  "/rental/agreements/bbbbbbbb-0000-0000-0000-0000000000e9",
		"/rental/handovers/{id}":   "/rental/handovers/bbbbbbbb-0000-0000-0000-0000000000ea",
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
