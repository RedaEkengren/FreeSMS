// Customers: who is paying, which is not the same question as who owns the car.
//
// Until this existed there was no way to create one at all. Setup made a shop
// and an owner, intake found a customer only if the vehicle already had an
// ownership period, and Issue refuses an order with nobody to bill -- so a
// fresh installation could take a car in and then never invoice it. Every
// end-to-end walk through this system worked only because the development seed
// planted a customer by hand.
package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Customer is somebody an invoice can be addressed to.
//
// Private or company, and the two are genuinely different: a private customer
// is a person, a company is a name with an organisation number and perhaps a
// contact person. The schema already refuses the combinations that make no
// sense, so this type carries both shapes and lets the constraint decide.
type Customer struct {
	ID        string
	Kind      string
	Name      string
	OrgNumber string
	VATNumber string
	Phone     string
	Email     string
	Address   string

	// How many orders this customer has been billed for. Shown beside a search
	// result so the front desk picks the Anna Andersson they have seen before
	// rather than making a second one.
	Orders int
}

// Private reports whether this is a person rather than a company.
func (c Customer) Private() bool { return c.Kind == "private" }

// NewCustomer is what the front desk typed.
type NewCustomer struct {
	Kind         string // private or company
	Name         string // the person's name, or the company's
	ContactName  string // a company's contact person, optional
	OrgNumber    string
	VATNumber    string
	Phone        string
	Email        string
	AddressLine1 string
	AddressLine2 string
	PostalCode   string
	City         string
}

const customerColumns = `
	c.id, c.kind,
	coalesce(p.display_name, c.company_name, ''),
	coalesce(c.org_number, ''), coalesce(c.vat_number, ''),
	coalesce(p.phone, ''), coalesce(p.email, ''),
	coalesce(nullif(concat_ws(', ', c.address_line1, c.address_line2,
	                          concat_ws(' ', c.postal_code, c.city)), ''), ''),
	(SELECT count(*) FROM work_orders w WHERE w.customer_id = c.id)`

func scanCustomer(row pgx.Row) (Customer, error) {
	var c Customer
	err := row.Scan(&c.ID, &c.Kind, &c.Name, &c.OrgNumber, &c.VATNumber,
		&c.Phone, &c.Email, &c.Address, &c.Orders)
	return c, err
}

// FindCustomers searches by name, telephone or organisation number.
//
// The telephone is in there because it is what somebody says on the phone
// before they say their surname, and the organisation number because a company
// with two spellings of its name is still one company.
func FindCustomers(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Customer, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// Digits only, so "070-123 45 67" finds a number stored as "0701234567"
	// and the other way round. Somebody reading a number aloud does not
	// reproduce the punctuation.
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, query)

	var out []Customer
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT`+customerColumns+`
			FROM customers c
			LEFT JOIN people p ON p.id = c.person_id
			WHERE p.erased_at IS NULL
			  AND (coalesce(p.display_name, c.company_name) ILIKE '%' || $1 || '%'
			       OR c.org_number ILIKE $1 || '%'
			       OR ($2 <> '' AND regexp_replace(coalesce(p.phone, ''), '\D', '', 'g') LIKE '%' || $2 || '%'))
			ORDER BY 9 DESC, 3
			LIMIT 20`, query, digits)
		if err != nil {
			return fmt.Errorf("find customers: %w", err)
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

// CreateCustomer makes one and returns its identifier.
func CreateCustomer(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewCustomer) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		return "", fmt.Errorf("%w: a customer needs a name to address an invoice to", ErrInvalid)
	}
	if n.Kind != "private" && n.Kind != "company" {
		return "", fmt.Errorf("%w: a customer is either a person or a company", ErrInvalid)
	}

	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// A private customer is a person. A company may have one as a contact,
		// and may not: a haulier's invoices go to the company whether or not
		// anybody there has introduced themselves.
		var personID *string
		contact := strings.TrimSpace(n.ContactName)
		if n.Kind == "private" {
			contact = n.Name
		}
		if contact != "" {
			var pid string
			if err := tx.QueryRow(ctx, `
				INSERT INTO people (shop_id, display_name, phone, email)
				VALUES ($1, $2, nullif($3,''), nullif($4,''))
				RETURNING id`,
				scope.ShopID, contact,
				strings.TrimSpace(n.Phone), strings.TrimSpace(n.Email)).Scan(&pid); err != nil {
				return fmt.Errorf("create person: %w", err)
			}
			personID = &pid
		}

		var company *string
		if n.Kind == "company" {
			company = &n.Name
		}

		err := tx.QueryRow(ctx, `
			INSERT INTO customers
			  (shop_id, kind, person_id, company_name, org_number, vat_number,
			   address_line1, address_line2, postal_code, city)
			VALUES ($1,$2,$3,$4,nullif($5,''),nullif($6,''),
			        nullif($7,''),nullif($8,''),nullif($9,''),nullif($10,''))
			RETURNING id`,
			scope.ShopID, n.Kind, personID, company,
			strings.TrimSpace(n.OrgNumber), strings.TrimSpace(n.VATNumber),
			strings.TrimSpace(n.AddressLine1), strings.TrimSpace(n.AddressLine2),
			strings.TrimSpace(n.PostalCode), strings.TrimSpace(n.City)).Scan(&id)
		if err != nil {
			return fmt.Errorf("create customer: %w", err)
		}
		return nil
	})
	return id, err
}

// AssignCustomer says who is paying for an order, and optionally records them
// as the vehicle's owner so the next visit fills itself in.
//
// Those are two facts and this writes them separately. A company car is
// invoiced to the employer and owned by a leasing firm; a car sold last week
// has a new owner and a bill that belongs to the old one.
func AssignCustomer(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, customerID string, alsoOwner bool) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var state State
		var vehicleID string
		switch err := tx.QueryRow(ctx,
			`SELECT state, vehicle_id FROM work_orders WHERE id = $1 FOR UPDATE`,
			jobID).Scan(&state, &vehicleID); {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("read order: %w", err)
		}

		// An issued document carries the customer it was addressed to. Moving
		// the order's customer afterwards would make the two disagree, and the
		// document is the one that was sent.
		if !state.AcceptsWork() {
			return fmt.Errorf("%w: this order is finished; its invoice already says who it was addressed to", ErrInvalid)
		}

		// The customer has to be one of ours. Row level security means a
		// foreign identifier simply matches nothing, and that is a refusal
		// rather than a silent write of null.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM customers WHERE id = $1)`, customerID).Scan(&exists); err != nil {
			return fmt.Errorf("check customer: %w", err)
		}
		if !exists {
			return ErrNotFound
		}

		if _, err := tx.Exec(ctx,
			`UPDATE work_orders SET customer_id = $2 WHERE id = $1`, jobID, customerID); err != nil {
			return fmt.Errorf("assign customer: %w", err)
		}

		if !alsoOwner {
			return nil
		}
		// Ownership is a ledger: the old period is closed rather than
		// overwritten, because "who owned it in March" is a question the
		// vehicle's history has to be able to answer.
		if _, err := tx.Exec(ctx, `
			UPDATE vehicle_ownership SET owned_to = now()
			 WHERE vehicle_id = $1 AND owned_to IS NULL AND customer_id <> $2`,
			vehicleID, customerID); err != nil {
			return fmt.Errorf("close the previous ownership: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id)
			SELECT $1, $2, $3
			 WHERE NOT EXISTS (SELECT 1 FROM vehicle_ownership
			                    WHERE vehicle_id = $2 AND customer_id = $3 AND owned_to IS NULL)`,
			scope.ShopID, vehicleID, customerID); err != nil {
			return fmt.Errorf("record ownership: %w", err)
		}
		return nil
	})
}
