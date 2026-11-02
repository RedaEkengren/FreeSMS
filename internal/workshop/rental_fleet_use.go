package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// A fleet vehicle's regulatory lifecycle (#202): reported into rental use
// in the vehicle register and out of it again, and insured for rental.
// Each fact is a row with its date on the period in the fleet it is
// about. A check-out asks for them with the permit and the drivers
// (rentalBookingGapsTx): the permit and the licence check are never let
// go; reporting and insurance may be, by whoever runs the shop, with a
// reason.

// The gaps this adds.
const (
	GapNotReported = "not_reported"
	GapInsurance   = "insurance"
)

// Directions a report goes in.
const (
	ReportedIn  = "in"
	ReportedOut = "out"
)

// What an insurance says of rental use.
var InsuranceCovers = []string{"yes", "no", "unknown"}

// ExpiryWarning is how long before a permit or an insurance ends it is on
// the rental desk's list. A default until the shop can set it.
const ExpiryWarning = 30 * 24 * time.Hour

var (
	errReportDirection = fmt.Errorf("%w: a report is into rental use or out of it", ErrInvalid)
	errReportWhen      = fmt.Errorf("%w: say on which day it was reported", ErrInvalid)
	errReportReference = fmt.Errorf("%w: keep the reference to 200 characters", ErrInvalid)
	errInsuranceWho    = fmt.Errorf("%w: give the insurer and the policy number", ErrInvalid)
	errInsuranceDates  = fmt.Errorf("%w: an insurance runs from a day, and to the same day or later", ErrInvalid)
	errInsuranceCovers = fmt.Errorf("%w: say whether the insurance covers rental use", ErrInvalid)
	errOverrideGap     = fmt.Errorf("%w: only a missing report or insurance can be let go, and only while it is missing", ErrInvalid)
	errGapOverrideWhy  = fmt.Errorf("%w: say why it is let go, in 300 characters or fewer", ErrInvalid)
)

// FleetReport is one report into or out of rental use.
type FleetReport struct {
	ID         string
	Direction  string
	ReportedOn time.Time
	Reference  string
	RecordedBy string
	RecordedAt time.Time
}

// FleetInsurance is one insurance of a period in the fleet.
type FleetInsurance struct {
	ID           string
	Insurer      string
	PolicyRef    string
	ValidFrom    time.Time
	ValidTo      *time.Time
	CoversRental string
	RecordedBy   string
	RecordedAt   time.Time
}

// ValidOn reports whether it covers rental use on day.
func (i FleetInsurance) ValidOn(day time.Time) bool {
	d := dateOnly(day)
	return i.CoversRental == "yes" && !d.Before(dateOnly(i.ValidFrom)) && (i.ValidTo == nil || !d.After(dateOnly(*i.ValidTo)))
}

// FleetUse is what a period in the fleet has on record.
type FleetUse struct {
	MembershipID string
	Reports      []FleetReport
	Insurances   []FleetInsurance
}

// Reported reports whether its last report put it into rental use.
func (u FleetUse) Reported() bool {
	return len(u.Reports) > 0 && u.Reports[len(u.Reports)-1].Direction == ReportedIn
}

// FleetRegister is where a report into or out of rental use is made. The
// vehicle register has no open API -- an approved intermediary is the way
// in -- so the only one there is is the manual one: it says what to do,
// and a person records what they did.
type FleetRegister interface {
	// Instructions is what to do, as a catalogue key, and where.
	Instructions(direction string) (sentence, link string)
}

// ManualFleetRegister is the person doing it at Transportstyrelsen.
type ManualFleetRegister struct{}

// Instructions says where to report it.
func (ManualFleetRegister) Instructions(direction string) (string, string) {
	if direction == ReportedOut {
		return "Report the vehicle out of rental use in Transportstyrelsen's e-service for vehicles in commercial traffic, then record it here.",
			"https://www.transportstyrelsen.se/sv/vagtrafik/yrkestrafik/"
	}
	return "Report the vehicle into rental use in Transportstyrelsen's e-service for vehicles in commercial traffic, then record it here.",
		"https://www.transportstyrelsen.se/sv/vagtrafik/yrkestrafik/"
}

// RecordFleetReport records that a period in the fleet was reported into
// or out of rental use, on a day. The rental desk and whoever runs the shop.
func RecordFleetReport(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, membershipID, direction string, on time.Time, reference string) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(membershipID) {
		return "", ErrNotFound
	}
	reference = strings.TrimSpace(reference)
	switch {
	case direction != ReportedIn && direction != ReportedOut:
		return "", errReportDirection
	case on.IsZero():
		return "", errReportWhen
	case utf8.RuneCountInString(reference) > 200:
		return "", errReportReference
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO fleet_rental_reports (shop_id, membership_id, direction, reported_on, reference, recorded_by)
			SELECT $1, m.id, $3, $4, nullif($5, ''), NULLIF($6, '')::uuid FROM fleet_memberships m WHERE m.id = $2
			RETURNING id`, scope.ShopID, membershipID, direction, on.Format(time.DateOnly), reference, scope.UserID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return id, err
}

// RecordFleetInsurance records an insurance of a period in the fleet. A
// renewal is a row of its own. The rental desk and whoever runs the shop.
func RecordFleetInsurance(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, membershipID string, n FleetInsurance) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(membershipID) {
		return "", ErrNotFound
	}
	n.Insurer, n.PolicyRef = strings.TrimSpace(n.Insurer), strings.TrimSpace(n.PolicyRef)
	switch {
	case n.Insurer == "" || n.PolicyRef == "" || utf8.RuneCountInString(n.Insurer) > 120 || utf8.RuneCountInString(n.PolicyRef) > 100:
		return "", errInsuranceWho
	case n.ValidFrom.IsZero() || (n.ValidTo != nil && dateOnly(*n.ValidTo).Before(dateOnly(n.ValidFrom))):
		return "", errInsuranceDates
	case !slices.Contains(InsuranceCovers, n.CoversRental):
		return "", errInsuranceCovers
	}
	var to *string
	if n.ValidTo != nil {
		d := n.ValidTo.Format(time.DateOnly)
		to = &d
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO fleet_insurances (shop_id, membership_id, insurer, policy_ref, valid_from, valid_to, covers_rental, recorded_by)
			SELECT $1, m.id, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid FROM fleet_memberships m WHERE m.id = $2
			RETURNING id`, scope.ShopID, membershipID, n.Insurer, n.PolicyRef, n.ValidFrom.Format(time.DateOnly), to, n.CoversRental, scope.UserID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return id, err
}

// FleetUseOf reads what a period in the fleet has on record, oldest first.
func FleetUseOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, membershipID string) (FleetUse, error) {
	if !scope.Role.HandsOverRentals() {
		return FleetUse{}, access.ErrForbidden
	}
	if !looksLikeUUID(membershipID) {
		return FleetUse{}, ErrNotFound
	}
	var u FleetUse
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = fleetUseTx(ctx, tx, membershipID)
		return err
	})
	return u, err
}

func fleetUseTx(ctx context.Context, tx pgx.Tx, membershipID string) (FleetUse, error) {
	u := FleetUse{MembershipID: membershipID}
	rows, err := tx.Query(ctx, `
		SELECT f.id::text, f.direction, f.reported_on, coalesce(f.reference, ''), coalesce(p.display_name, ''), f.recorded_at
		FROM fleet_rental_reports f LEFT JOIN users us ON us.id = f.recorded_by LEFT JOIN people p ON p.id = us.person_id
		WHERE f.membership_id = $1 ORDER BY f.reported_on, f.recorded_at, f.id`, membershipID)
	if err != nil {
		return u, fmt.Errorf("read the reports: %w", err)
	}
	for rows.Next() {
		var r FleetReport
		if err := rows.Scan(&r.ID, &r.Direction, &r.ReportedOn, &r.Reference, &r.RecordedBy, &r.RecordedAt); err != nil {
			rows.Close()
			return u, err
		}
		u.Reports = append(u.Reports, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return u, err
	}
	rows, err = tx.Query(ctx, `
		SELECT f.id::text, f.insurer, f.policy_ref, f.valid_from, f.valid_to, f.covers_rental, coalesce(p.display_name, ''), f.recorded_at
		FROM fleet_insurances f LEFT JOIN users us ON us.id = f.recorded_by LEFT JOIN people p ON p.id = us.person_id
		WHERE f.membership_id = $1 ORDER BY f.valid_from, f.recorded_at, f.id`, membershipID)
	if err != nil {
		return u, fmt.Errorf("read the insurances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var i FleetInsurance
		if err := rows.Scan(&i.ID, &i.Insurer, &i.PolicyRef, &i.ValidFrom, &i.ValidTo, &i.CoversRental, &i.RecordedBy, &i.RecordedAt); err != nil {
			return u, err
		}
		u.Insurances = append(u.Insurances, i)
	}
	return u, rows.Err()
}

// insuranceCovers reports whether the insurances between them cover rental
// use on every day from first to last.
func insuranceCovers(ins []FleetInsurance, first, last time.Time) bool {
	for d := dateOnly(first); !d.After(dateOnly(last)); d = d.AddDate(0, 0, 1) {
		if !slices.ContainsFunc(ins, func(i FleetInsurance) bool { return i.ValidOn(d) }) {
			return false
		}
	}
	return true
}

// fleetUseGapsTx is what the booking's unit lacks for its period in the
// fleet: a report into rental use where the profile requires one, and for
// a vehicle an insurance covering rental use on every day of the rental.
// Gaps a person let go are left out.
func fleetUseGapsTx(ctx context.Context, tx pgx.Tx, bookingID string, p RentalProfile, starts, ends time.Time) ([]HandoverGap, error) {
	var membership *string
	var vehicle bool
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT m.id::text FROM fleet_memberships m WHERE m.unit_id = b.unit_id AND m.joined_at <= greatest(b.starts_at, clock_timestamp())
		          ORDER BY m.joined_at DESC LIMIT 1),
		       r.vehicle_id IS NOT NULL
		FROM rental_bookings b JOIN rental_units u ON u.id = b.unit_id JOIN resources r ON r.id = u.resource_id
		WHERE b.id = $1`, bookingID).Scan(&membership, &vehicle); err != nil {
		return nil, fmt.Errorf("read the unit: %w", err)
	}
	var use FleetUse
	if membership != nil {
		var err error
		if use, err = fleetUseTx(ctx, tx, *membership); err != nil {
			return nil, err
		}
	}
	loc, err := shopLocationTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	var gaps []HandoverGap
	if p.Rules.ReportingRequired && !use.Reported() {
		gaps = append(gaps, HandoverGap{Code: GapNotReported, Overridable: true,
			Sentence: "The unit has not been reported into rental use in the vehicle register."})
	}
	if vehicle && !insuranceCovers(use.Insurances, starts.In(loc), ends.Add(-time.Nanosecond).In(loc)) {
		gaps = append(gaps, HandoverGap{Code: GapInsurance, Overridable: true,
			Sentence: "No insurance covering rental use is recorded for every day of the rental."})
	}
	if len(gaps) == 0 {
		return nil, nil
	}
	let, err := gapOverridesTx(ctx, tx, bookingID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(gaps, func(g HandoverGap) bool {
		return slices.ContainsFunc(let, func(o GapOverride) bool { return o.Code == g.Code })
	}), nil
}

// GapOverride is a handover gap let go, by whom and why.
type GapOverride struct {
	Code   string
	Reason string
	By     string
	At     time.Time
}

func gapOverridesTx(ctx context.Context, tx pgx.Tx, bookingID string) ([]GapOverride, error) {
	rows, err := tx.Query(ctx, `
		SELECT o.code, o.reason, coalesce(p.display_name, ''), o.at
		FROM rental_gap_overrides o LEFT JOIN users us ON us.id = o.by_user LEFT JOIN people p ON p.id = us.person_id
		WHERE o.booking_id = $1 ORDER BY o.at`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("read what was let go: %w", err)
	}
	defer rows.Close()
	var out []GapOverride
	for rows.Next() {
		var o GapOverride
		if err := rows.Scan(&o.Code, &o.Reason, &o.By, &o.At); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RentalGapOverrides lists the gaps let go on a booking.
func RentalGapOverrides(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) ([]GapOverride, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return nil, ErrNotFound
	}
	var out []GapOverride
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = gapOverridesTx(ctx, tx, bookingID)
		return err
	})
	return out, err
}

// OverrideHandoverGap lets a missing report or insurance go for one
// booking, with the reason: the handover can then go ahead without it. The
// permit and the licence check are never let go. Whoever runs the shop.
func OverrideHandoverGap(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, code, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, errGapOverrideWhy)
	if err != nil {
		return err
	}
	if code != GapNotReported && code != GapInsurance {
		return errOverrideGap
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := lockRentalTx(ctx, tx, bookingID); err != nil {
			return err
		}
		gaps, err := rentalBookingGapsTx(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(gaps, func(g HandoverGap) bool { return g.Code == code && g.Overridable }) {
			return errOverrideGap
		}
		_, err = tx.Exec(ctx, `INSERT INTO rental_gap_overrides (shop_id, booking_id, code, reason, by_user) VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid)`,
			scope.ShopID, bookingID, code, reason, scope.UserID)
		return err
	})
}

// fleetUseWaitingTx is the rental desk's part of the action queue for the
// fleet's paperwork (#202): a unit in the fleet not reported into rental
// use where its profile asks it, one out of the fleet still reported in,
// an insurance or the permit ending within ExpiryWarning, and a confirmed
// booking within that time that would be refused at the counter today.
// Read from the rows each time, never kept; a task is done when a person
// records what they did.
func fleetUseWaitingTx(ctx context.Context, tx pgx.Tx) ([]ActionItem, error) {
	var out []ActionItem
	// One statement for the units' paperwork and the bookings ahead to
	// ask about: the home page reads this on every visit.
	rows, err := tx.Query(ctx, `
		WITH last AS (
		    SELECT DISTINCT ON (membership_id) membership_id, direction
		    FROM fleet_rental_reports ORDER BY membership_id, reported_on DESC, recorded_at DESC),
		t AS (SELECT (clock_timestamp() AT TIME ZONE timezone)::date AS today FROM shops)
		SELECT CASE WHEN m.left_at IS NULL THEN 'rental_report_in' ELSE 'rental_report_out' END,
		       u.id::text, u.fleet_number || coalesce(' · ' || reg.registration, ' · ' || r.name, ''), coalesce(m.left_at, m.joined_at)
		FROM fleet_memberships m
		JOIN rental_units u ON u.id = m.unit_id
		JOIN resources r ON r.id = u.resource_id
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
		JOIN LATERAL (SELECT reporting_required FROM rental_profile_versions WHERE profile_id = u.profile_id ORDER BY version DESC LIMIT 1) p ON true
		LEFT JOIN last ON last.membership_id = m.id
		WHERE (m.left_at IS NULL AND p.reporting_required AND last.direction IS DISTINCT FROM 'in')
		   OR (m.left_at IS NOT NULL AND last.direction = 'in')
		UNION ALL
		-- An insurance ending soon with none after it, on a vehicle in the fleet.
		SELECT 'rental_insurance_ends', u.id::text, u.fleet_number || coalesce(' · ' || reg.registration, ''), max(i.valid_to)::timestamptz
		FROM fleet_memberships m
		JOIN rental_units u ON u.id = m.unit_id
		JOIN resources r ON r.id = u.resource_id AND r.vehicle_id IS NOT NULL
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
		JOIN fleet_insurances i ON i.membership_id = m.id AND i.covers_rental = 'yes'
		CROSS JOIN t
		WHERE m.left_at IS NULL
		GROUP BY u.id, u.fleet_number, reg.registration, t.today
		HAVING bool_and(i.valid_to IS NOT NULL) AND max(i.valid_to) >= t.today AND max(i.valid_to) < t.today + $1::int
		UNION ALL
		-- A confirmed booking within the warning, to ask what it lacks.
		SELECT 'ahead', b.id::text, u.fleet_number || coalesce(' · ' || reg.registration, ' · ' || r.name, ''), b.starts_at
		FROM rental_bookings b
		JOIN resource_blocks k ON k.id = b.block_id
		JOIN rental_units u ON u.id = b.unit_id
		JOIN resources r ON r.id = u.resource_id
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
		WHERE `+rentalState+` = 'confirmed' AND b.starts_at < clock_timestamp() + $2::interval
		ORDER BY 4, 3`, int(ExpiryWarning/(24*time.Hour)), ExpiryWarning)
	if err != nil {
		return nil, fmt.Errorf("read the fleet's paperwork: %w", err)
	}
	var ids []string
	var labels []string
	var starts []time.Time
	for rows.Next() {
		it := ActionItem{Module: access.Rental, SubjectKind: "rental_unit", Urgency: UrgencySoon}
		if err := rows.Scan(&it.Kind, &it.SubjectID, &it.Text, &it.Due); err != nil {
			rows.Close()
			return nil, err
		}
		if it.Kind == "ahead" {
			ids, labels, starts = append(ids, it.SubjectID), append(labels, it.Text), append(starts, it.Due)
			continue
		}
		out = append(out, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	loc, err := shopLocationTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now().In(loc)
	permits, err := rentalPermitsTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if permitCovers(permits, now, now) && !permitCovers(permits, now, now.Add(ExpiryWarning)) {
		it := ActionItem{Kind: "rental_permit_ends", Module: access.Rental, SubjectKind: "rental_settings", Urgency: UrgencySoon}
		for d := dateOnly(now); ; d = d.AddDate(0, 0, 1) {
			if !permitCovers(permits, d, d) {
				it.Due = d
				break
			}
		}
		out = append(out, it)
	}
	for i, id := range ids {
		gaps, err := rentalBookingGapsTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if len(gaps) > 0 {
			out = append(out, ActionItem{Kind: "rental_gaps", Module: access.Rental, SubjectKind: "rental_booking",
				SubjectID: id, Text: labels[i], Due: starts[i], Urgency: UrgencySoon})
		}
	}
	return out, nil
}
