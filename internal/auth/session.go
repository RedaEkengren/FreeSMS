package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// A session is good for a fortnight, and dies after a week untouched.
	// A workshop machine at a counter is signed in all day; a phone left in a
	// drawer should not be.
	sessionLifetime = 14 * 24 * time.Hour
	sessionIdle     = 7 * 24 * time.Hour

	// Failed attempts tolerated per shop and email before sign-in is refused.
	maxAttempts    = 10
	attemptsWindow = 15 * time.Minute
)

var (
	// ErrInvalidCredentials covers a wrong password, an unknown email and a
	// deactivated account alike. Telling them apart tells an attacker which
	// addresses are worth guessing at.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrThrottled          = errors.New("auth: too many attempts, try again later")
	ErrNoSession          = errors.New("auth: no valid session")
)

// Session is an authenticated request's identity.
type Session struct {
	Scope     access.Scope
	ExpiresAt time.Time
}

// newToken returns a session token and the hash stored against it.
//
// 256 bits from crypto/rand. The token goes to the browser and is never
// written down here; only its SHA-256 is stored, so a leaked backup does not
// hand over live sessions.
func newToken() (token string, sum []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: read token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256([]byte(token))
	return token, h[:], nil
}

// Login verifies credentials and opens a session.
//
// Every failure path returns ErrInvalidCredentials and takes roughly the same
// time: when no user matches, a dummy verification still runs, so the absence
// of an account cannot be detected by how quickly the answer comes back.
func Login(ctx context.Context, pool *pgxpool.Pool, shopID, email, password, userAgent string) (token string, s Session, err error) {
	var ok bool

	// The verification runs in its own transaction; the record of the attempt
	// is written in another, afterwards, and that separation is load-bearing.
	//
	// Writing the failed attempt inside this transaction looked obviously
	// right and was wrong: returning the error rolls the transaction back, and
	// takes the audit row with it. Throttling counted failures that had erased
	// themselves, so it never triggered -- and a test that tried ten wrong
	// passwords sailed through on the eleventh.
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var recent int
		const countAttempts = `
			SELECT count(*) FROM login_attempts
			WHERE email = $1 AND NOT succeeded AND attempted_at > now() - $2::interval`
		if err := tx.QueryRow(ctx, countAttempts, email, attemptsWindow.String()).Scan(&recent); err != nil {
			return fmt.Errorf("count attempts: %w", err)
		}
		if recent >= maxAttempts {
			return ErrThrottled
		}

		const lookup = `
			SELECT u.id, u.role, u.password_hash, u.active
			FROM users u
			JOIN people p ON p.id = u.person_id
			WHERE p.email = $1`
		var userID, role, hash string
		var active bool
		scanErr := tx.QueryRow(ctx, lookup, email).Scan(&userID, &role, &hash, &active)

		if errors.Is(scanErr, pgx.ErrNoRows) {
			// Spend the same effort as a real verification would, so that a
			// missing account and a wrong password cannot be told apart by
			// timing.
			_ = VerifyPassword(dummyHash, password)
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("look up user: %w", scanErr)
		}

		if verifyErr := VerifyPassword(hash, password); verifyErr != nil || !active {
			return nil
		}

		tok, sum, err := newToken()
		if err != nil {
			return err
		}
		expires := time.Now().Add(sessionLifetime)

		const insert = `
			INSERT INTO sessions (shop_id, user_id, token_sha256, expires_at, user_agent)
			VALUES ($1, $2, $3, $4, $5)`
		if _, err := tx.Exec(ctx, insert, shopID, userID, sum, expires, userAgent); err != nil {
			return fmt.Errorf("create session: %w", err)
		}

		ok = true
		token = tok
		s = Session{
			Scope:     access.Scope{ShopID: shopID, UserID: userID, Role: access.Role(role)},
			ExpiresAt: expires,
		}
		return nil
	})
	if err != nil {
		return "", Session{}, err
	}

	if err := recordAttempt(ctx, pool, shopID, email, ok); err != nil {
		// Deliberately not best effort.
		//
		// Throttling counts rows in this table. A system that cannot write
		// them has no brute-force protection at all, and swallowing the error
		// means nobody finds out until the guessing has already happened. It
		// was swallowed once, and the throttle was silently off for as long as
		// the writes were failing.
		return "", Session{}, fmt.Errorf("record login attempt: %w", err)
	}
	if !ok {
		return "", Session{}, ErrInvalidCredentials
	}
	return token, s, nil
}

// dummyHash is a real argon2id hash of a value nobody knows, used to spend
// verification time on accounts that do not exist.
var dummyHash = func() string {
	h, err := HashPassword("this is not anybody's password")
	if err != nil {
		panic(err)
	}
	return h
}()

// recordAttempt writes the attempt in a transaction of its own, so that it
// survives the failure it is recording.
func recordAttempt(ctx context.Context, pool *pgxpool.Pool, shopID, email string, ok bool) error {
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO login_attempts (shop_id, email, succeeded) VALUES ($1, $2, $3)`,
			shopID, email, ok)
		return err
	})
}

// Authenticate turns a token from a cookie into a scope.
//
// The user's active flag is checked here, on every request, which is what
// makes deactivation immediate. A self-contained signed token would stay valid
// until it expired, because nothing would be consulted when it was presented.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	sum := sha256.Sum256([]byte(token))

	var s Session
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		const lookup = `
			SELECT s.id, s.user_id, u.role, s.expires_at, s.last_seen_at, u.active
			FROM sessions s
			JOIN users u ON u.id = s.user_id
			WHERE s.token_sha256 = $1`
		var sessionID, userID, role string
		var expires, lastSeen time.Time
		var active bool

		err := tx.QueryRow(ctx, lookup, sum[:]).Scan(&sessionID, &userID, &role, &expires, &lastSeen, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return fmt.Errorf("look up session: %w", err)
		}

		now := time.Now()
		switch {
		case !active, now.After(expires), now.Sub(lastSeen) > sessionIdle:
			// Delete rather than leave it: a session that can never be used
			// again is only a row waiting to be misread.
			_, _ = tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
			return ErrNoSession
		}

		if _, err := tx.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1`, sessionID); err != nil {
			return fmt.Errorf("touch session: %w", err)
		}

		s = Session{
			Scope:     access.Scope{ShopID: shopID, UserID: userID, Role: access.Role(role)},
			ExpiresAt: expires,
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return s, nil
}

// Logout ends one session.
func Logout(ctx context.Context, pool *pgxpool.Pool, shopID, token string) error {
	sum := sha256.Sum256([]byte(token))
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE token_sha256 = $1`, sum[:])
		return err
	})
}

// Deactivate turns a user off and ends their sessions in the same
// transaction.
//
// Both, together. Clearing the flag alone leaves whoever is signed in working
// until their session expires, which is the gap this is meant to close.
func Deactivate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		const off = `UPDATE users SET active = false, deactivated_at = now() WHERE id = $1`
		if _, err := tx.Exec(ctx, off, userID); err != nil {
			return fmt.Errorf("deactivate user: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("end sessions: %w", err)
		}
		return nil
	})
}
