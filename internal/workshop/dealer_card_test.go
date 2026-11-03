package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A stock car's card (#167): damage found and repaired, its history kept;
// keys and where each went, the newest move saying where it is, a lost
// key found again; the service book with its odometer. Written while the
// car is in stock, read by whoever buys or sells cars and nobody else.
func TestACarsCardKeepsItsDamageKeysAndServiceAsRows(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	dent, err := workshop.RecordDamage(ctx, pool, buyerScope(), p.VehicleID, "left", "Buckla i bakdörren", "minor", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordDamage(ctx, pool, buyerScope(), p.VehicleID, "nose", "x", "minor", time.Now()); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a damage somewhere not on the outline: %v", err)
	}
	if err := workshop.RepairDamage(ctx, pool, buyerScope(), dent, time.Now(), "Riktad"); err != nil {
		t.Fatal(err)
	}
	key, err := workshop.AddKey(ctx, pool, sales(), p.VehicleID, "remote", "Huvudnyckel")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{{"out", "Kim på provkörning"}, {"back", ""}, {"lost", ""}, {"found", ""}, {"out", "Verkstaden"}} {
		if err := workshop.MoveKey(ctx, pool, sales(), key, m[0], m[1], ""); err != nil {
			t.Fatalf("%s: %v", m[0], err)
		}
	}
	if err := workshop.MoveKey(ctx, pool, sales(), key, "out", "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a key out to nobody: %v", err)
	}
	km := int64(98_000)
	if err := workshop.AddServiceEntry(ctx, pool, buyerScope(), p.VehicleID, time.Now().AddDate(-1, 0, 0), &km, "Märkesverkstad", "Stor service"); err != nil {
		t.Fatal(err)
	}
	card, err := workshop.CardOf(ctx, pool, sales(), p.VehicleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Damages) != 1 || card.Damages[0].RepairedOn == nil || card.Damages[0].Description != "Buckla i bakdörren" {
		t.Errorf("the damage: %+v", card.Damages)
	}
	if len(card.Keys) != 1 || card.Keys[0].Where != "out" || card.Keys[0].Holder != "Verkstaden" {
		t.Errorf("the key: %+v", card.Keys)
	}
	if len(card.Service) != 1 || card.Service[0].OdometerKm == nil || *card.Service[0].OdometerKm != km {
		t.Errorf("the service book: %+v", card.Service)
	}
	for _, s := range []access.Scope{everything(technician()), everything(advisor()), rentalDesk()} {
		if got, err := workshop.CardOf(ctx, pool, s, p.VehicleID); !errors.Is(err, access.ErrForbidden) || len(got.Damages) != 0 {
			t.Errorf("%v read the card: %v", s.Role, err)
		}
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	if err := workshop.MoveStockUnit(ctx, pool, buyerScope(), p.StockUnitID, workshop.StockWrittenOff, "Skrotad"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordDamage(ctx, pool, buyerScope(), p.VehicleID, "front", "Ny skada", "minor", time.Now()); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a card written after the car left stock: %v", err)
	}
	if after, _ := workshop.CardOf(ctx, pool, sales(), p.VehicleID); len(after.Damages) != 1 {
		t.Errorf("the card after the car left: %+v", after)
	}
}
