package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Delivery (#179).

// A sold car is handed over once its contract is complete, its sale
// invoiced and paid: the moment is frozen, the consumer law's dates are
// derived from it, the odometer is read at delivery and the car's cycle
// ends. Before that the delivery is refused with what is missing; once
// done, it is not done again.
func TestACarIsDeliveredOncePaidAndTheConsumerClocksStart(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, eligibleCar(t, pool), workshop.TaxMarginScheme)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	km := int64(123_700)
	_, err := workshop.DeliverCar(ctx, pool, sales(), d.ID, workshop.NewDelivery{At: at, OdometerKm: &km})
	var refused *workshop.DeliveryRefused
	if !errors.As(err, &refused) || !slices.Contains(refused.Missing, workshop.MissingInvoice) {
		t.Fatalf("delivered before it was invoiced: %v", err)
	}
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.DeliverCar(ctx, pool, sales(), d.ID, workshop.NewDelivery{At: at}); !errors.As(err, &refused) ||
		!slices.Equal(refused.Missing, []string{workshop.MissingPayment}) {
		t.Fatalf("delivered before it was paid: %v", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, everything(owner()), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := workshop.DeliverCar(ctx, pool, sales(), d.ID, workshop.NewDelivery{At: at, OdometerKm: &km})
	if err != nil {
		t.Fatal(err)
	}
	if !got.DeliveredAt.Equal(at) || !got.Consumer || !got.PresumptionEnds().Equal(at.AddDate(2, 0, 0)) ||
		!got.LiabilityEnds().Equal(at.AddDate(3, 0, 0)) || got.OdometerKm == nil || *got.OdometerKm != km {
		t.Errorf("the delivery: %+v", got)
	}
	if u, _ := workshop.StockUnitByID(ctx, pool, sales(), d.StockUnitID); u.Status != workshop.StockDelivered || u.ClosedAt == nil {
		t.Errorf("the car after delivery: %+v", u)
	}
	if _, err := workshop.DeliverCar(ctx, pool, sales(), d.ID, workshop.NewDelivery{At: at}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("delivered twice: %v", err)
	}
	if read, err := workshop.DeliveryOfSale(ctx, pool, sales(), d.ID); err != nil || !read.DeliveredAt.Equal(at) {
		t.Errorf("read back: %+v, %v", read, err)
	}
	if _, err := workshop.DeliveryOfSale(ctx, pool, everything(technician()), d.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read a delivery: %v", err)
	}
}

// Before it is paid, a car goes only on the word of whoever runs the
// shop, with why -- an audit event -- and never on a salesperson's.
func TestOnlyTheOwnerLetsACarGoUnpaid(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, eligibleCar(t, pool), workshop.TaxMarginScheme)
	if _, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.DeliverCar(ctx, pool, sales(), d.ID, workshop.NewDelivery{At: time.Now(), Override: "Banken betalar på måndag"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a salesperson let a car go unpaid: %v", err)
	}
	ownerSells := everything(owner())
	if _, err := workshop.DeliverCar(ctx, pool, ownerSells, d.ID, workshop.NewDelivery{At: time.Now(), Override: "Banken betalar på måndag"}); err != nil {
		t.Fatal(err)
	}
	trail, err := workshop.AuditTrail(ctx, pool, ownerSells, "vehicle_sale", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(trail, func(e workshop.AuditEvent) bool {
		return e.Event == "vehicle_sale.delivered_early" && e.Reason == "Banken betalar på måndag"
	}) {
		t.Errorf("no audit event for the early delivery: %+v", trail)
	}
}
