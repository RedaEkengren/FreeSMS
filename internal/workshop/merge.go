package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Finding that two records are one, and joining them without losing what was
// written against either.
//
// People are never matched by name alone. Two Anna Anderssons are two people
// until something that identifies one person says otherwise: the same
// organisation number, the same email address, the same telephone number. A
// telephone number is the weakest of those, since a family shares one, so it
// is reported as what it is and decided by a person.

// Candidate is a customer that may be the one being typed, and why.
type Candidate struct {
	Customer
	Why []string
}

// The reasons, as catalogue keys.
const (
	whyOrg   = "the same company number"
	whyEmail = "the same email address"
	whyPhone = "the same telephone number"
)

func normaliseOrg(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// duplicatesTx finds customers sharing an identifier with what is given. exclude
// is a customer to leave out (the one being compared). A person at a company
// counts: the fleet manager's number is the company's.
func duplicatesTx(ctx context.Context, tx pgx.Tx, org, email, phone, exclude string) ([]Candidate, error) {
	org, email, digits := normaliseOrg(org), strings.ToLower(strings.TrimSpace(email)), digitsOf(phone)
	if len(digits) < 5 {
		digits = ""
	}
	if len(org) < 4 {
		org = ""
	}
	if org == "" && email == "" && digits == "" {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT`+customerColumns+`,
		       ($1 <> '' AND regexp_replace(upper(coalesce(c.org_number, '')), '[^0-9A-Z]', '', 'g') = $1),
		       ($2 <> '' AND EXISTS (SELECT 1 FROM people q WHERE lower(q.email::text) = $2 AND q.erased_at IS NULL
		            AND (q.id = c.person_id OR q.id IN (SELECT cp.person_id FROM customer_people cp WHERE cp.customer_id = c.id AND cp.removed_at IS NULL)))),
		       ($3 <> '' AND EXISTS (SELECT 1 FROM people q WHERE regexp_replace(coalesce(q.phone, ''), '\D', '', 'g') = $3 AND q.erased_at IS NULL
		            AND (q.id = c.person_id OR q.id IN (SELECT cp.person_id FROM customer_people cp WHERE cp.customer_id = c.id AND cp.removed_at IS NULL))))
		FROM customers c LEFT JOIN people p ON p.id = c.person_id
		WHERE c.merged_into IS NULL AND c.id::text <> $4 AND p.erased_at IS NULL
		ORDER BY 9 DESC, 3
		LIMIT 10`, org, email, digits, exclude)
	if err != nil {
		return nil, fmt.Errorf("look for duplicates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var o, e, p bool
		if err := rows.Scan(&c.ID, &c.Kind, &c.Name, &c.OrgNumber, &c.VATNumber, &c.Phone, &c.Email, &c.Address, &c.Orders, &o, &e, &p); err != nil {
			return nil, fmt.Errorf("scan duplicate: %w", err)
		}
		if o {
			c.Why = append(c.Why, whyOrg)
		}
		if e {
			c.Why = append(c.Why, whyEmail)
		}
		if p {
			c.Why = append(c.Why, whyPhone)
		}
		if len(c.Why) > 0 {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// CustomerDuplicates lists customers a new one may be a second copy of.
func CustomerDuplicates(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewCustomer) ([]Candidate, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []Candidate
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = duplicatesTx(ctx, tx, n.OrgNumber, n.Email, n.Phone, "")
		return err
	})
	return out, err
}

// LikelySame lists the other customers that share an identifier with this one,
// for the card to offer a review.
func LikelySame(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, customerID string) ([]Candidate, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(customerID) {
		return nil, ErrNotFound
	}
	var out []Candidate
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var org, email, phone string
		var merged *string
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(c.org_number, ''), coalesce(p.email::text, ''), coalesce(p.phone, ''), c.merged_into::text
			FROM customers c LEFT JOIN people p ON p.id = c.person_id WHERE c.id = $1`, customerID).Scan(&org, &email, &phone, &merged); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read customer: %w", err)
		}
		if merged != nil {
			return nil
		}
		var err error
		out, err = duplicatesTx(ctx, tx, org, email, phone, customerID)
		return err
	})
	return out, err
}

// CreateCustomerChecked makes a customer unless it may be a second copy of one
// we have. Then nothing is written, the candidates are returned, and the
// caller asks; confirmed says a person looked at them and chose to go on.
func CreateCustomerChecked(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewCustomer, confirmed bool) (string, []Candidate, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", nil, access.ErrForbidden
	}
	if !confirmed {
		dups, err := CustomerDuplicates(ctx, pool, scope, n)
		if err != nil {
			return "", nil, err
		}
		if len(dups) > 0 {
			return "", dups, nil
		}
	}
	id, err := CreateCustomer(ctx, pool, scope, n)
	return id, nil, err
}

// MergePreview is what a merge would do, shown before it is done.
type MergePreview struct {
	Keep, Drop   Customer
	KeepRevision int
	DropRevision int
	Jobs         int
	Ownerships   int
	People       int
	// Fields on the kept record that are empty and would be filled in from
	// the other.
	FillsIn []string
	// Issued documents are not touched; this is how many there are.
	Invoices int
}

// PreviewCustomerMerge reports what merging drop into keep would move.
func PreviewCustomerMerge(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keepID, dropID string) (MergePreview, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return MergePreview{}, access.ErrForbidden
	}
	var p MergePreview
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = customerMergeTx(ctx, tx, keepID, dropID, false)
		return err
	})
	return p, err
}

// MergeCustomers joins two customer records that are one. Jobs, ownership and
// contacts move to the one kept; the other stays as a marked record. Issued
// invoices carry the name they went out with and are not rewritten. Both
// forms were opened against the versions given.
func MergeCustomers(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keepID, dropID string, keepRev, dropRev int, reason string) (MergePreview, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return MergePreview{}, access.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 300 {
		return MergePreview{}, fmt.Errorf("%w: keep the reason to 300 characters", ErrInvalid)
	}
	var p MergePreview
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if p, err = customerMergeTx(ctx, tx, keepID, dropID, true); err != nil {
			return err
		}
		if p.KeepRevision != keepRev || p.DropRevision != dropRev {
			return ErrRecordChanged
		}
		return doCustomerMergeTx(ctx, tx, scope, keepID, dropID, p, reason)
	})
	return p, err
}

func customerMergeTx(ctx context.Context, tx pgx.Tx, keepID, dropID string, lock bool) (MergePreview, error) {
	var p MergePreview
	if !looksLikeUUID(keepID) || !looksLikeUUID(dropID) {
		return p, ErrNotFound
	}
	if keepID == dropID {
		return p, fmt.Errorf("%w: a customer cannot be merged into itself", ErrInvalid)
	}
	if lock {
		// In a fixed order, so two merges of the same pair cannot wait on
		// each other.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM customers WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, keepID, dropID); err != nil {
			return p, fmt.Errorf("lock customers: %w", err)
		}
	}
	read := func(id string, c *Customer, rev *int, kind *string, merged **string) error {
		err := tx.QueryRow(ctx, `SELECT`+customerColumns+`, c.revision, c.merged_into::text
			FROM customers c LEFT JOIN people p ON p.id = c.person_id WHERE c.id = $1`, id).Scan(&c.ID, &c.Kind, &c.Name,
			&c.OrgNumber, &c.VATNumber, &c.Phone, &c.Email, &c.Address, &c.Orders, rev, merged)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read customer: %w", err)
		}
		*kind = c.Kind
		return nil
	}
	var kk, dk string
	var km, dm *string
	if err := read(keepID, &p.Keep, &p.KeepRevision, &kk, &km); err != nil {
		return p, err
	}
	if err := read(dropID, &p.Drop, &p.DropRevision, &dk, &dm); err != nil {
		return p, err
	}
	if km != nil || dm != nil {
		return p, fmt.Errorf("%w: one of them has already been merged into another", ErrInvalid)
	}
	if kk != dk {
		return p, fmt.Errorf("%w: a person and a company are not the same customer", ErrInvalid)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE customer_id = $1`, dropID).Scan(&p.Jobs); err != nil {
		return p, fmt.Errorf("count jobs: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM vehicle_ownership WHERE customer_id = $1`, dropID).Scan(&p.Ownerships); err != nil {
		return p, fmt.Errorf("count ownership: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM customer_people WHERE customer_id = $1 AND removed_at IS NULL`, dropID).Scan(&p.People); err != nil {
		return p, fmt.Errorf("count contacts: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM invoices i JOIN work_orders w ON w.id = i.work_order_id WHERE w.customer_id = $1`, dropID).Scan(&p.Invoices); err != nil {
		return p, fmt.Errorf("count invoices: %w", err)
	}
	fill := func(field, keepV, dropV string) {
		if keepV == "" && dropV != "" {
			p.FillsIn = append(p.FillsIn, field)
		}
	}
	fill("telephone", p.Keep.Phone, p.Drop.Phone)
	fill("email", p.Keep.Email, p.Drop.Email)
	fill("company number", p.Keep.OrgNumber, p.Drop.OrgNumber)
	fill("VAT number", p.Keep.VATNumber, p.Drop.VATNumber)
	fill("address", p.Keep.Address, p.Drop.Address)
	return p, nil
}

func doCustomerMergeTx(ctx context.Context, tx pgx.Tx, scope access.Scope, keepID, dropID string, p MergePreview, reason string) error {
	var keepPerson, dropPerson, kind string
	if err := tx.QueryRow(ctx, `SELECT kind, coalesce(person_id::text, '') FROM customers WHERE id = $1`, keepID).Scan(&kind, &keepPerson); err != nil {
		return fmt.Errorf("read kept: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(person_id::text, '') FROM customers WHERE id = $1`, dropID).Scan(&dropPerson); err != nil {
		return fmt.Errorf("read merged: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE work_orders SET customer_id = $1 WHERE customer_id = $2`, keepID, dropID); err != nil {
		return fmt.Errorf("move jobs: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE vehicle_ownership SET customer_id = $1 WHERE customer_id = $2`, keepID, dropID); err != nil {
		return fmt.Errorf("move ownership: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE customer_people SET customer_id = $1 WHERE customer_id = $2`, keepID, dropID); err != nil {
		return fmt.Errorf("move contacts: %w", err)
	}
	// Fill what the kept record lacks, never overwrite what it has.
	if keepPerson != "" && dropPerson != "" {
		// An email address is unique in the shop, so it moves rather than
		// being copied.
		var keepEmail, dropEmail *string
		if err := tx.QueryRow(ctx, `SELECT (SELECT email::text FROM people WHERE id = $1), (SELECT email::text FROM people WHERE id = $2)`,
			keepPerson, dropPerson).Scan(&keepEmail, &dropEmail); err != nil {
			return fmt.Errorf("read emails: %w", err)
		}
		if keepEmail == nil && dropEmail != nil {
			if _, err := tx.Exec(ctx, `UPDATE people SET email = NULL, updated_at = now() WHERE id = $1`, dropPerson); err != nil {
				return fmt.Errorf("move email: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE people SET email = $2, updated_at = now() WHERE id = $1`, keepPerson, *dropEmail); err != nil {
				return fmt.Errorf("move email: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE people k SET phone = coalesce(k.phone, d.phone), updated_at = now()
			FROM people d WHERE k.id = $1 AND d.id = $2`, keepPerson, dropPerson); err != nil {
			return fmt.Errorf("fill in contact: %w", err)
		}
	}
	if kind == "company" && dropPerson != "" && dropPerson != keepPerson {
		// The other company's main contact is still somebody to call.
		if keepPerson == "" {
			if _, err := tx.Exec(ctx, `UPDATE customers SET person_id = $2 WHERE id = $1`, keepID, dropPerson); err != nil {
				return fmt.Errorf("keep contact: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `INSERT INTO customer_people (shop_id, customer_id, person_id, title) VALUES ($1, $2, $3, 'Merged in')`,
			scope.ShopID, keepID, dropPerson); err != nil {
			return fmt.Errorf("keep contact: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE customers k SET
		  org_number = coalesce(k.org_number, d.org_number), vat_number = coalesce(k.vat_number, d.vat_number),
		  address_line1 = CASE WHEN k.address_line1 IS NULL THEN d.address_line1 ELSE k.address_line1 END,
		  address_line2 = CASE WHEN k.address_line1 IS NULL THEN d.address_line2 ELSE k.address_line2 END,
		  postal_code = CASE WHEN k.address_line1 IS NULL THEN d.postal_code ELSE k.postal_code END,
		  city = CASE WHEN k.address_line1 IS NULL THEN d.city ELSE k.city END,
		  revision = k.revision + 1, updated_at = now()
		FROM customers d WHERE k.id = $1 AND d.id = $2`, keepID, dropID); err != nil {
		return fmt.Errorf("fill in details: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE customers SET merged_into = $2, merged_at = clock_timestamp(), revision = revision + 1, updated_at = now() WHERE id = $1`, dropID, keepID); err != nil {
		return fmt.Errorf("mark merged: %w", err)
	}
	summary := fmt.Sprintf("%d jobs, %d ownerships, %d contacts moved; %d issued invoices left as issued", p.Jobs, p.Ownerships, p.People, p.Invoices)
	if err := logChangeTx(ctx, tx, scope, "merge", keepID, dropPerson, "merged in", p.Drop.Name, summary, reason); err != nil {
		return err
	}
	return logChangeTx(ctx, tx, scope, "customer", dropID, "", "merged into", "", p.Keep.Name, reason)
}

// ---- Vehicles -------------------------------------------------------------

// VehicleMergePreview is what joining two vehicle records would do.
type VehicleMergePreview struct {
	Keep, Drop   Vehicle
	KeepRevision int
	DropRevision int
	Jobs         int
	Plates       int
	Ownerships   int
	Readings     int
	FillsIn      []string
	// Why it cannot be done, if it cannot.
	Blocked string
}

// PreviewVehicleMerge reports what merging drop into keep would move, or why
// it cannot be done.
func PreviewVehicleMerge(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keepID, dropID string) (VehicleMergePreview, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return VehicleMergePreview{}, access.ErrForbidden
	}
	var p VehicleMergePreview
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = vehicleMergeTx(ctx, tx, keepID, dropID, false)
		return err
	})
	return p, err
}

func vehicleMergeTx(ctx context.Context, tx pgx.Tx, keepID, dropID string, lock bool) (VehicleMergePreview, error) {
	var p VehicleMergePreview
	if !looksLikeUUID(keepID) || !looksLikeUUID(dropID) {
		return p, ErrNotFound
	}
	if keepID == dropID {
		return p, fmt.Errorf("%w: a vehicle cannot be merged into itself", ErrInvalid)
	}
	if lock {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM vehicles WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, keepID, dropID); err != nil {
			return p, fmt.Errorf("lock vehicles: %w", err)
		}
	}
	read := func(id string, v *Vehicle, rev *int) error {
		var year *int16
		var merged string
		err := tx.QueryRow(ctx, `
			SELECT v.id, coalesce(r.registration, ''), coalesce(v.vin, ''), coalesce(v.make, ''), coalesce(v.model, ''),
			       v.model_year, coalesce(v.engine, ''), coalesce(v.label, ''), v.revision, coalesce(v.merged_into::text, '')
			FROM vehicles v LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE v.id = $1`, id).Scan(&v.ID, &v.Registration, &v.VIN, &v.Make, &v.Model, &year, &v.Engine, &v.Label, rev, &merged)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read vehicle: %w", err)
		}
		v.ModelYear, v.MergedInto = year, merged
		return nil
	}
	if err := read(keepID, &p.Keep, &p.KeepRevision); err != nil {
		return p, err
	}
	if err := read(dropID, &p.Drop, &p.DropRevision); err != nil {
		return p, err
	}
	if p.Keep.MergedInto != "" || p.Drop.MergedInto != "" {
		return p, fmt.Errorf("%w: one of them has already been merged into another", ErrInvalid)
	}
	for q, dst := range map[string]*int{
		`SELECT count(*) FROM work_orders WHERE vehicle_id = $1`:           &p.Jobs,
		`SELECT count(*) FROM vehicle_registrations WHERE vehicle_id = $1`: &p.Plates,
		`SELECT count(*) FROM vehicle_ownership WHERE vehicle_id = $1`:     &p.Ownerships,
		`SELECT count(*) FROM odometer_readings WHERE vehicle_id = $1`:     &p.Readings,
	} {
		if err := tx.QueryRow(ctx, q, dropID).Scan(dst); err != nil {
			return p, fmt.Errorf("count: %w", err)
		}
	}
	fill := func(field, a, b string) {
		if a == "" && b != "" {
			p.FillsIn = append(p.FillsIn, field)
		}
	}
	fill("chassis number", p.Keep.VIN, p.Drop.VIN)
	fill("make", p.Keep.Make, p.Drop.Make)
	fill("model", p.Keep.Model, p.Drop.Model)
	fill("engine", p.Keep.Engine, p.Drop.Engine)
	if p.Keep.ModelYear == nil && p.Drop.ModelYear != nil {
		p.FillsIn = append(p.FillsIn, "model year")
	}
	if p.Keep.VIN != "" && p.Drop.VIN != "" && p.Keep.VIN != p.Drop.VIN {
		p.Blocked = "They carry different chassis numbers, so they are two cars."
	}
	var clash bool
	if err := tx.QueryRow(ctx, `
		SELECT exists(SELECT 1 FROM vehicle_ownership a JOIN vehicle_ownership b
		               ON a.vehicle_id = $1 AND b.vehicle_id = $2
		              AND tstzrange(a.owned_from, a.owned_to) && tstzrange(b.owned_from, b.owned_to))`, keepID, dropID).Scan(&clash); err != nil {
		return p, fmt.Errorf("check ownership: %w", err)
	}
	if clash && p.Blocked == "" {
		p.Blocked = "Both have an owner at the same time. Sell one of them first, so the dates agree."
	}
	if err := tx.QueryRow(ctx, `
		SELECT exists(SELECT 1 FROM vehicle_registrations a JOIN vehicle_registrations b
		               ON a.vehicle_id = $1 AND b.vehicle_id = $2 AND a.normalised = b.normalised
		              AND tstzrange(a.valid_from, a.valid_to) && tstzrange(b.valid_from, b.valid_to))`, keepID, dropID).Scan(&clash); err != nil {
		return p, fmt.Errorf("check plates: %w", err)
	}
	if clash && p.Blocked == "" {
		p.Blocked = "They carry the same plate at the same time."
	}
	return p, nil
}

// MergeVehicles joins two vehicle records that are one car. Jobs, plates,
// ownership and readings move to the one kept; a plate the other still carried
// becomes a plate it used to carry. Refused when the periods would overlap.
func MergeVehicles(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keepID, dropID string, keepRev, dropRev int, reason string) (VehicleMergePreview, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return VehicleMergePreview{}, access.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 300 {
		return VehicleMergePreview{}, fmt.Errorf("%w: keep the reason to 300 characters", ErrInvalid)
	}
	var p VehicleMergePreview
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if p, err = vehicleMergeTx(ctx, tx, keepID, dropID, true); err != nil {
			return err
		}
		if p.Blocked != "" {
			return fmt.Errorf("%w: %s", ErrInvalid, p.Blocked)
		}
		if p.KeepRevision != keepRev || p.DropRevision != dropRev {
			return ErrRecordChanged
		}
		// Only one plate is the current one. The other's current plate becomes
		// one it used to carry, from now.
		if _, err := tx.Exec(ctx, `
			UPDATE vehicle_registrations d SET valid_to = greatest(clock_timestamp(), d.valid_from + interval '1 microsecond')
			WHERE d.vehicle_id = $2 AND d.valid_to IS NULL
			  AND EXISTS (SELECT 1 FROM vehicle_registrations k WHERE k.vehicle_id = $1 AND k.valid_to IS NULL)`, keepID, dropID); err != nil {
			return fmt.Errorf("retire plate: %w", err)
		}
		for _, t := range []string{"vehicle_registrations", "vehicle_ownership", "odometer_readings", "work_orders", "attachments", "bookings"} {
			if _, err := tx.Exec(ctx, `UPDATE `+t+` SET vehicle_id = $1 WHERE vehicle_id = $2`, keepID, dropID); err != nil {
				if isOverlap(err) {
					return fmt.Errorf("%w: the periods overlap, so they cannot be put together", ErrInvalid)
				}
				return fmt.Errorf("move %s: %w", t, err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE vehicles k SET vin = coalesce(k.vin, d.vin), make = coalesce(k.make, d.make), model = coalesce(k.model, d.model),
			       model_year = coalesce(k.model_year, d.model_year), engine = coalesce(k.engine, d.engine),
			       revision = k.revision + 1, updated_at = now()
			FROM vehicles d WHERE k.id = $1 AND d.id = $2`, keepID, dropID); err != nil {
			return fmt.Errorf("fill in details: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE vehicles SET vin = NULL WHERE id = $1 AND EXISTS (SELECT 1 FROM vehicles WHERE id = $2 AND vin = (SELECT vin FROM vehicles WHERE id = $1))`, dropID, keepID); err != nil {
			return fmt.Errorf("release chassis number: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE vehicles SET merged_into = $2, merged_at = clock_timestamp(), revision = revision + 1, updated_at = now() WHERE id = $1`, dropID, keepID); err != nil {
			return fmt.Errorf("mark merged: %w", err)
		}
		summary := fmt.Sprintf("%d jobs, %d plates, %d ownerships, %d readings moved", p.Jobs, p.Plates, p.Ownerships, p.Readings)
		if err := logChangeTx(ctx, tx, scope, "merge", keepID, "", "merged in", p.Drop.Describe(), summary, reason); err != nil {
			return err
		}
		return logChangeTx(ctx, tx, scope, "vehicle", dropID, "", "merged into", "", p.Keep.Describe(), reason)
	})
	return p, err
}
