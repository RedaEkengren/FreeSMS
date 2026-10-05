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

	// Somebody else, doing the managing: switching yourself off is refused,
	// so the one switching off is never the one switched off.
	anOwner = "77777777-7777-7777-7777-777777777777"
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

	owner := access.Scope{ShopID: shopID, UserID: anOwner, Role: access.RoleOwner}
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

// Signing everybody out ends every session, in every shop, so a cookie that
// worked a moment ago no longer does -- for a process started afterwards as
// much as for this one -- and signing in again works.
//
// The documented way used to be rotating SESSION_SECRET, which nothing read:
// every session survived it.
func TestRevokeAllSignsEverybodyOut(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const otherShop, otherUser = "44444444-4444-4444-4444-444444444444", "55555555-5555-5555-5555-555555555555"
	hash, _ := HashPassword(pass)
	if err := database.InShop(ctx, pool, otherShop, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`INSERT INTO shops (id, name) VALUES ('` + otherShop + `', 'Another Verkstad')`,
			`INSERT INTO people (id, shop_id, display_name, email) VALUES
			 ('66666666-6666-6666-6666-666666666666', '` + otherShop + `', 'Someone Else', 'other@example.test')`,
			`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
			 ('` + otherUser + `', '` + otherShop + `', '66666666-6666-6666-6666-666666666666', 'owner', '` + hash + `')`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed another shop: %v", err)
	}

	here, _, err := Login(ctx, pool, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	there, _, err := Login(ctx, pool, otherShop, "other@example.test", pass, "test")
	if err != nil {
		t.Fatalf("Login in the other shop: %v", err)
	}

	ended, err := RevokeAll(ctx, pool)
	if err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if ended != 2 {
		t.Errorf("ended %d sessions, want both", ended)
	}

	// A process started afterwards: a new pool, nothing carried over in memory.
	restarted, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer restarted.Close()
	for shop, token := range map[string]string{shopID: here, otherShop: there} {
		if _, err := Authenticate(ctx, restarted, shop, token); !errors.Is(err, ErrNoSession) {
			t.Errorf("a revoked session in %s still authenticates: %v", shop, err)
		}
	}

	again, _, err := Login(ctx, restarted, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("signing in after revocation: %v", err)
	}
	if _, err := Authenticate(ctx, restarted, shopID, again); err != nil {
		t.Errorf("a new session after revocation does not work: %v", err)
	}
}

// Only whoever runs the shop switches a colleague off. Deactivate checked the
// scope's shop and not its role, so any signed-in user handed to it could
// switch off anybody in the same shop. A refusal changes nothing: the user
// stays active and signed in.
func TestOnlyThoseWhoRunTheShopDeactivateAUser(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	token, _, err := Login(ctx, pool, shopID, email, pass, "test")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	for _, role := range []access.Role{access.RoleTechnician, access.RoleParts, access.RoleServiceAdvisor} {
		err := Deactivate(ctx, pool, access.Scope{ShopID: shopID, UserID: anOwner, Role: role}, userID)
		if !errors.Is(err, access.ErrForbidden) {
			t.Errorf("Deactivate as %s: %v, want forbidden", role, err)
		}
	}
	if _, err := Authenticate(ctx, pool, shopID, token); err != nil {
		t.Errorf("a refused deactivation ended the session anyway: %v", err)
	}
	if err := Deactivate(ctx, pool, access.Scope{ShopID: shopID, UserID: anOwner, Role: access.RoleOwner}, userID); err != nil {
		t.Fatalf("Deactivate as owner: %v", err)
	}
	if _, err := Authenticate(ctx, pool, shopID, token); !errors.Is(err, ErrNoSession) {
		t.Errorf("deactivated by the owner, still signed in: %v", err)
	}
}

// Changing your own password needs the current one, keeps the session it is
// done from and ends every other -- a password changed because a phone went
// missing signs the phone out.
func TestChangingYourPasswordKeepsThisSessionAndEndsTheRest(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	here, s, err := Login(ctx, pool, shopID, email, pass, "this browser")
	if err != nil {
		t.Fatal(err)
	}
	phone, _, _ := Login(ctx, pool, shopID, email, pass, "the lost phone")

	if err := ChangePassword(ctx, pool, s.Scope, here, "not the password", "a new one, long enough"); !errors.Is(err, ErrRefused) {
		t.Errorf("a wrong current password: %v, want refused", err)
	}
	if err := ChangePassword(ctx, pool, s.Scope, here, pass, "short"); !errors.Is(err, ErrRefused) {
		t.Errorf("a short new password: %v, want refused", err)
	}
	if err := ChangePassword(ctx, pool, s.Scope, here, pass, "a new one, long enough"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, err := Authenticate(ctx, pool, shopID, here); err != nil {
		t.Errorf("the session it was changed from ended: %v", err)
	}
	if _, err := Authenticate(ctx, pool, shopID, phone); !errors.Is(err, ErrNoSession) {
		t.Errorf("the other session survived: %v", err)
	}
	if _, _, err := Login(ctx, pool, shopID, email, pass, "x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the old password still signs in: %v", err)
	}
	if _, _, err := Login(ctx, pool, shopID, email, "a new one, long enough", "x"); err != nil {
		t.Errorf("the new password does not sign in: %v", err)
	}
}

// Nobody switches themselves off.
func TestYouCannotSwitchYourselfOff(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	err := Deactivate(ctx, pool, access.Scope{ShopID: shopID, UserID: userID, Role: access.RoleOwner}, userID)
	if !errors.Is(err, ErrRefused) {
		t.Errorf("switching yourself off: %v, want refused", err)
	}
	if _, _, err := Login(ctx, pool, shopID, email, pass, "x"); err != nil {
		t.Errorf("a refused switch-off locked the user out: %v", err)
	}
}
