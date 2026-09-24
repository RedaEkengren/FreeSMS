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

// CheckIdempotency reports whether this request has already been carried out.
//
// A flaky connection means the same request arrives twice: the phone gave up
// waiting, the technician pressed again, the offline queue replayed. Without
// this, each one opens a second job.
func CheckIdempotency(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, key string, body []byte) (*Replay, error) {
	if key == "" {
		return nil, nil
	}
	if len(key) < 16 || len(key) > 128 {
		return nil, fmt.Errorf("%w: an idempotency key is 16 to 128 characters", ErrInvalid)
	}
	hash := sha256.Sum256(body)

	var replay *Replay
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var stored []byte
		var status int
		var location *string
		err := tx.QueryRow(ctx,
			`SELECT request_hash, status, location FROM idempotency_keys WHERE key = $1`,
			key).Scan(&stored, &status, &location)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read idempotency key: %w", err)
		}
		if string(stored) != string(hash[:]) {
			return ErrKeyReused
		}
		replay = &Replay{Status: status}
		if location != nil {
			replay.Location = *location
		}
		return nil
	})
	return replay, err
}

// RecordIdempotency stores what a request did, so a repeat can be answered
// without doing it again.
func RecordIdempotency(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, key string, body []byte, status int, location string) error {
	if key == "" {
		return nil
	}
	hash := sha256.Sum256(body)
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (shop_id, key, request_hash, status, location)
			VALUES ($1, $2, $3, $4, nullif($5, ''))
			ON CONFLICT (shop_id, key) DO NOTHING`,
			scope.ShopID, key, hash[:], status, location)
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
