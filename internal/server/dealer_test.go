package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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
	const calcB, leadB = "bbbbbbbb-0000-0000-0000-0000000000ca", "bbbbbbbb-0000-0000-0000-0000000000cb"
	const unitB = "bbbbbbbb-0000-0000-0000-0000000000cf"
	seedShop(t, pool, shopB, []string{
		`INSERT INTO purchase_calculations (id, shop_id, calculation_id, version, vehicle_id, note)
		 VALUES ('` + calcB + `','` + shopB + `','` + calcB + `',1,'` + vehicleB + `','` + secretB + `')`,
		`INSERT INTO leads (id, shop_id, contact_name, contact_email, wishes, channel)
		 VALUES ('` + leadB + `','` + shopB + `','` + secretB + `','b@example.test','` + secretB + `','phone')`,
		`INSERT INTO lead_events (shop_id, lead_id, kind, status) VALUES ('` + shopB + `','` + leadB + `','status','new')`,
		`INSERT INTO resources (id, shop_id, kind, vehicle_id) VALUES ('bbbbbbbb-0000-0000-0000-0000000000d1','` + shopB + `','vehicle','` + vehicleB + `')`,
		`INSERT INTO stock_units (id, shop_id, vehicle_id, resource_id, reason)
		 VALUES ('` + unitB + `','` + shopB + `','` + vehicleB + `','bbbbbbbb-0000-0000-0000-0000000000d1','defleet')`,
	})
	for _, email := range []string{ownerEmail, buyerEmail, salesEmail, techEmail} {
		c := signIn(t, ts, email)
		for _, try := range []struct{ method, path string }{
			{http.MethodGet, "/cars/calculations/" + calcB},
			{http.MethodPost, "/cars/calculations/" + calcB + "/accept"},
			{http.MethodGet, "/cars/leads/" + leadB},
			{http.MethodPost, "/cars/leads/" + leadB + "/contacts"},
			{http.MethodPost, "/cars/leads/" + leadB + "/status"},
			{http.MethodPost, "/cars/leads/" + leadB + "/assign"},
			{http.MethodPost, "/cars/leads/" + leadB + "/next"},
			{http.MethodGet, "/cars/stock/" + unitB},
			{http.MethodPost, "/cars/stock/" + unitB + "/move"},
			{http.MethodPost, "/cars/stock/" + unitB + "/checks"},
			{http.MethodPost, "/cars/stock/" + unitB + "/costs"},
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

// A version edited with something that does not read as a number comes
// back as the same version, still an edit of that calculation, with what
// was typed: not as a new calculation asking for the car again.
func TestAnEditWithAMistakeStaysAnEditOfTheCalculation(t *testing.T) {
	ts, pool := testServer(t)
	dealerShop(t, pool)
	buyer := signIn(t, ts, buyerEmail)
	resp := postResponse(t, buyer, ts.URL+"/cars/calculations", url.Values{"registration": {"KLM 456"}, "sale": {"125000"}})
	where := resp.Header.Get("Location")
	id := strings.TrimPrefix(where, "/cars/calculations/")
	c, err := workshop.CalculationByID(context.Background(), pool, dealerShopOwner(), id)
	if err != nil {
		t.Fatal(err)
	}
	body := post(t, buyer, ts.URL+"/cars/calculations", url.Values{"calculation": {c.CalculationID}, "version": {"1"},
		"vehicle": {c.VehicleID}, "from": {id}, "sale": {"abc"}})
	if !strings.Contains(body, `name="calculation" value="`+c.CalculationID+`"`) || strings.Contains(body, `name="registration"`) ||
		!strings.Contains(body, `value="abc"`) {
		t.Errorf("the edit came back as a new calculation, or lost what was typed:\n%s", visibleText(body))
	}
}

// The stock list (#168) shows the car to whoever buys or sells cars, and
// what it was bought for only to whoever may see it: the salesperson's
// response holds no purchase price, not just a page that hides one.
func TestTheStockListShowsThePurchasePriceOnlyToTheBuyer(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	seller, err := workshop.CreateParty(ctx, pool, owner, workshop.NewParty{Kind: "private", Name: "Sam Säljare", Role: workshop.PartySeller})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordPurchase(ctx, pool, owner, workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase,
		SellerPartyID: seller, PurchasedOn: time.Now(), PriceMinor: 8_543_210, SellerType: workshop.SellerPrivate}); err != nil {
		t.Fatal(err)
	}
	price := "85,432.10"
	buyer := strings.ReplaceAll(visibleText(get(t, signIn(t, ts, buyerEmail), ts.URL+"/cars/stock", http.StatusOK)), " ", " ")
	if !strings.Contains(buyer, "0 days in stock") || !strings.Contains(buyer, price) || !strings.Contains(buyer, "Stock value") {
		t.Errorf("the buyer's stock list:\n%s", buyer)
	}
	sales := get(t, signIn(t, ts, salesEmail), ts.URL+"/cars/stock", http.StatusOK)
	if !strings.Contains(sales, "0 days in stock") || strings.Contains(sales, "85,432") || strings.Contains(sales, "Bought for") ||
		strings.Contains(sales, "Stock value") {
		t.Errorf("the salesperson's stock list:\n%s", visibleText(sales))
	}
	if status, _ := getBody(t, signIn(t, ts, techEmail), ts.URL+"/cars/stock"); status != http.StatusForbidden {
		t.Errorf("a technician's stock list answered %d, want 403", status)
	}
}

// A buyer records a car bought on the page: the car by its plate, the
// seller typed with it, what was paid. It is in stock at once. Refused --
// no seller named -- the form comes back as typed and nobody is kept. A
// salesperson is not given the form.
func TestABuyerRecordsACarBoughtOnThePage(t *testing.T) {
	ts, pool := testServer(t)
	dealerShop(t, pool)
	buyer := signIn(t, ts, buyerEmail)
	form := url.Values{"registration": {"KLM 456"}, "make": {"Volvo"}, "model": {"V70"}, "reason": {workshop.StockFromPurchase},
		"seller_kind": {"private"}, "seller_name": {""}, "seller_phone": {"070-111 22 33"}, "seller_type": {workshop.SellerPrivate},
		"purchased_on": {time.Now().Format(time.DateOnly)}, "odometer": {"154 000"}, "price": {"72 500"}, "key": {"buy-page-1"}}
	refused := post(t, buyer, ts.URL+"/cars/stock", form)
	if !strings.Contains(refused, "a party needs a name") || !strings.Contains(refused, "070-111 22 33") {
		t.Errorf("a purchase with no seller named:\n%s", visibleText(refused))
	}
	var people int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM people WHERE phone = '070-111 22 33'`).Scan(&people); err != nil {
		t.Fatal(err)
	}
	if people != 0 {
		t.Errorf("a refused purchase kept %d people", people)
	}
	form.Set("seller_name", "Sam Säljare")
	if resp := postResponse(t, buyer, ts.URL+"/cars/stock", form); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/cars/stock" {
		t.Fatalf("recording the purchase answered %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	list := strings.ReplaceAll(visibleText(get(t, buyer, ts.URL+"/cars/stock", http.StatusOK)), " ", " ")
	if !strings.Contains(list, "KLM 456") || !strings.Contains(list, "Volvo V70") || !strings.Contains(list, "72,500.00") ||
		!strings.Contains(list, "154000 km") {
		t.Errorf("the stock list after the purchase:\n%s", list)
	}
	if status, _ := getBody(t, signIn(t, ts, salesEmail), ts.URL+"/cars/stock/new"); status != http.StatusForbidden {
		t.Errorf("a salesperson was given the purchase form: %d", status)
	}
}

// One car in stock, worked on its page: the buyer sees what it was bought
// for, records a check and a cost and offers it for sale; the salesperson
// sees the same car with no figure and may reserve it, not take it back
// into preparation.
func TestACarInStockIsWorkedOnItsPage(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	p, err := workshop.RecordPurchase(ctx, pool, owner, workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase,
		NewSeller: &workshop.NewParty{Kind: "private", Name: "Sam Säljare"}, PurchasedOn: time.Now(), PriceMinor: 8_543_210,
		SellerType: workshop.SellerPrivate})
	if err != nil {
		t.Fatal(err)
	}
	page := ts.URL + "/cars/stock/" + p.StockUnitID
	buyer := signIn(t, ts, buyerEmail)
	text := strings.ReplaceAll(visibleText(get(t, buyer, page, http.StatusOK)), "\u00a0", " ")
	for _, want := range []string{"85,432.10", "Checks before money leaves", "Costs", "For sale"} {
		if !strings.Contains(text, want) {
			t.Errorf("the buyer's page does not show %q:\n%s", want, text)
		}
	}
	if status := postStatus(t, buyer, page+"/checks", url.Values{"kind": {workshop.CheckDrivingBan}, "result": {"pass"}}); status != http.StatusSeeOther {
		t.Errorf("recording a check answered %d", status)
	}
	if status := postStatus(t, buyer, page+"/costs", url.Values{"cost_kind": {workshop.CostTransport}, "net": {"1 234,50"}, "rate": {"25"},
		"incurred_on": {time.Now().Format(time.DateOnly)}}); status != http.StatusSeeOther {
		t.Errorf("recording a cost answered %d", status)
	}
	if status := postStatus(t, buyer, page+"/move", url.Values{"to": {string(workshop.StockForSale)}}); status != http.StatusSeeOther {
		t.Errorf("offering it for sale answered %d", status)
	}
	text = strings.ReplaceAll(visibleText(get(t, buyer, page, http.StatusOK)), "\u00a0", " ")
	if !strings.Contains(text, "1,234.50") || !strings.Contains(text, "No driving ban") {
		t.Errorf("the buyer's page after the check and the cost:\n%s", text)
	}
	sales := get(t, signIn(t, ts, salesEmail), page, http.StatusOK)
	if strings.Contains(sales, "85,432") || strings.Contains(sales, "1,234") || strings.Contains(sales, "Checks before money leaves") ||
		!strings.Contains(sales, `value="reserved"`) || strings.Contains(sales, `value="in_preparation"`) {
		t.Errorf("the salesperson's page:\n%s", visibleText(sales))
	}
}

// A salesperson reserves a car on its page for a customer typed there,
// takes a deposit, extends the reservation and cancels it with the deposit
// to be refunded; the car is for sale again (#174).
func TestASalespersonReservesACarOnItsPage(t *testing.T) {
	ts, pool := testServer(t)
	owner := dealerShop(t, pool)
	ctx := context.Background()
	p, err := workshop.RecordPurchase(ctx, pool, owner, workshop.NewPurchase{VehicleID: vehicleA, Reason: workshop.StockFromPurchase,
		NewSeller: &workshop.NewParty{Kind: "private", Name: "Sam Säljare"}, PurchasedOn: time.Now(), PriceMinor: 8_500_000,
		SellerType: workshop.SellerPrivate})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MoveStockUnit(ctx, pool, owner, p.StockUnitID, workshop.StockForSale, ""); err != nil {
		t.Fatal(err)
	}
	page := ts.URL + "/cars/stock/" + p.StockUnitID
	seller := signIn(t, ts, salesEmail)
	until := time.Now().Add(72 * time.Hour).Format("2006-01-02T15:04")
	if status := postStatus(t, seller, page+"/reserve", url.Values{"holder_kind": {"private"}, "holder_name": {"Kim Köpare"},
		"holder_phone": {"070-765 43 21"}, "until": {until}, "refundable": {"yes"}, "key": {"res-page-1"}}); status != http.StatusSeeOther {
		t.Fatalf("reserving answered %d", status)
	}
	rs, err := workshop.ReservationsOfUnit(ctx, pool, owner, p.StockUnitID)
	if err != nil || len(rs) != 1 {
		t.Fatalf("the reservations: %+v, %v", rs, err)
	}
	res := page + "/reservations/" + rs[0].ID
	if status := postStatus(t, seller, res+"/deposit", url.Values{"amount": {"5 000"}, "method": {"swish"},
		"paid_on": {time.Now().Format(time.DateOnly)}, "key": {"dep-page-1"}}); status != http.StatusSeeOther {
		t.Errorf("recording the deposit answered %d", status)
	}
	text := strings.ReplaceAll(visibleText(get(t, seller, page, http.StatusOK)), " ", " ")
	if !strings.Contains(text, "Reserved for Kim Köpare") || !strings.Contains(text, "Deposit held SEK 5,000.00") {
		t.Errorf("the reserved car's page:\n%s", text)
	}
	if status := postStatus(t, seller, res+"/extend", url.Values{"until": {time.Now().Add(120 * time.Hour).Format("2006-01-02T15:04")}}); status != http.StatusSeeOther {
		t.Errorf("extending answered %d", status)
	}
	rs, _ = workshop.ReservationsOfUnit(ctx, pool, owner, p.StockUnitID)
	if status := postStatus(t, seller, page+"/reservations/"+rs[0].ID+"/end", url.Values{"how": {"cancelled"}, "deposit": {"refund"},
		"end_reason": {"Banken sa nej"}}); status != http.StatusSeeOther {
		t.Errorf("cancelling answered %d", status)
	}
	text = visibleText(get(t, seller, page, http.StatusOK))
	if !strings.Contains(text, "Reserve the car") || !strings.Contains(text, "Banken sa nej") {
		t.Errorf("the car's page after the reservation was cancelled:\n%s", text)
	}
	if strings.Contains(get(t, signIn(t, ts, buyerEmail), page, http.StatusOK), "Reserve the car") {
		t.Error("the buyer was offered to reserve the car")
	}
}
