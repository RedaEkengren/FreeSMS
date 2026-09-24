package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	shopID = "11111111-1111-1111-1111-111111111111"
	userID = "33333333-3333-3333-3333-333333333333"
	email  = "tech@example.test"
	pass   = "a reasonable workshop password"
)

// Skipped unless REDASMS_TEST_DATABASE_URL points at a database that may be
// wiped. See internal/database for the local invocation.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool := testsupport.FreshPool(t)
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	hash, err := HashPassword(pass)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO shops (id, name) VALUES ($1, 'Test Verkstad')`, []any{shopID}},
			{`INSERT INTO people (id, shop_id, display_name, email)
			  VALUES ('22222222-2222-2222-2222-222222222222', $1, 'A Technician', $2)`, []any{shopID, email}},
			{`INSERT INTO users (id, shop_id, person_id, role, password_hash)
			  VALUES ($1, $2, '22222222-2222-2222-2222-222222222222', 'technician', $3)`, []any{userID, shopID, hash}},
		} {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool
}

func TestLoginReturnsTheUsersScope(t *testing.T) {
	pool := testPool(t)
	token, s, err := Login(context.Background(), pool, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if token == "" {
		t.Error("Login returned an empty token")
	}
	if s.Scope.UserID != userID || s.Scope.Role != access.RoleTechnician || s.Scope.ShopID != shopID {
		t.Errorf("scope = %+v, want the seeded technician", s.Scope)
	}
}

// A wrong password and an address nobody has must be indistinguishable.
func TestWrongPasswordAndUnknownEmailGiveTheSameAnswer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, _, wrong := Login(ctx, pool, shopID, email, "not the password", "test")
	_, _, unknown := Login(ctx, pool, shopID, "nobody@example.test", pass, "test")

	if !errors.Is(wrong, ErrInvalidCredentials) {
		t.Errorf("wrong password gave %v, want ErrInvalidCredentials", wrong)
	}
	if !errors.Is(unknown, ErrInvalidCredentials) {
		t.Errorf("unknown email gave %v, want ErrInvalidCredentials", unknown)
	}
	if wrong.Error() != unknown.Error() {
		t.Errorf("the two failures are distinguishable: %q vs %q", wrong, unknown)
	}
}

func TestRepeatedFailuresAreThrottled(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for i := 0; i < maxAttempts; i++ {
		if _, _, err := Login(ctx, pool, shopID, email, "wrong", "test"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d gave %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	// Even the correct password is refused once the window is full.
	if _, _, err := Login(ctx, pool, shopID, email, pass, "test"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("after %d failures Login gave %v, want ErrThrottled", maxAttempts, err)
	}
}

func TestAuthenticateAcceptsAValidToken(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	token, _, err := Login(ctx, pool, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}
	s, err := Authenticate(ctx, pool, shopID, token)
	if err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
	if s.Scope.UserID != userID {
		t.Errorf("scope user = %s, want %s", s.Scope.UserID, userID)
	}
	if _, err := Authenticate(ctx, pool, shopID, "not a real token"); !errors.Is(err, ErrNoSession) {
		t.Errorf("a made-up token gave %v, want ErrNoSession", err)
	}
}

// The requirement in the issue, stated as a test: a deactivated user's session
// dies at once, not at expiry.
func TestDeactivationEndsSessionsImmediately(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	token, _, err := Login(ctx, pool, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if _, err := Authenticate(ctx, pool, shopID, token); err != nil {
		t.Fatalf("the session was not usable before deactivation: %v", err)
	}

	owner := access.Scope{ShopID: shopID, UserID: userID, Role: access.RoleOwner}
	if err := Deactivate(ctx, pool, owner, userID); err != nil {
		t.Fatalf("Deactivate() = %v", err)
	}

	if _, err := Authenticate(ctx, pool, shopID, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("the session still worked after deactivation: %v", err)
	}
	if _, _, err := Login(ctx, pool, shopID, email, pass, "test"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("a deactivated user could sign in again: %v", err)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	token, _, err := Login(ctx, pool, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if err := Logout(ctx, pool, shopID, token); err != nil {
		t.Fatalf("Logout() = %v", err)
	}
	if _, err := Authenticate(ctx, pool, shopID, token); !errors.Is(err, ErrNoSession) {
		t.Errorf("the token still worked after logout: %v", err)
	}
}

// A leaked backup must not hand over live sessions.
func TestTheTokenIsNotStored(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	token, _, err := Login(ctx, pool, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}

	var found bool
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM sessions WHERE encode(token_sha256, 'escape') = $1)`,
			token).Scan(&found)
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if found {
		t.Error("the session token is stored in the database in a recoverable form")
	}
}
