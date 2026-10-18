package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Keeping the customer and vehicle records straight between visits: what a
// correction leaves behind, what it must not touch, and what a sale, a new
// plate and a typing mistake are -- three different things.

const fixtureCustomer = "44444444-4444-4444-4444-444444444444"

func cardOf(t *testing.T, pool *pgxpool.Pool, id string) workshop.CustomerCard {
	t.Helper()
	c, err := workshop.CustomerByID(context.Background(), pool, advisor(), id)
	if err != nil {
		t.Fatalf("CustomerByID: %v", err)
	}
	return c
}

func editOf(c workshop.CustomerCard) workshop.CustomerEdit {
	return workshop.CustomerEdit{
		Name: c.Name, ContactName: c.ContactName, OrgNumber: c.OrgNumber, VATNumber: c.VATNumber,
		Phone: c.Phone, Email: c.Email, AddressLine1: c.AddressLine1, AddressLine2: c.AddressLine2,
		PostalCode: c.PostalCode, City: c.City,
		Country: c.Country, EndpointID: c.EndpointID, BuyerReference: c.BuyerReference,
	}
}

func companyWithContact(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := workshop.CreateCustomer(context.Background(), pool, advisor(), workshop.NewCustomer{
		Kind: "company", Name: "Andersson Åkeri AB", ContactName: "Anna Andersson", OrgNumber: "556677-8899", Phone: "070-111 22 33"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAChangedPhoneNumberIsCorrectedAndRemembered(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	inv := invoicesOf(t, pool, job)

	c := cardOf(t, pool, fixtureCustomer)
	e := editOf(c)
	e.Phone, e.Reason = "070-999 88 77", "Rang to say the number changed"
	changes, err := workshop.EditCustomer(ctx, pool, advisor(), c.ID, c.Revision, e)
	if err != nil || len(changes) != 1 || changes[0].Field != "phone" {
		t.Fatalf("EditCustomer = %v, %v", changes, err)
	}
	after := cardOf(t, pool, c.ID)
	if after.Phone != "070-999 88 77" || after.Revision != c.Revision+1 {
		t.Errorf("after the edit: phone %q, revision %d", after.Phone, after.Revision)
	}
	if len(after.Changes) != 1 || after.Changes[0].By == "" || after.Changes[0].Reason == "" {
		t.Errorf("the change is not recorded with who and why: %+v", after.Changes)
	}

	// Saving what is already there is not an error, even from an old form.
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), c.ID, c.Revision, e); err != nil {
		t.Errorf("a retried save = %v, want quiet success", err)
	}
	if got := cardOf(t, pool, c.ID); got.Revision != after.Revision {
		t.Error("a no-op save bumped the revision")
	}

	// The issued invoice still says who it went to.
	if now := invoicesOf(t, pool, job); now != inv {
		t.Errorf("an issued invoice changed: %q -> %q", inv, now)
	}
	// A rename does not reach it either.
	e2 := editOf(after)
	e2.Name = "Annika Andersson"
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), c.ID, after.Revision, e2); err != nil {
		t.Fatal(err)
	}
	if now := invoicesOf(t, pool, job); now != inv {
		t.Errorf("renaming the customer rewrote an issued invoice: %q", now)
	}
}

func invoicesOf(t *testing.T, pool *pgxpool.Pool, job string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(string_agg(customer_name, ','), '') FROM invoices WHERE work_order_id = $1`, job).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTwoPeopleEditingTheSameCustomerAreToldApart(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := cardOf(t, pool, fixtureCustomer)
	a, b := editOf(c), editOf(c)
	a.Phone, b.Phone = "070-111 11 11", "070-222 22 22"
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), c.ID, c.Revision, a); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), c.ID, c.Revision, b); !errors.Is(err, workshop.ErrRecordChanged) {
		t.Errorf("the second save = %v, want ErrRecordChanged", err)
	}
	if got := cardOf(t, pool, c.ID).Phone; got != "070-111 11 11" {
		t.Errorf("the phone is %q after the refused save", got)
	}
}

func TestACorrectionIsCheckedAndOnlyForTheDesk(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	c := cardOf(t, pool, fixtureCustomer)
	for name, mut := range map[string]func(*workshop.CustomerEdit){
		"no name":     func(e *workshop.CustomerEdit) { e.Name = "  " },
		"bad email":   func(e *workshop.CustomerEdit) { e.Email = "anna at example" },
		"short phone": func(e *workshop.CustomerEdit) { e.Phone = "12" },
		"bad org":     func(e *workshop.CustomerEdit) { e.OrgNumber = "55!66" },
	} {
		e := editOf(c)
		mut(&e)
		if _, err := workshop.EditCustomer(ctx, pool, advisor(), c.ID, c.Revision, e); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := workshop.EditCustomer(ctx, pool, technician(), c.ID, c.Revision, editOf(c)); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician edited a customer: %v", err)
	}
	if _, err := workshop.CustomerByID(ctx, pool, technician(), c.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician opened a customer card: %v", err)
	}
	if _, err := workshop.BrowseCustomers(ctx, pool, technician(), ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician browsed the customers: %v", err)
	}
	if _, err := workshop.EditVehicle(ctx, pool, technician(), vehicleA, 1, workshop.VehicleEdit{Make: "Saab"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician edited a vehicle: %v", err)
	}
}

func TestACompanyPayerHasSeveralContactsApartFromTheOwner(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	company := companyWithContact(t, pool)
	job := newJob(t, pool)
	// The company pays; the person who owns the car is somebody else.
	if err := workshop.AssignCustomer(ctx, pool, advisor(), job, company, false); err != nil {
		t.Fatal(err)
	}
	c := cardOf(t, pool, company)
	if c.Private() || c.ContactName != "Anna Andersson" || c.Phone != "070-111 22 33" {
		t.Fatalf("company card = %+v", c.Customer)
	}
	if len(c.Jobs) != 1 || len(c.Vehicles) != 0 {
		t.Errorf("the payer has %d jobs and %d vehicles; paying is not owning", len(c.Jobs), len(c.Vehicles))
	}
	if err := workshop.AddCustomerPerson(ctx, pool, advisor(), company, "Bertil Berg", "Fleet manager", "070-333 44 55", "bertil@example.com"); err != nil {
		t.Fatal(err)
	}
	c = cardOf(t, pool, company)
	if len(c.People) != 1 || c.People[0].Title != "Fleet manager" {
		t.Fatalf("people = %+v", c.People)
	}
	if err := workshop.RemoveCustomerPerson(ctx, pool, advisor(), company, c.People[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := cardOf(t, pool, company); len(got.People) != 0 || len(got.Changes) < 2 {
		t.Errorf("after removing: %d people, %d changes", len(got.People), len(got.Changes))
	}
	// A person is not a company, and has no other people.
	if err := workshop.AddCustomerPerson(ctx, pool, advisor(), fixtureCustomer, "Someone", "", "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("adding a person to a person = %v, want ErrInvalid", err)
	}
	// A company whose contact is renamed keeps the company's own name.
	e := editOf(c)
	e.ContactName, e.OrgNumber = "Annika Andersson", "5566778899"
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), company, c.Revision, e); err != nil {
		t.Fatal(err)
	}
	if got := cardOf(t, pool, company); got.Name != "Andersson Åkeri AB" || got.ContactName != "Annika Andersson" || got.OrgNumber != "556677-8899" {
		t.Errorf("after the edit: %q / %q / %q", got.Name, got.ContactName, got.OrgNumber)
	}
}

func TestASoldVehicleKeepsItsHistoryAndTheNewOwnerGetsNoneOfTheOldOnesDetails(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	old := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), old); err != nil {
		t.Fatal(err)
	}
	buyer, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "Nils Nilsson", Phone: "070-555 66 77"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SellVehicle(ctx, pool, advisor(), vehicleA, buyer, time.Time{}, "Sold privately"); err != nil {
		t.Fatalf("SellVehicle: %v", err)
	}
	if err := workshop.SellVehicle(ctx, pool, advisor(), vehicleA, buyer, time.Time{}, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("selling to the owner = %v, want ErrInvalid", err)
	}

	v, err := workshop.VehicleByID(ctx, pool, advisor(), vehicleA)
	if err != nil {
		t.Fatal(err)
	}
	if v.OwnerName != "Nils Nilsson" {
		t.Errorf("owner = %q", v.OwnerName)
	}
	var sawOld bool
	for _, h := range v.History {
		if h.WorkOrderID == old {
			sawOld = true
			if !h.PreviousOwner || h.CustomerName != "" || h.TotalMinor() != nil {
				t.Errorf("the new owner is shown the old one's job: %+v", h)
			}
		}
	}
	if !sawOld {
		t.Error("the sale hid the car's history")
	}
	// The old owner keeps their side: the ownership period is closed, not gone.
	card := cardOf(t, pool, fixtureCustomer)
	if len(card.Vehicles) != 1 || card.Vehicles[0].Current() || len(card.Jobs) == 0 {
		t.Errorf("the old owner's card after the sale: %+v", card.Vehicles)
	}
	// A new job for the car is the new owner's.
	next := newJob(t, pool)
	j, _, _ := workshop.JobByID(ctx, pool, advisor(), next)
	if j.VehicleID != vehicleA {
		t.Fatalf("the new job is on another vehicle")
	}
	var cust string
	if err := pool.QueryRow(ctx, `SELECT coalesce(customer_id::text, '') FROM work_orders WHERE id = $1`, next).Scan(&cust); err != nil {
		t.Fatal(err)
	}
	if cust != buyer {
		t.Errorf("a new job for the sold car was filled in with %q, want the buyer", cust)
	}
	if ch, _ := workshop.VehicleChanges(ctx, pool, advisor(), vehicleA); len(ch) != 1 || ch[0].New != "Nils Nilsson" {
		t.Errorf("the sale is not in the log: %+v", ch)
	}
	// A date before they bought it, or in the future, is refused.
	if err := workshop.SellVehicle(ctx, pool, advisor(), vehicleA, fixtureCustomer, time.Now().Add(-48*time.Hour), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a sale dated before the owner bought it = %v", err)
	}
	if err := workshop.SellVehicle(ctx, pool, advisor(), vehicleA, fixtureCustomer, time.Now().Add(48*time.Hour), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a sale dated in the future = %v", err)
	}
}

func TestAMistakenPlateIsRewrittenAndANewPlateIsAdded(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	plates := func() []string {
		v, err := workshop.VehicleByID(ctx, pool, advisor(), vehicleA)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range v.Registrations {
			out = append(out, r.Registration)
		}
		return out
	}
	if err := workshop.CorrectRegistration(ctx, pool, advisor(), vehicleA, "abc 12e", "Misread the O"); err != nil {
		t.Fatal(err)
	}
	if got := plates(); len(got) != 1 || got[0] != "ABC 12E" {
		t.Fatalf("after correcting the plates are %v; the wrong one was never carried", got)
	}
	// It is found by the right plate now, and not by the wrong one.
	if r, _ := workshop.Search(ctx, pool, advisor(), "ABC12E"); len(r) != 1 {
		t.Errorf("searching the right plate found %d", len(r))
	}
	if r, _ := workshop.Search(ctx, pool, advisor(), "ABC12D"); len(r) != 0 {
		t.Errorf("searching the wrong plate found %d", len(r))
	}

	time.Sleep(10 * time.Millisecond)
	if err := workshop.ChangeRegistration(ctx, pool, advisor(), vehicleA, "XYZ 999", time.Time{}, "Personalised plate"); err != nil {
		t.Fatal(err)
	}
	if got := plates(); len(got) != 2 {
		t.Fatalf("after a real change the plates are %v, want both", got)
	}
	// The old plate still finds the car, which is how a service book is read.
	if r, _ := workshop.Search(ctx, pool, advisor(), "ABC12E"); len(r) != 1 {
		t.Errorf("the plate it used to carry found %d", len(r))
	}
	if err := workshop.ChangeRegistration(ctx, pool, advisor(), vehicleA, "xyz999", time.Time{}, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("changing to the plate it has = %v, want ErrInvalid", err)
	}
	ch, _ := workshop.VehicleChanges(ctx, pool, advisor(), vehicleA)
	if len(ch) != 2 {
		t.Errorf("%d changes logged, want the correction and the change", len(ch))
	}
}

func TestAPlateOnAnotherVehicleIsReportedAsADuplicate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	other := newOtherVehicle(t, pool, "KLM 456")
	err := workshop.CorrectRegistration(ctx, pool, advisor(), vehicleA, "KLM456", "")
	var dup *workshop.DuplicateError
	if !errors.As(err, &dup) || dup.ID != other || !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("correcting onto another vehicle's plate = %v, want a duplicate pointing at it", err)
	}
	// The same chassis number on two vehicles is the same kind of finding.
	v1, _ := workshop.VehicleRevision(ctx, pool, advisor(), vehicleA)
	if _, err := workshop.EditVehicle(ctx, pool, advisor(), vehicleA, v1, workshop.VehicleEdit{Make: "Volvo", Model: "V70", VIN: "YV1SW61N123456789"}); err != nil {
		t.Fatal(err)
	}
	rev, _ := workshop.VehicleRevision(ctx, pool, advisor(), other)
	_, err = workshop.EditVehicle(ctx, pool, advisor(), other, rev, workshop.VehicleEdit{VIN: "yv1sw61n123456789"})
	if !errors.As(err, &dup) || dup.ID != vehicleA {
		t.Errorf("the same VIN twice = %v, want a duplicate pointing at the first", err)
	}
	for _, vin := range []string{"YV1SW61N12345678I", "12", "ÅÄÖ12345"} {
		if _, err := workshop.EditVehicle(ctx, pool, advisor(), other, rev, workshop.VehicleEdit{VIN: vin}); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("VIN %q = %v, want ErrInvalid", vin, err)
		}
	}
}

func newOtherVehicle(t *testing.T, pool *pgxpool.Pool, plate string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO vehicles (shop_id, make) VALUES ($1, 'Saab') RETURNING id`, shopID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised) VALUES ($1, $2, $3, $4)`,
		shopID, id, plate, workshop.NormaliseRegistration(plate)); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestErasingAPersonBlanksTheLogOfTheirCorrectionsButNotThatTheyHappened(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := cardOf(t, pool, fixtureCustomer)
	e := editOf(c)
	e.Phone = "070-444 55 66"
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), c.ID, c.Revision, e); err != nil {
		t.Fatal(err)
	}
	ex, err := workshop.ExportPerson(ctx, pool, advisor(), c.PersonID)
	if err != nil || len(ex.Corrections) != 1 || ex.Corrections[0].Now != "070-444 55 66" {
		t.Fatalf("the export does not carry the correction: %v %+v", err, ex.Corrections)
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), c.PersonID, "Asked"); err != nil {
		t.Fatal(err)
	}
	got := cardOf(t, pool, c.ID)
	if len(got.Changes) != 1 || got.Changes[0].New != "" || got.Changes[0].Old != "" {
		t.Errorf("after erasure the log still holds values: %+v", got.Changes)
	}
	if strings.Contains(got.Changes[0].Field, "070") {
		t.Error("the field name leaks")
	}
}
