package workshop_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The stock feed (#183) is built from an allow-list: the asking price, the
// description, the photographs' public ids -- and never what the car cost,
// its costs, its seller or anything a person wrote about it inside. Sold
// or reserved, it leaves; behind a token, only the token reads it.
func TestTheStockFeedShowsOnlyWhatIsForTheWebsite(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCost{Kind: workshop.CostTransport,
		NetMinor: 345_678, VATRateBasis: 2500, IncurredOn: time.Now(), Note: "Hemlig anteckning"}); err != nil {
		t.Fatal(err)
	}
	price := int64(12_990_000)
	if err := workshop.SetListing(ctx, pool, sales(), p.StockUnitID, workshop.Listing{AskingPriceMinor: &price, Variant: "D4 Momentum",
		Fuel: "Diesel", Equipment: []string{"Dragkrok", "Värmare"}, Description: "Välvårdad"}); err != nil {
		t.Fatal(err)
	}
	first, err := workshop.AttachStockPhoto(ctx, pool, sales(), p.StockUnitID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "image/jpeg", 10, "a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	second, err := workshop.AttachStockPhoto(ctx, pool, sales(), p.StockUnitID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "image/jpeg", 10, "b.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.MakeCover(ctx, pool, sales(), second.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.StockFeed(ctx, pool, shopID, "", time.Time{}); !errors.Is(err, workshop.ErrFeedClosed) {
		t.Fatalf("a feed nobody published: %v", err)
	}
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, false, false, "token-one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.StockFeed(ctx, pool, shopID, "wrong", time.Time{}); !errors.Is(err, workshop.ErrFeedClosed) {
		t.Errorf("read with the wrong token: %v", err)
	}
	// Still in preparation: not on the site.
	if cars, _, err := workshop.StockFeed(ctx, pool, shopID, "token-one", time.Time{}); err != nil || len(cars) != 0 {
		t.Errorf("a car in preparation in the feed: %+v, %v", cars, err)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	cars, _, err := workshop.StockFeed(ctx, pool, shopID, "token-one", time.Time{})
	if err != nil || len(cars) != 1 {
		t.Fatalf("the feed: %+v, %v", cars, err)
	}
	c := cars[0]
	if c.PriceIncludingVAT == nil || *c.PriceIncludingVAT != price || c.Variant != "D4 Momentum" || len(c.Photos) != 2 ||
		c.Photos[0] != second.ID || c.Photos[1] != first.ID {
		t.Errorf("the car in the feed: %+v", c)
	}
	body, _ := json.Marshal(cars)
	for _, secret := range []string{"8500000", "85000", "345678", "Hemlig", "Sam", "Säljare", p.ID, "aaaaaaaa"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the feed carries %q:\n%s", secret, body)
		}
	}
	if key, err := workshop.FeedPhoto(ctx, pool, shopID, "token-one", first.ID); err != nil || key == "" {
		t.Errorf("a photograph in the feed: %q, %v", key, err)
	}
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, false, false, "token-two"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.StockFeed(ctx, pool, shopID, "token-one", time.Time{}); !errors.Is(err, workshop.ErrFeedClosed) {
		t.Errorf("a replaced token still reads the feed: %v", err)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockReserved)
	if cars, _, _ := workshop.StockFeed(ctx, pool, shopID, "token-two", time.Time{}); len(cars) != 0 {
		t.Errorf("a reserved car in a feed that leaves them out: %+v", cars)
	}
	if _, err := workshop.FeedPhoto(ctx, pool, shopID, "token-two", first.ID); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a reserved car's photograph served: %v", err)
	}
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, true, true, ""); err != nil {
		t.Fatal(err)
	}
	if cars, _, _ := workshop.StockFeed(ctx, pool, shopID, "", time.Time{}); len(cars) != 1 || !cars[0].Reserved {
		t.Errorf("an open feed with reserved cars: %+v", cars)
	}
}
