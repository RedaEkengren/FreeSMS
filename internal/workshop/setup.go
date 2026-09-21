package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/RedaEkengren/RedaSMS/internal/auth"
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
