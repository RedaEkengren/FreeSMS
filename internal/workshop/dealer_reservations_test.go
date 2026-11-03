package workshop_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Reservations (#174).

// dealTo contracts a deal on unit with buyer, as completedDealOn does for
// a buyer of its own; the error is the completion's, and a deal not
// completed is cancelled, so the car is free for the next.
func dealTo(t *testing.T, pool *pgxpool.Pool, unit, buyer string) (workshop.Deal, error) {
	t.Helper()
	ctx := context.Background()
	terms := consumerTerms(buyer)
	terms.TaxTreatment = workshop.TaxMarginScheme
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	done, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID)
	if err != nil {
		if cerr := workshop.CancelDeal(ctx, pool, sales(), d.ID, "Fel köpare"); cerr != nil {
			t.Fatal(cerr)
		}
	}
	return done, err
}

func stockStatus(t *testing.T, pool *pgxpool.Pool, unit string) workshop.StockStatus {
	t.Helper()
	u, err := workshop.StockUnitByID(context.Background(), pool, sales(), unit)
	if err != nil {
		t.Fatal(err)
	}
	return u.Status
}

// A car reserved is reserved for its holder alone: a second reservation is
// refused, two at once leave one, a sale to anyone else is refused and a
// sale to the holder ends the reservation as sold.
func TestAReservedCarIsTheHoldersAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	kim, other := driver(t, pool), driver(t, pool)
	until := time.Now().Add(48 * time.Hour)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, who := range []string{kim, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: who, ExpiresAt: until, Refundable: true})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("a refused reservation: %v", err)
		}
	}
	if ok != 1 || stockStatus(t, pool, unit) != workshop.StockReserved {
		t.Fatalf("two reservations at once: %d stood, status %s", ok, stockStatus(t, pool, unit))
	}
	rs, err := workshop.ReservationsOfUnit(ctx, pool, sales(), unit)
	if err != nil || len(rs) != 1 {
		t.Fatalf("the car's reservations: %+v, %v", rs, err)
	}
	holder, stranger := rs[0].PartyID, kim
	if holder == kim {
		stranger = other
	}
	if err := workshop.MoveStockUnit(ctx, pool, sales(), unit, workshop.StockForSale, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("moved back to for sale by hand with the reservation standing: %v", err)
	}
	if _, err := dealTo(t, pool, unit, stranger); !errors.Is(err, workshop.ErrReservedForAnother) {
		t.Errorf("sold to someone else than the holder: %v", err)
	}
	if _, err := dealTo(t, pool, unit, holder); err != nil {
		t.Fatalf("sold to the holder: %v", err)
	}
	rs, _ = workshop.ReservationsOfUnit(ctx, pool, sales(), unit)
	if rs[0].Standing() || rs[0].EndKind != "sold" || stockStatus(t, pool, unit) != workshop.StockSold {
		t.Errorf("after the sale to the holder: %+v, status %s", rs[0], stockStatus(t, pool, unit))
	}
}

// Extended, a reservation is a new version with its own expiry, the same
// holder and the first as its root. Ended without a sale, a person says why
// and decides on the deposit, and the car is for sale again; lapsed only
// once it has run out.
func TestAReservationIsExtendedAndEndedByAPerson(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	kim := driver(t, pool)
	first, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: kim,
		ExpiresAt: time.Now().Add(24 * time.Hour), Refundable: false, Conditions: "Handpenningen återbetalas inte", Key: "res-1"})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: kim,
		ExpiresAt: time.Now().Add(24 * time.Hour), Key: "res-1"}); err != nil || again.ID != first.ID {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	second, err := workshop.ExtendReservation(ctx, pool, sales(), first.ID, time.Now().Add(72*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if second.RootID != first.ID || second.SupersedesID != first.ID || second.PartyID != first.PartyID ||
		second.Conditions != first.Conditions || !second.ExpiresAt.After(first.ExpiresAt) {
		t.Errorf("the extension: %+v", second)
	}
	if _, err := workshop.ExtendReservation(ctx, pool, sales(), first.ID, time.Now().Add(96*time.Hour), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an ended reservation extended: %v", err)
	}
	if err := workshop.EndReservation(ctx, pool, sales(), second.ID, workshop.ReservationLapsed, workshop.DepositRetain, "Hörde aldrig av sig"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("ended as lapsed before it ran out: %v", err)
	}
	if err := workshop.EndReservation(ctx, pool, sales(), second.ID, workshop.ReservationCancelled, "", "Ångrade sig"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("ended with no decision on the deposit: %v", err)
	}
	for _, s := range []access.Scope{buyerScope(), everything(advisor()), rentalDesk()} {
		if err := workshop.EndReservation(ctx, pool, s, second.ID, workshop.ReservationCancelled, workshop.DepositRefund, "x"); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%v ended a reservation: %v", s.Role, err)
		}
	}
	if err := workshop.EndReservation(ctx, pool, sales(), second.ID, workshop.ReservationCancelled, workshop.DepositRefund, "Ångrade sig"); err != nil {
		t.Fatal(err)
	}
	rs, _ := workshop.ReservationsOfUnit(ctx, pool, sales(), unit)
	if len(rs) != 2 || rs[0].Standing() || rs[0].DepositDecision != workshop.DepositRefund || rs[1].EndKind != "extended" ||
		stockStatus(t, pool, unit) != workshop.StockForSale {
		t.Errorf("after ending it: %+v, status %s", rs, stockStatus(t, pool, unit))
	}
	trail, err := workshop.AuditTrail(ctx, pool, buyerScope(), "stock_unit", unit)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range trail {
		found = found || (e.Event == "reservation.ended" && e.Reason == "Ångrade sig")
	}
	if !found {
		t.Errorf("no audit event for the reservation ending: %+v", trail)
	}
}

// A reservation that has run out stands, in front of whoever sells cars,
// until a person ends it: nothing is decided about the deposit unasked.
func TestALapsedReservationWaitsForAPerson(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	r, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: driver(t, pool),
		ExpiresAt: time.Now().Add(time.Hour), Refundable: true})
	if err != nil {
		t.Fatal(err)
	}
	// Run out: the expiry moved into the past as the clock would.
	if _, err := pool.Exec(ctx, `ALTER TABLE vehicle_reservations DISABLE TRIGGER vehicle_reservations_end_once`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE vehicle_reservations SET created_at = now() - interval '3 hours', expires_at = now() - interval '1 hour' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE vehicle_reservations ENABLE TRIGGER vehicle_reservations_end_once`); err != nil {
		t.Fatal(err)
	}
	items, err := workshop.WaitingFor(ctx, pool, sales())
	if err != nil {
		t.Fatal(err)
	}
	lapsed := false
	for _, it := range items {
		lapsed = lapsed || (it.Kind == "reservation_lapsed" && it.Link() == "/cars/stock/"+unit && it.Urgency == workshop.UrgencyNow)
	}
	if !lapsed || stockStatus(t, pool, unit) != workshop.StockReserved {
		t.Errorf("a lapsed reservation: %+v, status %s", items, stockStatus(t, pool, unit))
	}
	if err := workshop.EndReservation(ctx, pool, sales(), r.ID, workshop.ReservationLapsed, workshop.DepositNone, "Ingen handpenning"); err != nil {
		t.Errorf("ending a lapsed reservation: %v", err)
	}
}

// Where the shop lets nobody drive a reserved car, the holder still drives
// the car they reserved; anyone else is refused (#173, #174).
func TestTheHolderDrivesTheCarTheyReserved(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	kim, other := driver(t, pool), driver(t, pool)
	if err := workshop.SetTestDriveRules(ctx, pool, everything(owner()), 18, true); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: kim,
		ExpiresAt: time.Now().Add(48 * time.Hour), Refundable: true}); err != nil {
		t.Fatal(err)
	}
	from := time.Now().Add(2 * time.Hour)
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: other,
		From: from, To: from.Add(time.Hour)}); !errors.Is(err, workshop.ErrDriveReservedBlocked) {
		t.Errorf("someone else booked to drive the reserved car: %v", err)
	}
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: kim,
		From: from, To: from.Add(time.Hour)}); err != nil {
		t.Errorf("the holder booked to drive the car they reserved: %v", err)
	}
}

// A person's export carries the cars reserved for them (#174).
func TestAPersonsExportHasTheirReservations(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	kim := driver(t, pool)
	if _, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: kim,
		ExpiresAt: time.Now().Add(48 * time.Hour), Refundable: true, Conditions: "Återbetalas om banken säger nej"}); err != nil {
		t.Fatal(err)
	}
	var person string
	if err := pool.QueryRow(ctx, `SELECT person_id FROM parties WHERE id = $1`, kim).Scan(&person); err != nil {
		t.Fatal(err)
	}
	e, err := workshop.ExportPerson(ctx, pool, everything(advisor()), person)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Reservations) != 1 || e.Reservations[0].Car != "ABC 12D" || e.Reservations[0].Conditions == "" {
		t.Errorf("the export's reservations: %+v", e.Reservations)
	}
}

// A reservation's deposit is held against its chain: paid by somebody else
// than the holder, it is still the reservation's, an extension keeps it,
// and nothing more is taken once the reservation has ended.
func TestAReservationsDepositIsHeldAgainstIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	kim, parent := driver(t, pool), driver(t, pool)
	r, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: kim,
		ExpiresAt: time.Now().Add(48 * time.Hour), Refundable: true})
	if err != nil {
		t.Fatal(err)
	}
	five := int64(500_000)
	pay := workshop.NewPayment{AmountMinor: &five, Method: "swish", PaidOn: time.Now(), Reference: "Handpenning", Key: "dep-1"}
	if _, err := workshop.RecordReservationDeposit(ctx, pool, sales(), r.ID, parent, pay); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordReservationDeposit(ctx, pool, sales(), r.ID, parent, pay); err != nil {
		t.Fatalf("the same payment again: %v", err)
	}
	next, err := workshop.ExtendReservation(ctx, pool, sales(), r.ID, time.Now().Add(96*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := workshop.ReservationDeposit(ctx, pool, sales(), next.ID)
	if err != nil || b.Held() != five || b.Received != five {
		t.Errorf("the extension's deposit: %+v, %v", b, err)
	}
	if got, err := workshop.ReservationDeposit(ctx, pool, rentalDesk(), next.ID); !errors.Is(err, access.ErrForbidden) || got.Received != 0 {
		t.Errorf("the rental desk read a reservation's deposit: %v", err)
	}
	if err := workshop.EndReservation(ctx, pool, sales(), next.ID, workshop.ReservationCancelled, workshop.DepositRefund, "Banken sa nej"); err != nil {
		t.Fatal(err)
	}
	pay.Key = "dep-2"
	if _, err := workshop.RecordReservationDeposit(ctx, pool, sales(), next.ID, "", pay); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a deposit taken on an ended reservation: %v", err)
	}
}

// Sold to its holder, a reserved car's invoice is issued with the deposit
// already paid on it: the money moves from the deposit to the invoice, and
// nothing is held any more (#174).
func TestTheDepositIsAppliedToTheSaleInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	kim := driver(t, pool)
	r, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: kim,
		ExpiresAt: time.Now().Add(48 * time.Hour), Refundable: true})
	if err != nil {
		t.Fatal(err)
	}
	five := int64(500_000)
	if _, err := workshop.RecordReservationDeposit(ctx, pool, sales(), r.ID, "", workshop.NewPayment{AmountMinor: &five, Method: "swish",
		PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d, err := dealTo(t, pool, unit, kim)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paid int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(amount_minor), 0) FROM invoice_payments WHERE invoice_id = $1 AND method = 'deposit'`,
		inv.ID).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	b, err := workshop.ReservationDeposit(ctx, pool, sales(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paid != five || b.Held() != 0 || b.Applied != five {
		t.Errorf("the invoice was paid %d from the deposit, which holds %d (%+v)", paid, b.Held(), b)
	}
}

// A reservation's terms are a frozen document given to the holder: the
// car, until when, the deposit held and whether it is refunded. Made again
// after an extension, the new one supersedes the old, which stays (#174).
func TestAReservationsTermsAreAFrozenDocument(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	r, err := workshop.Reserve(ctx, pool, sales(), workshop.NewReservation{StockUnitID: unit, PartyID: driver(t, pool),
		ExpiresAt: time.Now().Add(48 * time.Hour), Refundable: false, Conditions: "Hämtas senast på fredag"})
	if err != nil {
		t.Fatal(err)
	}
	five := int64(500_000)
	if _, err := workshop.RecordReservationDeposit(ctx, pool, sales(), r.ID, "", workshop.NewPayment{AmountMinor: &five, Method: "cash",
		PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	first, err := workshop.ProduceReservationTerms(ctx, pool, sales(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := workshop.DocumentRecordByID(ctx, pool, sales(), first)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ABC 12D", "Kim Köpare", "5000.00", "Hämtas senast på fredag"} {
		if !strings.Contains(strings.ReplaceAll(doc.Body, " ", ""), strings.ReplaceAll(want, " ", "")) {
			t.Errorf("the terms do not say %q:\n%s", want, doc.Body)
		}
	}
	next, err := workshop.ExtendReservation(ctx, pool, sales(), r.ID, time.Now().Add(96*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := workshop.ProduceReservationTerms(ctx, pool, sales(), next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := workshop.DocumentRecordByID(ctx, pool, sales(), second); d.SupersedesID != first {
		t.Errorf("the terms made again supersede %q, want %q", d.SupersedesID, first)
	}
	if in, err := workshop.ReservationTermsOf(ctx, pool, sales(), next.ID); err != nil || in != second {
		t.Errorf("the terms in force: %q, %v; want %q", in, err, second)
	}
	if _, err := workshop.ProduceReservationTerms(ctx, pool, buyerScope(), next.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer made a reservation's terms: %v", err)
	}
}
