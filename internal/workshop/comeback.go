package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// A car that comes back.
//
// A customer saying it is not right is not yet a warranty case. Whether the
// first repair was at fault, whether a part failed, whether it is something
// new on the same car, is found out afterwards and sometimes never. So opening
// a comeback records what was reported and nothing about whose fault it is;
// every opinion on that is a row of its own, written by a person with their
// reason, and the classification is the latest of them. Nothing here infers
// one, and looking into a complaint does not confirm it.
//
// The follow-up is a job like any other, with the vehicle's history, the
// timeline and the approval rules that come with one. That is deliberate: a
// separately chargeable line on it still needs the customer's priced yes, and
// calling a line warranty makes it free to the customer, never unapproved.
// The original invoice stays as it was issued; if it was wrong, it is
// corrected the way any invoice is.

// ComebackKinds are the classifications, in the order a case usually moves.
var ComebackKinds = []string{"investigating", "warranty", "goodwill", "unrelated"}

// Option is a value for a form and the catalogue key it is shown as.
type Option struct{ Value, Label string }

// ComebackPositions are the positions the shop can take.
func ComebackPositions() []Option {
	var out []Option
	for _, k := range ComebackKinds {
		out = append(out, Option{k, ComebackKindLabel(k)})
	}
	return out
}

// ClaimAnswers are what a supplier can say.
func ClaimAnswers() []Option {
	return []Option{{"accepted", "claim accepted"}, {"rejected", "claim rejected"}, {"paid", "claim paid"}}
}

// Amount is the amount asked for, or nothing.
func (c Claim) Amount() int64 {
	if c.AmountMinor == nil {
		return 0
	}
	return *c.AmountMinor
}

// ComebackInput is what the front desk writes down when a car comes back.
type ComebackInput struct {
	OriginalJobID string
	// The one operation the customer says is not right, if there is one.
	LineID     string
	Symptoms   string
	ReturnedOn time.Time
	OdometerKm *int64
}

// ComebackEvent is one thing said or done about a comeback.
type ComebackEvent struct {
	Kind    string
	Note    string
	ClaimID string
	By      string
	At      time.Time
}

// Claim is money asked of a supplier for a part that failed.
type Claim struct {
	ID          string
	Supplier    string
	Reference   string
	AmountMinor *int64
	Status      string
}

// CostShare is what a follow-up job used, for one of the three who can bear
// it. Parts are counted at the shop's cost price, because a part given free
// to the customer has a price of nothing and that is not what it cost. Time is
// what was clocked; this is no wage and no hourly cost, which are not known
// here.
type CostShare struct {
	Bearer        string
	PartsCost     int64
	PartsUnpriced int
	LabourSeconds int64
}

// Minutes is the clocked time in whole minutes.
func (c CostShare) Minutes() int64 { return (c.LabourSeconds + 30) / 60 }

// Comeback is a comeback and everything known about how it stands.
type Comeback struct {
	ID               string
	OriginalJobID    string
	OriginalNumber   int64
	OriginalLine     string
	FollowUpJobID    string
	FollowUpNumber   int64
	Symptoms         string
	ReturnedOn       time.Time
	OdometerKm       *int64
	OwnerChanged     bool
	Kind             string
	KindNote         string
	Events           []ComebackEvent
	Claims           []Claim
	Costs            []CostShare
	CostsVisible     bool
	OriginalOdometer *int64
}

// Settled is whether the shop has taken a position: a case being looked into
// has not.
func (c Comeback) Settled() bool { return c.Kind != "reported" && c.Kind != "investigating" }

// NeedsClaimAnswer is whether a supplier claim is waiting for an answer.
func (c Comeback) NeedsClaimAnswer() bool {
	for _, cl := range c.Claims {
		if cl.Status == "filed" {
			return true
		}
	}
	return false
}

// OpenComeback opens the follow-up job for a car that has come back, on the
// vehicle of the original, and returns it.
func OpenComeback(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, in ComebackInput) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	in.Symptoms = strings.TrimSpace(in.Symptoms)
	switch {
	case !looksLikeUUID(in.OriginalJobID):
		return "", ErrNotFound
	case in.Symptoms == "":
		return "", fmt.Errorf("%w: write what the customer says is wrong", ErrInvalid)
	case len(in.Symptoms) > 1000:
		return "", fmt.Errorf("%w: the symptoms are limited to 1000 characters", ErrInvalid)
	case in.ReturnedOn.IsZero():
		return "", fmt.Errorf("%w: say which day it came back", ErrInvalid)
	case in.OdometerKm != nil && (*in.OdometerKm < 0 || *in.OdometerKm >= 10000000):
		return "", fmt.Errorf("%w: that is not a reading", ErrInvalid)
	case in.LineID != "" && !looksLikeUUID(in.LineID):
		return "", ErrNotFound
	}

	var jobID string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var vehicleID, state string
		var originalCustomer *string
		var number int64
		if err := tx.QueryRow(ctx, `
			SELECT vehicle_id::text, customer_id::text, state, number FROM work_orders WHERE id = $1`,
			in.OriginalJobID).Scan(&vehicleID, &originalCustomer, &state, &number); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read the original: %w", err)
		}
		switch State(state) {
		case StateReady, StateInvoiced, StateClosed:
		default:
			return fmt.Errorf("%w: a car cannot come back from a job that has not been finished", ErrInvalid)
		}
		if in.LineID != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM work_order_lines WHERE id = $1 AND work_order_id = $2)`,
				in.LineID, in.OriginalJobID).Scan(&ok); err != nil {
				return fmt.Errorf("check the line: %w", err)
			}
			if !ok {
				return fmt.Errorf("%w: that line is not on the original job", ErrInvalid)
			}
		}

		// The person who has the car now, which is not always the one who paid
		// for the first repair.
		customer := originalCustomer
		var owner string
		err := tx.QueryRow(ctx, `SELECT customer_id::text FROM vehicle_ownership WHERE vehicle_id = $1 AND owned_to IS NULL`, vehicleID).Scan(&owner)
		switch {
		case err == nil:
			customer = &owner
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("look up the owner: %w", err)
		}

		var next int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(number), 0) + 1 FROM work_orders`).Scan(&next); err != nil {
			return fmt.Errorf("next number: %w", err)
		}
		complaint := fmt.Sprintf("Comeback on job %d: %s", number, in.Symptoms)
		if len(complaint) > 1100 {
			complaint = complaint[:1100]
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO work_orders (shop_id, number, vehicle_id, customer_id, complaint, state, created_by)
			VALUES ($1, $2, $3, $4, $5, 'draft', $6) RETURNING id`,
			scope.ShopID, next, vehicleID, customer, complaint, scope.UserID).Scan(&jobID); err != nil {
			return fmt.Errorf("open the follow-up: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_presence (shop_id, work_order_id, event, recorded_by) VALUES ($1, $2, 'left', $3)`,
			scope.ShopID, jobID, scope.UserID); err != nil {
			return fmt.Errorf("record the car left: %w", err)
		}
		if in.OdometerKm != nil {
			var decreasing bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM odometer_readings WHERE vehicle_id = $1 AND km > $2)`,
				vehicleID, *in.OdometerKm).Scan(&decreasing); err != nil {
				return fmt.Errorf("compare odometer: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, decreasing) VALUES ($1, $2, $3, 'drop_off', $4)`,
				scope.ShopID, vehicleID, *in.OdometerKm, decreasing); err != nil {
				return fmt.Errorf("record odometer: %w", err)
			}
		}

		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO comebacks (shop_id, original_work_order_id, original_line_id, follow_up_work_order_id,
			                       symptoms, returned_on, odometer_km, owner_changed, opened_by)
			VALUES ($1, $2, nullif($3, '')::uuid, $4, $5, $6, $7, $8, $9) RETURNING id`,
			scope.ShopID, in.OriginalJobID, in.LineID, jobID, in.Symptoms, in.ReturnedOn, in.OdometerKm,
			deref(customer) != deref(originalCustomer), scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record the comeback: %w", err)
		}
		return comebackEventTx(ctx, tx, scope, id, "reported", "", "")
	})
	if err != nil {
		return "", err
	}
	return jobID, nil
}

func comebackEventTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id, kind, note, claimID string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO comeback_events (shop_id, comeback_id, kind, note, claim_id, by_user)
		VALUES ($1, $2, $3, nullif($4, ''), nullif($5, '')::uuid, $6)`,
		scope.ShopID, id, kind, strings.TrimSpace(note), claimID, scope.UserID); err != nil {
		return fmt.Errorf("record the event: %w", err)
	}
	return nil
}

func lockComeback(ctx context.Context, tx pgx.Tx, id string) (kind string, err error) {
	if !looksLikeUUID(id) {
		return "", ErrNotFound
	}
	// Locked through the follow-up job's row: the table's own rows are never
	// updated, and the job is the thing a classification has to agree with.
	var locked bool
	if err := tx.QueryRow(ctx, `
		SELECT true FROM comebacks cb JOIN work_orders w ON w.id = cb.follow_up_work_order_id
		WHERE cb.id = $1 FOR UPDATE OF w`, id).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("lock the comeback: %w", err)
	}
	return currentKindTx(ctx, tx, id)
}

func currentKindTx(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	var kind string
	if err := tx.QueryRow(ctx, `
		SELECT kind FROM comeback_events
		WHERE comeback_id = $1 AND kind IN ('reported', 'investigating', 'warranty', 'goodwill', 'unrelated')
		ORDER BY at DESC, id DESC LIMIT 1`, id).Scan(&kind); err != nil {
		return "", fmt.Errorf("read the classification: %w", err)
	}
	return kind, nil
}

// Classify records the shop's position on a comeback. A verdict -- warranty,
// goodwill or unrelated -- is a statement by somebody and says why; looking
// into it needs no reason and decides nothing.
func Classify(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, comebackID, kind, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	known := false
	for _, k := range ComebackKinds {
		known = known || k == kind
	}
	note = strings.TrimSpace(note)
	switch {
	case !known:
		return fmt.Errorf("%w: %q is not a position", ErrInvalid, kind)
	case kind != "investigating" && note == "":
		return fmt.Errorf("%w: say why", ErrInvalid)
	case len(note) > 500:
		return fmt.Errorf("%w: the reason is limited to 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		current, err := lockComeback(ctx, tx, comebackID)
		if err != nil {
			return err
		}
		if current == kind {
			return fmt.Errorf("%w: that is already the position", ErrInvalid)
		}
		return comebackEventTx(ctx, tx, scope, comebackID, kind, note, "")
	})
}

// AddComebackNote writes something down about a comeback.
func AddComebackNote(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, comebackID, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	note = strings.TrimSpace(note)
	if note == "" || len(note) > 500 {
		return fmt.Errorf("%w: write a note of up to 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := lockComeback(ctx, tx, comebackID); err != nil {
			return err
		}
		return comebackEventTx(ctx, tx, scope, comebackID, "note", note, "")
	})
}

// ClaimInput is a claim on a supplier.
type ClaimInput struct {
	Supplier    string
	Reference   string
	AmountMinor *int64
}

// FileClaim records that the shop asked a supplier to cover a failed part. It
// needs the shop to have called the case warranty: a complaint being looked
// into is not a claim, and a claim filed on one is a promise nobody has the
// facts for.
func FileClaim(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, comebackID string, in ClaimInput) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	in.Supplier, in.Reference = strings.TrimSpace(in.Supplier), strings.TrimSpace(in.Reference)
	switch {
	case in.Supplier == "" || len(in.Supplier) > 100:
		return "", fmt.Errorf("%w: name the supplier, up to 100 characters", ErrInvalid)
	case len(in.Reference) > 100:
		return "", fmt.Errorf("%w: the reference is limited to 100 characters", ErrInvalid)
	case in.AmountMinor != nil && *in.AmountMinor < 0:
		return "", fmt.Errorf("%w: an amount cannot be negative", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		current, err := lockComeback(ctx, tx, comebackID)
		if err != nil {
			return err
		}
		if current != "warranty" {
			return fmt.Errorf("%w: a claim needs the case to be called warranty first", ErrInvalid)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO comeback_claims (shop_id, comeback_id, supplier, reference, amount_minor)
			VALUES ($1, $2, $3, nullif($4, ''), $5) RETURNING id`,
			scope.ShopID, comebackID, in.Supplier, in.Reference, in.AmountMinor).Scan(&id); err != nil {
			return fmt.Errorf("file the claim: %w", err)
		}
		return comebackEventTx(ctx, tx, scope, comebackID, "claim_filed", in.Supplier, id)
	})
	return id, err
}

// AnswerClaim records what the supplier said. A rejected claim can be argued
// and accepted later; a paid one is final. Its history stays in the events.
func AnswerClaim(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, claimID, status string, amountMinor *int64, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(claimID) {
		return ErrNotFound
	}
	if status != "accepted" && status != "rejected" && status != "paid" {
		return fmt.Errorf("%w: a claim is accepted, rejected or paid", ErrInvalid)
	}
	if amountMinor != nil && *amountMinor < 0 {
		return fmt.Errorf("%w: an amount cannot be negative", ErrInvalid)
	}
	if len(note) > 500 {
		return fmt.Errorf("%w: the note is limited to 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var comebackID, current string
		if err := tx.QueryRow(ctx, `SELECT comeback_id::text, status FROM comeback_claims WHERE id = $1 FOR UPDATE`, claimID).Scan(&comebackID, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock the claim: %w", err)
		}
		switch {
		case current == "paid":
			return fmt.Errorf("%w: a paid claim is settled", ErrInvalid)
		case current == status:
			return fmt.Errorf("%w: it is already %s", ErrInvalid, status)
		case status == "paid" && current != "accepted":
			return fmt.Errorf("%w: only an accepted claim is paid", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE comeback_claims SET status = $2, amount_minor = coalesce($3, amount_minor), updated_at = clock_timestamp()
			WHERE id = $1`, claimID, status, amountMinor); err != nil {
			return fmt.Errorf("update the claim: %w", err)
		}
		return comebackEventTx(ctx, tx, scope, comebackID, "claim_"+status, note, claimID)
	})
}

// ComebackFor reads the comeback a follow-up job is, or ErrNotFound if the job
// is not one. A technician sees what was reported and where it stands; the
// claims, the costs and whether the owner changed are for the counter.
func ComebackFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (Comeback, error) {
	if !looksLikeUUID(jobID) {
		return Comeback{}, ErrNotFound
	}
	desk := scope.Role.SeesCustomerPersonalData()
	var c Comeback
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		cs, err := comebacksTx(ctx, tx, `cb.follow_up_work_order_id = $1`, jobID, desk)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			return ErrNotFound
		}
		c = cs[0]
		return nil
	})
	return c, err
}

// ComebacksOf lists the times a job has been come back to, oldest first.
func ComebacksOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]Comeback, error) {
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	desk := scope.Role.SeesCustomerPersonalData()
	var out []Comeback
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = comebacksTx(ctx, tx, `cb.original_work_order_id = $1`, jobID, desk)
		return err
	})
	return out, err
}

func comebacksTx(ctx context.Context, tx pgx.Tx, where, arg string, desk bool) ([]Comeback, error) {
	rows, err := tx.Query(ctx, `
		SELECT cb.id::text, cb.original_work_order_id::text, o.number, coalesce(l.description, ''),
		       cb.follow_up_work_order_id::text, f.number, cb.symptoms, cb.returned_on, cb.odometer_km, cb.owner_changed,
		       (SELECT k.km FROM odometer_readings k WHERE k.vehicle_id = o.vehicle_id AND k.read_at <= o.opened_at
		         ORDER BY k.read_at DESC LIMIT 1)
		FROM comebacks cb
		JOIN work_orders o ON o.id = cb.original_work_order_id
		JOIN work_orders f ON f.id = cb.follow_up_work_order_id
		LEFT JOIN work_order_lines l ON l.id = cb.original_line_id
		WHERE `+where+` ORDER BY cb.opened_at`, arg)
	if err != nil {
		return nil, fmt.Errorf("read comebacks: %w", err)
	}
	var out []Comeback
	for rows.Next() {
		var c Comeback
		if err := rows.Scan(&c.ID, &c.OriginalJobID, &c.OriginalNumber, &c.OriginalLine, &c.FollowUpJobID, &c.FollowUpNumber,
			&c.Symptoms, &c.ReturnedOn, &c.OdometerKm, &c.OwnerChanged, &c.OriginalOdometer); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan comeback: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		c := &out[i]
		c.CostsVisible = desk
		if !desk {
			c.OwnerChanged = false
		}
		ev, err := tx.Query(ctx, `
			SELECT e.kind, coalesce(e.note, ''), coalesce(e.claim_id::text, ''), coalesce(p.display_name, ''), e.at
			FROM comeback_events e LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
			WHERE e.comeback_id = $1 ORDER BY e.at, e.id`, c.ID)
		if err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		for ev.Next() {
			var e ComebackEvent
			if err := ev.Scan(&e.Kind, &e.Note, &e.ClaimID, &e.By, &e.At); err != nil {
				ev.Close()
				return nil, fmt.Errorf("scan event: %w", err)
			}
			if strings.HasPrefix(e.Kind, "claim_") && !desk {
				continue
			}
			c.Events = append(c.Events, e)
			switch e.Kind {
			case "reported", "investigating", "warranty", "goodwill", "unrelated":
				c.Kind, c.KindNote = e.Kind, e.Note
			}
		}
		ev.Close()
		if err := ev.Err(); err != nil {
			return nil, err
		}
		if !desk {
			continue
		}
		cl, err := tx.Query(ctx, `
			SELECT id::text, supplier, coalesce(reference, ''), amount_minor, status
			FROM comeback_claims WHERE comeback_id = $1 ORDER BY updated_at, id`, c.ID)
		if err != nil {
			return nil, fmt.Errorf("read claims: %w", err)
		}
		for cl.Next() {
			var x Claim
			if err := cl.Scan(&x.ID, &x.Supplier, &x.Reference, &x.AmountMinor, &x.Status); err != nil {
				cl.Close()
				return nil, fmt.Errorf("scan claim: %w", err)
			}
			c.Claims = append(c.Claims, x)
		}
		cl.Close()
		if err := cl.Err(); err != nil {
			return nil, err
		}
		c.Costs, err = costSharesTx(ctx, tx, c.FollowUpJobID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// costSharesTx is what the follow-up job used, by who bears it. A bearer that
// bore nothing is not listed. Time clocked against no line is listed as
// "unassigned": it is real work and nobody has said whose it is.
func costSharesTx(ctx context.Context, tx pgx.Tx, jobID string) ([]CostShare, error) {
	shares := map[string]*CostShare{}
	get := func(b string) *CostShare {
		if shares[b] == nil {
			shares[b] = &CostShare{Bearer: b}
		}
		return shares[b]
	}
	rows, err := tx.Query(ctx, `
		SELECT l.cost_bearer, coalesce(sum(round(l.quantity * p.cost_minor)), 0)::bigint,
		       count(*) FILTER (WHERE p.id IS NULL OR p.cost_minor IS NULL)
		FROM work_order_lines l LEFT JOIN parts p ON p.id = l.part_id
		WHERE l.work_order_id = $1 AND l.kind = 'part' AND l.withdrawn_at IS NULL AND l.declined_at IS NULL
		GROUP BY l.cost_bearer`, jobID)
	if err != nil {
		return nil, fmt.Errorf("read parts: %w", err)
	}
	for rows.Next() {
		var b string
		var cost int64
		var unpriced int
		if err := rows.Scan(&b, &cost, &unpriced); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan parts: %w", err)
		}
		s := get(b)
		s.PartsCost, s.PartsUnpriced = cost, unpriced
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `
		SELECT coalesce(l.cost_bearer, 'unassigned'),
		       coalesce(sum(extract(epoch FROM (t.ended_at - t.started_at))), 0)::bigint
		FROM time_entries t LEFT JOIN work_order_lines l ON l.id = t.line_id
		WHERE t.work_order_id = $1 AND t.ended_at IS NOT NULL
		GROUP BY 1`, jobID)
	if err != nil {
		return nil, fmt.Errorf("read time: %w", err)
	}
	for rows.Next() {
		var b string
		var secs int64
		if err := rows.Scan(&b, &secs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan time: %w", err)
		}
		get(b).LabourSeconds = secs
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []CostShare
	for _, b := range []string{"shop", "supplier", "customer", "unassigned"} {
		if s := shares[b]; s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

// ComebackKindLabel is a position, as a catalogue key.
func ComebackKindLabel(kind string) string {
	switch kind {
	case "reported":
		return "reported"
	case "investigating":
		return "being looked into"
	case "warranty":
		return "warranty"
	case "goodwill":
		return "goodwill"
	case "unrelated":
		return "unrelated to the first repair"
	case "claim_filed":
		return "claim filed"
	case "claim_accepted":
		return "claim accepted"
	case "claim_rejected":
		return "claim rejected"
	case "claim_paid":
		return "claim paid"
	}
	return kind
}

// Label is what happened, as a catalogue key.
func (e ComebackEvent) Label() string { return ComebackKindLabel(e.Kind) }

// IsClaim reports that the event is about a supplier claim.
func (e ComebackEvent) IsClaim() bool { return strings.HasPrefix(e.Kind, "claim_") }

// Label is the position now, as a catalogue key.
func (c Comeback) Label() string { return ComebackKindLabel(c.Kind) }

// BearerLabel is who bears it, as a catalogue key.
func (c CostShare) BearerLabel() string {
	switch c.Bearer {
	case "shop":
		return "the workshop"
	case "supplier":
		return "the supplier"
	case "customer":
		return "the customer"
	}
	return "nobody has said"
}

// StatusLabel is where the claim stands, as a catalogue key.
func (c Claim) StatusLabel() string { return ComebackKindLabel("claim_" + c.Status) }

// Odometer is the reading when the car came back, or zero.
func (c Comeback) Odometer() int64 {
	if c.OdometerKm == nil {
		return 0
	}
	return *c.OdometerKm
}

// FirstOdometer is the reading at the first repair, or zero.
func (c Comeback) FirstOdometer() int64 {
	if c.OriginalOdometer == nil {
		return 0
	}
	return *c.OriginalOdometer
}
