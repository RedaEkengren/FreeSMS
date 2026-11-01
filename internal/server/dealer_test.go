package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Car sales through the pages (M7).

const vehicleA = "aaaaaaaa-0000-0000-0000-000000000005"

// dealerShop makes shop A run car sales as well as the workshop, with a
// salesperson (salesEmail) and a buyer (buyerEmail), and gives its car a
// chassis number so it can come into stock. It returns the owner's scope,
// for making what a test needs without its pages.
func dealerShop(t *testing.T, pool *pgxpool.Pool) access.Scope {
	t.Helper()
	hash, err := auth.HashPassword(techPass)
	if err != nil {
		t.Fatal(err)
	}
	seedShop(t, pool, shopA, []string{
		`INSERT INTO shop_module_changes (shop_id, module, enabled) VALUES
		 ('` + shopA + `','workshop',true), ('` + shopA + `','dealer',true)`,
		`INSERT INTO people (id, shop_id, display_name, email) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000a1','` + shopA + `','A Salesperson','` + salesEmail + `'),
		 ('aaaaaaaa-0000-0000-0000-0000000000a3','` + shopA + `','A Buyer','` + buyerEmail + `')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000a2','` + shopA + `','aaaaaaaa-0000-0000-0000-0000000000a1','sales','` + hash + `'),
		 ('aaaaaaaa-0000-0000-0000-0000000000a4','` + shopA + `','aaaaaaaa-0000-0000-0000-0000000000a3','buyer','` + hash + `')`,
		`UPDATE vehicles SET vin = 'YV1SW61R021234567' WHERE id = '` + vehicleA + `'`,
	})
	return access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-000000000011",
		Role: access.Of(access.RoleOwner), Modules: access.Modules}
}

const buyerEmail = "buyer@shop-a.test"

// The vehicle page shows a car's cycles through stock -- both, for a car
// bought twice -- to whoever buys or sells cars, and not to the workshop.
func TestTheVehiclePageShowsEveryCycleThroughStock(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	first, err := workshop.TakeIntoStock(ctx, pool, owner, vehicleA, workshop.StockFromDefleet, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, owner, first.ID, workshop.StockWrittenOff, "Fel bil"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.TakeIntoStock(ctx, pool, owner, vehicleA, workshop.StockFromDefleet, ""); err != nil {
		t.Fatal(err)
	}
	page := get(t, signIn(t, ts, salesEmail), ts.URL+"/vehicles/"+vehicleA, http.StatusOK)
	if strings.Count(page, "From the rental fleet") != 2 || !strings.Contains(page, "Written off (scrapped or lost)") ||
		!strings.Contains(page, "In preparation") {
		t.Errorf("the salesperson's vehicle page does not show both cycles:\n%s", visibleText(page))
	}
	if page := get(t, signIn(t, ts, techEmail), ts.URL+"/vehicles/"+vehicleA, http.StatusOK); strings.Contains(page, "From the rental fleet") {
		t.Error("a technician was shown the car's cycles through stock")
	}
}

// A buyer works out a bid on a phone at a viewing: the car by its plate,
// what it should sell for and cost; the page shows the most to pay under
// each way of taxing the sale. Saved again, it is the next version; the bid
// is recorded on the version it was made on.
func TestABuyerWorksOutABidOnThePage(t *testing.T) {
	ts, pool := testServer(t)
	dealerShop(t, pool)
	buyer := signIn(t, ts, buyerEmail)
	form := url.Values{"registration": {"KLM 456"}, "sale": {"125 000"}, "seller_type": {workshop.SellerPrivate},
		"cost_reconditioning": {"6000"}, "cost_transport": {"4000"}, "days": {"30"}, "per_day": {"100"}, "margin": {"15000"}}
	resp := postResponse(t, buyer, ts.URL+"/cars/calculations", form)
	if resp.StatusCode != http.StatusSeeOther {
		_, b := getBody(t, buyer, ts.URL+"/cars/calculations/new")
		t.Fatalf("saving the calculation answered %d; the form: %s", resp.StatusCode, visibleText(b))
	}
	where := resp.Header.Get("Location")
	page := get(t, buyer, ts.URL+where, http.StatusOK)
	// Amounts are grouped with a non-breaking space.
	text := strings.ReplaceAll(visibleText(page), "\u00a0", " ")
	for _, want := range []string{"KLM 456", "Version 1 of 1", "SEK 72,000.00", "SEK 90,000.00", "SEK 7,000.00"} {
		if !strings.Contains(text, want) {
			t.Errorf("the calculation's page does not show %q:\n%s", want, text)
		}
	}
	id := strings.TrimPrefix(where, "/cars/calculations/")
	c, err := workshop.CalculationByID(context.Background(), pool, dealerShopOwner(), id)
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{"calculation": {c.CalculationID}, "version": {"1"}, "vehicle": {c.VehicleID}, "from": {id},
		"sale": {"125000"}, "seller_type": {workshop.SellerPrivate}, "margin": {"12 %"}}
	resp = postResponse(t, buyer, ts.URL+"/cars/calculations", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the next version answered %d", resp.StatusCode)
	}
	second := resp.Header.Get("Location")
	if page := visibleText(get(t, buyer, ts.URL+second, http.StatusOK)); !strings.Contains(page, "Version 2 of 2") {
		t.Errorf("the second version's page:\n%s", page)
	}
	if body := post(t, buyer, ts.URL+"/cars/calculations", form); !strings.Contains(visibleText(body), "a later version of this calculation was saved meanwhile") {
		t.Errorf("an edit from an old version was not refused:\n%s", visibleText(body))
	}
	if status := postStatus(t, buyer, ts.URL+second+"/accept", url.Values{"bid": {"85000"}}); status != http.StatusSeeOther {
		t.Errorf("recording the bid answered %d", status)
	}
	if page := strings.ReplaceAll(visibleText(get(t, buyer, ts.URL+second, http.StatusOK)), "\u00a0", " "); !strings.Contains(page, "Bid SEK 85,000.00 on this version") {
		t.Errorf("the bid is not on the version:\n%s", page)
	}

	// A salesperson is refused, a page and the save alike; so is the
	// workshop's front desk.
	for _, email := range []string{salesEmail, advisorEmail} {
		c := signIn(t, ts, email)
		if status := getStatus(t, c, ts.URL+second); status != http.StatusForbidden {
			t.Errorf("%s reading a calculation: %d, want 403", email, status)
		}
		if status := postStatus(t, c, ts.URL+"/cars/calculations", url.Values{"registration": {"KLM 456"}}); status != http.StatusForbidden {
			t.Errorf("%s saving a calculation: %d, want 403", email, status)
		}
	}
}

// dealerShopOwner is the owner's scope in a shop that runs car sales.
func dealerShopOwner() access.Scope {
	return access.Scope{ShopID: shopA, UserID: "aaaaaaaa-0000-0000-0000-000000000011", Role: access.Of(access.RoleOwner), Modules: access.Modules}
}

// In a shop that sells cars, another shop's calculation is not found by
// any of this shop's roles, and is never shown.
func TestNoCarSalesRouteServesAnotherShopsRow(t *testing.T) {
	ts, pool := testServer(t)
	dealerShop(t, pool)
	const calcB = "bbbbbbbb-0000-0000-0000-0000000000ca"
	seedShop(t, pool, shopB, []string{
		`INSERT INTO purchase_calculations (id, shop_id, calculation_id, version, vehicle_id, note)
		 VALUES ('` + calcB + `','` + shopB + `','` + calcB + `',1,'` + vehicleB + `','` + secretB + `')`,
	})
	for _, email := range []string{ownerEmail, buyerEmail, salesEmail, techEmail} {
		c := signIn(t, ts, email)
		for _, try := range []struct{ method, path string }{
			{http.MethodGet, "/cars/calculations/" + calcB},
			{http.MethodPost, "/cars/calculations/" + calcB + "/accept"},
		} {
			var status int
			var body string
			if try.method == http.MethodGet {
				status, body = getBody(t, c, ts.URL+try.path)
			} else {
				resp := postResponse(t, c, ts.URL+try.path, url.Values{"bid": {"1"}})
				status = resp.StatusCode
			}
			if status == http.StatusOK || (status >= 300 && status < 400) || strings.Contains(body, secretB) {
				t.Errorf("%s %s as %s: %d", try.method, try.path, email, status)
			}
		}
	}
}

// With car sales switched off, its pages are not there at all.
func TestCarSalesPagesAreNotThereWhenTheShopDoesNotSellCars(t *testing.T) {
	ts, _ := testServer(t)
	owner := signIn(t, ts, ownerEmail)
	if status := getStatus(t, owner, ts.URL+"/cars/calculations/new"); status != http.StatusNotFound {
		t.Errorf("the calculation form in a workshop-only shop: %d, want 404", status)
	}
	if page := get(t, owner, ts.URL+"/", http.StatusOK); strings.Contains(page, "/cars/calculations/new") {
		t.Error("the menu offers car sales in a shop that does not sell cars")
	}
}

// postResponse posts form and hands back the response itself, not
// following a redirect: where a save sends the browser is what is checked.
func postResponse(t *testing.T, c *http.Client, target string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.URL.Host)
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// getStatus is a GET's status and body, whatever the status.
func getStatus(t *testing.T, c *http.Client, target string) int {
	status, _ := getBody(t, c, target)
	return status
}

func getBody(t *testing.T, c *http.Client, target string) (int, string) {
	t.Helper()
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
