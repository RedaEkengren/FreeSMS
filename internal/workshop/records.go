package workshop

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Customer and vehicle records between visits: finding them again, putting
// right what was typed wrong, and telling a correction from a change.
//
// A correction rewrites the column and leaves a row in record_changes saying
// what it was. A change of owner or of plate is not a correction -- the old
// fact was true then -- so those close a period and open another.

// ErrRecordChanged is a form opened against a version that is no longer the
// current one: somebody else got there first.
var ErrRecordChanged = errors.New("workshop: the record was changed by somebody else")

// DuplicateError says a value is already held by another record, and which, so
// the caller can offer to look at it or to merge.
type DuplicateError struct {
	Kind  string // customer or vehicle
	ID    string
	Field string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("workshop: invalid: that %s is already on another %s", e.Field, e.Kind)
}

// Is makes a duplicate an invalid request to anything that asks.
func (e *DuplicateError) Is(target error) bool { return target == ErrInvalid }

// Change is one field of one record, as it was and as it became.
type Change struct {
	Subject string
	Field   string
	Old     string
	New     string
	Reason  string
	By      string
	At      time.Time
}

// ---- Reading the register -------------------------------------------------

// BrowseCustomers lists customers: the ones matching a search, or the most
// recently touched when there is none. Customers merged into another are not
// listed.
func BrowseCustomers(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Customer, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if strings.TrimSpace(query) != "" {
		return FindCustomers(ctx, pool, scope, query)
	}
	var out []Customer
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT`+customerColumns+`
			FROM customers c
			LEFT JOIN people p ON p.id = c.person_id
			WHERE c.merged_into IS NULL AND p.erased_at IS NULL
			ORDER BY c.updated_at DESC, c.id
			LIMIT 50`)
		if err != nil {
			return fmt.Errorf("list customers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCustomer(rows)
			if err != nil {
				return fmt.Errorf("scan customer: %w", err)
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// CustomerPerson is somebody besides the main contact who deals with the
// workshop for a company.
type CustomerPerson struct {
	ID    string // the row, not the person
	Name  string
	Title string
	Phone string
	Email string
	Since time.Time
}

// OwnedVehicle is a car a customer owns or owned.
type OwnedVehicle struct {
	VehicleID    string
	Registration string
	Make, Model  string
	From         time.Time
	To           *time.Time
}

// Current reports whether the customer still owns it.
func (o OwnedVehicle) Current() bool { return o.To == nil }

// CustomerJob is one job a customer is billed for.
type CustomerJob struct {
	ID           string
	Number       int64
	State        string
	OpenedAt     time.Time
	Registration string
}

// StateLabel is the job's state as a catalogue key.
func (j CustomerJob) StateLabel() string { return State(j.State).Label() }

// MergedRecord is a customer that was found to be this one.
type MergedRecord struct {
	ID   string
	Name string
	At   time.Time
}

// CustomerCard is everything held about a customer, for the front desk.
type CustomerCard struct {
	Customer
	Revision int
	PersonID string
	// Erased is set once the person asked to be, and the card says so rather
	// than offering to do it twice.
	Erased       bool
	ContactName  string
	AddressLine1 string
	AddressLine2 string
	PostalCode   string
	City         string

	People   []CustomerPerson
	Vehicles []OwnedVehicle
	Jobs     []CustomerJob
	Invoices int
	Changes  []Change
	Merged   []MergedRecord
	// Set when this record was merged into another: the card says so and
	// points there instead of offering edits.
	MergedInto string
}

// CustomerByID reads a customer with what hangs off it.
func CustomerByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (CustomerCard, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return CustomerCard{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return CustomerCard{}, ErrNotFound
	}
	var c CustomerCard
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = customerCardTx(ctx, tx, id)
		return err
	})
	return c, err
}

func customerCardTx(ctx context.Context, tx pgx.Tx, id string) (CustomerCard, error) {
	var c CustomerCard
	var merged *string
	err := tx.QueryRow(ctx, `SELECT`+customerColumns+`,
			c.revision, coalesce(c.person_id::text, ''), coalesce(p.display_name, ''),
			coalesce(c.address_line1, ''), coalesce(c.address_line2, ''),
			coalesce(c.postal_code, ''), coalesce(c.city, ''), c.merged_into::text,
			coalesce(p.erased_at IS NOT NULL, false)
		FROM customers c LEFT JOIN people p ON p.id = c.person_id
		WHERE c.id = $1`, id).Scan(&c.ID, &c.Kind, &c.Name, &c.OrgNumber, &c.VATNumber,
		&c.Phone, &c.Email, &c.Address, &c.Orders,
		&c.Revision, &c.PersonID, &c.ContactName, &c.AddressLine1, &c.AddressLine2, &c.PostalCode, &c.City, &merged, &c.Erased)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("read customer: %w", err)
	}
	c.MergedInto = deref(merged)

	rows, err := tx.Query(ctx, `
		SELECT cp.id, p.display_name, coalesce(cp.title, ''), coalesce(p.phone, ''), coalesce(p.email, ''), cp.added_at
		FROM customer_people cp JOIN people p ON p.id = cp.person_id
		WHERE cp.customer_id = $1 AND cp.removed_at IS NULL ORDER BY cp.added_at`, id)
	if err != nil {
		return c, fmt.Errorf("read contacts: %w", err)
	}
	for rows.Next() {
		var p CustomerPerson
		if err := rows.Scan(&p.ID, &p.Name, &p.Title, &p.Phone, &p.Email, &p.Since); err != nil {
			rows.Close()
			return c, fmt.Errorf("scan contact: %w", err)
		}
		c.People = append(c.People, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}

	rows, err = tx.Query(ctx, `
		SELECT v.id, coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''), o.owned_from, o.owned_to
		FROM vehicle_ownership o JOIN vehicles v ON v.id = o.vehicle_id
		LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
		WHERE o.customer_id = $1 ORDER BY o.owned_from DESC`, id)
	if err != nil {
		return c, fmt.Errorf("read vehicles: %w", err)
	}
	for rows.Next() {
		var o OwnedVehicle
		if err := rows.Scan(&o.VehicleID, &o.Registration, &o.Make, &o.Model, &o.From, &o.To); err != nil {
			rows.Close()
			return c, fmt.Errorf("scan vehicle: %w", err)
		}
		c.Vehicles = append(c.Vehicles, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}

	rows, err = tx.Query(ctx, `
		SELECT w.id, w.number, w.state, w.opened_at, coalesce(r.registration, '')
		FROM work_orders w
		LEFT JOIN vehicle_registrations r ON r.vehicle_id = w.vehicle_id AND r.valid_to IS NULL
		WHERE w.customer_id = $1 ORDER BY w.opened_at DESC LIMIT 50`, id)
	if err != nil {
		return c, fmt.Errorf("read jobs: %w", err)
	}
	for rows.Next() {
		var j CustomerJob
		if err := rows.Scan(&j.ID, &j.Number, &j.State, &j.OpenedAt, &j.Registration); err != nil {
			rows.Close()
			return c, fmt.Errorf("scan job: %w", err)
		}
		c.Jobs = append(c.Jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM invoices i JOIN work_orders w ON w.id = i.work_order_id
		WHERE w.customer_id = $1`, id).Scan(&c.Invoices); err != nil {
		return c, fmt.Errorf("count invoices: %w", err)
	}

	if c.Changes, err = changesTx(ctx, tx, id); err != nil {
		return c, err
	}

	rows, err = tx.Query(ctx, `
		SELECT m.id, coalesce(p.display_name, m.company_name, ''), m.merged_at
		FROM customers m LEFT JOIN people p ON p.id = m.person_id
		WHERE m.merged_into = $1 ORDER BY m.merged_at`, id)
	if err != nil {
		return c, fmt.Errorf("read merged records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m MergedRecord
		if err := rows.Scan(&m.ID, &m.Name, &m.At); err != nil {
			return c, fmt.Errorf("scan merged record: %w", err)
		}
		c.Merged = append(c.Merged, m)
	}
	return c, rows.Err()
}

func changesTx(ctx context.Context, tx pgx.Tx, subjectID string) ([]Change, error) {
	rows, err := tx.Query(ctx, `
		SELECT r.subject, r.field, coalesce(r.old_value, ''), coalesce(r.new_value, ''), coalesce(r.reason, ''),
		       coalesce(p.display_name, ''), r.at
		FROM record_changes r
		LEFT JOIN users u ON u.id = r.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE r.subject_id = $1 ORDER BY r.at DESC, r.id LIMIT 100`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("read changes: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Subject, &c.Field, &c.Old, &c.New, &c.Reason, &c.By, &c.At); err != nil {
			return nil, fmt.Errorf("scan change: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- Validation -----------------------------------------------------------

var emailShape = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func cleanEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s != "" && !emailShape.MatchString(s) {
		return "", fmt.Errorf("%w: that does not look like an email address", ErrInvalid)
	}
	return s, nil
}

func digitsOf(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func cleanPhone(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s != "" && len(digitsOf(s)) < 5 {
		return "", fmt.Errorf("%w: a telephone number needs at least five digits", ErrInvalid)
	}
	return s, nil
}

// cleanOrgNumber checks the shape of an organisation number and writes a
// Swedish ten-digit one the usual way. Numbers from elsewhere are kept as
// typed: the shape is not ours to enforce.
func cleanOrgNumber(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && r != '-' && r != ' ' && !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') {
			return "", fmt.Errorf("%w: an organisation number has digits and a hyphen only", ErrInvalid)
		}
	}
	if d := digitsOf(s); len(d) == 10 && strings.Trim(s, "0123456789- ") == "" {
		return d[:6] + "-" + d[6:], nil
	}
	if len(s) < 4 || len(s) > 20 {
		return "", fmt.Errorf("%w: that is not the length of an organisation number", ErrInvalid)
	}
	return s, nil
}

// ---- Editing a customer ---------------------------------------------------

// CustomerEdit is the form: every field, as it should be after the save.
type CustomerEdit struct {
	Name         string
	ContactName  string // a company's main contact
	OrgNumber    string
	VATNumber    string
	Phone        string
	Email        string
	AddressLine1 string
	AddressLine2 string
	PostalCode   string
	City         string
	Reason       string
}

func logChangeTx(ctx context.Context, tx pgx.Tx, scope access.Scope, subject, subjectID, personID, field, oldV, newV, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO record_changes (shop_id, subject, subject_id, person_id, field, old_value, new_value, reason, by_user)
		VALUES ($1, $2, $3, nullif($4, '')::uuid, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), $9)`,
		scope.ShopID, subject, subjectID, personID, field, oldV, newV, reason, scope.UserID)
	if err != nil {
		return fmt.Errorf("record the change: %w", err)
	}
	return nil
}

// EditCustomer puts a customer's details right, from the version the form was
// opened against. It says what changed. Nothing changing is a quiet success
// whatever the version, so a retried save is harmless.
//
// An invoice already issued keeps the name and address it went out with; this
// changes the customer from now on.
func EditCustomer(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, revision int, e CustomerEdit) ([]Change, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return nil, ErrNotFound
	}
	e.Name = strings.TrimSpace(e.Name)
	e.ContactName = strings.TrimSpace(e.ContactName)
	e.Reason = strings.TrimSpace(e.Reason)
	if e.Name == "" {
		return nil, fmt.Errorf("%w: a customer needs a name to address an invoice to", ErrInvalid)
	}
	if len([]rune(e.Reason)) > 300 {
		return nil, fmt.Errorf("%w: keep the reason to 300 characters", ErrInvalid)
	}
	var err error
	if e.Phone, err = cleanPhone(e.Phone); err != nil {
		return nil, err
	}
	if e.Email, err = cleanEmail(e.Email); err != nil {
		return nil, err
	}
	if e.OrgNumber, err = cleanOrgNumber(e.OrgNumber); err != nil {
		return nil, err
	}
	e.VATNumber = strings.TrimSpace(e.VATNumber)
	e.AddressLine1, e.AddressLine2 = strings.TrimSpace(e.AddressLine1), strings.TrimSpace(e.AddressLine2)
	e.PostalCode, e.City = strings.TrimSpace(e.PostalCode), strings.TrimSpace(e.City)

	var changes []Change
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var cur CustomerCard
		var rev int
		var merged *string
		if err := tx.QueryRow(ctx, `
			SELECT c.kind, c.revision, c.merged_into::text, coalesce(c.person_id::text, ''),
			       coalesce(c.company_name, ''), coalesce(c.org_number, ''), coalesce(c.vat_number, ''),
			       coalesce(c.address_line1, ''), coalesce(c.address_line2, ''), coalesce(c.postal_code, ''), coalesce(c.city, ''),
			       coalesce(p.display_name, ''), coalesce(p.phone, ''), coalesce(p.email, '')
			FROM customers c LEFT JOIN people p ON p.id = c.person_id
			WHERE c.id = $1 FOR UPDATE OF c`, id).Scan(&cur.Kind, &rev, &merged, &cur.PersonID,
			&cur.Name, &cur.OrgNumber, &cur.VATNumber, &cur.AddressLine1, &cur.AddressLine2, &cur.PostalCode, &cur.City,
			&cur.ContactName, &cur.Phone, &cur.Email); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock customer: %w", err)
		}
		if merged != nil {
			return fmt.Errorf("%w: this customer was merged into another; edit that one", ErrInvalid)
		}
		private := cur.Kind == "private"
		curName := cur.Name
		if private {
			curName = cur.ContactName
		}
		// What the person on the row holds, whichever kind of customer.
		want := []struct {
			field, old, new string
			personal        bool // belongs to the person on the row
			column          string
		}{
			{"name", curName, e.Name, private, ""},
			{"organisation number", cur.OrgNumber, e.OrgNumber, false, "org_number"},
			{"VAT number", cur.VATNumber, e.VATNumber, false, "vat_number"},
			{"address line 1", cur.AddressLine1, e.AddressLine1, private, "address_line1"},
			{"address line 2", cur.AddressLine2, e.AddressLine2, private, "address_line2"},
			{"postal code", cur.PostalCode, e.PostalCode, private, "postal_code"},
			{"city", cur.City, e.City, private, "city"},
			{"phone", cur.Phone, e.Phone, true, ""},
			{"email", cur.Email, e.Email, true, ""},
		}
		if !private {
			want = append(want, struct {
				field, old, new string
				personal        bool
				column          string
			}{"contact person", cur.ContactName, e.ContactName, true, ""})
		}

		type diff struct {
			field, old, new string
			personal        bool
			column          string
		}
		var diffs []diff
		for _, w := range want {
			if w.old != w.new {
				diffs = append(diffs, diff{w.field, w.old, w.new, w.personal, w.column})
			}
		}
		if len(diffs) == 0 {
			return nil
		}
		if rev != revision {
			return ErrRecordChanged
		}

		personID := cur.PersonID
		touchesPerson := false
		for _, d := range diffs {
			if d.field == "phone" || d.field == "email" || d.field == "contact person" || (private && d.field == "name") {
				touchesPerson = true
			}
		}
		// A company with nobody named yet gets its first contact now.
		if touchesPerson && personID == "" {
			if e.ContactName == "" {
				return fmt.Errorf("%w: name the contact person to give them a telephone number or email", ErrInvalid)
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO people (shop_id, display_name, phone, email)
				VALUES ($1, $2, nullif($3, ''), nullif($4, '')) RETURNING id`,
				scope.ShopID, e.ContactName, e.Phone, e.Email).Scan(&personID); err != nil {
				return fmt.Errorf("add contact: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE customers SET person_id = $2 WHERE id = $1`, id, personID); err != nil {
				return fmt.Errorf("attach contact: %w", err)
			}
			cur.ContactName, cur.Phone, cur.Email = e.ContactName, e.Phone, e.Email
		}

		for _, d := range diffs {
			var pid string
			if d.personal {
				pid = personID
			}
			switch d.field {
			case "name":
				if private {
					_, err := tx.Exec(ctx, `UPDATE people SET display_name = $2, updated_at = now() WHERE id = $1`, personID, d.new)
					if err != nil {
						return fmt.Errorf("rename: %w", err)
					}
				} else if _, err := tx.Exec(ctx, `UPDATE customers SET company_name = $2 WHERE id = $1`, id, d.new); err != nil {
					return fmt.Errorf("rename: %w", err)
				}
			case "contact person":
				if personID != "" && d.new != "" {
					if _, err := tx.Exec(ctx, `UPDATE people SET display_name = $2, updated_at = now() WHERE id = $1`, personID, d.new); err != nil {
						return fmt.Errorf("rename contact: %w", err)
					}
				}
			case "phone":
				if _, err := tx.Exec(ctx, `UPDATE people SET phone = nullif($2, ''), updated_at = now() WHERE id = $1`, personID, d.new); err != nil {
					return fmt.Errorf("change phone: %w", err)
				}
			case "email":
				if _, err := tx.Exec(ctx, `UPDATE people SET email = nullif($2, ''), updated_at = now() WHERE id = $1`, personID, d.new); err != nil {
					return fmt.Errorf("change email: %w", err)
				}
			default:
				if _, err := tx.Exec(ctx, `UPDATE customers SET `+d.column+` = nullif($2, '') WHERE id = $1`, id, d.new); err != nil {
					return fmt.Errorf("change %s: %w", d.field, err)
				}
			}
			if err := logChangeTx(ctx, tx, scope, "customer", id, pid, d.field, d.old, d.new, e.Reason); err != nil {
				return err
			}
			changes = append(changes, Change{Subject: "customer", Field: d.field, Old: d.old, New: d.new, Reason: e.Reason})
		}
		if _, err := tx.Exec(ctx, `UPDATE customers SET revision = revision + 1, updated_at = now() WHERE id = $1`, id); err != nil {
			return fmt.Errorf("bump revision: %w", err)
		}
		return nil
	})
	return changes, err
}

// AddCustomerPerson puts another person on a company: somebody else who rings
// or brings the car in. A private customer is a person and has no others.
func AddCustomerPerson(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, customerID, name, title, phone, email string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(customerID) {
		return ErrNotFound
	}
	name, title = strings.TrimSpace(name), strings.TrimSpace(title)
	if name == "" {
		return fmt.Errorf("%w: say who it is", ErrInvalid)
	}
	if len([]rune(title)) > 80 {
		return fmt.Errorf("%w: keep the title to 80 characters", ErrInvalid)
	}
	var err error
	if phone, err = cleanPhone(phone); err != nil {
		return err
	}
	if email, err = cleanEmail(email); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		var merged *string
		if err := tx.QueryRow(ctx, `SELECT kind, merged_into::text FROM customers WHERE id = $1 FOR UPDATE`, customerID).Scan(&kind, &merged); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock customer: %w", err)
		}
		if kind != "company" || merged != nil {
			return fmt.Errorf("%w: only a company has more than one contact", ErrInvalid)
		}
		var pid string
		if err := tx.QueryRow(ctx, `
			INSERT INTO people (shop_id, display_name, phone, email)
			VALUES ($1, $2, nullif($3, ''), nullif($4, '')) RETURNING id`,
			scope.ShopID, name, phone, email).Scan(&pid); err != nil {
			return fmt.Errorf("add person: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO customer_people (shop_id, customer_id, person_id, title)
			VALUES ($1, $2, $3, nullif($4, ''))`, scope.ShopID, customerID, pid, title); err != nil {
			return fmt.Errorf("add contact: %w", err)
		}
		return logChangeTx(ctx, tx, scope, "customer", customerID, pid, "contact added", "", name, "")
	})
}

// RemoveCustomerPerson takes somebody off a company. They stay on the record
// of what was agreed while they were on it.
func RemoveCustomerPerson(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, customerID, rowID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(customerID) || !looksLikeUUID(rowID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var pid, name string
		err := tx.QueryRow(ctx, `
			UPDATE customer_people cp SET removed_at = clock_timestamp()
			FROM people p
			WHERE cp.id = $1 AND cp.customer_id = $2 AND cp.removed_at IS NULL AND p.id = cp.person_id
			RETURNING cp.person_id, p.display_name`, rowID, customerID).Scan(&pid, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already off is a retried tap; never on is not there.
			var there bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM customer_people WHERE id = $1 AND customer_id = $2)`,
				rowID, customerID).Scan(&there); err != nil {
				return fmt.Errorf("check contact: %w", err)
			}
			if !there {
				return ErrNotFound
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("remove contact: %w", err)
		}
		return logChangeTx(ctx, tx, scope, "customer", customerID, pid, "contact removed", name, "", "")
	})
}

// ---- Vehicles -------------------------------------------------------------

// VehicleEdit is a vehicle's details as they should be after the save.
type VehicleEdit struct {
	VIN       string
	Make      string
	Model     string
	ModelYear *int16
	Engine    string
	Label     string
	Reason    string
}

// cleanVIN checks a chassis number. A 17-character one has no I, O or Q; older
// cars have shorter numbers of their own and are let through.
func cleanVIN(s string) (string, error) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	if s == "" {
		return "", nil
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'A' && r <= 'Z') && r != '-' {
			return "", fmt.Errorf("%w: a chassis number has letters and digits only", ErrInvalid)
		}
	}
	if len(s) < 5 || len(s) > 25 {
		return "", fmt.Errorf("%w: that is not the length of a chassis number", ErrInvalid)
	}
	if len(s) == 17 && strings.ContainsAny(s, "IOQ") {
		return "", fmt.Errorf("%w: a 17-character chassis number has no I, O or Q", ErrInvalid)
	}
	return s, nil
}

// EditVehicle corrects a vehicle's details. A chassis number already on
// another vehicle is a probable duplicate and is reported as one.
func EditVehicle(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, revision int, e VehicleEdit) ([]Change, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return nil, ErrNotFound
	}
	var err error
	if e.VIN, err = cleanVIN(e.VIN); err != nil {
		return nil, err
	}
	e.Make, e.Model = strings.TrimSpace(e.Make), strings.TrimSpace(e.Model)
	e.Engine, e.Label = strings.TrimSpace(e.Engine), strings.TrimSpace(e.Label)
	e.Reason = strings.TrimSpace(e.Reason)
	if e.ModelYear != nil && (*e.ModelYear < 1885 || *e.ModelYear > 2100) {
		return nil, fmt.Errorf("%w: that is not a model year", ErrInvalid)
	}
	if len([]rune(e.Reason)) > 300 {
		return nil, fmt.Errorf("%w: keep the reason to 300 characters", ErrInvalid)
	}
	year := ""
	if e.ModelYear != nil {
		year = fmt.Sprint(*e.ModelYear)
	}

	var changes []Change
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var rev int
		var vin, mk, model, engine, label, curYear string
		var merged *string
		if err := tx.QueryRow(ctx, `
			SELECT revision, merged_into::text, coalesce(vin, ''), coalesce(make, ''), coalesce(model, ''),
			       coalesce(model_year::text, ''), coalesce(engine, ''), coalesce(label, '')
			FROM vehicles WHERE id = $1 FOR UPDATE`, id).Scan(&rev, &merged, &vin, &mk, &model, &curYear, &engine, &label); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock vehicle: %w", err)
		}
		if merged != nil {
			return fmt.Errorf("%w: this vehicle was merged into another; edit that one", ErrInvalid)
		}
		type diff struct{ field, column, old, new string }
		var diffs []diff
		for _, w := range []diff{
			{"chassis number", "vin", vin, e.VIN}, {"make", "make", mk, e.Make}, {"model", "model", model, e.Model},
			{"model year", "model_year", curYear, year}, {"engine", "engine", engine, e.Engine}, {"description", "label", label, e.Label},
		} {
			if w.old != w.new {
				diffs = append(diffs, w)
			}
		}
		if len(diffs) == 0 {
			return nil
		}
		if rev != revision {
			return ErrRecordChanged
		}
		for _, d := range diffs {
			if d.column == "vin" && d.new != "" {
				var other string
				err := tx.QueryRow(ctx, `SELECT id FROM vehicles WHERE vin = $1 AND id <> $2 AND merged_into IS NULL LIMIT 1`, d.new, id).Scan(&other)
				if err == nil {
					return &DuplicateError{Kind: "vehicle", ID: other, Field: "chassis number"}
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("check chassis number: %w", err)
				}
			}
			cast := ""
			if d.column == "model_year" {
				cast = "::smallint"
			}
			if _, err := tx.Exec(ctx, `UPDATE vehicles SET `+d.column+` = nullif($2, '')`+cast+` WHERE id = $1`, id, d.new); err != nil {
				return fmt.Errorf("change %s: %w", d.field, err)
			}
			if err := logChangeTx(ctx, tx, scope, "vehicle", id, "", d.field, d.old, d.new, e.Reason); err != nil {
				return err
			}
			changes = append(changes, Change{Subject: "vehicle", Field: d.field, Old: d.old, New: d.new, Reason: e.Reason})
		}
		_, err := tx.Exec(ctx, `UPDATE vehicles SET revision = revision + 1, updated_at = now() WHERE id = $1`, id)
		return err
	})
	return changes, err
}

func isOverlap(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23P01"
}

func plateHolderTx(ctx context.Context, tx pgx.Tx, normalised, except string) string {
	var other string
	_ = tx.QueryRow(ctx, `
		SELECT vehicle_id FROM vehicle_registrations
		WHERE normalised = $1 AND vehicle_id <> $2 AND valid_to IS NULL LIMIT 1`, normalised, except).Scan(&other)
	return other
}

// CorrectRegistration fixes a plate that was typed wrong. The vehicle never
// carried the wrong one, so it is rewritten rather than added to the plates
// it has had.
func CorrectRegistration(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID, registration, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(vehicleID) {
		return ErrNotFound
	}
	registration = strings.ToUpper(strings.TrimSpace(registration))
	norm := NormaliseRegistration(registration)
	reason = strings.TrimSpace(reason)
	if norm == "" {
		return fmt.Errorf("%w: type the registration as it should be", ErrInvalid)
	}
	if len([]rune(reason)) > 300 {
		return fmt.Errorf("%w: keep the reason to 300 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var curID, cur, curNorm string
		if err := tx.QueryRow(ctx, `
			SELECT r.id, r.registration, r.normalised FROM vehicle_registrations r
			JOIN vehicles v ON v.id = r.vehicle_id
			WHERE r.vehicle_id = $1 AND r.valid_to IS NULL AND v.merged_into IS NULL
			FOR UPDATE OF r`, vehicleID).Scan(&curID, &cur, &curNorm); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: this vehicle has no plate to correct; add one as a change", ErrInvalid)
			}
			return fmt.Errorf("lock registration: %w", err)
		}
		if curNorm == norm && cur == registration {
			return nil
		}
		if holder := plateHolderTx(ctx, tx, norm, vehicleID); holder != "" {
			return &DuplicateError{Kind: "vehicle", ID: holder, Field: "registration"}
		}
		if _, err := tx.Exec(ctx, `UPDATE vehicle_registrations SET registration = $2, normalised = $3 WHERE id = $1`,
			curID, registration, norm); err != nil {
			if isOverlap(err) {
				return &DuplicateError{Kind: "vehicle", ID: plateHolderTx(ctx, tx, norm, vehicleID), Field: "registration"}
			}
			return fmt.Errorf("correct registration: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE vehicles SET revision = revision + 1, updated_at = now() WHERE id = $1`, vehicleID); err != nil {
			return fmt.Errorf("bump revision: %w", err)
		}
		return logChangeTx(ctx, tx, scope, "registration", vehicleID, "", "registration (corrected)", cur, registration, reason)
	})
}

// ChangeRegistration records that the vehicle took another plate from a date.
// The old plate stays in its history, which is how a service book with the
// previous number in it is made sense of.
func ChangeRegistration(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID, registration string, from time.Time, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(vehicleID) {
		return ErrNotFound
	}
	registration = strings.ToUpper(strings.TrimSpace(registration))
	norm := NormaliseRegistration(registration)
	reason = strings.TrimSpace(reason)
	if norm == "" {
		return fmt.Errorf("%w: type the new registration", ErrInvalid)
	}
	if len([]rune(reason)) > 300 {
		return fmt.Errorf("%w: keep the reason to 300 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var curID, cur, curNorm string
		var country *string
		var since time.Time
		if err := tx.QueryRow(ctx, `
			SELECT r.id, r.registration, r.normalised, r.country, r.valid_from FROM vehicle_registrations r
			JOIN vehicles v ON v.id = r.vehicle_id
			WHERE r.vehicle_id = $1 AND r.valid_to IS NULL AND v.merged_into IS NULL
			FOR UPDATE OF r`, vehicleID).Scan(&curID, &cur, &curNorm, &country, &since); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock registration: %w", err)
		}
		if curNorm == norm {
			return fmt.Errorf("%w: that is the plate it already carries; to fix a typing mistake, correct it instead", ErrInvalid)
		}
		if from.IsZero() {
			from = time.Now()
		}
		if !from.After(since) {
			return fmt.Errorf("%w: the change cannot be dated before the current plate was put on", ErrInvalid)
		}
		if from.After(time.Now().Add(time.Minute)) {
			return fmt.Errorf("%w: a change of plate cannot be dated in the future", ErrInvalid)
		}
		if holder := plateHolderTx(ctx, tx, norm, vehicleID); holder != "" {
			return &DuplicateError{Kind: "vehicle", ID: holder, Field: "registration"}
		}
		if _, err := tx.Exec(ctx, `UPDATE vehicle_registrations SET valid_to = $2 WHERE id = $1`, curID, from); err != nil {
			return fmt.Errorf("close the plate: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised, country, valid_from)
			VALUES ($1, $2, $3, $4, $5, $6)`, scope.ShopID, vehicleID, registration, norm, country, from); err != nil {
			if isOverlap(err) {
				return &DuplicateError{Kind: "vehicle", ID: plateHolderTx(ctx, tx, norm, vehicleID), Field: "registration"}
			}
			return fmt.Errorf("open the new plate: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE vehicles SET revision = revision + 1, updated_at = now() WHERE id = $1`, vehicleID); err != nil {
			return fmt.Errorf("bump revision: %w", err)
		}
		return logChangeTx(ctx, tx, scope, "registration", vehicleID, "", "registration (changed)", cur, registration, reason)
	})
}

// SellVehicle ends the current owner's period at a date and, when the new
// owner is known, starts theirs. The earlier jobs stay on the earlier
// customer; nothing the previous owner was charged or said is shown against
// the new one.
func SellVehicle(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID, newCustomerID string, from time.Time, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(vehicleID) || (newCustomerID != "" && !looksLikeUUID(newCustomerID)) {
		return ErrNotFound
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 300 {
		return fmt.Errorf("%w: keep the reason to 300 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var merged *string
		if err := tx.QueryRow(ctx, `SELECT merged_into::text FROM vehicles WHERE id = $1 FOR UPDATE`, vehicleID).Scan(&merged); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock vehicle: %w", err)
		}
		if merged != nil {
			return fmt.Errorf("%w: this vehicle was merged into another", ErrInvalid)
		}
		if from.IsZero() {
			from = time.Now()
		}
		if from.After(time.Now().Add(time.Minute)) {
			return fmt.Errorf("%w: a sale cannot be dated in the future", ErrInvalid)
		}
		var curID, curCustomer, curName string
		var since time.Time
		err := tx.QueryRow(ctx, `
			SELECT o.id, o.customer_id, coalesce(cp.display_name, c.company_name, ''), o.owned_from
			FROM vehicle_ownership o JOIN customers c ON c.id = o.customer_id LEFT JOIN people cp ON cp.id = c.person_id
			WHERE o.vehicle_id = $1 AND o.owned_to IS NULL FOR UPDATE OF o`, vehicleID).Scan(&curID, &curCustomer, &curName, &since)
		hasOwner := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock ownership: %w", err)
		}
		if hasOwner && curCustomer == newCustomerID {
			return fmt.Errorf("%w: they already own it", ErrInvalid)
		}
		if hasOwner && !from.After(since) {
			return fmt.Errorf("%w: the sale cannot be dated before the owner bought it", ErrInvalid)
		}
		var newName string
		if newCustomerID != "" {
			if err := tx.QueryRow(ctx, `
				SELECT coalesce(cp.display_name, c.company_name, '') FROM customers c LEFT JOIN people cp ON cp.id = c.person_id
				WHERE c.id = $1 AND c.merged_into IS NULL`, newCustomerID).Scan(&newName); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("read new owner: %w", err)
			}
		} else if !hasOwner {
			return fmt.Errorf("%w: nobody owns it on record, so there is nothing to end", ErrInvalid)
		}
		if hasOwner {
			if _, err := tx.Exec(ctx, `UPDATE vehicle_ownership SET owned_to = $2 WHERE id = $1`, curID, from); err != nil {
				return fmt.Errorf("end the ownership: %w", err)
			}
		}
		if newCustomerID != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id, owned_from) VALUES ($1, $2, $3, $4)`,
				scope.ShopID, vehicleID, newCustomerID, from); err != nil {
				if isOverlap(err) {
					return fmt.Errorf("%w: that overlaps another owner's period", ErrInvalid)
				}
				return fmt.Errorf("start the ownership: %w", err)
			}
		}
		return logChangeTx(ctx, tx, scope, "ownership", vehicleID, "", "owner", curName, newName, reason)
	})
}

// VehicleRevision is the version a vehicle's edit form is opened against.
func VehicleRevision(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (int, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return 0, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return 0, ErrNotFound
	}
	var rev int
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT revision FROM vehicles WHERE id = $1`, id).Scan(&rev); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return nil
	})
	return rev, err
}

// VehicleChanges lists what has been corrected on a vehicle.
func VehicleChanges(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) ([]Change, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return nil, ErrNotFound
	}
	var out []Change
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = changesTx(ctx, tx, id)
		return err
	})
	return out, err
}
