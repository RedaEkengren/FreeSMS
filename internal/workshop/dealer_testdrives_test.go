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

// Test drives (#173).

// driver is a private person who drives a car, as a party.
func driver(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := workshop.CreateParty(context.Background(), pool, everything(owner()), workshop.NewParty{
		Kind: "private", Name: "Kim Köpare", Phone: "0701234567", Role: workshop.PartyBuyer})
	if err != nil {
		t.Fatalf("CreateParty: %v", err)
	}
	return id
}

// carForSale is the stock car offered for sale.
func carForSale(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	p := buy(t, pool, workshop.StockFromPurchase)
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	return p.StockUnitID
}

func goodLicence() workshop.LicenceCheckInput {
	until := time.Now().AddDate(3, 0, 0)
	return workshop.LicenceCheckInput{IDChecked: true, Method: "card_inspected", Categories: []string{"b", "AM"}, ValidUntil: &until, AgeOK: true}
}

// A drive booked, the licence checked, the agreement signed: the keys
// leave with the odometer read and come back with it; the slot lets the
// car go as it returns. Each step in its order, and the same request sent
// twice is one drive.
func TestATestDriveGoesOutAndComesBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	lead, err := workshop.CreateLead(ctx, pool, sales(), workshop.NewLead{PartyID: kim, StockUnitID: unit, Channel: "walk_in"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	n := workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: kim, LeadID: lead.ID, From: now, To: now.Add(time.Hour),
		Area: "Inom Stockholm", Key: "td-1"}
	d, err := workshop.BookTestDrive(ctx, pool, sales(), n)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.BookTestDrive(ctx, pool, sales(), n); err != nil || again.ID != d.ID {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	if l, _ := workshop.LeadByID(ctx, pool, sales(), lead.ID); l.Status != workshop.LeadTestDriveBooked {
		t.Errorf("the lead after the drive was booked: %s", l.Status)
	}
	if err := workshop.StartTestDrive(ctx, pool, sales(), d.ID, 123_500, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("started with no licence checked: %v", err)
	}
	expired := goodLicence()
	yesterday := time.Now().AddDate(0, 0, -1)
	expired.ValidUntil = &yesterday
	if err := workshop.CheckDriverLicence(ctx, pool, sales(), d.ID, expired); err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartTestDrive(ctx, pool, sales(), d.ID, 123_500, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("started on an expired licence: %v", err)
	}
	if err := workshop.CheckDriverLicence(ctx, pool, sales(), d.ID, goodLicence()); err != nil {
		t.Fatal(err)
	}
	// The agreement is not signed: a salesperson cannot let it go, whoever
	// runs the shop can, with a note, which is audited.
	if err := workshop.StartTestDrive(ctx, pool, sales(), d.ID, 123_500, "Signeringstjänsten nere"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a salesperson started an unsigned drive: %v", err)
	}
	if err := workshop.StartTestDrive(ctx, pool, everything(owner()), d.ID, 123_500, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("started unsigned with no note: %v", err)
	}
	if err := workshop.StartTestDrive(ctx, pool, everything(owner()), d.ID, 123_500, "Signeringstjänsten nere"); err != nil {
		t.Fatal(err)
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event = 'test_drive.started_unsigned' AND subject_id = $1`, d.ID).Scan(&audited); err != nil || audited != 1 {
		t.Errorf("%d audit events for the unsigned start (%v)", audited, err)
	}
	if err := workshop.ReturnTestDrive(ctx, pool, sales(), d.ID, 123_400, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("returned with fewer kilometres: %v", err)
	}
	if err := workshop.ReturnTestDrive(ctx, pool, sales(), d.ID, 123_512, "Repa på bakre stötfångaren"); err != nil {
		t.Fatal(err)
	}
	got, err := workshop.TestDriveByID(ctx, pool, sales(), d.ID)
	if err != nil || got.State != workshop.DriveReturned || got.StartKm == nil || *got.StartKm != 123_500 || *got.ReturnKm != 123_512 ||
		got.ReturnedAt == nil || len(got.Notes) != 2 {
		t.Errorf("the drive after it came back: %+v, %v", got, err)
	}
	// The slot is let go: another drive can have the car now.
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: kim,
		From: time.Now(), To: time.Now().Add(30 * time.Minute)}); err != nil {
		t.Errorf("the car after the drive came back: %v", err)
	}
	// No odometer reading changed hands without a row.
	var readings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM odometer_readings WHERE source = 'test_drive'`).Scan(&readings); err != nil || readings != 2 {
		t.Errorf("%d readings from the drive (%v)", readings, err)
	}
}

// Two salespeople book the same car for the same slot at once: one drive
// stands. A car the workshop has, or one already out on a drive, is
// refused, naming what is in the way.
func TestTwoBookingsOfOneSlotGiveOneDrive(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	from := time.Now().Add(2 * time.Hour)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: kim,
				From: from.Add(time.Duration(i) * time.Minute), To: from.Add(time.Hour)})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, workshop.ErrInvalid):
			t.Errorf("a booking that lost: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d of %d overlapping drives stood; want one", ok, len(errs))
	}
	var drives int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM test_drives`).Scan(&drives); err != nil || drives != 1 {
		t.Errorf("%d drives (%v)", drives, err)
	}
}

// A car reserved for somebody else is a warning the salesperson books
// through, or a refusal where the shop says so. A sold car is not driven.
// The workshop's roles book nothing, and nothing is booked with car sales
// off.
func TestAReservedCarWarnsAndASoldCarIsNotDriven(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	moveStock(t, pool, unit, workshop.StockReserved)
	n := workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: kim, From: time.Now(), To: time.Now().Add(time.Hour)}
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), n); !errors.Is(err, workshop.ErrDriveReserved) {
		t.Errorf("a reserved car, no warning: %v", err)
	}
	if err := workshop.SetTestDriveRules(ctx, pool, sales(), 20, true); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson set the rules: %v", err)
	}
	if err := workshop.SetTestDriveRules(ctx, pool, everything(owner()), 20, true); err != nil {
		t.Fatal(err)
	}
	n.DespiteReservation = true
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), n); !errors.Is(err, workshop.ErrDriveReservedBlocked) {
		t.Errorf("a reserved car where the shop blocks it: %v", err)
	}
	if err := workshop.SetTestDriveRules(ctx, pool, everything(owner()), 20, false); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), n); err != nil {
		t.Errorf("a reserved car, booked through the warning: %v", err)
	}
	if age, blocks, err := workshop.TestDriveRules(ctx, pool, sales()); err != nil || age != 20 || blocks {
		t.Errorf("the rules: %d, %v, %v", age, blocks, err)
	}
	moveStock(t, pool, unit, workshop.StockSold)
	n.From, n.To = time.Now().Add(3*time.Hour), time.Now().Add(4*time.Hour)
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a sold car booked for a drive: %v", err)
	}
	for name, s := range map[string]access.Scope{"the front desk": everything(advisor()), "a technician": everything(technician())} {
		if _, err := workshop.BookTestDrive(ctx, pool, s, n); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s booked a drive: %v", name, err)
		}
	}
	off := sales()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.BookTestDrive(ctx, pool, off, n); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("booked with car sales off: %v", err)
	}
	for _, stmt := range []string{`DELETE FROM test_drives`, `UPDATE test_drives SET ends_at = now()`, `DELETE FROM test_drive_events`,
		`UPDATE test_drive_licence_checks SET age_ok = true`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}

// A drive out past its end is waiting for whoever sells cars.
func TestADriveNotBackIsWaitedFor(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	d, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: kim,
		From: time.Now(), To: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CheckDriverLicence(ctx, pool, sales(), d.ID, goodLicence()); err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartTestDrive(ctx, pool, everything(owner()), d.ID, 1000, "Papper saknas"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE test_drives SET ends_at = now() - interval '1 minute', starts_at = now() - interval '1 hour' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	items, err := workshop.WaitingFor(ctx, pool, sales())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		found = found || (it.Kind == "test_drive_overdue" && it.SubjectID == d.ID)
	}
	if !found {
		t.Errorf("the drive not back is not waited for: %+v", items)
	}
}

// unsignedDrive is a drive on a car of its own, started by whoever runs
// the shop before the agreement was signed.
func unsignedDrive(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	p := boughtCar(t, pool)
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	d, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: p.StockUnitID, DriverPartyID: driver(t, pool),
		From: time.Now(), To: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CheckDriverLicence(ctx, pool, sales(), d.ID, goodLicence()); err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartTestDrive(ctx, pool, everything(owner()), d.ID, 1000, "Papperet slut"); err != nil {
		t.Fatal(err)
	}
	return d.ID
}

// A person who asked about cars and drove one is given what the leads and
// the drives hold about them -- as a party, and where they were only a
// name with their number or email -- and erasing them clears it from both.
func TestLeadsAndTestDrivesAreInAPersonsExportAndErasure(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim, err := workshop.CreateParty(ctx, pool, everything(owner()), workshop.NewParty{
		Kind: "private", Name: "Kim Köpare", Phone: "070-123 45 67", Email: "kim@example.test", Role: workshop.PartyBuyer})
	if err != nil {
		t.Fatal(err)
	}
	var person string
	if err := pool.QueryRow(ctx, `SELECT person_id::text FROM parties WHERE id = $1`, kim).Scan(&person); err != nil {
		t.Fatal(err)
	}
	asParty, err := workshop.CreateLead(ctx, pool, sales(), workshop.NewLead{PartyID: kim, StockUnitID: unit, Channel: "walk_in",
		Note: "Kim vill ha dragkrok"})
	if err != nil {
		t.Fatal(err)
	}
	byNumber, err := workshop.CreateLead(ctx, pool, sales(), workshop.NewLead{Name: "Kim K", Phone: "+46 70 123 45 67",
		Channel: "phone", Wishes: "Kombi åt Kim", Alongside: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: kim, LeadID: asParty.ID,
		From: time.Now(), To: time.Now().Add(time.Hour), Area: "Inom stan"})
	if err != nil {
		t.Fatal(err)
	}
	check := goodLicence()
	check.Note = "Kims körkort är nytt"
	if err := workshop.CheckDriverLicence(ctx, pool, sales(), d.ID, check); err != nil {
		t.Fatal(err)
	}

	e, err := workshop.ExportPerson(ctx, pool, advisor(), person)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Leads) != 2 || len(e.TestDrives) != 1 || e.TestDrives[0].LicenceNote != "Kims körkort är nytt" {
		t.Errorf("the export: %d leads, %+v", len(e.Leads), e.TestDrives)
	}

	if err := workshop.ErasePerson(ctx, pool, advisor(), &recordingRemover{}, person, "Begäran"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{asParty.ID, byNumber.ID} {
		l, err := workshop.LeadByID(ctx, pool, everything(owner()), id)
		if err != nil || !l.Erased || l.Name != "" || l.Phone != "" || l.Wishes != "" || l.PartyID != "" ||
			(len(l.Contacts) > 0 && l.Contacts[0].Note != "") {
			t.Errorf("lead %s after the erasure: %+v, %v", id, l, err)
		}
	}
	var note *string
	if err := pool.QueryRow(ctx, `SELECT note FROM test_drive_licence_checks WHERE test_drive_id = $1`, d.ID).Scan(&note); err != nil || note != nil {
		t.Errorf("the licence check's note after the erasure: %v, %v", note, err)
	}
}

// A walk-in driver is a party made with the drive, in its transaction:
// the booking sent twice makes one driver, and a booking refused makes
// none, so no stranger's details are left behind with nothing to show why.
func TestAWalkInDriverIsMadeWithTheDrive(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	parties := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM parties`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := parties()
	n := workshop.NewTestDrive{StockUnitID: unit, NewDriver: &workshop.NewParty{Name: "Lo Letare", Phone: "0731234567"},
		From: time.Now(), To: time.Now().Add(time.Hour), Key: "walk-in-1"}
	first, err := workshop.BookTestDrive(ctx, pool, sales(), n)
	if err != nil || first.Driver != "Lo Letare" {
		t.Fatalf("the walk-in drive: %+v, %v", first, err)
	}
	if again, err := workshop.BookTestDrive(ctx, pool, sales(), n); err != nil || again.ID != first.ID {
		t.Errorf("sent again: %+v, %v", again, err)
	}
	if got := parties(); got != before+1 {
		t.Errorf("%d parties made for one walk-in driver", got-before)
	}
	moveStock(t, pool, unit, workshop.StockReserved, workshop.StockSold)
	n.Key, n.From, n.To = "walk-in-2", time.Now().Add(2*time.Hour), time.Now().Add(3*time.Hour)
	if _, err := workshop.BookTestDrive(ctx, pool, sales(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("a sold car booked: %v", err)
	}
	if got := parties(); got != before+1 {
		t.Errorf("a refused booking left %d parties behind", got-before-1)
	}
}

// A car sold to someone else after its drive was booked does not leave on
// that drive; one reserved meanwhile does not where the shop blocks a
// reserved car.
func TestADriveDoesNotStartOnACarSoldSinceItWasBooked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	d, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: driver(t, pool),
		From: time.Now(), To: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CheckDriverLicence(ctx, pool, sales(), d.ID, goodLicence()); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetTestDriveRules(ctx, pool, everything(owner()), 18, true); err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, unit, workshop.StockReserved)
	if err := workshop.StartTestDrive(ctx, pool, everything(owner()), d.ID, 1000, "Papper saknas"); !errors.Is(err, workshop.ErrDriveReservedBlocked) {
		t.Errorf("started on a car reserved since, where the shop blocks it: %v", err)
	}
	moveStock(t, pool, unit, workshop.StockSold)
	if err := workshop.StartTestDrive(ctx, pool, everything(owner()), d.ID, 1000, "Papper saknas"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("started on a car sold since: %v", err)
	}
	if got, _ := workshop.TestDriveByID(ctx, pool, sales(), d.ID); got.State != workshop.DriveBooked {
		t.Errorf("the drive after the refusals: %s", got.State)
	}
}

// "Make the agreement" pressed twice at once, or again on a page that
// showed no agreement yet, makes one: the second finds the first's.
func TestAnAgreementMadeTwiceAtOnceIsOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	d, err := workshop.BookTestDrive(ctx, pool, sales(), workshop.NewTestDrive{StockUnitID: unit, DriverPartyID: driver(t, pool),
		From: time.Now(), To: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	tpls, err := workshop.TestDriveTemplates(ctx, pool, sales())
	if err != nil || len(tpls) != 1 {
		t.Fatalf("the shipped template: %+v, %v", tpls, err)
	}
	var wg sync.WaitGroup
	ids := make([]string, 4)
	errs := make([]error, 4)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = workshop.ProduceTestDriveAgreement(ctx, pool, sales(), d.ID, tpls[0].VersionID, "")
		}()
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Errorf("press %d: %s, %v; want the one agreement", i, ids[i], errs[i])
		}
	}
	var docs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM document_records WHERE subject_id = $1`, d.ID).Scan(&docs); err != nil || docs != 1 {
		t.Errorf("%d agreements made (%v)", docs, err)
	}
	// Made again from the page that showed it: a new version superseding it.
	again, err := workshop.ProduceTestDriveAgreement(ctx, pool, sales(), d.ID, tpls[0].VersionID, ids[0])
	if err != nil || again == ids[0] {
		t.Errorf("made again: %s, %v", again, err)
	}
	if got, _ := workshop.TestDriveByID(ctx, pool, sales(), d.ID); got.AgreementID != again {
		t.Errorf("the drive's agreement: %s, want %s", got.AgreementID, again)
	}
}
