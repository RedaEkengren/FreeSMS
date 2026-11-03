package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Parties (#148). The shop here runs every module by its scope, as a
// session in such a shop would carry, since car sales and rental have no
// pages yet.

func partyOf(t *testing.T, pool *pgxpool.Pool, customer string) string {
	t.Helper()
	id, err := workshop.PartyOfCustomer(context.Background(), pool, advisor(), customer)
	if err != nil {
		t.Fatalf("PartyOfCustomer: %v", err)
	}
	return id
}

// privateSeller is a person who sells the dealer a car and is no customer.
func privateSeller(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id, err := workshop.CreateParty(context.Background(), pool, otherRoles(access.Of(access.RoleBuyer)), workshop.NewParty{
		Kind: "private", Name: "Sven Säljare", Phone: "070-555 66 77", Email: "sven@example.test",
		AddressLine1: "Storgatan 1", PostalCode: "123 45", City: "Kvarnby", Role: workshop.PartySeller,
	})
	if err != nil {
		t.Fatalf("CreateParty: %v", err)
	}
	return id
}

func rolesOf(p workshop.Party, role string) (open, all int) {
	for _, r := range p.Roles {
		if r.Role == role {
			all++
			if r.Open() {
				open++
			}
		}
	}
	return open, all
}

// Every customer is exactly one party: those there were before, one made
// now, and one a release that knows nothing of parties writes -- in the
// customer's own transaction, by the database. A party is never a second
// customer's.
func TestEveryCustomerIsExactlyOneParty(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p, err := workshop.PartyByID(ctx, pool, advisor(), partyOf(t, pool, fixtureCustomer))
	if err != nil {
		t.Fatal(err)
	}
	if p.CustomerID != fixtureCustomer || p.Name == "" {
		t.Errorf("the fixture customer's party reads %+v", p)
	}
	if open, _ := rolesOf(p, workshop.PartyCustomer); open != 1 {
		t.Errorf("the party has %d open customer roles, want 1", open)
	}

	made, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "company", Name: "Åkeri AB"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := workshop.PartyByID(ctx, pool, advisor(), partyOf(t, pool, made)); err != nil || p.Kind != "company" || p.Name != "Åkeri AB" {
		t.Errorf("a new customer's party: %+v, %v", p, err)
	}

	var raw string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO customers (shop_id, kind, company_name) VALUES ($1, 'company', 'Gammal AB') RETURNING id`, shopID).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	partyOf(t, pool, raw)
	if n := countRows(t, pool, `SELECT count(*) FROM customers c WHERE (SELECT count(*) FROM parties p WHERE p.customer_id = c.id) <> 1`); n != 0 {
		t.Errorf("%d customers without exactly one party", n)
	}
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO parties (shop_id, kind, customer_id) VALUES ($1, 'company', $2)`, shopID, raw)
		return err
	}); err == nil {
		t.Error("a second party was linked to the same customer")
	}
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE parties SET customer_id = $1 WHERE customer_id = $2`, fixtureCustomer, raw)
		return err
	}); err == nil {
		t.Error("a party was moved to another customer")
	}
}

// What a party is to the business is rows with periods: a private seller
// who later rents is a seller from one day and a renter from another. One
// open period of a role at a time; an ended one stays, and the role can be
// given again.
func TestAPartysRolesAreRowsWithPeriods(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	buyer := otherRoles(access.Of(access.RoleBuyer))
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	seller := privateSeller(t, pool)

	if err := workshop.StartPartyRole(ctx, pool, buyer, seller, workshop.PartySeller, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second open seller role: %v, want refused", err)
	}
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), seller, workshop.PartyRenter, "Hyr släp"); err != nil {
		t.Fatalf("the front desk makes the seller a renter: %v", err)
	}
	p, err := workshop.PartyByID(ctx, pool, owner(), seller)
	if err != nil {
		t.Fatal(err)
	}
	var sellerRole string
	for _, r := range p.Roles {
		if r.Role == workshop.PartySeller {
			sellerRole = r.ID
		}
	}
	if err := workshop.EndPartyRole(ctx, pool, desk, sellerRole); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk ended a seller's role: %v, want forbidden", err)
	}
	if err := workshop.EndPartyRole(ctx, pool, buyer, sellerRole); err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartPartyRole(ctx, pool, buyer, seller, workshop.PartySeller, "Säljer en till"); err != nil {
		t.Errorf("a seller again after the first period ended: %v", err)
	}
	p, _ = workshop.PartyByID(ctx, pool, owner(), seller)
	if open, all := rolesOf(p, workshop.PartySeller); open != 1 || all != 2 {
		t.Errorf("seller periods: %d open of %d, want 1 of 2", open, all)
	}
	if open, _ := rolesOf(p, workshop.PartyRenter); open != 1 {
		t.Error("the renter's role is not open")
	}

	// Who may give which role, and only in a shop that runs its module.
	for name, c := range map[string]struct {
		s    access.Scope
		role string
		want error
	}{
		"technician a supplier": {everything(technician()), workshop.PartySupplier, access.ErrForbidden},
		"sales a seller":        {otherRoles(access.Of(access.RoleSales)), workshop.PartySeller, access.ErrForbidden},
		"anyone a customer":     {owner(), workshop.PartyCustomer, workshop.ErrInvalid},
		"no such role":          {owner(), "landlord", workshop.ErrInvalid},
		"a renter, rental off":  {func() access.Scope { s := desk; s.Modules = []access.Module{access.Workshop}; return s }(), workshop.PartyRenter, access.ErrModuleOff},
	} {
		if err := workshop.StartPartyRole(ctx, pool, c.s, seller, c.role, ""); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if err := workshop.StartPartyRole(ctx, pool, advisor(), seller, workshop.PartySupplier, ""); err != nil {
		t.Errorf("the front desk, a supplier, in a workshop-only shop: %v", err)
	}
}

// A role makes a party visible to whoever deals in it, so a role is given
// only to a party the caller already sees: a salesperson cannot make a
// workshop customer a buyer and so read them, and is answered exactly as
// for an id that names nothing. The front desk hands a customer on; after
// that the salesperson deals with them.
func TestARoleIsGivenOnlyToAPartyTheCallerSees(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	sales := otherRoles(access.Of(access.RoleSales))
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	customer := partyOf(t, pool, fixtureCustomer)
	seller := privateSeller(t, pool)
	before := countRows(t, pool, `SELECT count(*) FROM party_roles`)

	for name, c := range map[string]struct {
		s     access.Scope
		party string
		role  string
	}{
		"sales, a workshop customer":      {sales, customer, workshop.PartyBuyer},
		"sales, no such party":            {sales, "77777777-0000-0000-0000-00000000dead", workshop.PartyBuyer},
		"rental desk, the buyer's seller": {desk, seller, workshop.PartyRenter},
		"parts desk, the buyer's seller":  {partsDeskScope(), seller, workshop.PartySupplier},
	} {
		if err := workshop.StartPartyRole(ctx, pool, c.s, c.party, c.role, ""); !errors.Is(err, workshop.ErrNotFound) || errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM party_roles`); n != before {
		t.Errorf("%d roles given to parties the caller cannot see", n-before)
	}
	if _, err := workshop.PartyByID(ctx, pool, sales, customer); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("the salesperson reads the workshop customer: %v", err)
	}

	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), customer, workshop.PartyBuyer, "Vill köpa bil"); err != nil {
		t.Fatalf("the front desk hands the customer to sales: %v", err)
	}
	if p, err := workshop.PartyByID(ctx, pool, sales, customer); err != nil || p.Name == "" {
		t.Errorf("the salesperson, once the customer is a buyer: %+v, %v", p, err)
	}
	if err := workshop.StartPartyRole(ctx, pool, advisor(), customer, workshop.PartyRenter, ""); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("the front desk gave a renter's role with rental off: %v", err)
	}
}

// What each role is shown of a party is decided in the query: a technician
// nothing; the buyer the sellers they buy from, without the link to the
// workshop's customer or the roles that are not theirs; a salesperson not a
// party they have no dealing with at all. The front desk sees everything.
func TestEachRoleIsShownOfAPartyWhatItsWorkNeeds(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	buyer := otherRoles(access.Of(access.RoleBuyer))
	// A workshop customer who sells the dealer a car.
	party := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartySeller, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := workshop.PartyByID(ctx, pool, technician(), party); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read a party: %v", err)
	}
	if _, err := workshop.PartyByID(ctx, pool, otherRoles(access.Of(access.RoleSales)), party); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a salesperson read a seller they never sold to: %v, want not found", err)
	}
	p, err := workshop.PartyByID(ctx, pool, buyer, party)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "A Customer" {
		t.Errorf("the buyer is not shown who the seller is: %+v", p)
	}
	if got := testsupport.Handed(p, "CustomerID"); len(got) != 0 {
		t.Errorf("the buyer was handed %v", got)
	}
	for _, r := range p.Roles {
		if r.Role != workshop.PartySeller {
			t.Errorf("the buyer was shown the party's %s role", r.Role)
		}
	}
	all, err := workshop.PartyByID(ctx, pool, advisor(), party)
	if err != nil || all.CustomerID != fixtureCustomer || len(all.Roles) != 2 {
		t.Errorf("the front desk reads %+v, %v; want the customer and both roles", all, err)
	}
	if _, err := workshop.PartyOfCustomer(ctx, pool, buyer, fixtureCustomer); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer went from a customer to their party: %v", err)
	}
}

// A party that is no customer is edited as itself, and each change is in
// the log -- field, what it was, what it is. A customer's party is edited
// as the customer, where its details are kept.
func TestAPartysChangesAreLogged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	buyer := otherRoles(access.Of(access.RoleBuyer))
	seller := privateSeller(t, pool)
	p, _ := workshop.PartyByID(ctx, pool, buyer, seller)

	changes, err := workshop.EditParty(ctx, pool, buyer, seller, p.Revision, workshop.PartyEdit{
		Name: "Sven Säljare", Phone: "070-999 00 00", Email: "sven@example.test",
		AddressLine1: "Storgatan 1", PostalCode: "123 45", City: "Kvarnby", Reason: "Nytt nummer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Field != "telephone" || changes[0].Old != "070-555 66 77" || changes[0].New != "070-999 00 00" {
		t.Errorf("changes = %+v", changes)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM record_changes WHERE subject = 'party' AND subject_id = $1
		AND field = 'telephone' AND old_value = '070-555 66 77' AND new_value = '070-999 00 00' AND person_id IS NOT NULL`, seller); n != 1 {
		t.Errorf("%d logged changes to the party's telephone, want 1", n)
	}
	if _, err := workshop.EditParty(ctx, pool, buyer, seller, p.Revision, workshop.PartyEdit{Name: "Sven"}); !errors.Is(err, workshop.ErrRecordChanged) {
		t.Errorf("an edit from a form opened before the last: %v", err)
	}
	cust := partyOf(t, pool, fixtureCustomer)
	c, _ := workshop.PartyByID(ctx, pool, advisor(), cust)
	if _, err := workshop.EditParty(ctx, pool, advisor(), cust, c.Revision, workshop.PartyEdit{Name: "Någon annan"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a customer's party edited as a party: %v, want refused", err)
	}
	if _, err := workshop.EditParty(ctx, pool, technician(), seller, 1, workshop.PartyEdit{Name: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician edited a party: %v", err)
	}
}

// Merging two customers merges their parties; the same person entered
// twice in two modules is merged as parties. Either way the other is
// marked, not rewritten, and its roles are read through the merge with the
// periods they had.
func TestMergingJoinsPartiesWithoutRewritingTheirHistory(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	buyer := otherRoles(access.Of(access.RoleBuyer))
	keep := partyOf(t, pool, fixtureCustomer)

	dupCustomer, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "A. Customer"})
	if err != nil {
		t.Fatal(err)
	}
	dupParty := partyOf(t, pool, dupCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), dupParty, workshop.PartySeller, ""); err != nil {
		t.Fatal(err)
	}
	pv, err := workshop.PreviewCustomerMerge(ctx, pool, advisor(), fixtureCustomer, dupCustomer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.MergeCustomers(ctx, pool, advisor(), fixtureCustomer, dupCustomer, pv.KeepRevision, pv.DropRevision, "Samma person"); err != nil {
		t.Fatal(err)
	}
	gone, _ := workshop.PartyByID(ctx, pool, advisor(), dupParty)
	if gone.MergedInto != keep {
		t.Errorf("the merged customer's party points at %q, want %s", gone.MergedInto, keep)
	}
	p, err := workshop.PartyByID(ctx, pool, advisor(), keep)
	if err != nil {
		t.Fatal(err)
	}
	var moved bool
	for _, r := range p.Roles {
		moved = moved || (r.Role == workshop.PartySeller && r.PartyID == dupParty)
	}
	if !moved {
		t.Errorf("the kept party's roles %+v leave out the other's seller role, or rewrote whose it was", p.Roles)
	}

	// Entered again by the buyer, as a seller; then found to be the same.
	seller := privateSeller(t, pool)
	if err := workshop.MergeParties(ctx, pool, buyer, keep, seller, ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer merged parties: %v", err)
	}
	if err := workshop.MergeParties(ctx, pool, advisor(), seller, keep, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a customer's party merged away: %v, want refused", err)
	}
	if err := workshop.MergeParties(ctx, pool, advisor(), keep, seller, "Samma person"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), seller, workshop.PartyRenter, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a role given to a merged-away party: %v, want refused", err)
	}
	p, _ = workshop.PartyByID(ctx, pool, advisor(), keep)
	if _, all := rolesOf(p, workshop.PartySeller); all != 2 {
		t.Errorf("the kept party has %d seller periods, want both", all)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM record_changes WHERE subject = 'party' AND field IN ('merged in', 'merged into')`); n != 2 {
		t.Errorf("%d merge entries in the log, want 2", n)
	}
}

// A party's roles and its own address are in the person's export, and
// erasure clears the address and keeps the roles: that somebody sold the
// dealer a car on a date is part of the purchase's record.
func TestAPartyIsInThePersonsExportAndErasure(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	seller := privateSeller(t, pool)
	var person string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id::text FROM parties WHERE id = $1`, seller).Scan(&person)
	}); err != nil {
		t.Fatal(err)
	}
	e, err := workshop.ExportPerson(ctx, pool, advisor(), person)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Roles) != 1 || e.Roles[0].Role != workshop.PartySeller || len(e.Addresses) != 1 || e.Addresses[0].City != "Kvarnby" {
		t.Errorf("the export: roles %+v, addresses %+v", e.Roles, e.Addresses)
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), &recordingRemover{}, person, "Begäran"); err != nil {
		t.Fatal(err)
	}
	p, err := workshop.PartyByID(ctx, pool, owner(), seller)
	if err != nil {
		t.Fatal(err)
	}
	if p.Address != "" || p.Phone != "" || p.Name != workshop.ErasedName {
		t.Errorf("after erasure the party reads %+v", p)
	}
	if open, _ := rolesOf(p, workshop.PartySeller); open != 1 {
		t.Error("erasure took the party's role away")
	}
}

// What is kept is kept by the database, not only by the code in front of
// it: the application may not delete a party or move it to another
// customer, rewrite a role's period, rewrite the record of a removed
// photograph, or delete a resource or a block rather than release it.
func TestTheApplicationCannotRewriteWhatIsKept(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM parties`,
		`UPDATE parties SET customer_id = NULL`,
		`UPDATE parties SET person_id = NULL`,
		`UPDATE party_roles SET role = 'buyer'`,
		`UPDATE party_roles SET valid_from = now()`,
		`DELETE FROM party_roles`,
		`UPDATE protocol_photo_removals SET reason = 'x'`,
		`DELETE FROM protocol_photo_removals`,
		`DELETE FROM resources`,
		`UPDATE resources SET vehicle_id = NULL`,
		`DELETE FROM resource_blocks`,
		`UPDATE resource_blocks SET kind = 'rental'`,
		`UPDATE resource_block_changes SET reason = 'x'`,
		`DELETE FROM resource_block_changes`,
		`UPDATE resource_block_jobs SET joined_at = now()`,
		`UPDATE resource_retirements SET retired_at = now()`,
		`DELETE FROM resource_retirements`,
		`DELETE FROM resource_block_jobs`,
	} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("%q as the application: %v, want refused for want of the privilege", stmt, err)
		}
	}
}
