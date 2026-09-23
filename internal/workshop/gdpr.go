package workshop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// accountingRetention is how long a record that supports an invoice has to be
// kept. Swedish bookkeeping law says seven years after the end of the calendar
// year the financial year ended in; seven years is the working number.
const accountingRetention = 7 * 365 * 24 * time.Hour

// ErasedName is what replaces somebody's name when they are erased.
//
// A placeholder rather than an empty string, so that a screen showing a work
// order from 2019 says something honest instead of looking broken.
const ErasedName = "Erased at the person's request"

// PersonExport is everything held about one person.
//
// Article 15 asks for a copy of the personal data, not a database dump. This
// is the former: what it is, where it came from, and why it is still here.
type PersonExport struct {
	ExportedAt time.Time `json:"exported_at"`
	Shop       string    `json:"shop"`

	Person struct {
		Name     string     `json:"name"`
		Email    string     `json:"email,omitempty"`
		Phone    string     `json:"phone,omitempty"`
		Since    time.Time  `json:"known_since"`
		ErasedAt *time.Time `json:"erased_at,omitempty"`
	} `json:"person"`

	Addresses  []ExportedAddress   `json:"addresses,omitempty"`
	Vehicles   []ExportedVehicle   `json:"vehicles,omitempty"`
	Jobs       []ExportedJob       `json:"jobs,omitempty"`
	Invoices   []ExportedInvoice   `json:"invoices,omitempty"`
	Employment *ExportedEmployment `json:"employment,omitempty"`

	// Written out rather than left for somebody to infer.
	Notes []string `json:"notes"`
}

type ExportedAddress struct {
	Line1      string `json:"line1,omitempty"`
	Line2      string `json:"line2,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	City       string `json:"city,omitempty"`
	Country    string `json:"country,omitempty"`
}

type ExportedVehicle struct {
	Registration string     `json:"registration,omitempty"`
	VIN          string     `json:"vin,omitempty"`
	Make         string     `json:"make,omitempty"`
	Model        string     `json:"model,omitempty"`
	OwnedFrom    time.Time  `json:"owned_from"`
	OwnedTo      *time.Time `json:"owned_to,omitempty"`
}

type ExportedJob struct {
	Number    int64     `json:"number"`
	OpenedAt  time.Time `json:"opened_at"`
	State     string    `json:"state"`
	Complaint string    `json:"complaint,omitempty"`
	Work      []string  `json:"work,omitempty"`
}

type ExportedInvoice struct {
	Reference string    `json:"reference"`
	IssuedAt  time.Time `json:"issued_at"`
	Gross     string    `json:"gross"`
	KeptUntil time.Time `json:"kept_until"`
}

type ExportedEmployment struct {
	Role          string     `json:"role"`
	Active        bool       `json:"active"`
	HoursClocked  float64    `json:"hours_clocked"`
	Since         time.Time  `json:"since"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

// ExportPerson gathers everything held about somebody.
func ExportPerson(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, personID string) (PersonExport, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return PersonExport{}, access.ErrForbidden
	}
	if !looksLikeUUID(personID) {
		return PersonExport{}, ErrNotFound
	}

	var e PersonExport
	e.ExportedAt = time.Now()

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var email, phone *string
		err := tx.QueryRow(ctx, `
			SELECT s.name, p.display_name, p.email, p.phone, p.created_at, p.erased_at
			FROM people p JOIN shops s ON s.id = p.shop_id
			WHERE p.id = $1`, personID).Scan(
			&e.Shop, &e.Person.Name, &email, &phone, &e.Person.Since, &e.Person.ErasedAt)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read person: %w", err)
		}
		if email != nil {
			e.Person.Email = *email
		}
		if phone != nil {
			e.Person.Phone = *phone
		}

		addrs, err := tx.Query(ctx, `
			SELECT coalesce(address_line1,''), coalesce(address_line2,''),
			       coalesce(postal_code,''), coalesce(city,''), coalesce(country,'')
			FROM customers WHERE person_id = $1`, personID)
		if err != nil {
			return fmt.Errorf("read addresses: %w", err)
		}
		for addrs.Next() {
			var a ExportedAddress
			if err := addrs.Scan(&a.Line1, &a.Line2, &a.PostalCode, &a.City, &a.Country); err != nil {
				addrs.Close()
				return err
			}
			if a != (ExportedAddress{}) {
				e.Addresses = append(e.Addresses, a)
			}
		}
		addrs.Close()

		vehicles, err := tx.Query(ctx, `
			SELECT coalesce(r.registration,''), coalesce(v.vin,''),
			       coalesce(v.make,''), coalesce(v.model,''), o.owned_from, o.owned_to
			FROM vehicle_ownership o
			JOIN customers c ON c.id = o.customer_id
			JOIN vehicles v  ON v.id = o.vehicle_id
			LEFT JOIN vehicle_registrations r
			       ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE c.person_id = $1
			ORDER BY o.owned_from`, personID)
		if err != nil {
			return fmt.Errorf("read vehicles: %w", err)
		}
		for vehicles.Next() {
			var v ExportedVehicle
			if err := vehicles.Scan(&v.Registration, &v.VIN, &v.Make, &v.Model, &v.OwnedFrom, &v.OwnedTo); err != nil {
				vehicles.Close()
				return err
			}
			e.Vehicles = append(e.Vehicles, v)
		}
		vehicles.Close()

		jobs, err := tx.Query(ctx, `
			SELECT w.number, w.opened_at, w.state, coalesce(w.complaint,''),
			       coalesce(array_agg(l.description ORDER BY l.position)
			                FILTER (WHERE l.id IS NOT NULL), '{}')
			FROM work_orders w
			JOIN customers c ON c.id = w.customer_id
			LEFT JOIN work_order_lines l ON l.work_order_id = w.id
			WHERE c.person_id = $1
			GROUP BY w.id
			ORDER BY w.opened_at`, personID)
		if err != nil {
			return fmt.Errorf("read jobs: %w", err)
		}
		for jobs.Next() {
			var j ExportedJob
			if err := jobs.Scan(&j.Number, &j.OpenedAt, &j.State, &j.Complaint, &j.Work); err != nil {
				jobs.Close()
				return err
			}
			e.Jobs = append(e.Jobs, j)
		}
		jobs.Close()

		invoices, err := tx.Query(ctx, `
			SELECT i.series, i.number, i.issued_at, i.gross_minor
			FROM invoices i
			JOIN work_orders w ON w.id = i.work_order_id
			JOIN customers c   ON c.id = w.customer_id
			WHERE c.person_id = $1
			ORDER BY i.issued_at`, personID)
		if err != nil {
			return fmt.Errorf("read invoices: %w", err)
		}
		for invoices.Next() {
			var series string
			var number int64
			var issued time.Time
			var gross int64
			if err := invoices.Scan(&series, &number, &issued, &gross); err != nil {
				invoices.Close()
				return err
			}
			e.Invoices = append(e.Invoices, ExportedInvoice{
				Reference: fmt.Sprintf("%s-%d", series, number),
				IssuedAt:  issued,
				Gross:     money.Format(gross),
				KeptUntil: issued.Add(accountingRetention),
			})
		}
		invoices.Close()

		// Somebody can be an employee and a customer at once, and an export
		// that showed only one half would be incomplete in the way that
		// matters.
		var emp ExportedEmployment
		err = tx.QueryRow(ctx, `
			SELECT u.role, u.active, u.created_at, u.deactivated_at,
			       coalesce((SELECT sum(extract(epoch from (t.ended_at - t.started_at)) / 3600)
			                 FROM time_entries t WHERE t.user_id = u.id AND t.ended_at IS NOT NULL), 0)
			FROM users u WHERE u.person_id = $1`, personID).Scan(
			&emp.Role, &emp.Active, &emp.Since, &emp.DeactivatedAt, &emp.HoursClocked)
		if err == nil {
			e.Employment = &emp
		} else if err != pgx.ErrNoRows {
			return fmt.Errorf("read employment: %w", err)
		}
		return nil
	})
	if err != nil {
		return PersonExport{}, err
	}

	e.Notes = []string{
		"This is a copy of the personal data this workshop holds about you, under GDPR article 15.",
		"Invoices are kept for seven years after they were issued, because Swedish bookkeeping law requires it. Each one above says until when.",
		"A vehicle's technical history belongs to the vehicle and stays with it when it is sold. The next owner is shown what was done and is not shown your name, your details or what you paid.",
		"No national identity number and no driving licence details are held. There is no field for either.",
	}
	return e, nil
}

// MarshalExport renders an export as indented JSON.
//
// A format a person can open and a machine can read. A PDF would look more
// official and be harder to do anything with, and the right is to receive the
// data, not a picture of it.
func MarshalExport(e PersonExport) ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
}

// ErasePerson clears somebody's identifying details while keeping what the law
// requires.
//
// The conflict is real: the right to erasure against a seven-year obligation
// to keep accounting records. Refusing is wrong and deleting is illegal, so
// the person is anonymised and the records that must survive survive. What was
// cleared and what was kept is written down, because anonymising is not
// deleting and a shop asked "did you action my request" needs an answer better
// than a row that now says nothing.
func ErasePerson(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, personID, reason string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(personID) {
		return ErrNotFound
	}

	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var alreadyErased *time.Time
		if err := tx.QueryRow(ctx, `SELECT erased_at FROM people WHERE id = $1`, personID).
			Scan(&alreadyErased); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return fmt.Errorf("read person: %w", err)
		}
		if alreadyErased != nil {
			return fmt.Errorf("%w: this person has already been erased", ErrInvalid)
		}

		// An employee who can still sign in is not erased, they are renamed.
		var active bool
		err := tx.QueryRow(ctx, `SELECT active FROM users WHERE person_id = $1`, personID).Scan(&active)
		if err == nil && active {
			return fmt.Errorf("%w: this person still has an account that can sign in; deactivate it first", ErrInvalid)
		}
		if err != nil && err != pgx.ErrNoRows {
			return fmt.Errorf("check for an account: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE people SET display_name = $2, email = NULL, phone = NULL,
			                  erased_at = now(), updated_at = now()
			WHERE id = $1`, personID, ErasedName); err != nil {
			return fmt.Errorf("clear the person: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE customers SET address_line1 = NULL, address_line2 = NULL,
			                     postal_code = NULL, city = NULL, updated_at = now()
			WHERE person_id = $1`, personID); err != nil {
			return fmt.Errorf("clear addresses: %w", err)
		}

		const cleared = "Name, email address, telephone number and postal address."
		const kept = "Issued invoices, including the name and address they were addressed to, " +
			"because Swedish bookkeeping law requires them for seven years after issue. " +
			"The technical history of vehicles, which belongs to the vehicle rather than to a person. " +
			"Clocked hours, where the person was an employee, because they support payroll records."

		if _, err := tx.Exec(ctx, `
			INSERT INTO erasures (shop_id, person_id, erased_by, cleared, kept, reason)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''))`,
			scope.ShopID, personID, scope.UserID, cleared, kept, strings.TrimSpace(reason)); err != nil {
			return fmt.Errorf("record the erasure: %w", err)
		}
		return nil
	})
}

// Erasure is a record that somebody was erased.
type Erasure struct {
	PersonID string
	ErasedAt time.Time
	ErasedBy string
	Cleared  string
	Kept     string
	Reason   string
}

// Erasures lists what has been actioned, so the shop can answer for it.
func Erasures(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Erasure, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []Erasure
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.person_id, e.erased_at, coalesce(p.display_name, ''),
			       e.cleared, e.kept, coalesce(e.reason, '')
			FROM erasures e
			LEFT JOIN users u  ON u.id = e.erased_by
			LEFT JOIN people p ON p.id = u.person_id
			ORDER BY e.erased_at DESC`)
		if err != nil {
			return fmt.Errorf("list erasures: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e Erasure
			if err := rows.Scan(&e.PersonID, &e.ErasedAt, &e.ErasedBy, &e.Cleared, &e.Kept, &e.Reason); err != nil {
				return fmt.Errorf("scan erasure: %w", err)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// PhotoRemover is what Sweep and DeleteAttachment use to reach the files.
//
// An interface so that the domain does not import the storage package, and so
// that a test can watch what was deleted.
type PhotoRemover interface {
	Remove(key string) error
}

// DeleteAttachment removes a photograph, row and file together.
//
// Deleting the row alone is not deleting anything. A photograph taken in a
// workshop catches what it was not aimed at -- another customer's plate, a
// colleague, paperwork on a bench -- so the file is the personal data and it
// has to go with the record of it.
func DeleteAttachment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, photos PhotoRemover, attachmentID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(attachmentID) {
		return ErrNotFound
	}

	var key string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`DELETE FROM attachments WHERE id = $1 RETURNING storage_key`, attachmentID).Scan(&key)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return err
	}

	// After the commit, deliberately. A file removed inside a transaction that
	// then rolls back is gone with the row still pointing at it; this way the
	// worst case is a file nobody references, which a sweep can find.
	if err := photos.Remove(key); err != nil {
		return fmt.Errorf("remove the file: %w", err)
	}
	return nil
}

// Retention periods, per category. Each one is short enough to be defensible
// and long enough to be useful, and each is applied rather than written down
// and forgotten.
const (
	// Failed sign-in attempts exist to throttle guessing over minutes. Keeping
	// them for months would be keeping a record of who tried to sign in, which
	// is personal data with no remaining purpose.
	loginAttemptRetention = 90 * 24 * time.Hour

	// A link the customer was sent stops working after a fortnight; the row is
	// kept a while longer so "was this sent" can still be answered, and then
	// it goes.
	expiredShareRetention = 180 * 24 * time.Hour
)

// SweepResult says what a retention sweep removed.
type SweepResult struct {
	LoginAttempts int64
	Shares        int64
}

// Empty reports whether there was nothing to do.
func (s SweepResult) Empty() bool { return s.LoginAttempts == 0 && s.Shares == 0 }

// Sweep applies the retention policy.
//
// Run on a schedule rather than offered as a button, because a policy that
// depends on somebody remembering is not a policy. What it does not touch is
// as deliberate as what it does: invoices are kept for seven years by law, and
// a vehicle's technical history has no expiry because it belongs to the
// vehicle rather than to a person.
func Sweep(ctx context.Context, pool *pgxpool.Pool, shopID string) (SweepResult, error) {
	var r SweepResult
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM login_attempts WHERE attempted_at < now() - $1::interval`,
			fmt.Sprintf("%d seconds", int(loginAttemptRetention.Seconds())))
		if err != nil {
			return fmt.Errorf("sweep login attempts: %w", err)
		}
		r.LoginAttempts = tag.RowsAffected()

		tag, err = tx.Exec(ctx,
			`DELETE FROM inspection_shares WHERE expires_at < now() - $1::interval`,
			fmt.Sprintf("%d seconds", int(expiredShareRetention.Seconds())))
		if err != nil {
			return fmt.Errorf("sweep shares: %w", err)
		}
		r.Shares = tag.RowsAffected()
		return nil
	})
	return r, err
}
