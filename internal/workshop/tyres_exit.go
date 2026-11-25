package workshop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// How a set leaves the hotel (#232): collected by its owner, transferred to
// another owner, or disposed of. Transfer and disposal need the owner's yes,
// on a link made for them or as a signed note somebody at the shop records
// with where it is kept. The exit is one row; the database refuses anything
// added to a set once it has one.

var (
	errSetGone         = fmt.Errorf("%w: that set has left the hotel", ErrInvalid)
	errCollector       = fmt.Errorf("%w: write down who collected it", ErrInvalid)
	errAgreementKind   = fmt.Errorf("%w: an agreement is to a transfer or to a disposal", ErrInvalid)
	errAgreementNote   = fmt.Errorf("%w: write down where the signed note is kept", ErrInvalid)
	errNoAgreement     = fmt.Errorf("%w: the owner has not agreed to that", ErrInvalid)
	errTransferTo      = fmt.Errorf("%w: a set is transferred to another customer", ErrInvalid)
	errDisposalFeeNeed = fmt.Errorf("%w: a disposal fee goes on a job: give both the job and the fee, or neither", ErrInvalid)
)

// agreementLifetime is how long the owner has to answer on their link.
const agreementLifetime = 30 * 24 * time.Hour

// setLiveTx refuses a set that has left.
func setLiveTx(ctx context.Context, tx pgx.Tx, setID string) error {
	var gone bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyre_set_exits WHERE tyre_set_id = $1)`, setID).Scan(&gone); err != nil {
		return err
	}
	if gone {
		return errSetGone
	}
	return nil
}

// tyreSetLiveTx is the same for the set a tyre stands in.
func tyreSetLiveTx(ctx context.Context, tx pgx.Tx, tyreID string) error {
	var gone bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyre_set_exits x JOIN tyres t ON t.tyre_set_id = x.tyre_set_id WHERE t.id = $1)`, tyreID).Scan(&gone); err != nil {
		return err
	}
	if gone {
		return errSetGone
	}
	return nil
}

// TyreSetExit is how a set left, if it has.
type TyreSetExit struct {
	Kind        string // collected, transferred or disposed
	CollectedBy string
	NewSetID    string
	At          time.Time
}

// TyreSetExitOf says how a set left and when, or that it has not. Whoever
// works in the workshop.
func TyreSetExitOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID string) (*TyreSetExit, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(setID) {
		return nil, ErrNotFound
	}
	var x *TyreSetExit
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var e TyreSetExit
		err := tx.QueryRow(ctx, `SELECT kind, coalesce(collected_by, ''), coalesce(new_set_id::text, ''), at FROM tyre_set_exits WHERE tyre_set_id = $1`, setID).
			Scan(&e.Kind, &e.CollectedBy, &e.NewSetID, &e.At)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		x = &e
		return nil
	})
	return x, err
}

func exitTx(ctx context.Context, tx pgx.Tx, scope access.Scope, setID, kind, collectedBy, agreementID, newSet, jobID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tyre_set_exits (shop_id, tyre_set_id, kind, collected_by, agreement_id, new_set_id, work_order_id, by_user)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, '')::uuid, NULLIF($6, '')::uuid, NULLIF($7, '')::uuid, NULLIF($8, '')::uuid)`,
		scope.ShopID, setID, kind, collectedBy, agreementID, newSet, jobID, scope.UserID)
	return err
}

// CollectTyreSet records that a set was collected, by whom, and frees its place.
// Whoever works in the workshop: it is handed over at the counter.
func CollectTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID, collectedBy string) error {
	if !scope.Role.WorksInTheWorkshop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(setID) {
		return ErrNotFound
	}
	collectedBy = strings.TrimSpace(collectedBy)
	if collectedBy == "" || utf8.RuneCountInString(collectedBy) > 200 {
		return errCollector
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "tyreset:"+setID); err != nil {
			return err
		}
		if err := setLiveTx(ctx, tx, setID); err != nil {
			return err
		}
		if err := takeOutTx(ctx, tx, scope, setID); err != nil {
			return err
		}
		if err := exitTx(ctx, tx, scope, setID, "collected", collectedBy, "", "", ""); err != nil {
			return err
		}
		return auditTx(ctx, tx, scope, setID, "tyre_set.collected", "", collectedBy, "", nil)
	})
}

// RequestSetAgreement makes the link on which the owner says yes or no to a
// transfer or a disposal of their set, and returns its token, once. A new link
// for the same set and kind closes the earlier ones. Whoever deals with
// customers.
func RequestSetAgreement(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID, kind, newCustomerID string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if kind != "transfer" && kind != "disposal" {
		return "", errAgreementKind
	}
	if !looksLikeUUID(setID) || (newCustomerID != "" && !looksLikeUUID(newCustomerID)) {
		return "", ErrNotFound
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := agreementSubjectTx(ctx, tx, setID, kind, newCustomerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tyre_set_agreements a SET revoked_at = clock_timestamp()
			WHERE a.tyre_set_id = $1 AND a.kind = $2 AND a.revoked_at IS NULL AND a.token_sha256 IS NOT NULL
			  AND NOT EXISTS (SELECT 1 FROM tyre_set_agreement_answers w WHERE w.agreement_id = a.id)`, setID, kind); err != nil {
			return fmt.Errorf("close earlier links: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO tyre_set_agreements (shop_id, tyre_set_id, kind, new_customer_id, token_sha256, expires_at, created_by)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, clock_timestamp() + $6::interval, NULLIF($7, '')::uuid)`,
			scope.ShopID, setID, kind, newCustomerID, sum[:], fmt.Sprintf("%d seconds", int(agreementLifetime.Seconds())), scope.UserID)
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// agreementSubjectTx checks what an agreement is about: a set still here and
// stored, and for a transfer another customer who exists.
func agreementSubjectTx(ctx context.Context, tx pgx.Tx, setID, kind, newCustomerID string) error {
	var owner string
	if err := tx.QueryRow(ctx, `SELECT customer_id::text FROM tyre_sets WHERE id = $1`, setID).Scan(&owner); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := setLiveTx(ctx, tx, setID); err != nil {
		return err
	}
	if kind == "transfer" {
		if newCustomerID == "" || newCustomerID == owner {
			return errTransferTo
		}
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM customers WHERE id = $1`, newCustomerID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
	} else if newCustomerID != "" {
		return errAgreementKind
	}
	return nil
}

// RecordSetAgreement records the owner's yes given on a signed note, with where
// the note is kept. Whoever deals with customers.
func RecordSetAgreement(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID, kind, newCustomerID, note string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if kind != "transfer" && kind != "disposal" {
		return "", errAgreementKind
	}
	if !looksLikeUUID(setID) || (newCustomerID != "" && !looksLikeUUID(newCustomerID)) {
		return "", ErrNotFound
	}
	note = strings.TrimSpace(note)
	if note == "" || utf8.RuneCountInString(note) > 500 {
		return "", errAgreementNote
	}
	var id string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := agreementSubjectTx(ctx, tx, setID, kind, newCustomerID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO tyre_set_agreements (shop_id, tyre_set_id, kind, new_customer_id, created_by)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid) RETURNING id::text`,
			scope.ShopID, setID, kind, newCustomerID, scope.UserID).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO tyre_set_agreement_answers (shop_id, agreement_id, answer, how, note, by_user)
			VALUES ($1, $2, 'agreed', 'note', $3, NULLIF($4, '')::uuid)`, scope.ShopID, id, note, scope.UserID)
		return err
	})
	return id, err
}

// AgreementView is what the owner is shown on their link: the shop, the set and
// what is asked. No price, no other customer.
type AgreementView struct {
	ShopName string
	Kind     string
	Set      string
	Answered string // "", "agreed" or "declined"
}

func agreementByTokenTx(ctx context.Context, tx pgx.Tx, token string) (id string, v AgreementView, err error) {
	if token == "" {
		return "", v, ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	err = tx.QueryRow(ctx, `
		SELECT a.id::text, z.name, a.kind, coalesce(ts.brand, '') || CASE WHEN coalesce(ts.model, '') <> '' THEN ' ' || ts.model ELSE '' END || ' (' || ts.season || ')',
		       coalesce((SELECT w.answer FROM tyre_set_agreement_answers w WHERE w.agreement_id = a.id), '')
		FROM tyre_set_agreements a JOIN tyre_sets ts ON ts.id = a.tyre_set_id JOIN shops z ON z.id = a.shop_id
		WHERE a.token_sha256 = $1 AND a.revoked_at IS NULL AND a.expires_at > clock_timestamp()
		  AND NOT EXISTS (SELECT 1 FROM tyre_set_exits x WHERE x.tyre_set_id = a.tyre_set_id)`, sum[:]).
		Scan(&id, &v.ShopName, &v.Kind, &v.Set, &v.Answered)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", v, ErrShareNotUsable
	}
	return id, v, err
}

// SetAgreement answers the owner's link. Wrong, expired, closed and a set that
// has gone are one answer.
func SetAgreement(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (AgreementView, error) {
	var out AgreementView
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, v, err := agreementByTokenTx(ctx, tx, token)
		out = v
		return err
	})
	return out, err
}

// AnswerSetAgreement is the owner saying yes or no on their link. It is given
// once: a second answer, from a stale page, is refused rather than rewriting the
// first. No user is on the row, which is how the owner's own is told from a note.
func AnswerSetAgreement(ctx context.Context, pool *pgxpool.Pool, shopID, token string, agree bool) error {
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		id, v, err := agreementByTokenTx(ctx, tx, token)
		if err != nil {
			return err
		}
		if v.Answered != "" {
			return ErrShareNotUsable
		}
		answer := "declined"
		if agree {
			answer = "agreed"
		}
		tag, err := tx.Exec(ctx, `INSERT INTO tyre_set_agreement_answers (shop_id, agreement_id, answer, how) VALUES ($1, $2, $3, 'link')
			ON CONFLICT (agreement_id) DO NOTHING`, shopID, id, answer)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrShareNotUsable
		}
		return nil
	})
}

// agreedTx reads an agreement the owner said yes to, of the kind and for the
// set, and returns the customer a transfer is to.
func agreedTx(ctx context.Context, tx pgx.Tx, agreementID, setID, kind string) (newCustomer string, err error) {
	var k, answer string
	var set string
	var nc *string
	err = tx.QueryRow(ctx, `
		SELECT a.kind, a.tyre_set_id::text, a.new_customer_id::text, coalesce(w.answer, '')
		FROM tyre_set_agreements a LEFT JOIN tyre_set_agreement_answers w ON w.agreement_id = a.id WHERE a.id = $1`, agreementID).Scan(&k, &set, &nc, &answer)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if k != kind || set != setID || answer != "agreed" {
		return "", errNoAgreement
	}
	if nc != nil {
		newCustomer = *nc
	}
	return newCustomer, nil
}

// TransferTyreSet hands a stored set to another customer once the owner has
// agreed. The old set keeps its history and is closed; the new owner has a new
// set, with the same tyres, in the same place, that says where it came from.
// Whoever deals with customers.
func TransferTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID, agreementID string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(setID) || !looksLikeUUID(agreementID) {
		return "", ErrNotFound
	}
	var newSet string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "tyreset:"+setID); err != nil {
			return err
		}
		if err := setLiveTx(ctx, tx, setID); err != nil {
			return err
		}
		to, err := agreedTx(ctx, tx, agreementID, setID, "transfer")
		if err != nil {
			return err
		}
		var place string
		if err := tx.QueryRow(ctx, `SELECT location_id::text FROM tyre_set_placements WHERE tyre_set_id = $1 AND removed_at IS NULL`, setID).Scan(&place); errors.Is(err, pgx.ErrNoRows) {
			return errNotStored
		} else if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO tyre_sets (shop_id, customer_id, season, brand, model, rims, tpms, fasteners, note, created_by, transferred_from_id)
			SELECT shop_id, $2, season, brand, model, rims, tpms, fasteners, note, NULLIF($3, '')::uuid, id FROM tyre_sets WHERE id = $1 RETURNING id::text`,
			setID, to, scope.UserID).Scan(&newSet); err != nil {
			return fmt.Errorf("create the new owner's set: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO tyres (shop_id, tyre_set_id, position, dimension, dot_week, dot_year, created_by)
			SELECT t.shop_id, $2, t.position, t.dimension, t.dot_week, t.dot_year, NULLIF($3, '')::uuid
			FROM tyres t WHERE t.tyre_set_id = $1 AND NOT EXISTS (SELECT 1 FROM tyres r WHERE r.replaces_id = t.id)`, setID, newSet, scope.UserID); err != nil {
			return fmt.Errorf("copy the tyres: %w", err)
		}
		if err := takeOutTx(ctx, tx, scope, setID); err != nil {
			return err
		}
		if err := exitTx(ctx, tx, scope, setID, "transferred", "", agreementID, newSet, ""); err != nil {
			return err
		}
		if err := placeTx(ctx, tx, scope, newSet, place); err != nil {
			return err
		}
		return auditTx(ctx, tx, scope, setID, "tyre_set.transferred", "", newSet, "", nil)
	})
	return newSet, err
}

// DisposeTyreSet disposes of a stored set once the owner has agreed, and puts
// the disposal fee on a job as a line when one is given. The set keeps its
// history, read-only. Whoever deals with customers.
func DisposeTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID, agreementID, jobID, feeExtraID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(setID) || !looksLikeUUID(agreementID) || (jobID != "" && !looksLikeUUID(jobID)) || (feeExtraID != "" && !looksLikeUUID(feeExtraID)) {
		return ErrNotFound
	}
	if (jobID == "") != (feeExtraID == "") {
		return errDisposalFeeNeed
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "tyreset:"+setID); err != nil {
			return err
		}
		if err := setLiveTx(ctx, tx, setID); err != nil {
			return err
		}
		if _, err := agreedTx(ctx, tx, agreementID, setID, "disposal"); err != nil {
			return err
		}
		if jobID != "" {
			job, err := jobTx(ctx, tx, jobID)
			if err != nil {
				return err
			}
			var owner string
			if err := tx.QueryRow(ctx, `SELECT customer_id::text FROM tyre_sets WHERE id = $1`, setID).Scan(&owner); err != nil {
				return err
			}
			if owner != job.customer {
				return errStorageOwner
			}
			var x TyreExtra
			err = tx.QueryRow(ctx, tyreExtrasSQL+` WHERE x.id = $1`, feeExtraID).
				Scan(&x.ID, &x.Name, &x.Per, &x.LineKind, &x.UnitPriceMinor, &x.VATRateBasis, &x.Active, &x.StoragePeriod)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if !x.Active || x.StoragePeriod != "" {
				return errTyreExtraRetired
			}
			qty, err := feeQuantityTx(ctx, tx, x, setID)
			if err != nil {
				return err
			}
			line := NewLine{Kind: x.LineKind, Description: x.Name, QuantityMilli: qty, UnitPriceMinor: x.UnitPriceMinor, VATRateBasis: x.VATRateBasis}
			if err := checkLine(&line); err != nil {
				return err
			}
			if err := addLineTx(ctx, tx, scope, jobID, line, lineSource{}); err != nil {
				return err
			}
		}
		if err := takeOutTx(ctx, tx, scope, setID); err != nil {
			return err
		}
		if err := exitTx(ctx, tx, scope, setID, "disposed", "", agreementID, "", jobID); err != nil {
			return err
		}
		return auditTx(ctx, tx, scope, setID, "tyre_set.disposed", "", "", "", nil)
	})
}

// AbandonedSet is a set in storage that has had no charge, no job, no booking
// and no move for as long as the shop says, with the reminders sent since: the
// list a shop works through before it decides anything about the tyres.
type AbandonedSet struct {
	SetID     string
	Label     string
	Place     string
	Owner     string
	Phone     string
	Since     time.Time
	Reminders int
}

// AbandonedSets lists the sets that may have been left, the longest quiet
// first. Whoever deals with customers. It decides nothing: what a shop may do
// with tyres nobody comes for waits on a lawyer (#221), and a set leaves only by
// one of the three recorded ways.
func AbandonedSets(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]AbandonedSet, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []AbandonedSet
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		set, err := changeSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT ts.id::text, coalesce(ts.brand, '') || CASE WHEN coalesce(ts.model, '') <> '' THEN ' ' || ts.model ELSE '' END,
			       l.name, coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name, ''), coalesce(p.phone, ''), q.last,
			       (SELECT count(*)::int FROM tyre_campaign_recipients r WHERE r.customer_id = c.id AND r.at > q.last)
			FROM tyre_sets ts
			JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
			JOIN storage_locations l ON l.id = pl.location_id
			JOIN customers c ON c.id = ts.customer_id LEFT JOIN people p ON p.id = c.person_id
			CROSS JOIN LATERAL (SELECT greatest(pl.placed_at,
			        coalesce((SELECT max(x.charged_at) FROM tyre_storage_charges x WHERE x.tyre_set_id = ts.id), pl.placed_at),
			        coalesce((SELECT max(m.at) FROM tyre_job_moves m WHERE m.tyre_set_id = ts.id), pl.placed_at),
			        coalesce((SELECT max(tb.at) FROM tyre_change_bookings tb WHERE tb.tyre_set_id = ts.id), pl.placed_at)) AS last) q
			WHERE q.last <= clock_timestamp() - make_interval(days => $1) AND NOT EXISTS (SELECT 1 FROM shop_own_customers o WHERE o.customer_id = c.id)
			ORDER BY q.last, ts.id`, set.AbandonDays)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a AbandonedSet
			if err := rows.Scan(&a.SetID, &a.Label, &a.Place, &a.Owner, &a.Phone, &a.Since, &a.Reminders); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// SetAgreementRow is one agreement asked for or recorded about a set.
type SetAgreementRow struct {
	ID          string
	Kind        string
	NewCustomer string
	// waiting, agreed, declined, or closed: a link replaced or run out.
	State string
	How   string
	Note  string
	At    time.Time
}

// SetAgreementsOf lists the agreements about a set, the newest first. Whoever
// deals with customers.
func SetAgreementsOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID string) ([]SetAgreementRow, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(setID) {
		return nil, ErrNotFound
	}
	var out []SetAgreementRow
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT a.id::text, a.kind, coalesce(p.display_name, c.company_name, ''),
			       CASE WHEN w.answer IS NOT NULL THEN w.answer
			            WHEN a.revoked_at IS NOT NULL OR a.expires_at <= clock_timestamp() THEN 'closed' ELSE 'waiting' END,
			       coalesce(w.how, 'link'), coalesce(w.note, ''), coalesce(w.at, a.created_at)
			FROM tyre_set_agreements a
			LEFT JOIN tyre_set_agreement_answers w ON w.agreement_id = a.id
			LEFT JOIN customers c ON c.id = a.new_customer_id LEFT JOIN people p ON p.id = c.person_id
			WHERE a.tyre_set_id = $1 ORDER BY a.created_at DESC, a.id`, setID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r SetAgreementRow
			if err := rows.Scan(&r.ID, &r.Kind, &r.NewCustomer, &r.State, &r.How, &r.Note, &r.At); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
