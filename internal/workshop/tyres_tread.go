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
)

// Tread depth (#227): a measurement is a row, in tenths of a millimetre, and a
// wrong one is corrected by another row that says why. What the figure means
// is told against the legal minimum and the shop's own limit.

// The statuses a measured or unmeasured tyre has.
const (
	TreadNotMeasured = "not_measured"
	TreadOK          = "ok"
	// At or below the shop's own limit: worth putting to the customer.
	TreadWarning = "warning"
	// Below the legal minimum.
	TreadBelowLegal = "below_legal"
)

const (
	// LegalTreadTenths is 1.6 mm, the minimum for a car tyre in Sweden.
	LegalTreadTenths = 16
	// DefaultTreadWarningTenths is the shop's limit until it says another, 3.0
	// mm: the winter tyre rule is not shown as law until it has been confirmed
	// (#221), so it is the shop's limit and nothing more.
	DefaultTreadWarningTenths = 30
)

var (
	errTread          = fmt.Errorf("%w: a tread depth is from 0 to 25 millimetres, in tenths", ErrInvalid)
	errTreadNote      = fmt.Errorf("%w: a note on a measurement is at most 300 characters", ErrInvalid)
	errTreadReplaced  = fmt.Errorf("%w: that tyre has been replaced; measure the one that took its place", ErrInvalid)
	errTreadCorrected = fmt.Errorf("%w: that measurement has already been corrected", ErrInvalid)
	// ErrTreadNoReason is a measurement corrected without saying why.
	ErrTreadNoReason = fmt.Errorf("%w: say why the measurement is corrected, in 300 characters or fewer", ErrInvalid)
	errTreadWarning  = fmt.Errorf("%w: the shop's tread limit is from 1.6 to 10 millimetres", ErrInvalid)
)

// TreadReading is one measurement of a tyre.
type TreadReading struct {
	ID         string
	Tenths     int
	Note       string
	At         time.Time
	By         string
	CorrectsID string
	Reason     string
}

// TreadStatusOf is what a depth means against the shop's limit.
func TreadStatusOf(tenths *int, warning int) string {
	switch {
	case tenths == nil:
		return TreadNotMeasured
	case *tenths < LegalTreadTenths:
		return TreadBelowLegal
	case *tenths <= warning:
		return TreadWarning
	}
	return TreadOK
}

func treadWarningTx(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT warning_tenths FROM tread_settings ORDER BY changed_at DESC, id DESC LIMIT 1), $1)`, DefaultTreadWarningTenths).Scan(&n)
	return n, err
}

// SetTreadWarning says at what depth, in tenths of a millimetre, a tyre is
// worth putting to the customer. Whoever runs the shop.
func SetTreadWarning(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, tenths int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if tenths < LegalTreadTenths || tenths > 100 {
		return errTreadWarning
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tread_settings (shop_id, warning_tenths, changed_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
			scope.ShopID, tenths, scope.UserID)
		return err
	})
}

// TreadWarningOf reads the shop's limit in tenths of a millimetre.
func TreadWarningOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (int, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return 0, access.ErrForbidden
	}
	var n int
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = treadWarningTx(ctx, tx)
		return err
	})
	return n, err
}

// RecordTreadDepth records a tyre's depth, in tenths of a millimetre. A tyre
// that has been replaced is not measured: the one that took its place is.
// Whoever works in the workshop.
func RecordTreadDepth(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, tyreID string, tenths int, note string) (string, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(tyreID) {
		return "", ErrNotFound
	}
	if tenths < 0 || tenths > 250 {
		return "", errTread
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 300 {
		return "", errTreadNote
	}
	var id string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var replaced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyres r WHERE r.replaces_id = t.id) FROM tyres t WHERE t.id = $1`, tyreID).Scan(&replaced); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if replaced {
			return errTreadReplaced
		}
		if err := tyreSetLiveTx(ctx, tx, tyreID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO tyre_measurements (shop_id, tyre_id, tenths, note, measured_by) VALUES ($1, $2, $3, nullif($4, ''), NULLIF($5, '')::uuid) RETURNING id`,
			scope.ShopID, tyreID, tenths, note, scope.UserID).Scan(&id)
	})
	return id, err
}

// CorrectTreadDepth corrects a measurement with another that says why; the
// first stays. Whoever works in the workshop.
func CorrectTreadDepth(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, measurementID string, tenths int, reason string) (string, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(measurementID) {
		return "", ErrNotFound
	}
	if tenths < 0 || tenths > 250 {
		return "", errTread
	}
	reason, err := checkReason(reason, ErrTreadNoReason)
	if err != nil {
		return "", err
	}
	var id string
	err = inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var tyre string
		if err := tx.QueryRow(ctx, `SELECT tyre_id FROM tyre_measurements WHERE id = $1`, measurementID).Scan(&tyre); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "tread:"+tyre); err != nil {
			return err
		}
		if err := tyreSetLiveTx(ctx, tx, tyre); err != nil {
			return err
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyre_measurements WHERE corrects_id = $1)`, measurementID).Scan(&done); err != nil {
			return err
		}
		if done {
			return errTreadCorrected
		}
		return tx.QueryRow(ctx, `INSERT INTO tyre_measurements (shop_id, tyre_id, tenths, measured_by, corrects_id, reason, measured_at)
			SELECT $1, $2, $3, NULLIF($4, '')::uuid, $5, $6, m.measured_at FROM tyre_measurements m WHERE m.id = $5 RETURNING id`,
			scope.ShopID, tyre, tenths, scope.UserID, measurementID, reason).Scan(&id)
	})
	return id, err
}

// treadReadingsTx reads every measurement of the tyres, oldest first, with the
// ones that were corrected left in so the history is whole.
func treadReadingsTx(ctx context.Context, tx pgx.Tx, tyreIDs []string) (map[string][]TreadReading, error) {
	out := map[string][]TreadReading{}
	if len(tyreIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT m.tyre_id, m.id, m.tenths, coalesce(m.note, ''), m.measured_at, coalesce(pe.display_name, ''), coalesce(m.corrects_id::text, ''), coalesce(m.reason, '')
		FROM tyre_measurements m
		LEFT JOIN users u ON u.id = m.measured_by LEFT JOIN people pe ON pe.id = u.person_id
		WHERE m.tyre_id = ANY($1::uuid[]) ORDER BY m.measured_at, m.entered_at, m.id`, tyreIDs)
	if err != nil {
		return nil, fmt.Errorf("read the measurements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tyre string
		var r TreadReading
		if err := rows.Scan(&tyre, &r.ID, &r.Tenths, &r.Note, &r.At, &r.By, &r.CorrectsID, &r.Reason); err != nil {
			return nil, err
		}
		out[tyre] = append(out[tyre], r)
	}
	return out, rows.Err()
}

// standing is the measurement nothing corrects: the newest of those that have
// not been corrected.
func standing(rs []TreadReading) *TreadReading {
	corrected := map[string]bool{}
	for _, r := range rs {
		if r.CorrectsID != "" {
			corrected[r.CorrectsID] = true
		}
	}
	for i := len(rs) - 1; i >= 0; i-- {
		if !corrected[rs[i].ID] {
			return &rs[i]
		}
	}
	return nil
}
