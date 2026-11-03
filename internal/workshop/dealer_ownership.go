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
)

// Filing a change of ownership with Transportstyrelsen (#180). There is no
// public API: a person files it and records here that it was filed, and
// then Transportstyrelsen's confirmation, which alone changes the
// ownership history -- once, whatever is retried. The control number is
// never kept.

// OwnershipFiler is a way a filing reaches Transportstyrelsen. The manual
// one, shipped and the default, does nothing itself: it is a person's
// checklist. An adapter (#214) files through a provider instead, its calls
// through the outbox (#154).
type OwnershipFiler interface {
	// Name is the adapter as a filing records it.
	Name() string
	// Automatic reports whether the adapter files by itself; the manual
	// one does not.
	Automatic() bool
}

// ManualFiler is a person filing by hand at Transportstyrelsen.
type ManualFiler struct{}

func (ManualFiler) Name() string    { return "manual" }
func (ManualFiler) Automatic() bool { return false }

// OwnershipFiling is a filing as it is read.
type OwnershipFiling struct {
	ID          string
	SubjectKind string // purchase or sale
	SubjectID   string
	VehicleID   string
	Car         string
	FromPartyID string // "" for the shop
	ToPartyID   string // "" for the shop
	From, To    string
	PassesOn    time.Time
	Status      string
	Reference   string
	Events      []FilingEvent
}

// FilingEvent is one step of a filing.
type FilingEvent struct {
	Status    string
	Reference string
	Note      string
	At        time.Time
	By        string
}

// FilingChecklist is what the person filing by hand says they had.
type FilingChecklist struct {
	ControlNumberInHand bool
	// Nil where it does not apply.
	LeasingConsent   *bool
	RetentionOfTitle *bool
	Note             string
}

var (
	errFilingControl   = fmt.Errorf("%w: the control number is needed to file; get it from the registration certificate first", ErrInvalid)
	errFilingStatus    = fmt.Errorf("%w: a filing is filed before it is confirmed or rejected, and not after it is confirmed", ErrInvalid)
	errFilingReference = fmt.Errorf("%w: say Transportstyrelsen's reference for the confirmation, in 100 characters or fewer", ErrInvalid)
	// ErrFilingNoReason is a filing rejected without saying why.
	ErrFilingNoReason = fmt.Errorf("%w: say why the filing was rejected, in 500 characters or fewer", ErrInvalid)
)

// prepareFilingTx makes a subject's filing, prepared, once: from a party to
// the shop at a purchase, from the shop -- or the principal of a consigned
// car -- to the buyer at a sale. Asked again, it finds the one there is.
func prepareFilingTx(ctx context.Context, tx pgx.Tx, scope access.Scope, kind, subject, vehicle, from, to string, on time.Time) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO ownership_filings (shop_id, subject_kind, subject_id, vehicle_id, from_party_id, to_party_id, passes_on, created_by)
		VALUES ($1, $2, $3, $4, CASE WHEN $5 <> '' THEN party_now($5::uuid) END, CASE WHEN $6 <> '' THEN party_now($6::uuid) END,
		        $7::date, NULLIF($8, '')::uuid)
		ON CONFLICT (subject_kind, subject_id) DO NOTHING RETURNING id`,
		scope.ShopID, kind, subject, vehicle, from, to, on.Format(time.DateOnly), scope.UserID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id FROM ownership_filings WHERE subject_kind = $1 AND subject_id = $2`, kind, subject).Scan(&id)
		return id, err
	}
	if err != nil {
		return "", fmt.Errorf("prepare the ownership filing: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ownership_filing_events (shop_id, filing_id, status, by_user)
		VALUES ($1, $2, 'prepared', NULLIF($3, '')::uuid)`, scope.ShopID, id, scope.UserID); err != nil {
		return "", fmt.Errorf("prepare the ownership filing: %w", err)
	}
	return id, nil
}

// mayFileTx checks roles for a filing of kind: the buyer files a purchase,
// whoever sells cars a sale.
func mayFile(scope access.Scope, kind string) error {
	switch kind {
	case "purchase":
		if !scope.Role.BuysVehicles() {
			return access.ErrForbidden
		}
	case "sale":
		if !scope.Role.SellsVehicles() {
			return access.ErrForbidden
		}
	default:
		return ErrNotFound
	}
	return scope.Require(access.Dealer)
}

// RecordFilingFiled records that a person filed the change by hand, with
// the checklist as it stood: the control number in hand -- which is never
// recorded -- leasing consent and retention of title where they apply.
func RecordFilingFiled(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, c FilingChecklist) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	if !c.ControlNumberInHand {
		return errFilingControl
	}
	c.Note = strings.TrimSpace(c.Note)
	if utf8.RuneCountInString(c.Note) > 500 {
		return errFilingStatus
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		f, err := lockFilingTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if f.Status == "confirmed" {
			return errFilingStatus
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO ownership_filing_events (shop_id, filing_id, status, control_number_in_hand, leasing_consent, retention_of_title,
			                                     note, by_user)
			VALUES ($1, $2, 'filed', true, $3, $4, nullif($5, ''), NULLIF($6, '')::uuid)`,
			scope.ShopID, id, c.LeasingConsent, c.RetentionOfTitle, c.Note, scope.UserID)
		return err
	})
}

// ConfirmFiling records Transportstyrelsen's confirmation and changes the
// ownership history in the same transaction: the previous owner's period
// ends on the day it passes, and the buyer's begins. Once: confirmed
// again, it changes nothing more.
func ConfirmFiling(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, reference string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	reference = strings.TrimSpace(reference)
	if reference == "" || utf8.RuneCountInString(reference) > 100 {
		return errFilingReference
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		f, err := lockFilingTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		switch f.Status {
		case "confirmed":
			return nil
		case "filed":
		default:
			return errFilingStatus
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ownership_filing_events (shop_id, filing_id, status, reference, by_user)
			VALUES ($1, $2, 'confirmed', $3, NULLIF($4, '')::uuid)`, scope.ShopID, id, reference, scope.UserID); err != nil {
			return fmt.Errorf("confirm the filing: %w", err)
		}
		passes := f.PassesOn.Format(time.DateOnly)
		// Whoever owned it until now stops owning it the day it passes.
		if _, err := tx.Exec(ctx, `
			UPDATE vehicle_ownership SET owned_to = greatest($2::date::timestamptz, owned_from + interval '1 second')
			WHERE vehicle_id = $1 AND owned_to IS NULL`, f.VehicleID, passes); err != nil {
			return fmt.Errorf("end the previous ownership: %w", err)
		}
		if f.ToPartyID != "" {
			customer, err := customerForPartyTx(ctx, tx, scope, f.ToPartyID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id, owned_from)
				VALUES ($1, $2, $3, greatest($4::date::timestamptz,
				        coalesce((SELECT max(owned_to) FROM vehicle_ownership WHERE vehicle_id = $2), $4::date::timestamptz)))`,
				scope.ShopID, f.VehicleID, customer, passes); err != nil {
				return fmt.Errorf("begin the new ownership: %w", err)
			}
		}
		return auditTx(ctx, tx, scope, id, "ownership.confirmed", "filed", "confirmed", reference, nil)
	})
}

// RejectFiling records that Transportstyrelsen rejected the filing, and
// why: the ownership history stays as it was, and it may be filed again.
func RejectFiling(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, note string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if note == "" || utf8.RuneCountInString(note) > 500 {
		return ErrFilingNoReason
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		f, err := lockFilingTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if f.Status != "filed" {
			return errFilingStatus
		}
		_, err = tx.Exec(ctx, `INSERT INTO ownership_filing_events (shop_id, filing_id, status, note, by_user)
			VALUES ($1, $2, 'rejected', $3, NULLIF($4, '')::uuid)`, scope.ShopID, id, note, scope.UserID)
		return err
	})
}

// lockFilingTx reads a filing to change, the role checked against its kind.
func lockFilingTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string) (OwnershipFiling, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM vehicles WHERE id = (SELECT vehicle_id FROM ownership_filings WHERE id = $1) FOR UPDATE`, id); err != nil {
		return OwnershipFiling{}, fmt.Errorf("lock the car: %w", err)
	}
	f, err := filingTx(ctx, tx, id)
	if err != nil {
		return OwnershipFiling{}, err
	}
	return f, mayFile(scope, f.SubjectKind)
}

func filingTx(ctx context.Context, tx pgx.Tx, id string) (OwnershipFiling, error) {
	var f OwnershipFiling
	err := tx.QueryRow(ctx, `
		SELECT f.id, f.subject_kind, f.subject_id, f.vehicle_id, `+carNameSQL+`,
		       coalesce(f.from_party_id::text, ''), coalesce(f.to_party_id::text, ''),
		       coalesce((SELECT `+partyNameSQL+` FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id
		                 LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id WHERE pa.id = f.from_party_id), ''),
		       coalesce((SELECT `+partyNameSQL+` FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id
		                 LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id WHERE pa.id = f.to_party_id), ''),
		       f.passes_on
		FROM ownership_filings f JOIN vehicles v ON v.id = f.vehicle_id
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
		WHERE f.id = $1`, id).Scan(&f.ID, &f.SubjectKind, &f.SubjectID, &f.VehicleID, &f.Car, &f.FromPartyID, &f.ToPartyID, &f.From, &f.To,
		&f.PassesOn)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnershipFiling{}, ErrNotFound
	}
	if err != nil {
		return OwnershipFiling{}, fmt.Errorf("read the filing: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT e.status, coalesce(e.reference, ''), coalesce(e.note, ''), e.at, coalesce(p.display_name, '')
		FROM ownership_filing_events e LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE e.filing_id = $1 ORDER BY e.seq`, id)
	if err != nil {
		return OwnershipFiling{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var e FilingEvent
		if err := rows.Scan(&e.Status, &e.Reference, &e.Note, &e.At, &e.By); err != nil {
			return OwnershipFiling{}, err
		}
		f.Events = append(f.Events, e)
		f.Status = e.Status
		if e.Status == "confirmed" {
			f.Reference = e.Reference
		}
	}
	return f, rows.Err()
}

// FilingOf reads a purchase's or a sale's filing, for whoever files that
// kind; ErrNotFound when none was prepared. Reading is Core's.
func FilingOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, kind, subjectID string) (OwnershipFiling, error) {
	switch kind {
	case "purchase":
		if !scope.Role.BuysVehicles() {
			return OwnershipFiling{}, access.ErrForbidden
		}
	case "sale":
		if !scope.Role.SellsVehicles() {
			return OwnershipFiling{}, access.ErrForbidden
		}
	default:
		return OwnershipFiling{}, ErrNotFound
	}
	if !looksLikeUUID(subjectID) {
		return OwnershipFiling{}, ErrNotFound
	}
	var out OwnershipFiling
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM ownership_filings WHERE subject_kind = $1 AND subject_id = $2`, kind, subjectID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var err error
		out, err = filingTx(ctx, tx, id)
		return err
	})
	return out, err
}

// filingsWaiting is the queue's rows for a filing not confirmed two days
// after the ownership passed, in dealerWaiting's shape: the car, and a
// link to the sale or the car's page.
const filingsWaiting = `
	SELECT 'ownership_unconfirmed', CASE WHEN f.subject_kind = 'sale' THEN 'vehicle_sale' ELSE 'stock_unit' END,
	       CASE WHEN f.subject_kind = 'sale' THEN f.subject_id::text
	            ELSE (SELECT p.stock_unit_id::text FROM vehicle_purchases p WHERE p.id = f.subject_id) END,
	       ` + carNameSQL + `, f.passes_on::timestamptz + interval '2 days', f.created_at
	FROM ownership_filings f JOIN vehicles v ON v.id = f.vehicle_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
	WHERE f.passes_on < (clock_timestamp() AT TIME ZONE (SELECT timezone FROM shops))::date - 2
	  AND NOT EXISTS (SELECT 1 FROM ownership_filing_events e WHERE e.filing_id = f.id AND e.status = 'confirmed')`
