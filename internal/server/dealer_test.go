package server

import (
	"context"
	"net/http"
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
