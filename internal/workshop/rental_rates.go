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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Rate plans and extras (#191). A plan prices a category of units or one
// unit, by season; a season is a row from a day, never an edited price.
// Turning a period into billed units is money.PriceRentalSeasons, one rule
// written once. A quote is what confirming a booking freezes (#192): every
// line with its description, quantity, unit price, VAT and where it came
// from, so a later change to a plan changes nothing already agreed.

// The kinds of extra.
var RentalExtraKinds = []string{"extra_driver", "child_seat", "excess_reduction", "delivery",
	"cancellation_fee", "no_show_fee", "young_driver", "other"}

// RatePlan is a plan and its seasons, newest first.
type RatePlan struct {
	ID         string
	Name       string
	Category   string
	UnitID     string
	VATRateBp  int
	SeasonRule string
	Archived   bool
	Seasons    []RateSeason
}

// RateSeason is a plan's prices from one day. Prices are net, in minor
// units; nil is a unit the season does not price.
type RateSeason struct {
	ID               string
	ValidFrom        time.Time
	ValidUntil       *time.Time
	Hour, Day        *int64
	Weekend          *int64
	Week, Month      *int64
	IncludedKmPerDay *int
	ExtraKmMinor     *int64
	Minimum          time.Duration
	Grace            time.Duration
}

func (s RateSeason) terms() money.RateTerms {
	return money.RateTerms{Prices: money.RatePrices{Hour: s.Hour, Day: s.Day, Weekend: s.Weekend, Week: s.Week, Month: s.Month},
		Minimum: s.Minimum, Grace: s.Grace}
}

// RentalExtra is something sold beside the rental.
type RentalExtra struct {
	ID          string
	Kind        string
	Name        string
	Per         string // "rental" or "day"
	PriceMinor  int64
	VATRateBp   int
	VATCategory string // "S" or "O"
	Unconfirmed bool
	Archived    bool
}

var (
	errPlanName       = fmt.Errorf("%w: give the plan a name, in 80 characters or fewer", ErrInvalid)
	errPlanFor        = fmt.Errorf("%w: a plan prices a category of units or one unit", ErrInvalid)
	errPlanVAT        = fmt.Errorf("%w: a VAT rate is between 0 and 100 per cent", ErrInvalid)
	errPlanRule       = fmt.Errorf("%w: say how a rental across two seasons is priced", ErrInvalid)
	errSeasonPrices   = fmt.Errorf("%w: a season prices at least one of an hour, a day, a weekend, a week and a month, none below nothing", ErrInvalid)
	errSeasonDates    = fmt.Errorf("%w: a season ends after it starts", ErrInvalid)
	errSeasonOverlap  = fmt.Errorf("%w: the plan has a season then already", ErrInvalid)
	errSeasonLimits   = fmt.Errorf("%w: the minimum is at most a year and the grace at most a day, neither below nothing", ErrInvalid)
	errPlanArchived   = fmt.Errorf("%w: that plan is archived", ErrInvalid)
	errExtraKind      = fmt.Errorf("%w: that is not a kind of extra", ErrInvalid)
	errExtraName      = fmt.Errorf("%w: give the extra a name, in 80 characters or fewer", ErrInvalid)
	errExtraPer       = fmt.Errorf("%w: an extra is charged per rental or per day", ErrInvalid)
	errExtraPrice     = fmt.Errorf("%w: a price is nothing or more", ErrInvalid)
	errExtraVAT       = fmt.Errorf("%w: an extra is at a VAT rate, or outside the scope of VAT at none", ErrInvalid)
	errNoPlan         = fmt.Errorf("%w: no rate plan prices that unit; add one first", ErrInvalid)
	errNoSeason       = fmt.Errorf("%w: the plan has no season for the day the rental starts", ErrInvalid)
	errNoPriceForLong = fmt.Errorf("%w: the plan has no price for a rental this long", ErrInvalid)
	errRentalTooLong  = fmt.Errorf("%w: a rental is at most five years", ErrInvalid)
	errQuoteTimes     = fmt.Errorf("%w: a rental ends after it starts", ErrInvalid)
	errExtraGone      = fmt.Errorf("%w: that extra is not offered any more", ErrInvalid)
	errExtraCount     = fmt.Errorf("%w: take an extra between once and 99 times", ErrInvalid)
)

// NewRatePlan is a plan to create.
type NewRatePlan struct {
	Name       string
	Category   string
	UnitID     string
	VATRateBp  int
	SeasonRule string
}

// CreateRatePlan makes a plan with no seasons yet. Whoever runs the shop
// sets prices; the rental desk applies them.
func CreateRatePlan(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewRatePlan) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	n.Name, n.Category = strings.TrimSpace(n.Name), strings.TrimSpace(n.Category)
	switch {
	case n.Name == "" || utf8.RuneCountInString(n.Name) > 80:
		return "", errPlanName
	case (n.Category == "") == (n.UnitID == ""), utf8.RuneCountInString(n.Category) > 60:
		return "", errPlanFor
	case n.UnitID != "" && !looksLikeUUID(n.UnitID):
		return "", ErrNotFound
	case n.VATRateBp < 0 || n.VATRateBp > 10000:
		return "", errPlanVAT
	case n.SeasonRule != "at_start" && n.SeasonRule != "split":
		return "", errPlanRule
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO rate_plans (shop_id, name, category, unit_id, vat_rate_bp, season_rule, created_by)
			SELECT $1, $2, nullif($3, ''), u.id, $5, $6, NULLIF($7, '')::uuid
			FROM (SELECT NULLIF($4, '')::uuid AS id) want LEFT JOIN rental_units u ON u.id = want.id
			WHERE want.id IS NULL OR u.id IS NOT NULL
			RETURNING id`, scope.ShopID, n.Name, n.Category, n.UnitID, n.VATRateBp, n.SeasonRule, scope.UserID).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// ArchiveRatePlan stops a plan being offered. Bookings that used it keep
// their lines and still name it.
func ArchiveRatePlan(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, planID string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(planID) {
		return ErrNotFound
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE rate_plans SET archived_at = clock_timestamp(), archived_by = NULLIF($2, '')::uuid
			WHERE id = $1 AND archived_at IS NULL`, planID, scope.UserID)
		if err == nil && tag.RowsAffected() == 0 {
			var there bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rate_plans WHERE id = $1)`, planID).Scan(&there); err != nil || !there {
				return ErrNotFound
			}
		}
		return err
	})
}

// AddRateSeason adds a season's prices to a plan from a day. A season still
// open that began before it ends where it begins; any other overlap is
// refused, by the database as well.
func AddRateSeason(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, planID string, s RateSeason) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(planID) {
		return "", ErrNotFound
	}
	priced := false
	for _, p := range []*int64{s.Hour, s.Day, s.Weekend, s.Week, s.Month} {
		if p != nil {
			if *p < 0 {
				return "", errSeasonPrices
			}
			priced = true
		}
	}
	switch {
	case !priced:
		return "", errSeasonPrices
	case s.ValidFrom.IsZero(), s.ValidUntil != nil && !dateOnly(*s.ValidUntil).After(dateOnly(s.ValidFrom)):
		return "", errSeasonDates
	case s.Minimum < 0 || s.Minimum > 365*24*time.Hour || s.Grace < 0 || s.Grace > 24*time.Hour,
		s.IncludedKmPerDay != nil && *s.IncludedKmPerDay < 0, s.ExtraKmMinor != nil && *s.ExtraKmMinor < 0:
		return "", errSeasonLimits
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var archived bool
		err := tx.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM rate_plans WHERE id = $1 FOR UPDATE`, planID).Scan(&archived)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read plan: %w", err)
		}
		if archived {
			return errPlanArchived
		}
		from := s.ValidFrom.Format("2006-01-02")
		if _, err := tx.Exec(ctx, `UPDATE rate_plan_seasons SET valid_until = $2::date
			WHERE plan_id = $1 AND valid_until IS NULL AND valid_from < $2::date`, planID, from); err != nil {
			return fmt.Errorf("end the season before: %w", err)
		}
		var until any
		if s.ValidUntil != nil {
			until = s.ValidUntil.Format("2006-01-02")
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO rate_plan_seasons (shop_id, plan_id, valid_from, valid_until, hour_minor, day_minor, weekend_minor,
				week_minor, month_minor, included_km_per_day, extra_km_minor, minimum_minutes, grace_minutes, created_by)
			VALUES ($1, $2, $3::date, $4::date, $5, $6, $7, $8, $9, $10, $11, $12, $13, NULLIF($14, '')::uuid) RETURNING id`,
			scope.ShopID, planID, from, until, s.Hour, s.Day, s.Weekend, s.Week, s.Month, s.IncludedKmPerDay, s.ExtraKmMinor,
			int(s.Minimum/time.Minute), int(s.Grace/time.Minute), scope.UserID).Scan(&id)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23P01" {
			return errSeasonOverlap
		}
		return err
	})
	return id, err
}

// RatePlans lists every plan with its seasons, archived ones last.
func RatePlans(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]RatePlan, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []RatePlan
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = ratePlansTx(ctx, tx, "")
		return err
	})
	return out, err
}

func ratePlansTx(ctx context.Context, tx pgx.Tx, id string) ([]RatePlan, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, name, coalesce(category, ''), coalesce(unit_id::text, ''), vat_rate_bp, season_rule, archived_at IS NOT NULL
		FROM rate_plans WHERE $1 = '' OR id::text = $1
		ORDER BY archived_at IS NOT NULL, name, created_at`, id)
	if err != nil {
		return nil, fmt.Errorf("read plans: %w", err)
	}
	var out []RatePlan
	for rows.Next() {
		var p RatePlan
		if err := rows.Scan(&p.ID, &p.Name, &p.Category, &p.UnitID, &p.VATRateBp, &p.SeasonRule, &p.Archived); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Seasons, err = seasonsTx(ctx, tx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func seasonsTx(ctx context.Context, tx pgx.Tx, planID string) ([]RateSeason, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, valid_from, valid_until, hour_minor, day_minor, weekend_minor, week_minor, month_minor,
		       included_km_per_day, extra_km_minor, minimum_minutes, grace_minutes
		FROM rate_plan_seasons WHERE plan_id = $1 ORDER BY valid_from DESC`, planID)
	if err != nil {
		return nil, fmt.Errorf("read seasons: %w", err)
	}
	defer rows.Close()
	var out []RateSeason
	for rows.Next() {
		var s RateSeason
		var minimum, grace int
		if err := rows.Scan(&s.ID, &s.ValidFrom, &s.ValidUntil, &s.Hour, &s.Day, &s.Weekend, &s.Week, &s.Month,
			&s.IncludedKmPerDay, &s.ExtraKmMinor, &minimum, &grace); err != nil {
			return nil, err
		}
		s.Minimum, s.Grace = time.Duration(minimum)*time.Minute, time.Duration(grace)*time.Minute
		out = append(out, s)
	}
	return out, rows.Err()
}

// NewRentalExtra is an extra to offer.
type NewRentalExtra struct {
	Kind        string
	Name        string
	Per         string
	PriceMinor  int64
	VATRateBp   int
	VATCategory string
}

// extraUnconfirmed is whether the VAT treatment of an extra rests on a
// reading nobody has confirmed (#221): an excess reduction may be insurance
// rather than part of the rental (lag 2018:1219), and anything put outside
// the scope of VAT is a decision the shop's adviser makes.
func extraUnconfirmed(kind, category string) bool {
	return kind == "excess_reduction" || category == "O"
}

// AddRentalExtra offers an extra. A new price is a new extra, the old one
// archived (ArchiveRentalExtra): a booking keeps what it was sold at.
func AddRentalExtra(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewRentalExtra) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	n.Name = strings.TrimSpace(n.Name)
	switch {
	case !slices.Contains(RentalExtraKinds, n.Kind):
		return "", errExtraKind
	case n.Name == "" || utf8.RuneCountInString(n.Name) > 80:
		return "", errExtraName
	case n.Per != "rental" && n.Per != "day":
		return "", errExtraPer
	case n.PriceMinor < 0:
		return "", errExtraPrice
	case n.VATCategory == "S" && (n.VATRateBp <= 0 || n.VATRateBp > 10000), n.VATCategory == "O" && n.VATRateBp != 0,
		n.VATCategory != "S" && n.VATCategory != "O":
		return "", errExtraVAT
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO rental_extras (shop_id, kind, name, per, price_minor, vat_rate_bp, vat_category, unconfirmed, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, '')::uuid) RETURNING id`,
			scope.ShopID, n.Kind, n.Name, n.Per, n.PriceMinor, n.VATRateBp, n.VATCategory,
			extraUnconfirmed(n.Kind, n.VATCategory), scope.UserID).Scan(&id)
	})
	return id, err
}

// ArchiveRentalExtra stops offering an extra.
func ArchiveRentalExtra(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, extraID string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(extraID) {
		return ErrNotFound
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE rental_extras SET archived_at = clock_timestamp(), archived_by = NULLIF($2, '')::uuid
			WHERE id = $1 AND archived_at IS NULL`, extraID, scope.UserID)
		return err
	})
}

// RentalExtras lists the extras, offered ones first.
func RentalExtras(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]RentalExtra, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []RentalExtra
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, kind, name, per, price_minor, vat_rate_bp, vat_category, unconfirmed, archived_at IS NOT NULL
			FROM rental_extras ORDER BY archived_at IS NOT NULL, kind, name`)
		if err != nil {
			return fmt.Errorf("read extras: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e RentalExtra
			if err := rows.Scan(&e.ID, &e.Kind, &e.Name, &e.Per, &e.PriceMinor, &e.VATRateBp, &e.VATCategory, &e.Unconfirmed, &e.Archived); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// QuotedLine is one line of a rental's price, as confirming freezes it.
type QuotedLine struct {
	// "rent" or "extra".
	Kind string
	// The unit counted ("day", "week" ...) for rent, or the extra's kind.
	Unit string
	// The plan's or the extra's own name: the shop's words.
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBp      int
	VATCategory    string
	Unconfirmed    bool
	PlanID         string
	SeasonID       string
	ExtraID        string
}

// Net is the line's amount before VAT.
func (l QuotedLine) Net() int64 { return money.LineNet(l.QuantityMilli, l.UnitPriceMinor) }

// RentalQuote is a rental's price before anything is agreed.
type RentalQuote struct {
	PlanID       string
	Lines        []QuotedLine
	Totals       money.Totals
	IncludedKm   *int
	ExtraKmMinor *int64
	Days         int64
}

// ExtraChoice is an extra taken, and how many.
type ExtraChoice struct {
	ExtraID string
	Count   int
}

// QuoteRequest is what to price: a unit, a period, a plan (or the one that
// prices the unit) and the extras taken.
type QuoteRequest struct {
	UnitID string
	PlanID string
	From   time.Time
	To     time.Time
	Extras []ExtraChoice
}

// QuoteRental prices a rental under today's plans. Nothing is kept.
func QuoteRental(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, q QuoteRequest) (RentalQuote, error) {
	if !scope.Role.HandsOverRentals() {
		return RentalQuote{}, access.ErrForbidden
	}
	if !looksLikeUUID(q.UnitID) || (q.PlanID != "" && !looksLikeUUID(q.PlanID)) {
		return RentalQuote{}, ErrNotFound
	}
	var out RentalQuote
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = quoteTx(ctx, tx, q)
		return err
	})
	return out, err
}

// rentalDays is how many days a per-day extra is charged for: whole
// wall-clock days begun, at least one.
func rentalDays(from, to time.Time, loc *time.Location) int64 {
	d := money.WallDuration(from, to, loc)
	n := int64((d + 24*time.Hour - 1) / (24 * time.Hour))
	if n < 1 {
		n = 1
	}
	return n
}

func quoteTx(ctx context.Context, tx pgx.Tx, q QuoteRequest) (RentalQuote, error) {
	var out RentalQuote
	if q.From.IsZero() || !q.To.After(q.From) {
		return out, errQuoteTimes
	}
	loc, err := shopLocationTx(ctx, tx)
	if err != nil {
		return out, err
	}
	// The plan asked for, or the one for this unit, or for its category.
	var category string
	if err := tx.QueryRow(ctx, `SELECT category FROM rental_units WHERE id = $1`, q.UnitID).Scan(&category); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrNotFound
		}
		return out, fmt.Errorf("read unit: %w", err)
	}
	err = tx.QueryRow(ctx, `
		SELECT id FROM rate_plans
		WHERE archived_at IS NULL AND (unit_id = $1 OR category = $2) AND ($3 = '' OR id::text = $3)
		ORDER BY unit_id IS NULL, created_at LIMIT 1`, q.UnitID, category, q.PlanID).Scan(&out.PlanID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, errNoPlan
	}
	if err != nil {
		return out, fmt.Errorf("read plan: %w", err)
	}
	plans, err := ratePlansTx(ctx, tx, out.PlanID)
	if err != nil {
		return out, err
	}
	plan := plans[0]
	// Seasons oldest first, each from its first day's midnight in the
	// shop's zone; the one the rental starts in decides the allowance.
	var seasons []money.Season
	var seasonIDs []string
	var starting *RateSeason
	for i := len(plan.Seasons) - 1; i >= 0; i-- {
		s := plan.Seasons[i]
		begins := time.Date(s.ValidFrom.Year(), s.ValidFrom.Month(), s.ValidFrom.Day(), 0, 0, 0, 0, loc)
		if !begins.After(q.From) {
			starting = &plan.Seasons[i]
			seasons, seasonIDs = seasons[:0], seasonIDs[:0]
		}
		if s.ValidUntil != nil && !time.Date(s.ValidUntil.Year(), s.ValidUntil.Month(), s.ValidUntil.Day(), 0, 0, 0, 0, loc).After(q.From) {
			starting, seasons, seasonIDs = nil, seasons[:0], seasonIDs[:0]
			continue
		}
		seasons = append(seasons, money.Season{Starts: begins, Terms: s.terms()})
		seasonIDs = append(seasonIDs, s.ID)
	}
	if starting == nil {
		return out, errNoSeason
	}
	lines, err := money.PriceRentalSeasons(q.From, q.To, loc, seasons, plan.SeasonRule == "split")
	if errors.Is(err, money.ErrTooLong) {
		return out, errRentalTooLong
	}
	if errors.Is(err, money.ErrNoPriceFor) {
		return out, errNoPriceForLong
	}
	if err != nil {
		return out, err
	}
	for _, l := range lines {
		out.Lines = append(out.Lines, QuotedLine{Kind: "rent", Unit: l.Unit, Description: plan.Name,
			QuantityMilli: l.Count * 1000, UnitPriceMinor: l.UnitPrice, VATRateBp: plan.VATRateBp,
			VATCategory: vatCategoryFor(plan.VATRateBp), PlanID: plan.ID, SeasonID: seasonIDs[l.Season]})
	}
	out.Days = rentalDays(q.From, q.To, loc)
	if starting.IncludedKmPerDay != nil {
		km := *starting.IncludedKmPerDay * int(out.Days)
		out.IncludedKm = &km
	}
	out.ExtraKmMinor = starting.ExtraKmMinor
	for _, c := range q.Extras {
		if c.Count < 1 || c.Count > 99 {
			return out, errExtraCount
		}
		if !looksLikeUUID(c.ExtraID) {
			return out, ErrNotFound
		}
		var e RentalExtra
		err := tx.QueryRow(ctx, `
			SELECT id, kind, name, per, price_minor, vat_rate_bp, vat_category, unconfirmed
			FROM rental_extras WHERE id = $1 AND archived_at IS NULL`, c.ExtraID).Scan(
			&e.ID, &e.Kind, &e.Name, &e.Per, &e.PriceMinor, &e.VATRateBp, &e.VATCategory, &e.Unconfirmed)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, errExtraGone
		}
		if err != nil {
			return out, fmt.Errorf("read extra: %w", err)
		}
		qty := int64(c.Count)
		if e.Per == "day" {
			qty *= out.Days
		}
		out.Lines = append(out.Lines, QuotedLine{Kind: "extra", Unit: e.Kind, Description: e.Name, QuantityMilli: qty * 1000,
			UnitPriceMinor: e.PriceMinor, VATRateBp: e.VATRateBp, VATCategory: e.VATCategory, Unconfirmed: e.Unconfirmed, ExtraID: e.ID})
	}
	out.Totals = quoteTotals(out.Lines)
	return out, nil
}

// vatCategoryFor is a plan's category: rent is at the standard rate.
func vatCategoryFor(int) string { return "S" }

func quoteTotals(lines []QuotedLine) money.Totals {
	ml := make([]money.Line, 0, len(lines))
	for _, l := range lines {
		ml = append(ml, money.Line{QuantityMilli: l.QuantityMilli, UnitPriceMinor: l.UnitPriceMinor, VATRateBasis: l.VATRateBp, ChargedToCustomer: true})
	}
	return money.Compute(ml)
}
