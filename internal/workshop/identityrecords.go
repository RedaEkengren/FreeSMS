package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/identity"
)

// Identity records (#153): a personal identity number, a licence check or an
// ID document check, held apart from the party, for one purpose with its
// legal basis, sealed with a key the database does not hold, read only by
// roles that need it -- every read an audit event -- and removed when the
// purpose ends. A value never goes into a log, a search, an export in the
// clear, the action queue or an error: it is returned only by
// ReadIdentityValue, to the caller who asked.

// The kinds of identity record.
const (
	IdentityNumber   = "personal_identity_number"
	IdentityLicence  = "licence_check"
	IdentityDocument = "id_document_check"
)

// IdentityPurpose is a reason the business may hold an identity record: the
// law or contract it rests on, the kinds it needs, for how long by default,
// whether a reference to the check is all it may keep, the module whose
// dealing it is, and who records it.
type IdentityPurpose struct {
	Name       string
	LegalBasis string
	Kinds      []string
	Retention  time.Duration
	// Only that the check was made, and where: no value.
	ReferenceOnly bool
	Module        access.Module
	Records       func(access.Roles) bool
}

// IdentityPurposes are the purposes there are. A purpose not here has no
// legal basis and cannot be recorded. The retention is a default the shop
// may set otherwise where the law leaves it to them (G2, #221): five years
// for money-laundering due diligence, which the law states (lag 2017:630,
// 5 kap. 3 §); the others until somebody who knows says otherwise.
var IdentityPurposes = []IdentityPurpose{
	{Name: "aml_due_diligence", LegalBasis: "legal_obligation", Kinds: []string{IdentityNumber, IdentityDocument},
		Retention: 5 * 365 * 24 * time.Hour, Module: access.Dealer,
		Records: func(r access.Roles) bool { return r.SellsVehicles() || r.BuysVehicles() }},
	{Name: "ownership_change", LegalBasis: "legal_obligation", Kinds: []string{IdentityNumber},
		Retention: 90 * 24 * time.Hour, Module: access.Dealer,
		Records: func(r access.Roles) bool { return r.SellsVehicles() || r.BuysVehicles() }},
	{Name: "licence_check", LegalBasis: "legal_obligation", Kinds: []string{IdentityLicence},
		Retention: 30 * 24 * time.Hour, ReferenceOnly: true, Module: access.Rental,
		Records: access.Roles.HandsOverRentals},
}

func identityPurpose(name string) (IdentityPurpose, bool) {
	for _, p := range IdentityPurposes {
		if p.Name == name {
			return p, true
		}
	}
	return IdentityPurpose{}, false
}

var (
	errIdentityPurpose = fmt.Errorf("%w: that is not a purpose an identity record can be held for", ErrInvalid)
	errIdentityKind    = fmt.Errorf("%w: that purpose does not need that kind of record", ErrInvalid)
	errIdentityRefOnly = fmt.Errorf("%w: for that purpose only a reference to the check is kept, not the value", ErrInvalid)
	errIdentityOne     = fmt.Errorf("%w: give either the value or a reference to the check, not both and not neither", ErrInvalid)
	errIdentityGone    = fmt.Errorf("%w: that value has been removed; that the check was made is all that is kept", ErrInvalid)
	errIdentityEnded   = fmt.Errorf("%w: that purpose has already ended", ErrInvalid)
	errIdentityRef     = fmt.Errorf("%w: that record holds a reference to a check, not a value", ErrInvalid)
	// ErrIdentityEndNoReason is ending a purpose early without saying why.
	ErrIdentityEndNoReason = fmt.Errorf("%w: say why the purpose has ended, in 300 characters or fewer", ErrInvalid)
)

// ErrIdentityKey is a value that cannot be read or recorded for want of its
// key: none configured, or not the one it was sealed with. Never answered
// with the value in the clear.
var ErrIdentityKey = errors.New("workshop: the key for identity values is not configured; they can be neither recorded nor read")

// identityAAD is what a value is sealed for: this shop, this record, this
// party, this kind. Moved to another row, it does not open.
func identityAAD(shopID, id, partyID, kind string) []byte {
	return []byte(strings.ToLower(shopID + "/" + id + "/" + partyID + "/" + kind))
}

// NewIdentityRecord is a check to record: of whom, what kind, for what, and
// the value or a reference to where the check was made.
type NewIdentityRecord struct {
	PartyID   string
	Kind      string
	Purpose   string
	Value     string
	Reference string
}

// RecordIdentity records a check for a purpose on the list, by whoever
// records that purpose, in a shop that runs its module. The value is sealed
// before it leaves this function; the database never sees it.
func RecordIdentity(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keys *identity.Keyring, n NewIdentityRecord) (string, error) {
	// Whoever records one may read them, and only the roles of the
	// purpose's dealing record it.
	if !scope.Role.SeesIdentityRecords() {
		return "", access.ErrForbidden
	}
	p, ok := identityPurpose(n.Purpose)
	if !ok {
		return "", errIdentityPurpose
	}
	if !p.Records(scope.Role) {
		return "", access.ErrForbidden
	}
	// Written out per module, so the permission contract sees the gate.
	switch p.Module {
	case access.Dealer:
		if err := scope.Require(access.Dealer); err != nil {
			return "", err
		}
	case access.Rental:
		if err := scope.Require(access.Rental); err != nil {
			return "", err
		}
	}
	n.Value, n.Reference = strings.TrimSpace(n.Value), strings.TrimSpace(n.Reference)
	switch {
	case !looksLikeUUID(n.PartyID):
		return "", ErrNotFound
	case !containsString(p.Kinds, n.Kind):
		return "", errIdentityKind
	case (n.Value == "") == (n.Reference == ""):
		return "", errIdentityOne
	case p.ReferenceOnly && n.Value != "":
		return "", errIdentityRefOnly
	case utf8.RuneCountInString(n.Value) > 100 || utf8.RuneCountInString(n.Reference) > 200:
		return "", fmt.Errorf("%w: that is longer than it can be", ErrInvalid)
	}
	if n.Value != "" && keys.Current() == "" {
		return "", ErrIdentityKey
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := partySeenTx(ctx, tx, scope, n.PartyID); err != nil {
			return err
		}
		var party string
		if err := tx.QueryRow(ctx, `SELECT party_now($1)::text`, n.PartyID).Scan(&party); err != nil {
			return fmt.Errorf("read party: %w", err)
		}
		retention, err := identityRetentionTx(ctx, tx, p)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return err
		}
		var keyID *string
		var sealed []byte
		if n.Value != "" {
			k, s, err := keys.Seal([]byte(n.Value), identityAAD(scope.ShopID, id, party, n.Kind))
			if err != nil {
				return ErrIdentityKey
			}
			keyID, sealed = &k, s
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO identity_records (id, shop_id, party_id, kind, purpose, legal_basis, recorded_by, expires_at,
			                              key_id, sealed, verification_ref)
			VALUES ($1, $2, $3, $4, $5, $6, $7, clock_timestamp() + make_interval(secs => $8), $9, $10, nullif($11, ''))`,
			id, scope.ShopID, party, n.Kind, p.Name, p.LegalBasis, scope.UserID, retention.Seconds(), keyID, sealed, n.Reference)
		if err != nil {
			return fmt.Errorf("record the check: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// identityRetentionTx is how long a purpose keeps its values: the shop's
// latest setting, or the purpose's default.
func identityRetentionTx(ctx context.Context, tx pgx.Tx, p IdentityPurpose) (time.Duration, error) {
	var days *int
	err := tx.QueryRow(ctx, `SELECT days FROM identity_retention_settings WHERE purpose = $1 ORDER BY set_at DESC, id LIMIT 1`, p.Name).Scan(&days)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && days == nil) {
		return p.Retention, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read retention: %w", err)
	}
	return time.Duration(*days) * 24 * time.Hour, nil
}

// SetIdentityRetention sets how long a purpose keeps its values, in days,
// as a row: what it was before stays. Whoever runs the shop, who answers
// for it. Records made before keep the expiry they were made with.
func SetIdentityRetention(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, purpose string, days int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if _, ok := identityPurpose(purpose); !ok {
		return errIdentityPurpose
	}
	if days < 1 || days > 3660 {
		return fmt.Errorf("%w: between a day and ten years", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO identity_retention_settings (shop_id, purpose, days, set_by) VALUES ($1, $2, $3, $4)`,
			scope.ShopID, purpose, days, scope.UserID)
		return err
	})
}

// IdentityRecord is a record as it is listed: that a check was made, never
// the value. The reference to a verification only for those who may read
// identity records.
type IdentityRecord struct {
	ID           string
	Kind         string
	Purpose      string
	LegalBasis   string
	RecordedAt   time.Time
	RecordedBy   string
	ExpiresAt    time.Time
	EndedAt      *time.Time
	HasValue     bool
	Reference    string
	ValueRemoved bool
}

// IdentityRecordsFor lists a party's identity records for whoever may see
// the party: "verified on this day by this person". The values are not
// read here at all.
func IdentityRecordsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, partyID string) ([]IdentityRecord, error) {
	if len(partyRolesSeen(scope.Role, PartyRoles)) == 0 {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(partyID) {
		return nil, ErrNotFound
	}
	var out []IdentityRecord
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := partySeenTx(ctx, tx, scope, partyID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT r.id::text, r.kind, r.purpose, r.legal_basis, r.recorded_at, coalesce(p.display_name, ''), r.expires_at, r.ended_at,
			       r.sealed IS NOT NULL, CASE WHEN $2 THEN coalesce(r.verification_ref, '') ELSE '' END, r.value_removed_at IS NOT NULL
			FROM identity_records r
			LEFT JOIN users u ON u.id = r.recorded_by LEFT JOIN people p ON p.id = u.person_id
			WHERE r.party_id = ANY((SELECT party_ids_read_as(party_now($1)))::uuid[])
			ORDER BY r.recorded_at, r.id`, partyID, scope.Role.SeesIdentityRecords())
		if err != nil {
			return fmt.Errorf("read identity records: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r IdentityRecord
			if err := rows.Scan(&r.ID, &r.Kind, &r.Purpose, &r.LegalBasis, &r.RecordedAt, &r.RecordedBy, &r.ExpiresAt, &r.EndedAt,
				&r.HasValue, &r.Reference, &r.ValueRemoved); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// MaskIdentity is a value as it is shown by default: its last four
// characters, the rest hidden.
func MaskIdentity(v string) string {
	r := []rune(v)
	if len(r) <= 4 {
		return strings.Repeat("•", len(r))
	}
	return strings.Repeat("•", len(r)-4) + string(r[len(r)-4:])
}

// ReadIdentityValue reads a record's value for a role that may read
// identity records: masked to its last four unless full is asked for. Every
// read, masked or full, is an audit event -- who, the purpose, when, and
// whether in full -- in the read's own transaction: a read that cannot be
// logged is not made.
func ReadIdentityValue(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keys *identity.Keyring, recordID string, full bool) (string, error) {
	if !scope.Role.SeesIdentityRecords() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(recordID) {
		return "", ErrNotFound
	}
	var value string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var party, kind, purpose string
		var keyID *string
		var sealed []byte
		var removed bool
		err := tx.QueryRow(ctx, `
			SELECT party_id::text, kind, purpose, key_id, sealed, value_removed_at IS NOT NULL
			FROM identity_records WHERE id = $1`, recordID).Scan(&party, &kind, &purpose, &keyID, &sealed, &removed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read identity record: %w", err)
		}
		if err := partySeenTx(ctx, tx, scope, party); err != nil {
			return err
		}
		switch {
		case removed:
			return errIdentityGone
		case sealed == nil:
			return errIdentityRef
		}
		plain, err := keys.Open(*keyID, sealed, identityAAD(scope.ShopID, recordID, party, kind))
		if err != nil {
			return ErrIdentityKey
		}
		value = string(plain)
		if !full {
			value = MaskIdentity(value)
		}
		return auditTx(ctx, tx, scope, recordID, "identity.read", "", "", "",
			map[string]any{"purpose": purpose, "kind": kind, "full": full})
	})
	if err != nil {
		return "", err
	}
	return value, nil
}

// EndIdentityPurpose ends a record's purpose before its expiry -- the rental
// cancelled, the sale fallen through -- and removes its value now. That the
// check was made stays. Whoever records that purpose.
func EndIdentityPurpose(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, recordID, reason string) error {
	if !scope.Role.SeesIdentityRecords() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(recordID) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, ErrIdentityEndNoReason)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var purpose string
		var ended bool
		err := tx.QueryRow(ctx, `SELECT purpose, ended_at IS NOT NULL FROM identity_records WHERE id = $1`, recordID).Scan(&purpose, &ended)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read identity record: %w", err)
		}
		if p, ok := identityPurpose(purpose); !ok || !p.Records(scope.Role) {
			return access.ErrForbidden
		}
		if ended {
			return errIdentityEnded
		}
		_, err = tx.Exec(ctx, `
			UPDATE identity_records SET ended_at = clock_timestamp(), ended_by = $2, end_reason = $3,
			       key_id = NULL, sealed = NULL, verification_ref = NULL, value_removed_at = coalesce(value_removed_at, clock_timestamp())
			WHERE id = $1`, recordID, scope.UserID, reason)
		return err
	})
}

// RotateIdentityKeys seals every value not sealed with the current key again
// with it, so the older keys can then be dropped. Reads go on throughout:
// every key in the keyring opens what it sealed. Whoever runs the shop.
// Returns how many were sealed again.
func RotateIdentityKeys(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keys *identity.Keyring) (int, error) {
	if !scope.Role.RunsTheShop() {
		return 0, access.ErrForbidden
	}
	if keys.Current() == "" {
		return 0, ErrIdentityKey
	}
	n := 0
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		type row struct {
			id, party, kind, key string
			sealed               []byte
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, party_id::text, kind, key_id, sealed FROM identity_records
			WHERE sealed IS NOT NULL AND key_id <> $1 FOR UPDATE`, keys.Current())
		if err != nil {
			return fmt.Errorf("read identity records: %w", err)
		}
		var todo []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.party, &r.kind, &r.key, &r.sealed); err != nil {
				rows.Close()
				return err
			}
			todo = append(todo, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range todo {
			aad := identityAAD(scope.ShopID, r.id, r.party, r.kind)
			plain, err := keys.Open(r.key, r.sealed, aad)
			if err != nil {
				return ErrIdentityKey
			}
			k, s, err := keys.Seal(plain, aad)
			if err != nil {
				return ErrIdentityKey
			}
			if _, err := tx.Exec(ctx, `UPDATE identity_records SET key_id = $2, sealed = $3 WHERE id = $1`, r.id, k, s); err != nil {
				return fmt.Errorf("seal again: %w", err)
			}
			n++
		}
		return nil
	})
	return n, err
}

// sweepIdentityTx removes the values past their expiry and keeps the record
// that a check was made.
func sweepIdentityTx(ctx context.Context, tx pgx.Tx) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE identity_records SET key_id = NULL, sealed = NULL, verification_ref = NULL, value_removed_at = clock_timestamp()
		WHERE value_removed_at IS NULL AND expires_at <= clock_timestamp()`)
	return tag.RowsAffected(), err
}

// eraseIdentityTx is a person's erasure reaching their identity records:
// a value the law has the shop keep -- a legal obligation, not yet expired
// -- stays until it expires, and the erasure says so; every other value is
// removed now. That a check was made stays either way.
func eraseIdentityTx(ctx context.Context, tx pgx.Tx, personID string) (removed, kept int64, err error) {
	const theirs = `party_id IN (SELECT pa.id FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id
	                             WHERE pa.person_id = $1 OR c.person_id = $1)`
	tag, err := tx.Exec(ctx, `
		UPDATE identity_records SET key_id = NULL, sealed = NULL, verification_ref = NULL, value_removed_at = clock_timestamp()
		WHERE value_removed_at IS NULL AND (legal_basis <> 'legal_obligation' OR expires_at <= clock_timestamp()) AND `+theirs, personID)
	if err != nil {
		return 0, 0, fmt.Errorf("remove identity values: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_records WHERE value_removed_at IS NULL AND `+theirs, personID).Scan(&kept); err != nil {
		return 0, 0, fmt.Errorf("count identity values kept: %w", err)
	}
	return tag.RowsAffected(), kept, nil
}
