package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// aContact makes a private customer with the contact the test gives, and
// returns the customer.
func aContact(t *testing.T, pool *pgxpool.Pool, name, phone, email string) string {
	t.Helper()
	ctx := context.Background()
	var person, customer string
	if err := pool.QueryRow(ctx, `INSERT INTO people (shop_id, display_name, phone, email) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, '')::citext) RETURNING id::text`,
		shopID, name, phone, email).Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO customers (shop_id, kind, person_id) VALUES ($1, 'private', $2) RETURNING id::text`, shopID, person).Scan(&customer); err != nil {
		t.Fatal(err)
	}
	return customer
}

// aStoredSet is a set of the season for the customer, put in the place.
func aStoredSet(t *testing.T, pool *pgxpool.Pool, customer, vehicle, season, place string) string {
	t.Helper()
	n := summerSet(customer, vehicle)
	n.Season = season
	id, err := workshop.CreateTyreSet(context.Background(), pool, advisor(), n)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.PlaceTyreSet(context.Background(), pool, technician(), id, place); err != nil {
		t.Fatal(err)
	}
	return id
}

func queuedFor(t *testing.T, pool *pgxpool.Pool, campaign string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbound_messages WHERE campaign_id = $1`, campaign).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Three customers, one of whom opted out, and one with sets on two cars who is
// written to once. A second run, and two at once, queue nothing again.
func TestACampaignReachesEachCustomerOnceAndLeavesOutWhoOptedOut(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE people SET phone = '+46701111111' WHERE id = '22222222-2222-2222-2222-222222222223'`); err != nil {
		t.Fatal(err)
	}
	place := aPlace(t, pool, "Hall", 20)
	second := anotherCar(t, pool, customerA)
	aStoredSet(t, pool, customerA, vehicleA, workshop.SeasonSummer, place)
	aStoredSet(t, pool, customerA, second, workshop.SeasonSummer, place)
	bea := aContact(t, pool, "Bea", "+46702222222", "")
	cleo := aContact(t, pool, "Cleo", "+46703333333", "")
	dan := aContact(t, pool, "Dan", "", "")
	eve := aContact(t, pool, "Eve", "", "eve@example.org")
	aStoredSet(t, pool, bea, "", workshop.SeasonSummer, place)
	aStoredSet(t, pool, cleo, "", workshop.SeasonSummer, place)
	aStoredSet(t, pool, dan, "", workshop.SeasonSummer, place)
	aStoredSet(t, pool, eve, "", workshop.SeasonWinterFriction, place) // not this campaign's season
	if err := workshop.SetMessageOptOut(ctx, pool, advisor(), cleo, true); err != nil {
		t.Fatal(err)
	}

	id, err := workshop.CreateCampaign(ctx, pool, owner(), workshop.NewCampaign{
		Name: "Spring", Season: "summer", Subject: "Time for summer tyres", Body: "Your tyres in storage: {sets}. Book your change."})
	if err != nil {
		t.Fatal(err)
	}
	_, aud, err := workshop.CampaignPreview(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(aud.Reached) != 2 || aud.OptedOut != 1 || aud.NoContact != 1 || aud.AlreadyQueued != 0 {
		t.Fatalf("the audience: %d reached, %d opted out, %d without contact, %d queued", len(aud.Reached), aud.OptedOut, aud.NoContact, aud.AlreadyQueued)
	}
	var first workshop.CampaignCustomer
	for _, r := range aud.Reached {
		if r.CustomerID == customerA {
			first = r
		}
	}
	if len(first.Sets) != 2 || first.Channel != "sms" {
		t.Fatalf("a customer with two cars: %+v", first)
	}
	if queuedFor(t, pool, id) != 0 {
		t.Fatal("a preview queued a message")
	}

	when := time.Now().Add(time.Hour)
	if _, err := workshop.RunCampaign(ctx, pool, owner(), id, when); err != nil {
		t.Fatal(err)
	}
	if n := queuedFor(t, pool, id); n != 2 {
		t.Fatalf("queued %d messages for two customers", n)
	}
	var body string
	if err := pool.QueryRow(ctx, `SELECT body FROM outbound_messages WHERE campaign_id = $1 AND recipient = '+46701111111'`, id).Scan(&body); err != nil || body != "Your tyres in storage: "+first.Sets[0]+", "+first.Sets[1]+". Book your change." {
		t.Errorf("the message: %q, %v", body, err)
	}

	// Run again, twice at once: nothing more is queued.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := workshop.RunCampaign(ctx, pool, owner(), id, when); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := queuedFor(t, pool, id); n != 2 {
		t.Errorf("a repeated run left %d messages", n)
	}

	// A customer who comes into the audience later is written to, and only they.
	fay := aContact(t, pool, "Fay", "+46704444444", "")
	aStoredSet(t, pool, fay, "", workshop.SeasonSummer, place)
	if _, err := workshop.RunCampaign(ctx, pool, owner(), id, when); err != nil {
		t.Fatal(err)
	}
	if n := queuedFor(t, pool, id); n != 3 {
		t.Errorf("a late customer: %d messages", n)
	}

	// Opting out after the message was queued stops it as it is about to go.
	var msg string
	if err := pool.QueryRow(ctx, `SELECT message_id::text FROM tyre_campaign_recipients WHERE campaign_id = $1 AND customer_id = $2`, id, fay).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := workshop.CampaignMessageStillWanted(ctx, pool, shopID, msg); err != nil || !ok {
		t.Fatalf("before opting out: %v, %v", ok, err)
	}
	if err := workshop.SetMessageOptOut(ctx, pool, advisor(), fay, true); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := workshop.CampaignMessageStillWanted(ctx, pool, shopID, msg); err != nil || ok {
		t.Errorf("after opting out: %v, %v", ok, err)
	}
	// And opting in again is allowed: the choice is the newest of them.
	if err := workshop.SetMessageOptOut(ctx, pool, advisor(), fay, false); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := workshop.CampaignMessageStillWanted(ctx, pool, shopID, msg); !ok {
		t.Error("a customer who opted back in is still left out")
	}
}

func TestOnlyTheOwnerMakesAndRunsACampaignAndTheCounterReadsIt(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	n := workshop.NewCampaign{Name: "Autumn", Season: "winter", Body: "Time for winter tyres: {sets}"}
	for name, scope := range map[string]access.Scope{"advisor": advisor(), "technician": technician()} {
		if _, err := workshop.CreateCampaign(ctx, pool, scope, n); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s made a campaign: %v", name, err)
		}
	}
	id, err := workshop.CreateCampaign(ctx, pool, owner(), n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RunCampaign(ctx, pool, advisor(), id, time.Now()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("an advisor ran a campaign: %v", err)
	}
	if _, err := workshop.RunCampaign(ctx, pool, owner(), id, time.Time{}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a run with no time: %v", err)
	}
	if _, _, err := workshop.CampaignPreview(ctx, pool, technician(), id); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician previewed a campaign: %v", err)
	}
	if err := workshop.SetMessageOptOut(ctx, pool, technician(), customerA, true); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician recorded an opt-out: %v", err)
	}
	if _, err := workshop.CreateCampaign(ctx, pool, owner(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same name twice: %v", err)
	}
	for _, bad := range []workshop.NewCampaign{
		{Name: "x", Season: "spring", Body: "b"}, {Name: " ", Season: "summer", Body: "b"}, {Name: "y", Season: "summer", Body: " "}} {
		if _, err := workshop.CreateCampaign(ctx, pool, owner(), bad); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}
