package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A collected set frees its place, says who took it and is closed: nothing can
// be done to a set that has left, by an operation or by the database.
func TestACollectedSetFreesItsPlaceAndIsClosed(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 1)
	set := aStoredSet(t, pool, customerA, "", workshop.SeasonSummer, place)
	if err := workshop.CollectTyreSet(ctx, pool, technician(), set, "  "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("nobody written down: %v", err)
	}
	if err := workshop.CollectTyreSet(ctx, pool, technician(), set, "The owner's brother"); err != nil {
		t.Fatal(err)
	}
	x, err := workshop.TyreSetExitOf(ctx, pool, technician(), set)
	if err != nil || x == nil || x.Kind != "collected" || x.CollectedBy != "The owner's brother" {
		t.Fatalf("the exit: %+v, %v", x, err)
	}
	if s, _ := workshop.TyreSetByID(ctx, pool, technician(), set); s.StoredIn != "" {
		t.Errorf("a collected set is still in %q", s.StoredIn)
	}
	// The place has room for another, and the set cannot be put back.
	aStoredSet(t, pool, customerA, "", workshop.SeasonWinterFriction, place)
	if err := workshop.CollectTyreSet(ctx, pool, technician(), set, "again"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("collected twice: %v", err)
	}
	if err := workshop.PlaceTyreSet(ctx, pool, technician(), set, place); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set that has left was placed: %v", err)
	}
	s, _ := workshop.TyreSetByID(ctx, pool, technician(), set)
	if _, err := workshop.RecordTreadDepth(ctx, pool, technician(), s.Tyres[0].ID, 40, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set that has left was measured: %v", err)
	}
	if err := workshop.MoveTyreSet(ctx, pool, advisor(), set, vehicleA); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set that has left was put on a car: %v", err)
	}
	if err := workshop.CheckOutTyreSet(ctx, pool, technician(), newJob(t, pool), set); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a set that has left was checked out: %v", err)
	}
	// The database refuses it too, whatever calls it.
	if _, err := pool.Exec(ctx, `INSERT INTO tyre_set_placements (shop_id, tyre_set_id, location_id) VALUES ($1, $2, $3)`, shopID, set, place); err == nil {
		t.Error("the database let a set that has left be placed")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tyre_measurements (shop_id, tyre_id, tenths) VALUES ($1, $2, 40)`, shopID, s.Tyres[0].ID); err == nil {
		t.Error("the database let a set that has left be measured")
	}
}

// A transfer needs the current owner's yes, of that set and that kind. The new
// owner gets a new set of the same tyres in the same place; the old one keeps
// its history, closed.
func TestATransferNeedsTheOwnersYesAndKeepsTheHistory(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 1)
	buyer := aContact(t, pool, "Bea", "+46702222222", "")
	set := aStoredSet(t, pool, customerA, "", workshop.SeasonSummer, place)
	other := aStoredSet(t, pool, buyer, "", workshop.SeasonWinterFriction, aPlace(t, pool, "Shelf", 2))
	measured(t, pool, set)

	if _, err := workshop.TransferTyreSet(ctx, pool, advisor(), set, "11111111-1111-1111-1111-111111111111"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a transfer with no agreement: %v", err)
	}
	if _, err := workshop.RecordSetAgreement(ctx, pool, advisor(), set, "transfer", customerA, "the owner herself"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a transfer to the owner: %v", err)
	}
	if _, err := workshop.RecordSetAgreement(ctx, pool, advisor(), set, "transfer", buyer, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a signed note with nothing written about it: %v", err)
	}
	disposal, err := workshop.RecordSetAgreement(ctx, pool, advisor(), set, "disposal", "", "signed note in the folder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.TransferTyreSet(ctx, pool, advisor(), set, disposal); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a transfer on a disposal's agreement: %v", err)
	}
	agreement, err := workshop.RecordSetAgreement(ctx, pool, advisor(), set, "transfer", buyer, "signed note, folder 3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.TransferTyreSet(ctx, pool, advisor(), other, agreement); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an agreement for another set: %v", err)
	}
	fresh, err := workshop.TransferTyreSet(ctx, pool, advisor(), set, agreement)
	if err != nil {
		t.Fatal(err)
	}
	now, err := workshop.TyreSetByID(ctx, pool, advisor(), fresh)
	if err != nil || now.CustomerID != buyer || now.StoredIn != "Hall" || len(now.Tyres) != 4 || now.Brand != "Michelin" {
		t.Fatalf("the new owner's set: %+v, %v", now, err)
	}
	old, _ := workshop.TyreSetByID(ctx, pool, advisor(), set)
	if old.CustomerID != customerA || old.StoredIn != "" {
		t.Errorf("the old set: owner %q, in %q", old.CustomerID, old.StoredIn)
	}
	// The old set's depths are still there to read.
	if len(old.Tyres) != 4 || len(old.Tyres[0].Readings) == 0 {
		t.Errorf("the old set lost its history: %+v", old.Tyres)
	}
	if x, _ := workshop.TyreSetExitOf(ctx, pool, advisor(), set); x == nil || x.Kind != "transferred" || x.NewSetID != fresh {
		t.Errorf("the exit: %+v", x)
	}
	if _, err := workshop.TransferTyreSet(ctx, pool, advisor(), set, agreement); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("transferred twice: %v", err)
	}
}

// A disposal needs the owner's yes: given on their link, once, or a signed note.
// The fee goes on the job as a line, and the set is closed.
func TestADisposalNeedsTheOwnersYesAndPutsTheFeeOnTheJob(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 3)
	set := aStoredSet(t, pool, customerA, "", workshop.SeasonSummer, place)
	fee, err := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{
		Name: "Disposal of worn tyres", Per: "tyre", LineKind: "fee", UnitPriceMinor: 5000, VATRateBasis: 2500})
	if err != nil {
		t.Fatal(err)
	}
	job := newJob(t, pool)
	shop := shopID

	token, err := workshop.RequestSetAgreement(ctx, pool, advisor(), set, "disposal", "")
	if err != nil {
		t.Fatal(err)
	}
	view, err := workshop.SetAgreement(ctx, pool, shop, token)
	if err != nil || view.Kind != "disposal" || view.Answered != "" {
		t.Fatalf("the owner's page: %+v, %v", view, err)
	}
	var agreement string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM tyre_set_agreements WHERE tyre_set_id = $1`, set).Scan(&agreement); err != nil {
		t.Fatal(err)
	}
	// Asked and not answered: refused.
	if err := workshop.DisposeTyreSet(ctx, pool, advisor(), set, agreement, job, fee); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("disposed of with no yes: %v", err)
	}
	if err := workshop.AnswerSetAgreement(ctx, pool, shop, "not a token", true); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a wrong token: %v", err)
	}
	if err := workshop.AnswerSetAgreement(ctx, pool, shop, token, true); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AnswerSetAgreement(ctx, pool, shop, token, false); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a second answer: %v", err)
	}
	if err := workshop.DisposeTyreSet(ctx, pool, advisor(), set, agreement, job, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a job with no fee: %v", err)
	}
	if err := workshop.DisposeTyreSet(ctx, pool, technician(), set, agreement, job, fee); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician disposed of a set: %v", err)
	}
	if err := workshop.DisposeTyreSet(ctx, pool, advisor(), set, agreement, job, fee); err != nil {
		t.Fatal(err)
	}
	_, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	ok := false
	for _, l := range lines {
		if l.Description == "Disposal of worn tyres" && l.QuantityMilli == 4000 && l.UnitPriceMinor == 5000 {
			ok = true
		}
	}
	if !ok {
		t.Errorf("the disposal fee is not on the job as four tyres: %+v", lines)
	}
	if x, _ := workshop.TyreSetExitOf(ctx, pool, advisor(), set); x == nil || x.Kind != "disposed" {
		t.Errorf("the exit: %+v", x)
	}
	if _, err := workshop.SetAgreement(ctx, pool, shop, token); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("the link of a set that has gone: %v", err)
	}
}

func TestADeclinedOrReplacedAgreementDoesNotAllowADisposal(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 3)
	set := aStoredSet(t, pool, customerA, "", workshop.SeasonSummer, place)
	first, _ := workshop.RequestSetAgreement(ctx, pool, advisor(), set, "disposal", "")
	second, err := workshop.RequestSetAgreement(ctx, pool, advisor(), set, "disposal", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.AnswerSetAgreement(ctx, pool, shopID, first, true); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("an older link, closed by the newer: %v", err)
	}
	if err := workshop.AnswerSetAgreement(ctx, pool, shopID, second, false); err != nil {
		t.Fatal(err)
	}
	var agreement string
	if err := pool.QueryRow(ctx, `SELECT a.id::text FROM tyre_set_agreements a JOIN tyre_set_agreement_answers w ON w.agreement_id = a.id WHERE a.tyre_set_id = $1`, set).Scan(&agreement); err != nil {
		t.Fatal(err)
	}
	if err := workshop.DisposeTyreSet(ctx, pool, advisor(), set, agreement, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("disposed of after a no: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_set_agreements SET expires_at = clock_timestamp() - interval '1 day' WHERE token_sha256 IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.SetAgreement(ctx, pool, shopID, second); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("an expired link: %v", err)
	}
}

func TestASetNobodyHasComeForIsListedWithTheRemindersSent(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	place := aPlace(t, pool, "Hall", 5)
	quiet := aStoredSet(t, pool, customerA, "", workshop.SeasonSummer, place)
	busy := aStoredSet(t, pool, customerA, "", workshop.SeasonWinterFriction, place)
	if list, err := workshop.AbandonedSets(ctx, pool, advisor()); err != nil || len(list) != 0 {
		t.Fatalf("a set placed today: %+v, %v", list, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_set_placements SET placed_at = clock_timestamp() - interval '400 days'`); err != nil {
		t.Fatal(err)
	}
	// One was charged and one was written to after that.
	fee, _ := workshop.CreateTyreExtra(ctx, pool, owner(), workshop.NewTyreExtra{Name: "Storage", Per: "set", LineKind: "fee", UnitPriceMinor: 100, VATRateBasis: 2500, StoragePeriod: "year"})
	if _, err := workshop.InvoiceStorage(ctx, pool, advisor(), customerA, fee, 2027); err != nil {
		t.Fatal(err)
	}
	list, err := workshop.AbandonedSets(ctx, pool, advisor())
	if err != nil || len(list) != 0 {
		t.Fatalf("sets charged today are listed: %+v, %v", list, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tyre_storage_charges SET charged_at = clock_timestamp() - interval '390 days' WHERE tyre_set_id = $1`, quiet); err != nil {
		t.Fatal(err)
	}
	list, err = workshop.AbandonedSets(ctx, pool, advisor())
	if err != nil || len(list) != 1 || list[0].SetID != quiet || list[0].Place != "Hall" || list[0].Reminders != 0 {
		t.Fatalf("the list: %+v, %v", list, err)
	}
	if list[0].SetID == busy {
		t.Error("a set charged this year is listed")
	}
	if _, err := workshop.AbandonedSets(ctx, pool, technician()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the list: %v", err)
	}
	if err := workshop.SetTyreChangeSettings(ctx, pool, owner(), workshop.TyreChangeSettings{MinutesPerSet: 30, SetsPerDay: 12, FollowUpDays: 14, AbandonDays: 500}); err != nil {
		t.Fatal(err)
	}
	if list, _ := workshop.AbandonedSets(ctx, pool, advisor()); len(list) != 0 {
		t.Errorf("with 500 days to wait: %+v", list)
	}
}
