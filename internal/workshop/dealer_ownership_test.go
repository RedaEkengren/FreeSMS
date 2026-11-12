package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Ownership filings (#180).

// A delivered sale's ownership is to be filed: prepared at delivery, filed
// by hand with the control number in hand -- never recorded -- and only
// Transportstyrelsen's confirmation changes the ownership history, once
// whatever is retried. A rejection changes nothing.
func TestOnlyAConfirmedFilingChangesTheOwnerAndOnlyOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := deliveredOn(t, pool, eligibleCar(t, pool), time.Now())
	f, err := workshop.FilingOf(ctx, pool, sales(), "sale", d.ID)
	if err != nil || f.Status != "prepared" || f.ToPartyID != d.Buyers[0].PartyID || f.FromPartyID != "" {
		t.Fatalf("the sale's filing: %+v, %v", f, err)
	}
	owners := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM vehicle_ownership WHERE vehicle_id = $1 AND owned_to IS NULL`, f.VehicleID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := owners()
	if err := workshop.ConfirmFiling(ctx, pool, sales(), f.ID, "TS-123"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("confirmed before it was filed: %v", err)
	}
	if err := workshop.RecordFilingFiled(ctx, pool, sales(), f.ID, workshop.FilingChecklist{}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("filed without the control number in hand: %v", err)
	}
	if err := workshop.RecordFilingFiled(ctx, pool, sales(), f.ID, workshop.FilingChecklist{ControlNumberInHand: true}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RejectFiling(ctx, pool, sales(), f.ID, "Fel kontrollnummer"); err != nil {
		t.Fatal(err)
	}
	if owners() != before {
		t.Error("a rejected filing changed the owner")
	}
	if err := workshop.RecordFilingFiled(ctx, pool, sales(), f.ID, workshop.FilingChecklist{ControlNumberInHand: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := workshop.ConfirmFiling(ctx, pool, sales(), f.ID, "TS-123"); err != nil {
			t.Fatalf("confirmation %d: %v", i+1, err)
		}
	}
	var buyers int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM vehicle_ownership o JOIN parties p ON p.customer_id = o.customer_id
		WHERE o.vehicle_id = $1 AND p.id = $2 AND o.owned_to IS NULL`, f.VehicleID, d.Buyers[0].PartyID).Scan(&buyers); err != nil {
		t.Fatal(err)
	}
	if buyers != 1 || owners() != 1 {
		t.Errorf("after the confirmation, retried: the buyer owns it %d times, %d open ownerships", buyers, owners())
	}
	read, _ := workshop.FilingOf(ctx, pool, sales(), "sale", d.ID)
	if read.Status != "confirmed" || read.Reference != "TS-123" {
		t.Errorf("read back: %+v", read)
	}
}

// A purchase's ownership is prepared with it, and one not confirmed two
// days after it passed is in the queue.
func TestAPurchaseIsFiledAndAnUnconfirmedSaleIsNotForgotten(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	var purchase string
	if err := pool.QueryRow(ctx, `SELECT id FROM vehicle_purchases WHERE stock_unit_id = $1`, unit).Scan(&purchase); err != nil {
		t.Fatal(err)
	}
	if f, err := workshop.FilingOf(ctx, pool, buyerScope(), "purchase", purchase); err != nil || f.Status != "prepared" || f.FromPartyID == "" {
		t.Errorf("the purchase's filing: %+v, %v", f, err)
	}
	if _, err := workshop.FilingOf(ctx, pool, sales(), "purchase", purchase); err == nil {
		t.Error("a salesperson read a purchase's filing")
	}
	d := deliveredOn(t, pool, unit, time.Now().AddDate(0, 0, -3))
	items, err := workshop.WaitingFor(ctx, pool, sales())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		found = found || (it.Kind == "ownership_unconfirmed" && it.Link() == "/cars/deals/"+d.ID)
	}
	if !found {
		t.Errorf("an unconfirmed filing three days on is not in the queue: %+v", items)
	}
}

// A leased car is not filed on without the lessor's consent (#162): said on
// the checklist, or by the unit's newest leasing check, and a later check
// that passes lets it through. A purchase with no leasing check at all is
// filed as before.
func TestALeasedCarIsNotFiledWithoutTheLessorsConsent(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	var purchase string
	if err := pool.QueryRow(ctx, `SELECT id FROM vehicle_purchases WHERE stock_unit_id = $1`, unit).Scan(&purchase); err != nil {
		t.Fatal(err)
	}
	f, err := workshop.FilingOf(ctx, pool, buyerScope(), "purchase", purchase)
	if err != nil {
		t.Fatal(err)
	}
	checked := workshop.FilingChecklist{ControlNumberInHand: true}
	no := false
	refused := workshop.FilingChecklist{ControlNumberInHand: true, LeasingConsent: &no}
	if err := workshop.RecordFilingFiled(ctx, pool, buyerScope(), f.ID, refused); !errors.Is(err, workshop.ErrFilingLeased) {
		t.Errorf("filed with the checklist saying no consent: %v", err)
	}
	for _, result := range []string{workshop.CheckFail, workshop.CheckUnknown} {
		if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), unit, workshop.NewStockCheck{Kind: workshop.CheckLeasingConsent, Result: result}); err != nil {
			t.Fatal(err)
		}
		if err := workshop.RecordFilingFiled(ctx, pool, buyerScope(), f.ID, checked); !errors.Is(err, workshop.ErrFilingLeased) {
			t.Errorf("filed with the leasing check at %q: %v", result, err)
		}
	}
	if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), unit, workshop.NewStockCheck{Kind: workshop.CheckLeasingConsent, Result: workshop.CheckPass,
		Note: "Medgivande från leasegivaren"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordFilingFiled(ctx, pool, buyerScope(), f.ID, checked); err != nil {
		t.Errorf("filed once the lessor had consented: %v", err)
	}
}
