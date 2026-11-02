package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// What a booking lacks before its unit may be handed over, asked in one
// statement for any number of bookings: the counter asks it for one, the
// rental desk's list for every booking ahead, an extension for the
// longer period. One implementation, so the list and the counter never
// disagree.
//
// bookingGapsCTEs follows a CTE asked(id, starts_at, ends_at) -- the
// bookings, and the period each is asked for -- and defines
// gaps(booking_id, ord, first, added, code, who): one row a gap, in the
// order the page lists them. Under the booking's own profile version:
//   - the permit standing on every day of the period, in the shop's zone;
//   - reported into rental use, where the profile requires it;
//   - insured for rental use on every day, where it requires that;
//   - each driver's newest licence check passed and lasting to the end;
//   - a driver named at all.
//
// A report or insurance let go for exactly this period does not count.
const bookingGapsCTEs = `
	g_zone AS (SELECT timezone FROM shops),
	g_booking AS (
	    SELECT q.id, q.starts_at, q.ends_at, v.permit_required, v.reporting_required, v.insurance_required, v.licence_check_required,
	           (q.starts_at AT TIME ZONE z.timezone)::date AS first_day,
	           ((q.ends_at - interval '1 microsecond') AT TIME ZONE z.timezone)::date AS last_day,
	           (q.ends_at AT TIME ZONE z.timezone)::date AS end_day,
	           (SELECT m.id FROM fleet_memberships m WHERE m.unit_id = b.unit_id AND m.joined_at <= greatest(q.starts_at, clock_timestamp())
	             ORDER BY m.joined_at DESC LIMIT 1) AS membership
	    FROM asked q
	    JOIN rental_bookings b ON b.id = q.id
	    JOIN rental_profile_versions v ON v.id = b.profile_version_id
	    CROSS JOIN g_zone z),
	gaps AS (
	    SELECT a.id AS booking_id, 1 AS ord, false AS first, NULL::timestamptz AS added, 'permit' AS code, '' AS who
	    FROM g_booking a
	    WHERE a.permit_required AND EXISTS (
	        SELECT 1 FROM generate_series(a.first_day, a.last_day, interval '1 day') d
	        WHERE NOT EXISTS (SELECT 1 FROM rental_permits p LEFT JOIN rental_permit_revocations x ON x.permit_id = p.id
	                          WHERE p.decided_on <= d::date AND (p.expires_on IS NULL OR d::date <= p.expires_on)
	                            AND (x.revoked_on IS NULL OR d::date < x.revoked_on)))
	    UNION ALL
	    SELECT a.id, 2, false, NULL, 'not_reported', ''
	    FROM g_booking a
	    WHERE a.reporting_required
	      AND (SELECT f.direction FROM fleet_rental_reports f WHERE f.membership_id = a.membership
	            ORDER BY f.reported_on DESC, f.recorded_at DESC LIMIT 1) IS DISTINCT FROM 'in'
	      AND NOT EXISTS (SELECT 1 FROM rental_gap_overrides o WHERE o.booking_id = a.id AND o.code = 'not_reported'
	                        AND o.starts_at = a.starts_at AND o.ends_at = a.ends_at)
	    UNION ALL
	    SELECT a.id, 3, false, NULL, 'insurance', ''
	    FROM g_booking a
	    WHERE a.insurance_required
	      AND EXISTS (SELECT 1 FROM generate_series(a.first_day, a.last_day, interval '1 day') d
	                  WHERE NOT EXISTS (SELECT 1 FROM fleet_insurances i WHERE i.membership_id = a.membership AND i.covers_rental = 'yes'
	                                    AND i.valid_from <= d::date AND (i.valid_to IS NULL OR d::date <= i.valid_to)))
	      AND NOT EXISTS (SELECT 1 FROM rental_gap_overrides o WHERE o.booking_id = a.id AND o.code = 'insurance'
	                        AND o.starts_at = a.starts_at AND o.ends_at = a.ends_at)
	    UNION ALL
	    SELECT a.id, 4, NOT dr.main, dr.added_at, 'licence',
	           coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name, '')
	    FROM g_booking a
	    JOIN rental_drivers dr ON dr.booking_id = a.id AND dr.removed_at IS NULL
	    JOIN parties pa ON pa.id = dr.party_id
	    LEFT JOIN customers c ON c.id = pa.customer_id
	    LEFT JOIN people cp ON cp.id = c.person_id
	    LEFT JOIN people pp ON pp.id = pa.person_id
	    LEFT JOIN LATERAL (SELECT k.passed, k.valid_until FROM rental_licence_checks k WHERE k.driver_id = dr.id
	                       ORDER BY k.checked_at DESC LIMIT 1) k ON true
	    WHERE a.licence_check_required AND (k.passed IS NOT TRUE OR k.valid_until IS NULL OR k.valid_until < a.end_day)
	    UNION ALL
	    SELECT a.id, 5, false, NULL, 'no_driver', ''
	    FROM g_booking a
	    WHERE a.licence_check_required
	      AND NOT EXISTS (SELECT 1 FROM rental_drivers dr WHERE dr.booking_id = a.id AND dr.removed_at IS NULL))`

// gapSentences are what each gap says, as catalogue keys.
var gapSentences = map[string]HandoverGap{
	GapPermit:      {Code: GapPermit, Sentence: "There is no rental permit that stands for the whole rental."},
	GapNotReported: {Code: GapNotReported, Overridable: true, Sentence: "The unit has not been reported into rental use in the vehicle register."},
	GapInsurance:   {Code: GapInsurance, Overridable: true, Sentence: "No insurance covering rental use is recorded for every day of the rental."},
	GapLicence:     {Code: GapLicence, Sentence: "%s has not shown a valid licence for this rental."},
	GapNoDriver:    {Code: GapNoDriver, Sentence: "No driver is named for the rental."},
}

// bookingGapsTx asks what each booking lacks for the period given with
// it, by booking.
func bookingGapsTx(ctx context.Context, tx pgx.Tx, ids []string, starts, ends []time.Time) (map[string][]HandoverGap, error) {
	rows, err := tx.Query(ctx, `
		WITH asked AS (SELECT * FROM unnest($1::uuid[], $2::timestamptz[], $3::timestamptz[]) AS q(id, starts_at, ends_at)),
		`+bookingGapsCTEs+`
		SELECT booking_id::text, code, who FROM gaps ORDER BY booking_id, ord, first, added`, ids, starts, ends)
	if err != nil {
		return nil, fmt.Errorf("read what is missing: %w", err)
	}
	defer rows.Close()
	out := map[string][]HandoverGap{}
	for rows.Next() {
		var id, code, who string
		if err := rows.Scan(&id, &code, &who); err != nil {
			return nil, err
		}
		g := gapSentences[code]
		g.Who = who
		out[id] = append(out[id], g)
	}
	return out, rows.Err()
}

// rentalBookingGapsTx is what a booking lacks before handover, for the
// period it is booked for, all at once.
func rentalBookingGapsTx(ctx context.Context, tx pgx.Tx, bookingID string) ([]HandoverGap, error) {
	var starts, ends time.Time
	if err := tx.QueryRow(ctx, `SELECT starts_at, ends_at FROM rental_bookings WHERE id = $1`, bookingID).Scan(&starts, &ends); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read booking: %w", err)
	}
	all, err := bookingGapsTx(ctx, tx, []string{bookingID}, []time.Time{starts}, []time.Time{ends})
	return all[bookingID], err
}
