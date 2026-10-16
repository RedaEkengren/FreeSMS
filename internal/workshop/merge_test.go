package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Two records that are one, and two that only look alike. People are never
// matched by name alone; a merge says what it moves first and leaves issued
// documents as they were issued.

func TestLikelyDuplicatesAreFoundByIdentifierNeverByNameAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	company := companyWithContact(t, pool)
	_ = company

	// Same name, nothing else shared: a different Anna.
	if d, err := workshop.CustomerDuplicates(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "A Customer"}); err != nil || len(d) != 0 {
		t.Errorf("the same name alone = %v, %v; want nothing", d, err)
	}
	cases := []struct {
		name string
		in   workshop.NewCustomer
		why  string
	}{
		{"company number, written another way", workshop.NewCustomer{Kind: "company", Name: "Andersson Akeri", OrgNumber: "5566778899"}, "the same company number"},
		{"telephone, punctuated another way", workshop.NewCustomer{Kind: "private", Name: "Someone Else", Phone: "0701112233"}, "the same telephone number"},
	}
	for _, c := range cases {
		d, err := workshop.CustomerDuplicates(ctx, pool, advisor(), c.in)
		if err != nil || len(d) != 1 || len(d[0].Why) != 1 || d[0].Why[0] != c.why {
			t.Errorf("%s: %v, %v; want one match for %q", c.name, d, err, c.why)
		}
	}
	// An email on a person at the company counts.
	if err := workshop.AddCustomerPerson(ctx, pool, advisor(), company, "Bertil Berg", "Fleet", "", "Bertil@Example.com"); err != nil {
		t.Fatal(err)
	}
	if d, _ := workshop.CustomerDuplicates(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "B. Berg", Email: "bertil@example.com"}); len(d) != 1 || d[0].Why[0] != "the same email address" {
		t.Errorf("a colleague's email = %v", d)
	}

	// Asked first, nothing is written; confirmed, it is.
	before := countRows(t, pool, `SELECT count(*) FROM customers`)
	in := workshop.NewCustomer{Kind: "private", Name: "Family Member", Phone: "070-111 22 33"}
	id, dups, err := workshop.CreateCustomerChecked(ctx, pool, advisor(), in, false)
	if err != nil || id != "" || len(dups) != 1 {
		t.Fatalf("CreateCustomerChecked = %q, %v, %v", id, dups, err)
	}
	if countRows(t, pool, `SELECT count(*) FROM customers`) != before {
		t.Error("asking about duplicates created a customer")
	}
	if id, _, err = workshop.CreateCustomerChecked(ctx, pool, advisor(), in, true); err != nil || id == "" {
		t.Errorf("a confirmed create = %q, %v", id, err)
	}
	if _, err := workshop.CustomerDuplicates(ctx, pool, technician(), in); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician looked for duplicates: %v", err)
	}
}

func TestMergingTwoCustomersMovesTheirWorkAndLeavesIssuedInvoicesAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	// The same person entered twice, once with a number and once with an email.
	keep := fixtureCustomer
	dup, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "A. Customer", Email: "a@example.com", Phone: "070-222 33 44"})
	if err != nil {
		t.Fatal(err)
	}
	job := readyToInvoice(t, pool)
	if err := workshop.AssignCustomer(ctx, pool, advisor(), job, dup, false); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	named := invoicesOf(t, pool, job)

	pv, err := workshop.PreviewCustomerMerge(ctx, pool, advisor(), keep, dup)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Jobs != 1 || pv.Invoices != 1 || len(pv.FillsIn) != 2 {
		t.Errorf("preview = jobs %d, invoices %d, fills %v", pv.Jobs, pv.Invoices, pv.FillsIn)
	}
	if countRows(t, pool, `SELECT count(*) FROM customers WHERE merged_into IS NOT NULL`) != 0 {
		t.Fatal("previewing merged something")
	}
	// A form opened before somebody edited the kept record is refused.
	if _, err := workshop.MergeCustomers(ctx, pool, advisor(), keep, dup, pv.KeepRevision+1, pv.DropRevision, ""); !errors.Is(err, workshop.ErrRecordChanged) {
		t.Errorf("a stale merge = %v, want ErrRecordChanged", err)
	}
	if _, err := workshop.MergeCustomers(ctx, pool, technician(), keep, dup, pv.KeepRevision, pv.DropRevision, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician merged: %v", err)
	}
	if _, err := workshop.MergeCustomers(ctx, pool, advisor(), keep, dup, pv.KeepRevision, pv.DropRevision, "Same person"); err != nil {
		t.Fatalf("MergeCustomers: %v", err)
	}

	card := cardOf(t, pool, keep)
	if len(card.Jobs) != 1 || card.Phone != "070-222 33 44" || card.Email != "a@example.com" {
		t.Errorf("the kept card: %d jobs, phone %q, email %q", len(card.Jobs), card.Phone, card.Email)
	}
	if len(card.Merged) != 1 || card.Changes[0].Field != "merged in" || !strings.Contains(card.Changes[0].New, "1 jobs") {
		t.Errorf("merged = %+v", card.Merged)
	}
	gone := cardOf(t, pool, dup)
	if gone.MergedInto != keep {
		t.Errorf("the merged record points at %q", gone.MergedInto)
	}
	if found, _ := workshop.FindCustomers(ctx, pool, advisor(), "Customer"); len(found) != 1 {
		t.Errorf("the search finds %d after the merge, want 1", len(found))
	}
	if invoicesOf(t, pool, job) != named {
		t.Error("the merge rewrote an issued invoice")
	}
	// Editing a merged-away record is refused, and merging it again too.
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), dup, gone.Revision, editOf(gone)); !errors.Is(err, workshop.ErrInvalid) && err != nil {
		t.Errorf("editing a merged record = %v", err)
	}
	if _, err := workshop.PreviewCustomerMerge(ctx, pool, advisor(), keep, dup); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("merging twice = %v, want ErrInvalid", err)
	}
	// A person and a company are never one.
	company := companyWithContact(t, pool)
	if _, err := workshop.PreviewCustomerMerge(ctx, pool, advisor(), keep, company); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a person into a company = %v, want ErrInvalid", err)
	}
	if _, err := workshop.PreviewCustomerMerge(ctx, pool, advisor(), keep, keep); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a customer into itself = %v, want ErrInvalid", err)
	}
}

func TestMergingTwoVehiclesJoinsTheirHistoryAndKeepsOneCurrentPlate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	// The car was taken in a second time under a misread plate.
	dup := newOtherVehicle(t, pool, "ABC 12O")
	if _, err := pool.Exec(ctx, `UPDATE vehicles SET model = 'V70' WHERE id = $1`, dup); err != nil {
		t.Fatal(err)
	}
	old, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12O", "Squeal", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = old
	pv, err := workshop.PreviewVehicleMerge(ctx, pool, advisor(), vehicleA, dup)
	if err != nil || pv.Blocked != "" || pv.Jobs != 1 || pv.Plates != 1 {
		t.Fatalf("preview = %+v, %v", pv, err)
	}
	if _, err := workshop.MergeVehicles(ctx, pool, advisor(), vehicleA, dup, pv.KeepRevision, pv.DropRevision, "Misread plate"); err != nil {
		t.Fatalf("MergeVehicles: %v", err)
	}
	v, _ := workshop.VehicleByID(ctx, pool, advisor(), vehicleA)
	if v.Registration != "ABC 12D" || len(v.Registrations) != 2 || len(v.History) != 1 {
		t.Errorf("after the merge: plate %q, %d plates, %d jobs", v.Registration, len(v.Registrations), len(v.History))
	}
	if r, _ := workshop.Search(ctx, pool, advisor(), "ABC12O"); len(r) != 1 || r[0].VehicleID != vehicleA {
		t.Errorf("the old plate finds %+v", r)
	}
	gone, _ := workshop.VehicleByID(ctx, pool, advisor(), dup)
	if gone.MergedInto != vehicleA {
		t.Errorf("the merged vehicle points at %q", gone.MergedInto)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM vehicle_registrations WHERE vehicle_id = $1 AND valid_to IS NULL`, vehicleA); n != 1 {
		t.Errorf("%d current plates after the merge, want 1", n)
	}
	if _, err := workshop.MergeVehicles(ctx, pool, technician(), vehicleA, dup, 1, 1, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician merged vehicles: %v", err)
	}
}

func TestTwoVehiclesThatAreTwoCarsAreNotMerged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	other := newOtherVehicle(t, pool, "KLM 456")
	r1, _ := workshop.VehicleRevision(ctx, pool, advisor(), vehicleA)
	r2, _ := workshop.VehicleRevision(ctx, pool, advisor(), other)
	if _, err := workshop.EditVehicle(ctx, pool, advisor(), vehicleA, r1, workshop.VehicleEdit{Make: "Volvo", Model: "V70", VIN: "YV1SW61N123456789"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.EditVehicle(ctx, pool, advisor(), other, r2, workshop.VehicleEdit{Make: "Saab", VIN: "YS3FD45S111222333"}); err != nil {
		t.Fatal(err)
	}
	pv, err := workshop.PreviewVehicleMerge(ctx, pool, advisor(), vehicleA, other)
	if err != nil || pv.Blocked == "" {
		t.Errorf("different chassis numbers: %+v, %v; want blocked", pv, err)
	}
	if _, err := workshop.MergeVehicles(ctx, pool, advisor(), vehicleA, other, pv.KeepRevision, pv.DropRevision, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("merging two cars = %v, want ErrInvalid", err)
	}
	// Two owners at once cannot be put together either.
	third := newOtherVehicle(t, pool, "NOP 789")
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopID, third, fixtureCustomer); err != nil {
		t.Fatal(err)
	}
	pv, err = workshop.PreviewVehicleMerge(ctx, pool, advisor(), vehicleA, third)
	if err != nil || !strings.Contains(pv.Blocked, "owner at the same time") {
		t.Errorf("two current owners: %+v, %v", pv, err)
	}
}
