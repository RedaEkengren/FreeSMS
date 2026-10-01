package workshop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention for the two tables that exist to survive a bad minute rather than
// to be a record.
const (
	idempotencyRetention = 48 * time.Hour
	draftRetention       = 30 * 24 * time.Hour
)

// ErrKeyReused is returned when an idempotency key comes back with a different
// request behind it.
//
// That is a client bug, and answering the second question with the first
// answer would be worse than refusing.
var ErrKeyReused = errors.New("workshop: that idempotency key was used for a different request")

// Replay is a request that has already been done.
type Replay struct {
	Status   int
	Location string
}

// ErrInFlight is returned when another request with the same key is being
// carried out right now. The answer is "ask again shortly", not "do it too".
var ErrInFlight = errors.New("workshop: a request with that key is still being carried out")

// ErrOutcomeUnknown is returned when a request with this key was started long
// ago and never recorded an outcome.
//
// That happens when the process stops between doing the work and writing down
// that it did. Nobody can tell from here whether the work committed, so it is
// neither replayed nor repeated: a person has to look. Doing a money operation
// twice is worse than asking somebody to check a job.
var ErrOutcomeUnknown = errors.New("workshop: a request with that key was started and its outcome is unknown")

// pending marks a claimed key whose request has not finished. A real HTTP
// status is never zero, so the column needs no second meaning.
const pending = 0

// stale is how long a claim may stay pending before it is treated as
// abandoned. Far longer than any request takes; short enough that a queue
// stuck behind a crash is noticed the same morning.
const stale = 10 * time.Minute

// Fingerprint is what makes two requests the same request.
//
// The body alone was not enough: the same form posted to a different job, or
// by a different person, has the same body. The method, the path and the
// person asking are part of what the request was.
func Fingerprint(method, path, userID string, body []byte) []byte {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(method), []byte(path), []byte(userID)} {
		h.Write(part)
		h.Write([]byte{0})
	}
	h.Write(body)
	return h.Sum(nil)
}

// ClaimIdempotency takes a key before the work is done, rather than recording
// it afterwards.
//
// It used to be the other way round: check in one transaction, let the handler
// commit its work in another, record the key in a third. Two copies arriving
// together both passed the check and both did the work, and a client that hung
// up after the work committed left no record, so its retry did the work again.
//
// Now the key is claimed first, in a statement the unique index serialises:
// exactly one caller inserts the row and owns the request. Everybody else
// reads what that one left -- a finished answer to replay, a claim still in
// progress, or one abandoned long enough ago that its outcome is unknown.
//
// A nil Replay and a nil error means the caller owns the key and must do the
// work, then Complete or Release it.
func ClaimIdempotency(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, key string, fingerprint []byte) (*Replay, error) {
	if len(key) < 16 || len(key) > 128 {
		return nil, fmt.Errorf("%w: an idempotency key is 16 to 128 characters", ErrInvalid)
	}

	var replay *Replay
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var mine bool
		err := tx.QueryRow(ctx, `
			INSERT INTO idempotency_keys (shop_id, key, request_hash, status)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (shop_id, key) DO NOTHING
			RETURNING true`, scope.ShopID, key, fingerprint, pending).Scan(&mine)
		if err == nil {
			return nil // ours
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("claim idempotency key: %w", err)
		}

		var stored []byte
		var status int
		var location *string
		var claimed time.Time
		if err := tx.QueryRow(ctx, `
			SELECT request_hash, status, location, created_at
			FROM idempotency_keys WHERE key = $1`, key).
			Scan(&stored, &status, &location, &claimed); err != nil {
			return fmt.Errorf("read idempotency key: %w", err)
		}
		if string(stored) != string(fingerprint) {
			return ErrKeyReused
		}
		if status == pending {
			if time.Since(claimed) > stale {
				return ErrOutcomeUnknown
			}
			return ErrInFlight
		}
		replay = &Replay{Status: status}
		if location != nil {
			replay.Location = *location
		}
		return nil
	})
	return replay, err
}

// CompleteIdempotency records what a claimed request did, so a repeat is
// answered with it.
func CompleteIdempotency(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, key string, status int, location string) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET status = $2, location = nullif($3, '')
			WHERE key = $1 AND status = $4`, key, status, location, pending)
		return err
	})
}

// ReleaseIdempotency gives a key back after a request that failed, so that
// trying again is allowed to succeed.
func ReleaseIdempotency(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, key string) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM idempotency_keys WHERE key = $1 AND status = $2`, key, pending)
		return err
	})
}

// SaveDraft stores what somebody has typed and not submitted.
//
// On the server as well as in the browser, because a phone that is lost, wiped
// or swapped takes its local storage with it.
func SaveDraft(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, form string, fields map[string]string) error {
	form = strings.TrimSpace(form)
	if form == "" {
		return fmt.Errorf("%w: a draft has to say which form it is", ErrInvalid)
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode draft: %w", err)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO drafts (shop_id, user_id, form, fields)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, form)
			DO UPDATE SET fields = excluded.fields, updated_at = now()`,
			scope.ShopID, scope.UserID, form, body)
		return err
	})
}

// LoadDraft returns what was typed, or nothing.
func LoadDraft(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, form string) (map[string]string, error) {
	fields := map[string]string{}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var body []byte
		err := tx.QueryRow(ctx,
			`SELECT fields FROM drafts WHERE user_id = $1 AND form = $2`,
			scope.UserID, form).Scan(&body)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read draft: %w", err)
		}
		return json.Unmarshal(body, &fields)
	})
	return fields, err
}

// DiscardDraft removes one, which is what submitting it means.
func DiscardDraft(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, form string) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM drafts WHERE user_id = $1 AND form = $2`, scope.UserID, form)
		return err
	})
}
