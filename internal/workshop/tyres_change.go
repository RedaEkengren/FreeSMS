package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// Booking a seasonal change (#230): an ordinary booking on the calendar for one
// customer's stored sets, a short time per set, and a day that holds only so
// many sets.

var (
	errChangeSettings = fmt.Errorf("%w: a set takes from 5 to 240 minutes, a day holds from 1 to 500 sets, and a customer is followed up after from 1 to 90 days, and a set may be left after from 30 to 3650 days", ErrInvalid)
	errChangeSets     = fmt.Errorf("%w: a change is booked for from 1 to 10 sets of one customer", ErrInvalid)
	errChangeBooked   = fmt.Errorf("%w: a set is already booked for a change", ErrInvalid)
	errChangeDayFull  = fmt.Errorf("%w: that day cannot take that many more sets", ErrInvalid)
)

// TyreChangeSettings are how the shop plans change-over days.
type TyreChangeSettings struct {
	MinutesPerSet int
	SetsPerDay    int
	FollowUpDays  int
	// How long a set can sit with no charge, no job and no move before it is on
	// the list of those that may have been left.
	AbandonDays int
}

const (
	defaultChangeMinutes = 30
	defaultChangePerDay  = 12
	defaultFollowUpDays  = 14
	defaultAbandonDays   = 365
)

func changeSettingsTx(ctx context.Context, tx pgx.Tx) (TyreChangeSettings, error) {
	s := TyreChangeSettings{MinutesPerSet: defaultChangeMinutes, SetsPerDay: defaultChangePerDay, FollowUpDays: defaultFollowUpDays, AbandonDays: defaultAbandonDays}
	err := tx.QueryRow(ctx, `SELECT minutes_per_set, sets_per_day, follow_up_days, abandon_days FROM tyre_change_settings ORDER BY changed_at DESC, id LIMIT 1`).
		Scan(&s.MinutesPerSet, &s.SetsPerDay, &s.FollowUpDays, &s.AbandonDays)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return s, err
	}
	return s, nil
}

// SetTyreChangeSettings changes how change-over days are planned. Whoever runs
// the shop.
func SetTyreChangeSettings(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, s TyreChangeSettings) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if s.MinutesPerSet < 5 || s.MinutesPerSet > 240 || s.SetsPerDay < 1 || s.SetsPerDay > 500 || s.FollowUpDays < 1 || s.FollowUpDays > 90 || s.AbandonDays < 30 || s.AbandonDays > 3650 {
		return errChangeSettings
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tyre_change_settings (shop_id, minutes_per_set, sets_per_day, follow_up_days, abandon_days, changed_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid)`, scope.ShopID, s.MinutesPerSet, s.SetsPerDay, s.FollowUpDays, s.AbandonDays, scope.UserID)
		return err
	})
}

// TyreChangeSettingsOf reads how the shop plans them. Whoever deals with
// customers or runs the shop.
func TyreChangeSettingsOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (TyreChangeSettings, error) {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.RunsTheShop() {
		return TyreChangeSettings{}, access.ErrForbidden
	}
	var s TyreChangeSettings
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = changeSettingsTx(ctx, tx)
		return err
	})
	return s, err
}

// ChangeBooking is a change to book.
type ChangeBooking struct {
	// The sets, all of one customer, each in a place.
	SetIDs []string
	Starts time.Time
	// What the calendar says it is for, in the shop's words.
	What          string
	ConfirmedPast bool
}

// dayKeyTx is the local day an instant falls on in the shop's zone.
func dayKeyTx(ctx context.Context, tx pgx.Tx, t time.Time) (string, error) {
	loc, err := shopLocationTx(ctx, tx)
	if err != nil {
		return "", err
	}
	return t.In(loc).Format("2006-01-02"), nil
}

// bookedSetsOnTx counts the sets booked for a change on a local day.
func bookedSetsOnTx(ctx context.Context, tx pgx.Tx, day string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*)::int FROM tyre_change_bookings tb JOIN bookings b ON b.id = tb.booking_id
		WHERE b.status = 'booked' AND (b.starts_at AT TIME ZONE (SELECT timezone FROM shops LIMIT 1))::date = $1::date`, day).Scan(&n)
	return n, err
}

// BookTyreChange books a customer's stored sets for a change. It is a booking
// like any other, as long as the sets take at the shop's time per set, and it
// is refused when the day cannot hold them: two bookings made at once are told
// apart by a lock on the day. Whoever deals with customers.
func BookTyreChange(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, b ChangeBooking) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if len(b.SetIDs) < 1 || len(b.SetIDs) > 10 || b.Starts.IsZero() {
		return "", errChangeSets
	}
	seen := map[string]bool{}
	for _, id := range b.SetIDs {
		if !looksLikeUUID(id) {
			return "", ErrNotFound
		}
		if seen[id] {
			return "", errChangeSets
		}
		seen[id] = true
	}
	var booking string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		set, err := changeSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		day, err := dayKeyTx(ctx, tx, b.Starts)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "tyrechange:"+day); err != nil {
			return err
		}
		var owner, name, phone, reg string
		for i, id := range b.SetIDs {
			var customer, who, tel, plate string
			var stored, booked bool
			err := tx.QueryRow(ctx, `
				SELECT ts.customer_id::text,
				       coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name, ''), coalesce(p.phone, ''),
				       coalesce((SELECT r.registration FROM vehicle_registrations r WHERE r.valid_to IS NULL AND r.vehicle_id =
				                 (SELECT e.vehicle_id FROM tyre_set_vehicle_events e WHERE e.tyre_set_id = ts.id ORDER BY e.seq DESC LIMIT 1)), ''),
				       EXISTS (SELECT 1 FROM tyre_set_placements pl WHERE pl.tyre_set_id = ts.id AND pl.removed_at IS NULL),
				       EXISTS (SELECT 1 FROM tyre_change_bookings tb JOIN bookings k ON k.id = tb.booking_id WHERE tb.tyre_set_id = ts.id AND k.status = 'booked')
				FROM tyre_sets ts JOIN customers c ON c.id = ts.customer_id LEFT JOIN people p ON p.id = c.person_id
				WHERE ts.id = $1`, id).Scan(&customer, &who, &tel, &plate, &stored, &booked)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if i == 0 {
				owner, name, phone, reg = customer, who, tel, plate
			} else if customer != owner {
				return errChangeSets
			}
			if !stored {
				return errNotStored
			}
			if booked {
				return errChangeBooked
			}
		}
		taken, err := bookedSetsOnTx(ctx, tx, day)
		if err != nil {
			return err
		}
		if taken+len(b.SetIDs) > set.SetsPerDay {
			return errChangeDayFull
		}
		ends := b.Starts.Add(time.Duration(len(b.SetIDs)*set.MinutesPerSet) * time.Minute)
		est := len(b.SetIDs) * set.MinutesPerSet
		nb := NewBooking{Starts: b.Starts, Ends: ends, Registration: reg, CustomerName: name, CustomerPhone: phone,
			What: b.What, EstimateMinutes: &est, ConfirmedPast: b.ConfirmedPast}
		if err := checkBooking(&nb); err != nil {
			return err
		}
		booking, err = createBookingTx(ctx, tx, scope, nb, "", "")
		if err != nil {
			return err
		}
		for _, id := range b.SetIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO tyre_change_bookings (shop_id, booking_id, tyre_set_id, by_user) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`,
				scope.ShopID, booking, id, scope.UserID); err != nil {
				return fmt.Errorf("mark the set booked: %w", err)
			}
		}
		return nil
	})
	return booking, err
}

// ChangeDay is one day of the planner's view of the change-over weeks.
type ChangeDay struct {
	Date   time.Time
	Booked int
	Free   int
}

// TyreChangeDays says how full each of the next days is: sets booked for a
// change and room left. Whoever deals with customers or runs the shop.
func TyreChangeDays(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from time.Time, days int) ([]ChangeDay, error) {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	if days < 1 || days > 120 {
		return nil, fmt.Errorf("%w: from 1 to 120 days", ErrInvalid)
	}
	var out []ChangeDay
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		set, err := changeSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		start := from.In(loc)
		first := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
		counts := map[string]int{}
		rows, err := tx.Query(ctx, `
			SELECT (b.starts_at AT TIME ZONE (SELECT timezone FROM shops LIMIT 1))::date::text, count(*)::int
			FROM tyre_change_bookings tb JOIN bookings b ON b.id = tb.booking_id
			WHERE b.status = 'booked' AND b.starts_at >= $1 AND b.starts_at < $2
			GROUP BY 1`, first, first.AddDate(0, 0, days))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			var n int
			if err := rows.Scan(&d, &n); err != nil {
				return err
			}
			counts[d] = n
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := 0; i < days; i++ {
			d := first.AddDate(0, 0, i)
			n := counts[d.Format("2006-01-02")]
			out = append(out, ChangeDay{Date: d, Booked: n, Free: max(set.SetsPerDay-n, 0)})
		}
		return nil
	})
	return out, err
}

// TyreChangeOf says when a set is booked for a change, if it is. Whoever
// works in the workshop: it says nothing about whose it is.
func TyreChangeOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID string) (*time.Time, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(setID) {
		return nil, ErrNotFound
	}
	var at *time.Time
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT min(b.starts_at) FROM tyre_change_bookings tb JOIN bookings b ON b.id = tb.booking_id
			WHERE tb.tyre_set_id = $1 AND b.status = 'booked'`, setID).Scan(&at)
		return err
	})
	return at, err
}

// ChangeFollowUp is a customer who was written to and has not booked.
type ChangeFollowUp struct {
	CustomerID string
	Name       string
	Phone      string
	WrittenAt  time.Time
	Sets       []string
}

// CampaignFollowUp lists the customers a campaign wrote to more than the
// shop's follow-up days ago who still have a set of its season in storage and
// none of them booked, oldest first: the list the front desk rings through.
// Whoever deals with customers.
func CampaignFollowUp(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, campaignID string) ([]ChangeFollowUp, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(campaignID) {
		return nil, ErrNotFound
	}
	var out []ChangeFollowUp
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		set, err := changeSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		var season string
		if err := tx.QueryRow(ctx, `SELECT season FROM tyre_campaigns WHERE id = $1`, campaignID).Scan(&season); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT c.id::text, coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name, ''), coalesce(p.phone, ''), r.at,
			       array_agg(ts.brand || CASE WHEN ts.model <> '' THEN ' ' || ts.model ELSE '' END ORDER BY ts.created_at, ts.id)
			FROM tyre_campaign_recipients r
			JOIN customers c ON c.id = r.customer_id LEFT JOIN people p ON p.id = c.person_id
			JOIN tyre_sets ts ON ts.customer_id = c.id AND ts.season = ANY($2)
			JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
			WHERE r.campaign_id = $1 AND r.at <= clock_timestamp() - make_interval(days => $3)
			  AND NOT EXISTS (SELECT 1 FROM tyre_sets s2
			                  JOIN tyre_change_bookings tb ON tb.tyre_set_id = s2.id JOIN bookings b ON b.id = tb.booking_id
			                  WHERE s2.customer_id = c.id AND s2.season = ANY($2) AND b.status = 'booked')
			GROUP BY c.id, p.display_name, p.phone, r.at ORDER BY r.at, c.id`, campaignID, campaignSeasons(season), set.FollowUpDays)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f ChangeFollowUp
			if err := rows.Scan(&f.CustomerID, &f.Name, &f.Phone, &f.WrittenAt, &f.Sets); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}
