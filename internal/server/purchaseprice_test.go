package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
)

const (
	salesEmail = "sales@shop-a.test"
	partA      = "aaaaaaaa-0000-0000-0000-0000000000a6"
)

// What the business paid is never put in front of a salesperson (#145): not
// on a page, a fragment, an export or a search, asked for directly. Today
// the purchase prices FreeSMS holds are the workshop's parts costs; a
// dealer's stock prices will be read the same way, under SeesPurchasePrice.
// The owner is asked first, as the control: the figure is there to leak.
func TestASalespersonIsHandedNoPurchasePriceByAnyRoute(t *testing.T) {
	ts, pool := testServer(t)
	hash, err := auth.HashPassword(techPass)
	if err != nil {
		t.Fatal(err)
	}
	// A shop that runs the workshop and car sales -- written as rows,
	// because this release offers no switch for car sales yet -- with a
	// salesperson, and a part bought in at 4 711,23.
	seedShop(t, pool, shopA, []string{
		`INSERT INTO shop_module_changes (shop_id, module, enabled) VALUES
		 ('` + shopA + `','workshop',true), ('` + shopA + `','dealer',true)`,
		`INSERT INTO people (id, shop_id, display_name, email) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000a1','` + shopA + `','A Salesperson','` + salesEmail + `')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000a2','` + shopA + `',
		  'aaaaaaaa-0000-0000-0000-0000000000a1','sales','` + hash + `')`,
		`INSERT INTO parts (id, shop_id, number, name, cost_minor, price_minor) VALUES
		 ('` + partA + `','` + shopA + `','COST-1','Costly part', 471123, 900000)`,
	})
	cost := []string{"4 711,23", "4 711,23", "4 711,23", "4711,23", "4711.23", "471123"}
	leaks := func(body string) bool {
		for _, c := range cost {
			if strings.Contains(body, c) {
				return true
			}
		}
		return false
	}
	get := func(client *http.Client, path string) (int, string) {
		c := *client
		c.Timeout = 10 * time.Second
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if _, body := get(signIn(t, ts, ownerEmail), "/stock/parts/"+partA); !leaks(body) {
		t.Fatal("the owner's page for the part does not show its cost; the sweep below would prove nothing")
	}

	sales := signIn(t, ts, salesEmail)
	swept := 0
	for _, pattern := range registeredRoutes(t) {
		method, path, _ := strings.Cut(pattern, " ")
		// The live stream says only that something changed, never what
		// (and never ends, so it is not read here).
		if method != http.MethodGet || path == "/events" {
			continue
		}
		for from, to := range map[string]string{
			"/stock/parts/{id}": "/stock/parts/" + partA,
			"/jobs/{id}":        "/jobs/aaaaaaaa-0000-0000-0000-00000000000f",
			"/customers/{id}":   "/customers/aaaaaaaa-0000-0000-0000-000000000004",
			"/vehicles/{id}":    "/vehicles/aaaaaaaa-0000-0000-0000-000000000005",
		} {
			path = strings.ReplaceAll(path, from, to)
		}
		_, path = fill(method + " " + path)
		for _, p := range []string{"{token}", "{secret}", "{kind}", "{step}", "{role}", "{file...}", "{name}"} {
			path = strings.ReplaceAll(path, p, "x")
		}
		for _, q := range []string{"", "?q=COST-1", "?code=COST-1"} {
			if _, body := get(sales, path+q); leaks(body) {
				t.Errorf("GET %s%s handed a salesperson the part's cost", path, q)
			}
		}
		swept++
	}
	if swept < 50 {
		t.Errorf("only %d GET routes swept", swept)
	}
}
