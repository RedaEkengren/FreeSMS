package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAlreadySetUp is returned when a shop exists.
//
// It is what stops the setup page from being an open account-creation endpoint
// on a live installation. The check is inside the transaction that creates the
// shop, not in the handler, because two people opening the page at the same
// moment would otherwise both pass a check and both create one.
var ErrAlreadySetUp = errors.New("workshop: this installation already has a shop")

// MinPasswordLength is the floor for the first owner's password.
//
// Length only. Composition rules -- a capital, a digit, a symbol -- push
// people towards Workshop1! and towards writing it on the wall by the coffee
// machine, which is worse than a long ordinary phrase. Current guidance is to
// require length and check nothing else.
const MinPasswordLength = 10

// NeedsSetup reports whether this installation has no shop yet.
func NeedsSetup(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var exists bool
	// shops is the one table the schema owner reads unscoped; see
	// 0004_sessions.sql for why.
	if err := pool.QueryRow(ctx, `SELECT exists(SELECT 1 FROM shops)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check for a shop: %w", err)
	}
	return !exists, nil
}

// Setup creates the shop and its first owner, once.
//
// Both in one transaction. A half-finished setup -- a shop with nobody able to
// sign in to it -- would leave an installation that cannot be used and cannot
// be set up again, because the setup page would see a shop and refuse.
func Setup(ctx context.Context, pool *pgxpool.Pool, shopName, ownerName, email, password string) (shopID string, err error) {
	shopName = strings.TrimSpace(shopName)
	ownerName = strings.TrimSpace(ownerName)
	email = strings.TrimSpace(email)

	switch {
	case shopName == "":
		return "", fmt.Errorf("%w: the workshop needs a name", ErrInvalid)
	case ownerName == "":
		return "", fmt.Errorf("%w: your name is needed for the first account", ErrInvalid)
	case !strings.Contains(email, "@"):
		return "", fmt.Errorf("%w: that does not look like an email address", ErrInvalid)
	case utf8.RuneCountInString(password) < MinPasswordLength:
		return "", fmt.Errorf("%w: the password needs at least %d characters",
			ErrInvalid, MinPasswordLength)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Two people opening the setup page at once must not produce two shops.
	// The lock makes the check and the insert one step; the loser gets
	// ErrAlreadySetUp and is told it is done, which is true.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(7_713_552_001)); err != nil {
		return "", fmt.Errorf("take setup lock: %w", err)
	}

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM shops)`).Scan(&exists); err != nil {
		return "", fmt.Errorf("check for a shop: %w", err)
	}
	if exists {
		return "", ErrAlreadySetUp
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO shops (name) VALUES ($1) RETURNING id`, shopName).Scan(&shopID); err != nil {
		return "", fmt.Errorf("create shop: %w", err)
	}

	// Row level security applies from here on, including to the owner, so the
	// rest of this transaction declares its scope like any other write.
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.current_shop', $1, true)`, shopID); err != nil {
		return "", fmt.Errorf("set scope: %w", err)
	}

	var personID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO people (shop_id, display_name, email) VALUES ($1, $2, $3) RETURNING id`,
		shopID, ownerName, email).Scan(&personID); err != nil {
		return "", fmt.Errorf("create person: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO users (shop_id, person_id, role, password_hash) VALUES ($1, $2, 'owner', $3)`,
		shopID, personID, hash); err != nil {
		return "", fmt.Errorf("create owner: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return shopID, nil
}

// ShopDetails is what a shop puts on its own invoices.
//
// Read and written as a whole rather than field by field: they are printed
// together, they are checked together by whoever is registering the business,
// and a partial save is how a shop ends up invoicing with half an address.
type ShopDetails struct {
	Name             string
	AddressLine1     string
	AddressLine2     string
	PostalCode       string
	City             string
	OrgNumber        string
	VATNumber        string
	Phone            string
	Email            string
	PaymentReference string
	PaymentTermsDays int
	FTax             bool
	Locale           string
	Currency         string
}

// ShopDetailsFor returns the shop's own particulars.
//
// Owner only, and checked here rather than in a handler: this is the
// organisation number and the bank details, which is not a technician's
// business and not a second caller's to decide.
func ShopDetailsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (ShopDetails, error) {
	if !scope.Role.RunsTheShop() {
		return ShopDetails{}, access.ErrForbidden
	}
	var d ShopDetails
	var line1, line2, postal, city, org, vat, phone, email, ref *string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT name, address_line1, address_line2, postal_code, city,
			       org_number, vat_number, phone, email,
			       payment_reference, payment_terms_days, f_tax, locale, currency
			FROM shops WHERE id = $1`, scope.ShopID).
			Scan(&d.Name, &line1, &line2, &postal, &city, &org, &vat, &phone, &email,
				&ref, &d.PaymentTermsDays, &d.FTax, &d.Locale, &d.Currency)
	})
	if err != nil {
		return ShopDetails{}, fmt.Errorf("read shop details: %w", err)
	}
	d.AddressLine1, d.AddressLine2 = deref(line1), deref(line2)
	d.PostalCode, d.City = deref(postal), deref(city)
	d.OrgNumber, d.VATNumber = deref(org), deref(vat)
	d.Phone, d.Email, d.PaymentReference = deref(phone), deref(email), deref(ref)
	d.Currency = strings.TrimSpace(d.Currency)
	return d, nil
}

// SaveDetails writes them back.
//
// Nothing here rewrites an invoice that has already been issued: the document
// carries its own copy, taken when it was issued. Changing these changes what
// the next one says and nothing that has already been sent.
func SaveDetails(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, d ShopDetails) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("%w: a workshop needs a name to put on an invoice", ErrInvalid)
	}
	if d.PaymentTermsDays < 0 {
		return fmt.Errorf("%w: payment terms cannot be negative", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE shops SET
			  name = $2,
			  address_line1 = nullif($3,''), address_line2 = nullif($4,''),
			  postal_code = nullif($5,''), city = nullif($6,''),
			  org_number = nullif($7,''), vat_number = nullif($8,''),
			  phone = nullif($9,''), email = nullif($10,''),
			  payment_reference = nullif($11,''),
			  payment_terms_days = $12, f_tax = $13
			WHERE id = $1`,
			scope.ShopID, strings.TrimSpace(d.Name),
			strings.TrimSpace(d.AddressLine1), strings.TrimSpace(d.AddressLine2),
			strings.TrimSpace(d.PostalCode), strings.TrimSpace(d.City),
			strings.TrimSpace(d.OrgNumber), strings.TrimSpace(d.VATNumber),
			strings.TrimSpace(d.Phone), strings.TrimSpace(d.Email),
			strings.TrimSpace(d.PaymentReference), d.PaymentTermsDays, d.FTax)
		if err != nil {
			return fmt.Errorf("save shop details: %w", err)
		}
		return nil
	})
}
