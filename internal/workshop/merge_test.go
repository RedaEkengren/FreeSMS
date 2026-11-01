package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// An invoice that is not a job's names its own customer, and keeps it: it
// is immutable as issued, so a merge does not rewrite it. It is read through
// the merge instead -- the kept card counts it, the worklist sees it as the
// same customer's, the kept person's export lists it.
func TestAMergedCustomersOwnInvoicesAreReadAsTheKeptOnes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	keep := fixtureCustomer
	dup, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "A. Customer", Email: "dup@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	line := []workshop.SourcedLine{{Description: "Vinterhjul", QuantityMilli: 1000, UnitPriceMinor: 100000, VATRateBasis: 2500}}
	if _, err := workshop.IssueManual(ctx, pool, advisor(), workshop.ManualInvoice{CustomerID: keep, Lines: line}); err != nil {
		t.Fatal(err)
	}
	theirs, err := workshop.IssueManual(ctx, pool, advisor(), workshop.ManualInvoice{CustomerID: dup, Lines: line})
	if err != nil {
		t.Fatal(err)
	}
	before := cardOf(t, pool, keep).Invoices

	pv, err := workshop.PreviewCustomerMerge(ctx, pool, advisor(), keep, dup)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Invoices != 1 {
		t.Errorf("the preview counts %d of the other's invoices, want 1", pv.Invoices)
	}
	if _, err := workshop.MergeCustomers(ctx, pool, advisor(), keep, dup, pv.KeepRevision, pv.DropRevision, "Same person"); err != nil {
		t.Fatal(err)
	}
	if _, _, customer := sourceRow(t, pool, theirs.ID); customer != dup {
		t.Errorf("the merge rewrote the issued invoice's customer to %s", customer)
	}
	if got := cardOf(t, pool, keep).Invoices; got != before+1 {
		t.Errorf("the kept card counts %d invoices, want %d", got, before+1)
	}
	items, err := workshop.Worklist(ctx, pool, advisor(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.OtherOpen != 1 {
			t.Errorf("%s: %d others open for the same customer, want 1", it.Reference, it.OtherOpen)
		}
	}
	exp, err := workshop.ExportPerson(ctx, pool, advisor(), customerPersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Invoices) != 2 {
		t.Errorf("the kept person's export lists %d invoices, want both", len(exp.Invoices))
	}
}

// A finished protocol keeps the vehicle it was made of; merging that car
// into another leaves it so, and the kept car's protocols include it.
func TestAMergedVehiclesProtocolsAreReadAsTheKeptOnes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	dup := newOtherVehicle(t, pool, "ABC 12O")
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	id, err := workshop.StartProtocol(ctx, pool, desk, workshop.ProtocolSubject{Kind: workshop.SubjectCheckOut,
		RefID: rentalA, VehicleID: dup}, rentalChecklist(t, pool), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CompleteProtocol(ctx, pool, desk, id); err != nil {
		t.Fatal(err)
	}
	pv, err := workshop.PreviewVehicleMerge(ctx, pool, advisor(), vehicleA, dup)
	if err != nil || pv.Blocked != "" {
		t.Fatalf("preview = %+v, %v", pv, err)
	}
	if _, err := workshop.MergeVehicles(ctx, pool, advisor(), vehicleA, dup, pv.KeepRevision, pv.DropRevision, "Misread plate"); err != nil {
		t.Fatalf("MergeVehicles with a finished protocol on the other: %v", err)
	}
	ps, err := workshop.ProtocolsForVehicle(ctx, pool, desk, vehicleA)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].ID != id {
		t.Fatalf("the kept car's protocols are %+v, want the other's check-out", ps)
	}
	if ps[0].VehicleID != dup || ps[0].Registration != "ABC 12D" {
		t.Errorf("the protocol reads vehicle %s, plate %q; want the vehicle it was made of, under the kept car's plate", ps[0].VehicleID, ps[0].Registration)
	}
}

// A car the business books is merged with the bookings in mind. One kept
// that is booked ahead refuses the other's open job as any job is refused
// -- naming the rental, not overlapping periods -- and one merged away
// would leave its resource and its bookings on a record nothing reads, so
// it is the one kept instead.
func TestAVehicleMergeMindsTheResourcesBookings(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	res, err := workshop.CreateResource(ctx, pool, rentalDesk(), workshop.NewResource{VehicleID: vehicleA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rent(pool, rentalDesk(), res, in2031(14, 9), in2031(14, 17)); err != nil {
		t.Fatal(err)
	}
	dup := newOtherVehicle(t, pool, "ABC 12O")
	if _, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12O", "Squeal", nil); err != nil {
		t.Fatal(err)
	}
	pv, err := workshop.PreviewVehicleMerge(ctx, pool, advisor(), vehicleA, dup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.MergeVehicles(ctx, pool, advisor(), vehicleA, dup, pv.KeepRevision, pv.DropRevision, "Felläst skylt"); !errors.Is(err, workshop.ErrResourceTaken) ||
		err.Error() != "workshop: invalid: it is taken then by a rental" {
		t.Errorf("an open job moved onto a car rented out ahead: %v", err)
	}

	pv, err = workshop.PreviewVehicleMerge(ctx, pool, advisor(), dup, vehicleA)
	if err != nil || pv.Blocked != "The other is a car the business books. Keep it, and merge this one into it." {
		t.Errorf("the resource merged away: %+v, %v", pv, err)
	}
	if _, err := workshop.MergeVehicles(ctx, pool, advisor(), dup, vehicleA, pv.KeepRevision, pv.DropRevision, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the resource merged away: %v, want refused", err)
	}
}
