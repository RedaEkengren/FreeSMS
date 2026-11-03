package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Parties (#148): one record for each real-world person or company the
// business deals with, and what they are to it, over time. The workshop's
// customer, the private seller of a car, its buyer, a consignor, a supplier
// and a renter are one party with a history of roles, so the same person is
// not entered once per module. Every customer has its party (migration 0069
// makes it, in the customer's own transaction); a customer's party keeps its
// details on the customer, and a party that is no customer has its own.

// The roles a party can have.
const (
	PartyCustomer  = "customer"
	PartySeller    = "seller"
	PartyBuyer     = "buyer"
	PartySupplier  = "supplier"
	PartyConsignor = "consignor"
	PartyRenter    = "renter"
)

// PartyRoles is every role, in the order a page lists them.
var PartyRoles = []string{PartyCustomer, PartySeller, PartyBuyer, PartyConsignor, PartySupplier, PartyRenter}

// partyRoleModule is the module a role's dealings belong to: the shop must
// run it for the role to be given. A customer and a supplier are Core's.
func partyRoleModule(role string) access.Module {
	switch role {
	case PartySeller, PartyBuyer, PartyConsignor:
		return access.Dealer
	case PartyRenter:
		return access.Rental
	}
	return ""
}

// recordsPartyRole reports whether roles may give or end a party's role:
// whoever does that business with them. The buyer buys cars from sellers and
// consignors, the salesperson sells to buyers, the rental desk rents to
// renters; suppliers are the parts desk's and the buyer's, who orders
// reconditioning. A customer is made with the customer's own record.
func recordsPartyRole(roles access.Roles, role string) bool {
	switch role {
	case PartySeller, PartyConsignor:
		return roles.BuysVehicles()
	case PartyBuyer:
		return roles.SellsVehicles()
	case PartyRenter:
		return roles.HandsOverRentals()
	case PartySupplier:
		return roles.SeesParts() || roles.BuysVehicles()
	}
	return false
}

// partyRolesSeen is which of these party roles roles may be shown, and so
// which parties: every party, with every role, for the front desk and those
// who run the shop, who already see every customer; otherwise only parties
// that are, or were, something that person deals with, and only those
// roles. A salesperson reads the buyers of cars, not who the workshop's
// customers are. A technician reads none: they need the vehicle and the job.
// Nil means none at all.
func partyRolesSeen(roles access.Roles, of []string) []string {
	if roles.SeesCustomerPersonalData() {
		return of
	}
	var out []string
	for _, r := range of {
		if r != PartyCustomer && recordsPartyRole(roles, r) {
			out = append(out, r)
		}
	}
	return out
}

// Party is a party as one person is shown it. What they may not be shown is
// not fetched: CustomerID, the link to the workshop's history, only for
// those who see customers; Roles only those they deal in.
type Party struct {
	ID         string
	Kind       string
	Name       string
	OrgNumber  string
	Phone      string
	Email      string
	Address    string
	CustomerID string
	MergedInto string
	Revision   int
	Roles      []PartyRole
}

// PartyRole is one period of one role.
type PartyRole struct {
	ID      string
	Role    string
	From    time.Time
	To      *time.Time
	PartyID string // the party it was given to, which a merge may since have joined to another
	Note    string
}

// Open reports whether the period has not ended.
func (r PartyRole) Open() bool { return r.To == nil }

// NewParty is a party that is no customer: the private seller of a car, a
// supplier, a renter met at the rental desk. It is made with the role it is
// made for, since a party exists because of a dealing.
type NewParty struct {
	Kind         string // private or company
	Name         string
	OrgNumber    string
	Phone        string
	Email        string
	AddressLine1 string
	AddressLine2 string
	PostalCode   string
	City         string
	Role         string
	Note         string
}

var (
	errPartyRole     = fmt.Errorf("%w: that is not a role a party can have", ErrInvalid)
	errPartyCustomer = fmt.Errorf("%w: a customer is made with the customer's own record", ErrInvalid)
	errPartyOpen     = fmt.Errorf("%w: the party already has that role", ErrInvalid)
	errPartyMerged   = fmt.Errorf("%w: that party has been merged into another", ErrInvalid)
)

// mayRecordPartyRole checks roles, and the scope's modules, for giving or
// ending role.
func mayRecordPartyRole(roles access.Roles, scope access.Scope, role string) error {
	if !slices.Contains(PartyRoles, role) {
		return errPartyRole
	}
	if role == PartyCustomer {
		return errPartyCustomer
	}
	if !recordsPartyRole(roles, role) {
		return access.ErrForbidden
	}
	// Written out per module, so the permission contract sees the gate.
	switch partyRoleModule(role) {
	case access.Dealer:
		return scope.Require(access.Dealer)
	case access.Rental:
		return scope.Require(access.Rental)
	}
	return nil
}

func checkNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 300 {
		return "", fmt.Errorf("%w: keep the note to 300 characters", ErrInvalid)
	}
	return note, nil
}

// CreateParty makes a party that is no customer, with the role it is made
// for. Whoever records that role may; the module the role belongs to must be
// running.
func CreateParty(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewParty) (string, error) {
	if err := mayRecordPartyRole(scope.Role, scope, n.Role); err != nil {
		return "", err
	}
	n, note, err := checkNewParty(n)
	if err != nil {
		return "", err
	}
	var id string
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = createPartyTx(ctx, tx, scope, n, note)
		return err
	})
	return id, err
}

// checkNewParty checks what a new party is made from, as CreateParty and
// an operation that makes one along the way (RecordPurchase) both must:
// the name trimmed, and the note.
func checkNewParty(n NewParty) (NewParty, string, error) {
	n.Name = strings.TrimSpace(n.Name)
	switch {
	case n.Kind != "private" && n.Kind != "company":
		return n, "", fmt.Errorf("%w: a party is either a person or a company", ErrInvalid)
	case n.Name == "" || utf8.RuneCountInString(n.Name) > 200:
		return n, "", fmt.Errorf("%w: a party needs a name, in 200 characters or fewer", ErrInvalid)
	}
	note, err := checkNote(n.Note)
	return n, note, err
}

// createPartyTx makes a party CreateParty has checked, in the caller's
// transaction.
func createPartyTx(ctx context.Context, tx pgx.Tx, scope access.Scope, n NewParty, note string) (string, error) {
	var id string
	err := func() error {
		// A private party is a person, as a private customer is: the name,
		// telephone and email live on people, where erasure and the
		// person's export already reach. A company's are the company's.
		var personID, company *string
		if n.Kind == "private" {
			var pid string
			if err := tx.QueryRow(ctx, `
				INSERT INTO people (shop_id, display_name, phone, email)
				VALUES ($1, $2, nullif($3, ''), nullif($4, '')) RETURNING id`,
				scope.ShopID, n.Name, strings.TrimSpace(n.Phone), strings.TrimSpace(n.Email)).Scan(&pid); err != nil {
				return emailTaken(err, "create person")
			}
			personID = &pid
		} else {
			company = &n.Name
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO parties (shop_id, kind, person_id, company_name, org_number,
			                     address_line1, address_line2, postal_code, city, created_by)
			VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), nullif($7, ''), nullif($8, ''), nullif($9, ''), NULLIF($10, '')::uuid)
			RETURNING id`,
			scope.ShopID, n.Kind, personID, company, strings.TrimSpace(n.OrgNumber),
			strings.TrimSpace(n.AddressLine1), strings.TrimSpace(n.AddressLine2),
			strings.TrimSpace(n.PostalCode), strings.TrimSpace(n.City), scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("create party: %w", err)
		}
		return startPartyRoleTx(ctx, tx, scope, id, n.Role, note)
	}()
	return id, err
}

// emailTaken turns the unique email on people into a sentence.
func emailTaken(err error, what string) error {
	if isUniqueViolation(err, "people_shop_email_idx") {
		return fmt.Errorf("%w: somebody here already has that email address", ErrInvalid)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// mayGivePartyRole checks roles, and the scope's modules, for giving a
// party role: whoever records it, or the front desk and those who run the
// shop, who see every customer and so are the deliberate way a workshop
// customer is handed to the dealer's or the rental desk's dealings.
func mayGivePartyRole(roles access.Roles, scope access.Scope, role string) error {
	err := mayRecordPartyRole(roles, scope, role)
	if errors.Is(err, access.ErrForbidden) && roles.SeesCustomerPersonalData() {
		// Written out per module, so the permission contract sees the gate.
		switch partyRoleModule(role) {
		case access.Dealer:
			return scope.Require(access.Dealer)
		case access.Rental:
			return scope.Require(access.Rental)
		}
		return nil
	}
	return err
}

// StartPartyRole gives a party a role from now: a workshop customer who
// sells the dealer a car becomes a seller as well. One open period of a role
// at a time.
//
// Only to a party the caller can already see. A role is what makes a party
// visible to those who deal in it (partyRolesSeen), so giving one to any id
// would let a salesperson make a workshop customer a buyer and then read
// them; the front desk, which sees every customer, is the way a customer
// reaches the dealer or the rental desk, or the person is entered anew. A
// party the caller cannot see answers as one that does not exist.
func StartPartyRole(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, partyID, role, note string) error {
	if err := mayGivePartyRole(scope.Role, scope, role); err != nil {
		return err
	}
	if !looksLikeUUID(partyID) {
		return ErrNotFound
	}
	note, err := checkNote(note)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := partySeenTx(ctx, tx, scope, partyID); err != nil {
			return err
		}
		var merged *string
		err := tx.QueryRow(ctx, `SELECT merged_into::text FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&merged)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read party: %w", err)
		}
		if merged != nil {
			return errPartyMerged
		}
		return startPartyRoleTx(ctx, tx, scope, partyID, role, note)
	})
}

func startPartyRoleTx(ctx context.Context, tx pgx.Tx, scope access.Scope, partyID, role, note string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO party_roles (shop_id, party_id, role, started_by, note)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, nullif($5, ''))`, scope.ShopID, partyID, role, scope.UserID, note)
	if isUniqueViolation(err, "party_roles_one_open") {
		return errPartyOpen
	}
	if err != nil {
		return fmt.Errorf("give the role: %w", err)
	}
	return nil
}

// EndPartyRole ends a period of a role, from now. The period stays: that
// somebody was a renter until March is the history.
func EndPartyRole(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, roleID string) error {
	if !looksLikeUUID(roleID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var role string
		var ended bool
		err := tx.QueryRow(ctx, `SELECT role, valid_to IS NOT NULL FROM party_roles WHERE id = $1 FOR UPDATE`, roleID).Scan(&role, &ended)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the role: %w", err)
		}
		if err := mayRecordPartyRole(scope.Role, scope, role); err != nil {
			return err
		}
		if ended {
			return fmt.Errorf("%w: that role has already ended", ErrInvalid)
		}
		_, err = tx.Exec(ctx, `
			UPDATE party_roles SET valid_to = greatest(clock_timestamp(), valid_from + interval '1 microsecond'),
			                       ended_by = NULLIF($2, '')::uuid
			WHERE id = $1`, roleID, scope.UserID)
		return err
	})
}

// partyNameSQL is a party's name as it is read everywhere: the company's
// for a company, else the person's -- over a party pa joined to its
// customer c, the customer's person cp and its own person pp.
const partyNameSQL = `coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name, '')`

// partyColumns reads a party's details from wherever they are kept: the
// customer and its person for a customer's party, the party and its person
// for any other. The link to the customer is read only when $2 is true.
const partyColumns = `
	pa.id::text, pa.kind,
	` + partyNameSQL + `,
	coalesce(c.org_number, pa.org_number, ''),
	coalesce(cp.phone, pp.phone, ''), coalesce(cp.email::text, pp.email::text, ''),
	coalesce(nullif(CASE WHEN c.id IS NOT NULL
	                     THEN concat_ws(', ', c.address_line1, c.address_line2, concat_ws(' ', c.postal_code, c.city))
	                     ELSE concat_ws(', ', pa.address_line1, pa.address_line2, concat_ws(' ', pa.postal_code, pa.city)) END, ''), ''),
	CASE WHEN $2 THEN coalesce(pa.customer_id::text, '') ELSE '' END, coalesce(pa.merged_into::text, ''), pa.revision`

const partyJoins = `
	FROM parties pa
	LEFT JOIN customers c ON c.id = pa.customer_id
	LEFT JOIN people cp ON cp.id = c.person_id
	LEFT JOIN people pp ON pp.id = pa.person_id`

// PartyByID reads a party for whoever may see it, with its roles -- its own
// and those of any party merged into it. What the caller may not see is not
// fetched: a party with none of the roles they deal in is not found at all,
// and the roles they do not deal in are left out.
func PartyByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Party, error) {
	seen := partyRolesSeen(scope.Role, PartyRoles)
	if len(seen) == 0 {
		return Party{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return Party{}, ErrNotFound
	}
	var p Party
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The roles first: they decide whether the party is there at all
		// for this caller, before anything about the party is read.
		rows, err := tx.Query(ctx, `
			SELECT r.id::text, r.role, r.valid_from, r.valid_to, r.party_id::text, coalesce(r.note, '')
			FROM party_roles r
			WHERE r.party_id = ANY((SELECT party_ids_read_as(party_now($1)))::uuid[]) AND r.role = ANY($2)
			ORDER BY r.valid_from, r.id`, id, seen)
		if err != nil {
			return fmt.Errorf("read roles: %w", err)
		}
		for rows.Next() {
			var r PartyRole
			if err := rows.Scan(&r.ID, &r.Role, &r.From, &r.To, &r.PartyID, &r.Note); err != nil {
				rows.Close()
				return fmt.Errorf("scan role: %w", err)
			}
			p.Roles = append(p.Roles, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(p.Roles) == 0 {
			return ErrNotFound
		}
		// The workshop's customer behind the party only for those who see
		// customers: not fetched for anybody else.
		err = tx.QueryRow(ctx, `SELECT`+partyColumns+partyJoins+` WHERE pa.id = $1`, id, scope.Role.SeesCustomerPersonalData()).Scan(
			&p.ID, &p.Kind, &p.Name, &p.OrgNumber, &p.Phone, &p.Email, &p.Address, &p.CustomerID, &p.MergedInto, &p.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return Party{}, err
	}
	return p, nil
}

// PartyOfCustomer is the party a customer is, for the front desk -- the
// way from the workshop's record to the rest of what the business does with
// the same person.
func PartyOfCustomer(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, customerID string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(customerID) {
		return "", ErrNotFound
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id::text FROM parties WHERE customer_id = $1`, customerID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return id, err
}

// PartyEdit is what can be put right on a party that is no customer. A
// customer's party is edited as the customer (EditCustomer), where its
// details are kept.
type PartyEdit struct {
	Name         string
	OrgNumber    string
	Phone        string
	Email        string
	AddressLine1 string
	AddressLine2 string
	PostalCode   string
	City         string
	Reason       string
}

// EditParty puts a party's details right, from the version the form was
// opened against, and logs each change -- field, what it was, what it is --
// as a change to the party. Whoever may read the party and deals in one of
// its roles may; a document already issued keeps what it was issued with.
func EditParty(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, revision int, e PartyEdit) ([]Change, error) {
	seen := partyRolesSeen(scope.Role, PartyRoles)
	if len(seen) == 0 {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return nil, ErrNotFound
	}
	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" || utf8.RuneCountInString(e.Name) > 200 {
		return nil, fmt.Errorf("%w: a party needs a name, in 200 characters or fewer", ErrInvalid)
	}
	reason, err := checkNote(e.Reason)
	if err != nil {
		return nil, err
	}
	var changes []Change
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind, personID string
		var customer, merged *string
		var rev int
		var visible bool
		err := tx.QueryRow(ctx, `
			SELECT pa.kind, coalesce(pa.person_id::text, ''), pa.customer_id::text, pa.merged_into::text, pa.revision,
			       EXISTS (SELECT 1 FROM party_roles r WHERE r.party_id = ANY(party_ids_read_as(pa.id)) AND r.role = ANY($2))
			FROM parties pa WHERE pa.id = $1 FOR UPDATE`, id, seen).Scan(&kind, &personID, &customer, &merged, &rev, &visible)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !visible) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read party: %w", err)
		}
		switch {
		case customer != nil:
			return fmt.Errorf("%w: this party is a customer; change the details on the customer", ErrInvalid)
		case merged != nil:
			return errPartyMerged
		case rev != revision:
			return ErrRecordChanged
		}
		var was Party
		if err := tx.QueryRow(ctx, `SELECT`+partyColumns+partyJoins+` WHERE pa.id = $1`, id, false).Scan(
			&was.ID, &was.Kind, &was.Name, &was.OrgNumber, &was.Phone, &was.Email, &was.Address, &was.CustomerID, &was.MergedInto, &was.Revision); err != nil {
			return fmt.Errorf("read party: %w", err)
		}
		if personID != "" {
			name := e.Name
			if kind == "company" {
				name = "" // a company's contact keeps their own name
			}
			if _, err := tx.Exec(ctx, `
				UPDATE people SET display_name = coalesce(nullif($2, ''), display_name),
				                  phone = nullif($3, ''), email = nullif($4, ''), updated_at = now()
				WHERE id = $1`, personID, name, strings.TrimSpace(e.Phone), strings.TrimSpace(e.Email)); err != nil {
				return emailTaken(err, "edit person")
			}
		}
		var company *string
		if kind == "company" {
			company = &e.Name
		}
		if _, err := tx.Exec(ctx, `
			UPDATE parties SET company_name = $2, org_number = nullif($3, ''),
			       address_line1 = nullif($4, ''), address_line2 = nullif($5, ''),
			       postal_code = nullif($6, ''), city = nullif($7, ''), revision = revision + 1
			WHERE id = $1`, id, company, strings.TrimSpace(e.OrgNumber),
			strings.TrimSpace(e.AddressLine1), strings.TrimSpace(e.AddressLine2),
			strings.TrimSpace(e.PostalCode), strings.TrimSpace(e.City)); err != nil {
			return fmt.Errorf("edit party: %w", err)
		}
		var now Party
		if err := tx.QueryRow(ctx, `SELECT`+partyColumns+partyJoins+` WHERE pa.id = $1`, id, false).Scan(
			&now.ID, &now.Kind, &now.Name, &now.OrgNumber, &now.Phone, &now.Email, &now.Address, &now.CustomerID, &now.MergedInto, &now.Revision); err != nil {
			return fmt.Errorf("read party: %w", err)
		}
		for _, f := range []struct{ field, old, new string }{
			{"name", was.Name, now.Name}, {"company number", was.OrgNumber, now.OrgNumber},
			{"telephone", was.Phone, now.Phone}, {"email", was.Email, now.Email}, {"address", was.Address, now.Address},
		} {
			if f.old == f.new {
				continue
			}
			changes = append(changes, Change{Field: f.field, Old: f.old, New: f.new})
			if err := logChangeTx(ctx, tx, scope, "party", id, personID, f.field, f.old, f.new, reason); err != nil {
				return err
			}
		}
		if len(changes) == 0 {
			// Nothing changed: no new version, as EditCustomer.
			_, err := tx.Exec(ctx, `UPDATE parties SET revision = revision - 1 WHERE id = $1`, id)
			return err
		}
		return nil
	})
	return changes, err
}

// MergeParties joins two parties that are one: the same person entered by
// the rental desk and again by the buyer before anyone noticed. The other
// is kept as a marked record pointing at the one kept, and nothing written
// against it is rewritten: its roles, and whatever names it, are read
// through the merge (party_now, 0069). Two customers are merged as
// customers (MergeCustomers), which merges their parties; a customer's
// party is merged into, never away, so the workshop's record stays the one
// kept. The front desk and those who run the shop, who see every party.
func MergeParties(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keepID, dropID, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(keepID) || !looksLikeUUID(dropID) {
		return ErrNotFound
	}
	if keepID == dropID {
		return fmt.Errorf("%w: a party cannot be merged into itself", ErrInvalid)
	}
	reason, err := checkNote(reason)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM parties WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, keepID, dropID); err != nil {
			return fmt.Errorf("lock parties: %w", err)
		}
		type side struct {
			kind, name, person string
			customer, merged   *string
		}
		read := func(id string) (side, error) {
			var s side
			err := tx.QueryRow(ctx, `
				SELECT pa.kind, coalesce(pa.person_id::text, c.person_id::text, ''), pa.customer_id::text, pa.merged_into::text,
				       `+partyNameSQL+`
				`+partyJoins+` WHERE pa.id = $1`, id).Scan(&s.kind, &s.person, &s.customer, &s.merged, &s.name)
			if errors.Is(err, pgx.ErrNoRows) {
				return s, ErrNotFound
			}
			return s, err
		}
		keep, err := read(keepID)
		if err != nil {
			return err
		}
		drop, err := read(dropID)
		if err != nil {
			return err
		}
		switch {
		case keep.merged != nil || drop.merged != nil:
			return errPartyMerged
		case keep.kind != drop.kind:
			return fmt.Errorf("%w: a person and a company are not the same party", ErrInvalid)
		case drop.customer != nil && keep.customer != nil:
			return fmt.Errorf("%w: both are customers; merge them as customers", ErrInvalid)
		case drop.customer != nil:
			return fmt.Errorf("%w: a customer's party is the one kept; merge the other into it", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `UPDATE parties SET merged_into = $2, merged_at = clock_timestamp(), revision = revision + 1 WHERE id = $1`,
			dropID, keepID); err != nil {
			return fmt.Errorf("mark merged: %w", err)
		}
		if err := logChangeTx(ctx, tx, scope, "party", keepID, keep.person, "merged in", "", drop.name, reason); err != nil {
			return err
		}
		return logChangeTx(ctx, tx, scope, "party", dropID, drop.person, "merged into", "", keep.name, reason)
	})
}

// CustomerForParty is the customer a party is invoiced as: the one it is
// already, or one made from it now (customerForPartyTx). For whoever
// invoices a party -- the front desk, a salesperson, the rental desk -- and
// only a party they may see. Core, whatever the shop runs.
func CustomerForParty(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, partyID string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.SellsVehicles() && !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(partyID) {
		return "", ErrNotFound
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := partySeenTx(ctx, tx, scope, partyID); err != nil {
			return err
		}
		var err error
		id, err = customerForPartyTx(ctx, tx, scope, partyID)
		return err
	})
	return id, err
}

// customerForPartyTx gives a party its one customer, in the caller's
// transaction, and returns it: the customer the party already is, or one
// made now from the party's own details, which move to it -- the person
// (name, telephone, email), or the company's name and number, and the
// address -- as the front desk keeps a customer's, the party then pointing
// at it (parties_details_in_one_place, 0115). Invoices are addressed to
// customers (plan v2: parties are linked 1:1 to customers), so a car's
// buyer (#177) and a renter (#199, Rental invoicing) are invoiced through
// this. Idempotent: the party is locked first, so two calls at once make
// one customer. The caller checks the role and that the party is one it
// may see.
func customerForPartyTx(ctx context.Context, tx pgx.Tx, scope access.Scope, partyID string) (string, error) {
	var id, kind string
	var customer, person, company, org, line1, line2, postal, city, country *string
	err := tx.QueryRow(ctx, `
		SELECT id, kind, customer_id::text, person_id::text, company_name, org_number, address_line1, address_line2,
		       postal_code, city, country
		FROM parties WHERE id = party_now($1) FOR UPDATE`, partyID).Scan(&id, &kind, &customer, &person, &company, &org,
		&line1, &line2, &postal, &city, &country)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read party: %w", err)
	}
	if customer != nil {
		return *customer, nil
	}
	// This party becomes the customer: the trigger that gives every new
	// customer a party of its own leaves it to this one (0115).
	if _, err := tx.Exec(ctx, `SELECT set_config('app.party_becoming_customer', $1, true)`, id); err != nil {
		return "", err
	}
	var made string
	if err := tx.QueryRow(ctx, `
		INSERT INTO customers (shop_id, kind, person_id, company_name, org_number, address_line1, address_line2, postal_code, city, country)
		VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		scope.ShopID, kind, person, company, org, line1, line2, postal, city, country).Scan(&made); err != nil {
		return "", fmt.Errorf("make the party's customer: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT party_becomes_customer($1, $2)`, id, made); err != nil {
		return "", fmt.Errorf("tie the party to its customer: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.party_becoming_customer', '', true)`); err != nil {
		return "", err
	}
	// A customer from now, as every customer's party is.
	if _, err := tx.Exec(ctx, `
		INSERT INTO party_roles (shop_id, party_id, role, valid_from, started_by)
		SELECT $1, $2, 'customer', clock_timestamp(), NULLIF($3, '')::uuid
		WHERE NOT EXISTS (SELECT 1 FROM party_roles WHERE party_id = $2 AND role = 'customer' AND valid_to IS NULL)`,
		scope.ShopID, id, scope.UserID); err != nil {
		return "", fmt.Errorf("make the party a customer: %w", err)
	}
	return made, nil
}
