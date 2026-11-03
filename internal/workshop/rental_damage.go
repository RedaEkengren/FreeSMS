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
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// What happens to a rental damage case (#197): steps a person takes, each
// with a reason. A damage is not a debt (PMÖD 2019:11): it becomes money
// owed only through an outcome that is refused before the amount is
// settled and the customer told, and a disputed case is set against the
// deposit only on the word of whoever runs the shop, an audit event.

// DamageStep is one step of a case.
type DamageStep struct {
	Step        string
	Liability   string
	AmountMinor *int64
	Channel     string
	Response    string
	Outcome     string
	// By when the customer may object, said when they are told (#196).
	RespondBy *time.Time
	Reason    string
	At        time.Time
	By        string
}

// DamageCaseDetail is a case with its steps and where it stands.
type DamageCaseDetail struct {
	DamageCase
	Steps []DamageStep
}

// The steps, liabilities, channels, responses and outcomes.
var (
	DamageSteps       = []string{"new_confirmed", "pre_existing", "liability_assessed", "amount_settled", "customer_informed", "customer_response", "outcome", "reopened"}
	DamageLiabilities = []string{"renter", "shop", "third_party", "unknown"}
	DamageChannels    = []string{"email", "sms", "letter", "in_person", "phone"}
	DamageResponses   = []string{"accepted", "disputed", "no_answer"}
	DamageOutcomes    = []string{"invoiced", "set_against_deposit", "written_off", "insurance_claim", "closed_not_renter"}
)

// Settled is the amount settled last, if any since the case was last
// reopened.
func (c DamageCaseDetail) Settled() *int64 {
	var out *int64
	for _, s := range c.Steps {
		switch s.Step {
		case "amount_settled":
			out = s.AmountMinor
		case "reopened":
			out = nil
		}
	}
	return out
}

// has reports whether the case has the step since it was last reopened.
func (c DamageCaseDetail) has(step string) bool {
	found := false
	for _, s := range c.Steps {
		switch s.Step {
		case step:
			found = true
		case "reopened":
			found = false
		}
	}
	return found
}

// Closed reports whether the case has an outcome since it was last reopened.
func (c DamageCaseDetail) Closed() bool { return c.has("outcome") }

// Disputed reports whether the customer's latest answer disputes it.
func (c DamageCaseDetail) Disputed() bool {
	disputed := false
	for _, s := range c.Steps {
		switch s.Step {
		case "customer_response":
			disputed = s.Response == "disputed"
		case "reopened":
			disputed = false
		}
	}
	return disputed
}

// RespondBy is by when the customer, told last, may object; nil when
// that was not said or the case was reopened since.
func (c DamageCaseDetail) RespondBy() *time.Time {
	var out *time.Time
	for _, s := range c.Steps {
		switch s.Step {
		case "customer_informed":
			out = s.RespondBy
		case "reopened":
			out = nil
		}
	}
	return out
}

// Accepted reports whether the customer's latest answer accepts it.
func (c DamageCaseDetail) Accepted() bool {
	accepted := false
	for _, s := range c.Steps {
		switch s.Step {
		case "customer_response":
			accepted = s.Response == "accepted"
		case "reopened":
			accepted = false
		}
	}
	return accepted
}

// Chargeable reports whether the case may become money owed: its amount
// settled and the customer told of it.
func (c DamageCaseDetail) Chargeable() bool {
	return c.Settled() != nil && c.has("customer_informed")
}

var (
	errDamageStep      = fmt.Errorf("%w: that is not a step a damage case takes, or a detail it needs is missing", ErrInvalid)
	errDamageOrder     = fmt.Errorf("%w: the customer is told once the amount is settled, and answers once told", ErrInvalid)
	errDamageNotOwed   = fmt.Errorf("%w: a damage becomes money owed only once its amount is settled and the customer told", ErrInvalid)
	errDamageDisputed  = fmt.Errorf("%w: the customer disputes it; only whoever runs the shop sets it against the deposit, with a reason", ErrInvalid)
	errDamageClosed    = fmt.Errorf("%w: that case is closed; reopen it first", ErrInvalid)
	errDamageNotClosed = fmt.Errorf("%w: only a closed case is reopened", ErrInvalid)
)

// TakeDamageStep records the next step of a case, with its reason. For the
// rental desk and whoever runs the shop.
func TakeDamageStep(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, caseID string, s DamageStep) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(caseID) {
		return ErrNotFound
	}
	s.Reason = strings.TrimSpace(s.Reason)
	need := map[string]bool{"liability_assessed": s.Liability != "", "amount_settled": s.AmountMinor != nil, "customer_informed": s.Channel != "",
		"customer_response": s.Response != "", "outcome": s.Outcome != ""}
	switch {
	case !slices.Contains(DamageSteps, s.Step), s.Reason == "", utf8.RuneCountInString(s.Reason) > 500,
		s.Liability != "" && !slices.Contains(DamageLiabilities, s.Liability),
		s.Channel != "" && !slices.Contains(DamageChannels, s.Channel),
		s.Response != "" && !slices.Contains(DamageResponses, s.Response),
		s.Outcome != "" && !slices.Contains(DamageOutcomes, s.Outcome),
		s.AmountMinor != nil && (*s.AmountMinor < 0 || *s.AmountMinor > 10_000_000_000),
		s.RespondBy != nil && s.Step != "customer_informed":
		return errDamageStep
	}
	for step, given := range need {
		if (s.Step == step) != given {
			return errDamageStep
		}
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		c, err := damageCaseTx(ctx, tx, caseID, true)
		if err != nil {
			return err
		}
		switch {
		case s.Step == "reopened":
			if !c.Closed() {
				return errDamageNotClosed
			}
		case c.Closed():
			return errDamageClosed
		case s.Step == "customer_informed" && c.Settled() == nil, s.Step == "customer_response" && !c.has("customer_informed"):
			return errDamageOrder
		case s.Step == "outcome" && (s.Outcome == "invoiced" || s.Outcome == "set_against_deposit") && !c.Chargeable():
			return errDamageNotOwed
		case s.Step == "outcome" && s.Outcome == "set_against_deposit" && c.Disputed():
			if !scope.Role.RunsTheShop() {
				return errDamageDisputed
			}
			if err := auditTx(ctx, tx, scope, caseID, "damage.set_against_disputed", "disputed", "set_against_deposit", s.Reason, nil); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO damage_case_steps (shop_id, case_id, step, liability, amount_minor, channel, response, outcome, reason, by_user, respond_by)
			VALUES ($1, $2, $3, nullif($4, ''), $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), $9, NULLIF($10, '')::uuid, $11::date)`,
			scope.ShopID, caseID, s.Step, s.Liability, s.AmountMinor, s.Channel, s.Response, s.Outcome, s.Reason, scope.UserID, s.RespondBy)
		return err
	})
}

// damageCaseTx reads a case and its steps, locked when lock.
func damageCaseTx(ctx context.Context, tx pgx.Tx, id string, lock bool) (DamageCaseDetail, error) {
	var c DamageCaseDetail
	if lock {
		// The unit is the lock, as for everything done to it.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM resources WHERE id = (SELECT resource_id FROM damage_cases WHERE id = $1) FOR UPDATE`, id); err != nil {
			return c, fmt.Errorf("lock the unit: %w", err)
		}
	}
	err := tx.QueryRow(ctx, `SELECT id::text, coalesce(item_key, ''), description, state, observed_at, coalesce(booking_id::text, ''), found_in::text
		FROM damage_cases WHERE id = $1`, id).Scan(&c.ID, &c.ItemKey, &c.Description, &c.State, &c.ObservedAt, &c.BookingID, &c.FoundIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("read the damage case: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT s.step, coalesce(s.liability, ''), s.amount_minor, coalesce(s.channel, ''), coalesce(s.response, ''), coalesce(s.outcome, ''),
		       s.reason, s.at, coalesce(p.display_name, ''), s.respond_by
		FROM damage_case_steps s LEFT JOIN users u ON u.id = s.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE s.case_id = $1 ORDER BY s.seq`, id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var s DamageStep
		if err := rows.Scan(&s.Step, &s.Liability, &s.AmountMinor, &s.Channel, &s.Response, &s.Outcome, &s.Reason, &s.At, &s.By, &s.RespondBy); err != nil {
			return c, err
		}
		c.Steps = append(c.Steps, s)
	}
	return c, rows.Err()
}

// DamageCaseByID reads a case with its steps, for the rental desk and
// whoever runs the shop. Reading is Core's.
func DamageCaseByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (DamageCaseDetail, error) {
	if !scope.Role.HandsOverRentals() {
		return DamageCaseDetail{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return DamageCaseDetail{}, ErrNotFound
	}
	var out DamageCaseDetail
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = damageCaseTx(ctx, tx, id, false)
		return err
	})
	return out, err
}

// damageWaiting is the queue's damage cases with nothing assessed two days
// after they were observed, and rentals over whose deposit is still held
// or whose card hold is not let go (#196) -- with no damage case of theirs
// open -- for the rental desk. One statement.
func damageWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, nil
	}
	var out []ActionItem
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT 'damage_to_assess', c.booking_id::text, c.description, c.observed_at + interval '2 days'
			FROM damage_cases c
			WHERE c.booking_id IS NOT NULL AND c.observed_at < clock_timestamp() - interval '2 days'
			  AND NOT EXISTS (SELECT 1 FROM damage_case_steps s WHERE s.case_id = c.id)
			UNION ALL
			SELECT CASE WHEN x.held <> 0 THEN 'deposit_to_return' ELSE 'deposit_hold_open' END, b.id::text, 'R-' || b.number, b.ends_at
			FROM rental_deposits rd
			JOIN rental_bookings b ON b.id = rd.booking_id
			JOIN resource_blocks k ON k.id = b.block_id
			CROSS JOIN LATERAL (SELECT
				(SELECT coalesce(sum(p.amount_minor + p.rounding_minor), 0) FROM invoice_payments p WHERE p.deposit_id = rd.deposit_id) AS held,
				coalesce((SELECT e.kind = 'held' FROM rental_deposit_events e
				           WHERE e.rental_deposit_id = rd.id AND e.kind IN ('held', 'hold_released') ORDER BY e.seq DESC LIMIT 1), false) AS hold) x
			WHERE `+rentalState+` IN ('returned', 'cancelled', 'no_show', 'hold_expired')
			  AND (x.held <> 0 OR x.hold)
			  AND NOT EXISTS (SELECT 1 FROM damage_cases c WHERE c.booking_id = b.id
			                  AND NOT EXISTS (SELECT 1 FROM damage_case_steps s WHERE s.case_id = c.id AND s.step = 'outcome'
			                                  AND s.seq > coalesce((SELECT max(r.seq) FROM damage_case_steps r WHERE r.case_id = c.id AND r.step = 'reopened'), 0))
			                  AND NOT EXISTS (SELECT 1 FROM damage_case_steps s WHERE s.case_id = c.id AND s.step = 'pre_existing'))
			ORDER BY 4`)
		if err != nil {
			return fmt.Errorf("read damage and deposits waiting: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			it := ActionItem{Module: access.Rental, SubjectKind: "rental_booking", Urgency: UrgencySoon}
			if err := rows.Scan(&it.Kind, &it.SubjectID, &it.Text, &it.Due); err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}
