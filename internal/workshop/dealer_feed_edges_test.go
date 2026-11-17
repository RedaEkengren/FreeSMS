package workshop_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The edges of the stock feed (#183).

type gonePhotos struct{ keys []string }

func (g *gonePhotos) Remove(key string) error { g.keys = append(g.keys, key); return nil }

// A site that asks for what changed since it last looked is told of a
// photograph removed or moved to the front as well as of a new price: a
// removal leaves no row behind, so it has a row of its own.
func TestTheStockFeedReportsAPhotographRemovedOrMovedSince(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	price := int64(12_990_000)
	if err := workshop.SetListing(ctx, pool, sales(), p.StockUnitID, workshop.Listing{AskingPriceMinor: &price}); err != nil {
		t.Fatal(err)
	}
	var photos []workshop.StockPhoto
	for _, k := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc"} {
		ph, err := workshop.AttachStockPhoto(ctx, pool, sales(), p.StockUnitID, k, "image/jpeg", 10, k[:1]+".jpg")
		if err != nil {
			t.Fatal(err)
		}
		photos = append(photos, ph)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, true, false, ""); err != nil {
		t.Fatal(err)
	}
	read := func(since time.Time) ([]workshop.FeedCar, time.Time) {
		t.Helper()
		cars, newest, err := workshop.StockFeed(ctx, pool, shopID, "", since)
		if err != nil {
			t.Fatal(err)
		}
		return cars, newest
	}
	_, seen := read(time.Time{})
	if cars, _ := read(seen); len(cars) != 0 {
		t.Fatalf("a car nothing has happened to since: %+v", cars)
	}

	if err := workshop.MakeCover(ctx, pool, sales(), photos[2].ID); err != nil {
		t.Fatal(err)
	}
	cars, seen := read(seen)
	if len(cars) != 1 || len(cars[0].Photos) != 3 || cars[0].Photos[0] != photos[2].ID {
		t.Fatalf("a new cover since the last look: %+v", cars)
	}
	if cars, _ := read(seen); len(cars) != 0 {
		t.Fatalf("the same car again with nothing new: %+v", cars)
	}

	gone := &gonePhotos{}
	if err := workshop.RemoveStockPhoto(ctx, pool, sales(), gone, photos[0].ID); err != nil {
		t.Fatal(err)
	}
	cars, _ = read(seen)
	if len(cars) != 1 || len(cars[0].Photos) != 2 || slices.Contains(cars[0].Photos, photos[0].ID) {
		t.Errorf("a photograph removed since the last look: %+v", cars)
	}
	// And what was removed is no longer served.
	if _, err := workshop.FeedPhoto(ctx, pool, shopID, "", photos[0].ID); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a removed photograph served: %v", err)
	}
	if len(gone.keys) != 1 {
		t.Errorf("the file of a removed photograph: %v", gone.keys)
	}
}

// A car with no photographs has an empty list, not a missing one, so a
// site reading it does not have to guard.
func TestACarWithNoPhotographsHasAnEmptyListInTheFeed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, true, false, ""); err != nil {
		t.Fatal(err)
	}
	cars, _, err := workshop.StockFeed(ctx, pool, shopID, "", time.Time{})
	if err != nil || len(cars) != 1 {
		t.Fatalf("the feed: %+v, %v", cars, err)
	}
	body, _ := json.Marshal(cars[0])
	if !strings.Contains(string(body), `"photos":[]`) || !strings.Contains(string(body), `"equipment":[]`) {
		t.Errorf("a car with no photographs or equipment:\n%s", body)
	}
}

// What a car carries in the feed is exactly what docs/stock-feed.md says it
// does: a field added to the struct without being written up there, which is
// also a field that has not been looked at for who may see it, fails this.
func TestTheFeedCarsFieldsAreTheOnesTheDocumentationNames(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(workshop.FeedCar{})
	for i := range ty.NumField() {
		tag, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			t.Errorf("FeedCar.%s has no JSON name: it would be written under its Go name", ty.Field(i).Name)
			continue
		}
		got = append(got, tag)
	}
	doc, err := os.ReadFile("../../docs/stock-feed.md")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile("(?s)```json\n(.*?)```").FindSubmatch(doc)
	if block == nil {
		t.Fatal("docs/stock-feed.md has no JSON example")
	}
	var example struct {
		Cars []map[string]any `json:"cars"`
	}
	// The example elides some values with an ellipsis, which is not JSON.
	if err := json.Unmarshal([]byte(strings.ReplaceAll(string(block[1]), "…", "")), &example); err != nil || len(example.Cars) != 1 {
		t.Fatalf("the documented example: %v", err)
	}
	var documented []string
	for k := range example.Cars[0] {
		documented = append(documented, k)
	}
	slices.Sort(got)
	slices.Sort(documented)
	if !slices.Equal(got, documented) {
		t.Errorf("the feed's fields %v are not the documented %v", got, documented)
	}
}

// A dealer with 300 cars and 80 photographs on each (#183): the feed names
// every photograph, in order, from one statement's worth of work -- not one
// read per car -- and answers in a time a site's request can wait for.
func TestTheFeedOf300CarsWith80PhotographsEachIsOneReadAndFast(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 300 cars")
	}
	pool := setup(t)
	ctx := context.Background()
	for range 300 {
		p := boughtCar(t, pool)
		moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	}
	// Photographs are seeded in bulk: what is measured is the read, and
	// AttachStockPhoto's own rules are tested above.
	if _, err := pool.Exec(ctx, `
		INSERT INTO attachments (shop_id, stock_unit_id, position, storage_key, content_type, byte_size)
		SELECT u.shop_id, u.id, n, md5(u.id::text || n::text) || md5(n::text || u.id::text), 'image/jpeg', 10
		FROM stock_units u, generate_series(1, 80) n WHERE u.closed_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetFeed(ctx, pool, everything(owner()), true, true, false, ""); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cars, _, err := workshop.StockFeed(ctx, pool, shopID, "", time.Time{})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(cars) != 300 {
		t.Fatalf("%d cars in the feed, want 300", len(cars))
	}
	for _, c := range cars {
		if len(c.Photos) != 80 {
			t.Fatalf("car %s names %d photographs, want 80", c.ID, len(c.Photos))
		}
	}
	if took > 3*time.Second {
		t.Errorf("the feed took %s for 300 cars with 80 photographs each", took)
	}
	t.Logf("300 cars x 80 photographs: %s", took)
}

// The same photograph put on the same car twice is one (#167): by its
// content, whatever it is called, and also when two arrive at once. A removed
// one may be put back, and another car may have the same picture.
func TestThePhotographOfACarUploadedTwiceIsOnePhotograph(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	a := buy(t, pool, workshop.StockFromPurchase)
	b := boughtCar(t, pool)
	sum := func(c byte) []byte { return slices.Repeat([]byte{c}, 32) }
	attach := func(unit, key string, s []byte) (workshop.StockPhoto, bool) {
		t.Helper()
		p, dup, err := workshop.AttachStockPhotoOnce(ctx, pool, sales(), unit, key, "image/jpeg", 10, "x.jpg", s)
		if err != nil {
			t.Fatal(err)
		}
		return p, dup
	}
	first, dup := attach(a.StockUnitID, strings.Repeat("a", 32), sum(1))
	if dup {
		t.Fatal("the first was a duplicate")
	}
	again, dup := attach(a.StockUnitID, strings.Repeat("b", 32), sum(1))
	if !dup || again.ID != first.ID || again.Key != first.Key {
		t.Errorf("the same content again: %+v duplicate %v, want the first %+v", again, dup, first)
	}
	if _, dup := attach(a.StockUnitID, strings.Repeat("c", 32), sum(2)); dup {
		t.Error("another picture was a duplicate")
	}
	if _, dup := attach(b.StockUnitID, strings.Repeat("d", 32), sum(1)); dup {
		t.Error("the same picture on another car was a duplicate")
	}
	// No hash, never compared.
	if _, dup := attach(a.StockUnitID, strings.Repeat("e", 32), nil); dup {
		t.Error("a photograph with no hash was a duplicate")
	}
	if _, dup := attach(a.StockUnitID, strings.Repeat("f", 32), nil); dup {
		t.Error("two with no hash were duplicates")
	}
	if photos, _ := workshop.StockPhotos(ctx, pool, sales(), a.StockUnitID); len(photos) != 4 {
		t.Errorf("the car has %d photographs, want 4", len(photos))
	}
	// Taken away, it may be put back.
	if err := workshop.RemoveStockPhoto(ctx, pool, sales(), &gonePhotos{}, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, dup := attach(a.StockUnitID, strings.Repeat("0", 32), sum(1)); dup {
		t.Error("a photograph taken away could not be put back")
	}
	// Two at once are one.
	results := make(chan bool, 2)
	for i := range 2 {
		go func() {
			_, dup, err := workshop.AttachStockPhotoOnce(ctx, pool, sales(), a.StockUnitID, strings.Repeat(string(rune('1'+i)), 32), "image/jpeg", 10, "x.jpg", sum(9))
			if err != nil {
				t.Error(err)
			}
			results <- dup
		}()
	}
	if d1, d2 := <-results, <-results; d1 == d2 {
		t.Errorf("two uploads at once: duplicates %v and %v, want exactly one", d1, d2)
	}
}
