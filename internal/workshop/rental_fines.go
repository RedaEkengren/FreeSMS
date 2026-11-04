package workshop

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Congestion tax, parking fees and fines, and tolls (#198). They arrive
// at the shop as the registered owner; passing one to a renter is the
// agreement's, never the law's. Which rental had the vehicle at the
// moment is read from the registration valid then and the handovers'
// actual times -- not the times booked -- and a match near a handover, or
// with more than one rental, is never made for a person: they choose.
// Confirmed to a rental, it becomes a return charge (#195), which is
// invoiced only once the renter has been told of it.
//
// Not built here: an administration fee, which the agreement must state
// first (#193); applying a deposit to a fine, which #196 does for damage
// alone; and naming the driver to an authority, which waits on the
// identity records (#153).

// FineKinds are the kinds of charge that arrive for a vehicle.
var FineKinds = []string{"congestion_tax", "parking_control_fee", "parking_fine", "toll", "other"}

// fineMargin is how near a handover a moment is too near to say which side
// of it the vehicle was: a person decides.
const fineMargin = 15 * time.Minute

// What matching says of a fine.
const (
	FineMatched     = "matched"
	FineOwn         = "own"
	FineAmbiguous   = "ambiguous"
	FineUnknownUnit = "unknown_vehicle"
)

// NewFine is a charge as it arrived.
type NewFine struct {
	Kind         string
	Registration string
	OccurredAt   time.Time
	AmountMinor  int64
	Issuer       string
	Reference    string
	DueOn        *time.Time
}

// Fine is a charge with what matching says and what was decided.
type Fine struct {
	ID           string
	Kind         string
	Registration string
	OccurredAt   time.Time
	AmountMinor  int64
	Issuer       string
	Reference    string
	DueOn        *time.Time
	// What matching says now, and the rentals it could be, with what a
	// person calls each.
	Match         string
	Candidates    []string
	CandidateRefs []string
	// The decision standing: "renter" with its booking and charge, or
	// "shop"; empty while nobody has decided.
	Decision  string
	BookingID string
	ChargeID  string
	Paid      bool
}

var (
	errFine          = fmt.Errorf("%w: a charge needs its kind, the registration, when, an amount above nothing, who issued it and their reference", ErrInvalid)
	errFineNotRental = fmt.Errorf("%w: that rental did not have the vehicle then; choose one that did", ErrInvalid)
	errFineDecided   = fmt.Errorf("%w: that charge is decided already", ErrInvalid)
	errFinePaid      = fmt.Errorf("%w: that charge is recorded as paid already", ErrInvalid)
)

func checkFine(f NewFine) (NewFine, error) {
	f.Registration, f.Issuer, f.Reference = strings.TrimSpace(f.Registration), strings.TrimSpace(f.Issuer), strings.TrimSpace(f.Reference)
	norm := NormaliseRegistration(f.Registration)
	switch {
	case !slices.Contains(FineKinds, f.Kind), norm == "", utf8.RuneCountInString(f.Registration) > 20, len(norm) > 20,
		f.OccurredAt.IsZero(), f.AmountMinor <= 0, f.AmountMinor > 10_000_000_000,
		f.Issuer == "", utf8.RuneCountInString(f.Issuer) > 100, f.Reference == "", utf8.RuneCountInString(f.Reference) > 100:
		return f, errFine
	}
	return f, nil
}

// RecordFines records charges as they arrived, typed or imported; one
// already recorded -- the same issuer's reference -- is not recorded
// again. Returns how many were new. For the rental desk.
func RecordFines(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, source string, fines []NewFine) (int, error) {
	if !scope.Role.HandsOverRentals() {
		return 0, access.ErrForbidden
	}
	if source != "typed" && source != "imported" {
		return 0, errFine
	}
	for i, f := range fines {
		var err error
		if fines[i], err = checkFine(f); err != nil {
			return 0, fmt.Errorf("%w (line %d)", err, i+1)
		}
	}
	created := 0
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, f := range fines {
			tag, err := tx.Exec(ctx, `
				INSERT INTO vehicle_fines (shop_id, kind, registration, normalised, occurred_at, amount_minor, issuer, reference, due_on, source, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::date, $10, NULLIF($11, '')::uuid)
				ON CONFLICT (shop_id, issuer, reference) DO NOTHING`,
				scope.ShopID, f.Kind, f.Registration, NormaliseRegistration(f.Registration), f.OccurredAt, f.AmountMinor, f.Issuer, f.Reference,
				f.DueOn, source, scope.UserID)
			if err != nil {
				return fmt.Errorf("record the charge: %w", err)
			}
			created += int(tag.RowsAffected())
		}
		return nil
	})
	return created, err
}

// ParseFinesCSV reads charges from a file: kind, registration, when (date
// and time in the shop's time zone, or RFC 3339), amount in kronor,
// issuer, reference and due date, with a header line. Every line is read
// before any is recorded; a line that does not read is an error naming it.
func ParseFinesCSV(r io.Reader, loc *time.Location) ([]NewFine, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%w: the file is not comma-separated text: %v", ErrInvalid, err)
	}
	var out []NewFine
	for i, rec := range records {
		if i == 0 && len(rec) > 0 && strings.EqualFold(strings.TrimSpace(rec[0]), "kind") {
			continue
		}
		if len(rec) < 6 {
			return nil, fmt.Errorf("%w: line %d has %d fields, not 6 or 7", ErrInvalid, i+1, len(rec))
		}
		f := NewFine{Kind: strings.TrimSpace(rec[0]), Registration: rec[1], Issuer: rec[4], Reference: rec[5]}
		if f.OccurredAt, err = parseMoment(strings.TrimSpace(rec[2]), loc); errors.Is(err, errTwice) {
			return nil, fmt.Errorf("%w: line %d: that time happens twice the night the clocks go back; give it with its offset, +02:00 or +01:00", ErrInvalid, i+1)
		} else if err != nil {
			return nil, fmt.Errorf("%w: line %d: the time does not read", ErrInvalid, i+1)
		}
		if f.AmountMinor, err = parseKronor(rec[3]); err != nil {
			return nil, fmt.Errorf("%w: line %d: the amount does not read", ErrInvalid, i+1)
		}
		if len(rec) > 6 && strings.TrimSpace(rec[6]) != "" {
			due, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(rec[6]), loc)
			if err != nil {
				return nil, fmt.Errorf("%w: line %d: the due date does not read", ErrInvalid, i+1)
			}
			f.DueOn = &due
		}
		if f, err = checkFine(f); err != nil {
			return nil, fmt.Errorf("%w (line %d)", err, i+1)
		}
		out = append(out, f)
	}
	return out, nil
}

// parseMoment reads a moment: RFC 3339, or a date and time on the shop's
// clock. A time on the clock that happens twice -- the hour repeated when
// summer time ends -- is refused: which of the two decides which rental
// had the car, and that is not guessed. The file gives it with its offset.
func parseMoment(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
		t, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		for _, d := range []time.Duration{-time.Hour, time.Hour} {
			if o := t.Add(d).In(loc); o.Format(layout) == t.Format(layout) {
				return time.Time{}, errTwice
			}
		}
		return t, nil
	}
	return time.Time{}, errors.New("no layout")
}

var errTwice = errors.New("twice")

// parseKronor reads an amount in kronor, with öre after a comma or a
// point, as minor units. Never a float.
func parseKronor(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	whole, frac, has := strings.Cut(strings.Replace(s, ",", ".", 1), ".")
	k, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || k < 0 {
		return 0, errors.New("not kronor")
	}
	var ore int64
	if has {
		if len(frac) == 0 || len(frac) > 2 {
			return 0, errors.New("not öre")
		}
		if len(frac) == 1 {
			frac += "0"
		}
		if ore, err = strconv.ParseInt(frac, 10, 64); err != nil || ore < 0 {
			return 0, errors.New("not öre")
		}
	}
	return k*100 + ore, nil
}

// matchFineTx says which rental had the vehicle a registration was on at a
// moment: the vehicle from the registration valid then, the rentals from
// their handovers. Near a handover, or more than one: ambiguous.
func matchFineTx(ctx context.Context, tx pgx.Tx, normalised string, at time.Time) (string, []string, []string, error) {
	var vehicle string
	err := tx.QueryRow(ctx, `
		SELECT vehicle_id::text FROM vehicle_registrations
		WHERE normalised = $1 AND valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)`, normalised, at).Scan(&vehicle)
	if errors.Is(err, pgx.ErrNoRows) {
		return FineUnknownUnit, nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, fmt.Errorf("read the registration: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT b.id::text, 'R-' || b.number, o.at, i.at
		FROM rental_bookings b
		JOIN rental_units u ON u.id = b.unit_id
		JOIN resources r ON r.id = u.resource_id
		JOIN rental_handovers o ON o.booking_id = b.id AND o.kind = 'check_out'
		LEFT JOIN rental_handovers i ON i.booking_id = b.id AND i.kind = 'check_in'
		WHERE r.vehicle_id = $1 AND o.at - $3::interval <= $2 AND (i.at IS NULL OR i.at + $3::interval > $2)
		ORDER BY o.at`, vehicle, at, fmt.Sprintf("%d minutes", int(fineMargin/time.Minute)))
	if err != nil {
		return "", nil, nil, fmt.Errorf("read the rentals: %w", err)
	}
	defer rows.Close()
	var ids, refs []string
	sure := 0
	for rows.Next() {
		var id, ref string
		var out time.Time
		var in *time.Time
		if err := rows.Scan(&id, &ref, &out, &in); err != nil {
			return "", nil, nil, err
		}
		ids, refs = append(ids, id), append(refs, ref)
		if !at.Before(out.Add(fineMargin)) && (in == nil || at.Before(in.Add(-fineMargin))) {
			sure++
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, nil, err
	}
	switch {
	case len(ids) == 0:
		return FineOwn, nil, nil, nil
	case len(ids) == 1 && sure == 1:
		return FineMatched, ids, refs, nil
	}
	return FineAmbiguous, ids, refs, nil
}

// fineTx reads a fine, what matching says of it now, and what was decided.
func fineTx(ctx context.Context, tx pgx.Tx, id string) (Fine, error) {
	var f Fine
	var normalised string
	err := tx.QueryRow(ctx, `
		SELECT f.id::text, f.kind, f.registration, f.normalised, f.occurred_at, f.amount_minor, f.issuer, f.reference, f.due_on,
		       coalesce(d.kind, ''), coalesce(d.booking_id::text, ''), coalesce(d.charge_id::text, ''),
		       EXISTS (SELECT 1 FROM vehicle_fine_events p WHERE p.fine_id = f.id AND p.kind = 'paid')
		FROM vehicle_fines f
		LEFT JOIN LATERAL (SELECT e.kind, e.booking_id, e.charge_id FROM vehicle_fine_events e
		                   WHERE e.fine_id = f.id AND e.kind IN ('renter', 'shop') ORDER BY e.seq DESC LIMIT 1) d ON true
		WHERE f.id = $1`, id).Scan(&f.ID, &f.Kind, &f.Registration, &normalised, &f.OccurredAt, &f.AmountMinor, &f.Issuer, &f.Reference,
		&f.DueOn, &f.Decision, &f.BookingID, &f.ChargeID, &f.Paid)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, fmt.Errorf("read the charge: %w", err)
	}
	f.Match, f.Candidates, f.CandidateRefs, err = matchFineTx(ctx, tx, normalised, f.OccurredAt)
	return f, err
}

// Fines lists the charges, newest first, each with what matching says.
// The rental desk; reading is Core's.
func Fines(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Fine, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []Fine
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM vehicle_fines ORDER BY occurred_at DESC, id LIMIT 500`)
		if err != nil {
			return fmt.Errorf("read the charges: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			f, err := fineTx(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// lockFineTx takes a fine's lock: one decision at a time.
func lockFineTx(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('vehicle_fine:' || $1::text, 0))`, id)
	return err
}

// PassFineToRenter confirms that a rental had the vehicle when the charge
// arose -- one of those matching found, which a person chooses when it is
// ambiguous -- and makes the charge a return charge of it (#195), at what
// the authority charged. The renter is then told of it before it is
// invoiced.
func PassFineToRenter(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fineID, bookingID string) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(fineID) || !looksLikeUUID(bookingID) {
		return "", ErrNotFound
	}
	var charge string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockFineTx(ctx, tx, fineID); err != nil {
			return err
		}
		f, err := fineTx(ctx, tx, fineID)
		if err != nil {
			return err
		}
		if f.Decision != "" {
			return errFineDecided
		}
		if !slices.ContainsFunc(f.Candidates, func(c string) bool { return strings.EqualFold(c, bookingID) }) {
			return errFineNotRental
		}
		kind := ChargeParkingTolls
		if f.Kind == "congestion_tax" {
			kind = ChargeCongestionTax
		}
		// The shop's words go on the invoice, so the line says only what
		// the issuer said: who, their reference, the plate and when.
		desc := fmt.Sprintf("%s %s, %s, %s", f.Issuer, f.Reference, f.Registration, f.OccurredAt.UTC().Format("2006-01-02 15:04 UTC"))
		if utf8.RuneCountInString(desc) > 200 {
			desc = string([]rune(desc)[:200])
		}
		if charge, err = confirmChargeTx(ctx, tx, scope, bookingID, NewCharge{Kind: kind, Description: desc, QuantityMilli: 1000,
			UnitPriceMinor: f.AmountMinor}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO vehicle_fine_events (shop_id, fine_id, kind, booking_id, charge_id, by_user)
			VALUES ($1, $2, 'renter', $3, $4, NULLIF($5, '')::uuid)`, scope.ShopID, fineID, bookingID, charge, scope.UserID)
		return err
	})
	return charge, err
}

// KeepFine records a charge as the shop's own: no rental had the vehicle,
// or the shop bears it, with why.
func KeepFine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fineID, reason string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	return fineEvent(ctx, pool, scope, fineID, "shop", reason)
}

// FinePaid records that the shop paid the authority. A reminder of what
// is due, not accounts payable: it says nothing of whether the renter has
// paid the shop.
func FinePaid(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fineID string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	return fineEvent(ctx, pool, scope, fineID, "paid", "")
}

// fineEvent records the shop's own decision, or the payment. Whatever is
// switched off: a charge the shop owes is paid after the module is gone.
func fineEvent(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fineID, kind, reason string) error {
	if !looksLikeUUID(fineID) {
		return ErrNotFound
	}
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > 300 {
		return errFine
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockFineTx(ctx, tx, fineID); err != nil {
			return err
		}
		f, err := fineTx(ctx, tx, fineID)
		if err != nil {
			return err
		}
		switch {
		case kind == "shop" && f.Decision != "":
			return errFineDecided
		case kind == "paid" && f.Paid:
			return errFinePaid
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO vehicle_fine_events (shop_id, fine_id, kind, reason, by_user) VALUES ($1, $2, $3, nullif($4, ''), NULLIF($5, '')::uuid)`,
			scope.ShopID, fineID, kind, reason, scope.UserID)
		return err
	})
}

// PreviewFines is what matching would say of charges not recorded yet:
// the import's preview, before anything is saved.
func PreviewFines(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fines []NewFine) ([]Fine, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	out := make([]Fine, 0, len(fines))
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, n := range fines {
			f := Fine{Kind: n.Kind, Registration: n.Registration, OccurredAt: n.OccurredAt, AmountMinor: n.AmountMinor,
				Issuer: n.Issuer, Reference: n.Reference, DueOn: n.DueOn}
			var err error
			if f.Match, f.Candidates, f.CandidateRefs, err = matchFineTx(ctx, tx, NormaliseRegistration(n.Registration), n.OccurredAt); err != nil {
				return err
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}
