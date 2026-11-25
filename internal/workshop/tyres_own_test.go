package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A car the shop controls -- in its stock or its fleet, which is to say with a
// resource -- can have sets that belong to the shop itself, kept like any other,
// and a car it does not control cannot. The shop is one customer however many
// sets it has.
func TestTheShopsOwnSetsGoOnItsOwnCarsAndBelongToOneCustomer(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	fleet := anotherCar(t, pool, "")
	if _, err := workshop.CreateResource(ctx, pool, rentalDesk(), workshop.NewResource{VehicleID: fleet}); err != nil {
		t.Fatal(err)
	}
	stranger := anotherCar(t, pool, "")

	n := summerSet("", fleet)
	if _, err := workshop.CreateShopTyreSet(ctx, pool, advisor(), n); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk made the shop's own set: %v", err)
	}
	if _, err := workshop.CreateShopTyreSet(ctx, pool, technician(), n); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician made the shop's own set: %v", err)
	}
	if _, err := workshop.CreateShopTyreSet(ctx, pool, owner(), summerSet("", stranger)); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set on a car the shop does not control: %v", err)
	}
	first, err := workshop.CreateShopTyreSet(ctx, pool, rentalDesk(), n)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workshop.CreateShopTyreSet(ctx, pool, owner(), summerSet("", ""))
	if err != nil {
		t.Fatal(err)
	}
	var owners, customers int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT customer_id) FROM tyre_sets WHERE id IN ($1, $2)`, first, second).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM shop_own_customers`).Scan(&customers); err != nil {
		t.Fatal(err)
	}
	if owners != 1 || customers != 1 {
		t.Errorf("the shop is %d customers across its sets and %d in the register", owners, customers)
	}
	sets, err := workshop.ShopTyreSets(ctx, pool, sales(), fleet)
	if err != nil || len(sets) != 1 || sets[0].ID != first || sets[0].Owner != "" {
		t.Errorf("the car's sets as car sales sees them: %+v, %v", sets, err)
	}
	off := sales()
	off.Modules = []access.Module{access.Dealer}
	if _, err := workshop.ShopTyreSets(ctx, pool, off, fleet); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("the car's sets with the workshop switched off: %v", err)
	}
	if _, err := workshop.CreateShopTyreSet(ctx, pool, func() access.Scope { s := owner(); s.Modules = []access.Module{access.Dealer}; return s }(), n); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a set made with the workshop switched off: %v", err)
	}
}

// The shop writes to itself and bills itself for nothing: its sets are left out
// of campaigns, of storage fees and of the list of sets nobody has come for.
func TestTheShopsOwnSetsAreNotWrittenToBilledOrCalledLeft(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 5)
	fleet := anotherCar(t, pool, "")
	if _, err := workshop.CreateResource(ctx, pool, rentalDesk(), workshop.NewResource{VehicleID: fleet}); err != nil {
		t.Fatal(err)
	}
	set, err := workshop.CreateShopTyreSet(ctx, pool, owner(), summerSet("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), set, place); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_set_placements SET placed_at = clock_timestamp() - interval '500 days'`); err != nil {
		t.Fatal(err)
	}
	// A customer's set beside it, so the lists are not empty for another reason.
	theirs := aStoredSet(t, pool, customerA, "", workshop.SeasonSummer, place)
	campaign, _ := workshop.CreateCampaign(ctx, pool, owner(), workshop.NewCampaign{Name: "Spring", Season: "summer", Body: "{sets}"})
	_, aud, err := workshop.CampaignPreview(ctx, pool, advisor(), campaign)
	if err != nil || len(aud.Reached)+aud.NoContact+aud.OptedOut != 1 {
		t.Errorf("the campaign counts the shop: %+v, %v", aud, err)
	}
	fee, _ := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{Name: "Storage", Per: "set", LineKind: "fee", UnitPriceMinor: 100, VATRateBasis: 2500, StoragePeriod: "year"})
	due, err := workshop.StorageDueFor(ctx, pool, advisor(), fee, 2027)
	if err != nil || len(due) != 1 || due[0].CustomerID != customerA {
		t.Errorf("who is due storage: %+v, %v", due, err)
	}
	var own string
	if err := pool.QueryRow(ctx, `SELECT customer_id::text FROM shop_own_customers`).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.InvoiceStorage(ctx, pool, advisor(), own, fee, 2027); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("the shop was invoiced for storing its own set: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_set_placements SET placed_at = clock_timestamp() - interval '500 days' WHERE tyre_set_id = $1`, theirs); err != nil {
		t.Fatal(err)
	}
	list, err := workshop.AbandonedSets(ctx, pool, advisor())
	if err != nil || len(list) != 1 || list[0].SetID != theirs {
		t.Errorf("the sets that may have been left: %+v, %v", list, err)
	}
	_ = time.Now
}
