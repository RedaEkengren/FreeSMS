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

// The reconditioning plan (#236): the steps a car goes through between being
// bought and being in the ad, each a row with the moves it has been through,
// so "how long did it stand at the painter" is answered by the rows. A step
// is waiting until it has an event; the newest event says where it is.

// Step kinds: done by the shop's own people, or by an outside supplier.
const (
	ReconInHouse = "in_house"
	ReconOutside = "outside"
)

// Step states, as the newest event says.
const (
	ReconWaiting    = "waiting"
	ReconInProgress = "in_progress"
	ReconDone       = "done"
	ReconSkipped    = "skipped"
)

// The moves a step goes through.
const (
	ReconStart  = "started"
	ReconFinish = "done"
	ReconSkip   = "skipped"
	ReconReopen = "reopened"
)

// reconStallDays is how many days a car may stand in one step before it is
// on the queue, until the shop says otherwise.
const reconStallDays = 5

// reconStallSQL is the days a car may stand in one step in this shop: the
// newest setting, or the default.
var reconStallSQL = fmt.Sprintf(`coalesce((SELECT days FROM recon_stall_settings ORDER BY changed_at DESC, id DESC LIMIT 1), %d)`, reconStallDays)

var errReconStall = fmt.Errorf("%w: a car may stand in a step for from one day to a year before it is on the queue", ErrInvalid)

// SetReconStallDays says how many days a car may stand in one step before it
// is on the queue. Whoever runs the shop, in a shop that sells cars.
func SetReconStallDays(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, days int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if days < 1 || days > 365 {
		return errReconStall
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO recon_stall_settings (shop_id, days, changed_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
			scope.ShopID, days, scope.UserID)
		return err
	})
}

// ReconStallDaysOf reads the days in force. For whoever buys or sells cars.
func ReconStallDaysOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (int, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return 0, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return 0, err
	}
	var days int
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT `+reconStallSQL).Scan(&days)
	})
	return days, err
}

// ReconTemplateStep is one step of what a new plan is made from.
type ReconTemplateStep struct {
	Name string
	Kind string
}

// defaultReconSteps are what a shop with no template of its own starts with.
var defaultReconSteps = []ReconTemplateStep{
	{"Intake check", ReconInHouse}, {"Workshop", ReconInHouse}, {"Valet", ReconInHouse},
	{"Photographs", ReconInHouse}, {"Ad", ReconInHouse},
}

// ReconStep is one step of a car's plan.
type ReconStep struct {
	ID       string
	Position int
	Name     string
	Kind     string
	Owner    string
	DueOn    *time.Time
	// The price agreed for it: a cost, so nil for a role that may not see
	// what a car cost, and never read for one.
	AgreedPriceMinor *int64
	// What the costs entered against it come to, before VAT, with any
	// reversed left out: nil when none is entered, and for a role that may
	// not see what a car cost.
	CostedMinor *int64
	State       string
	// Since when it has been in its state: its newest move, or when it was
	// planned.
	Since time.Time
	// Whole days in that state, in the shop's days.
	DaysInState int
}

// Label is the step's state in words, a catalogue key.
func (s ReconStep) Label() string {
	switch s.State {
	case ReconInProgress:
		return "In progress"
	case ReconDone:
		return "Done"
	case ReconSkipped:
		return "Skipped"
	}
	return "Waiting"
}

// ReconPlan is a car's steps in order.
type ReconPlan struct {
	UnitID string
	Steps  []ReconStep
}

// Current is the first step not yet done or skipped, nil when the plan is
// finished or empty.
func (p ReconPlan) Current() *ReconStep {
	for i := range p.Steps {
		if s := p.Steps[i].State; s == ReconWaiting || s == ReconInProgress {
			return &p.Steps[i]
		}
	}
	return nil
}

var (
	errReconStep     = fmt.Errorf("%w: a step has a name of 100 characters or fewer, is done in the shop or by a supplier, and has an owner of 200 characters or fewer and a price of up to a hundred million kronor", ErrInvalid)
	errReconTemplate = fmt.Errorf("%w: a template is one to thirty steps, each with a name and whether it is done in the shop or outside", ErrInvalid)
	errReconHasPlan  = fmt.Errorf("%w: the car already has a plan; add steps to it", ErrInvalid)
	errReconMove     = fmt.Errorf("%w: a step is started or skipped while it waits, finished or skipped while it is under way, and reopened once it is finished or skipped", ErrInvalid)
	errReconNotDone  = fmt.Errorf("%w: a cost is entered against a step once it is finished", ErrInvalid)
	errReconNote     = fmt.Errorf("%w: a note is at most 300 characters", ErrInvalid)
)

// SetReconTemplate replaces what new plans are made from; plans already made
// are left as they are. Whoever runs the shop, in a shop that sells cars.
func SetReconTemplate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, steps []ReconTemplateStep) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if len(steps) < 1 || len(steps) > 30 {
		return errReconTemplate
	}
	for i := range steps {
		steps[i].Name = strings.TrimSpace(steps[i].Name)
		if steps[i].Name == "" || utf8.RuneCountInString(steps[i].Name) > 100 || (steps[i].Kind != ReconInHouse && steps[i].Kind != ReconOutside) {
			return errReconTemplate
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO recon_templates (shop_id, created_by) VALUES ($1, NULLIF($2, '')::uuid) RETURNING id`,
			scope.ShopID, scope.UserID).Scan(&id); err != nil {
			return err
		}
		for i, s := range steps {
			if _, err := tx.Exec(ctx, `INSERT INTO recon_template_steps (shop_id, template_id, position, name, kind) VALUES ($1, $2, $3, $4, $5)`,
				scope.ShopID, id, i+1, s.Name, s.Kind); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReconTemplateOf reads what a new plan is made from: the shop's newest
// template, or the steps the application starts with. For whoever buys or
// sells cars.
func ReconTemplateOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ReconTemplateStep, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return nil, err
	}
	var out []ReconTemplateStep
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = reconTemplateTx(ctx, tx)
		return err
	})
	return out, err
}

func reconTemplateTx(ctx context.Context, tx pgx.Tx) ([]ReconTemplateStep, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.name, s.kind FROM recon_template_steps s
		WHERE s.template_id = (SELECT id FROM recon_templates ORDER BY created_at DESC, id DESC LIMIT 1)
		ORDER BY s.position`)
	if err != nil {
		return nil, fmt.Errorf("read the template: %w", err)
	}
	defer rows.Close()
	var out []ReconTemplateStep
	for rows.Next() {
		var s ReconTemplateStep
		if err := rows.Scan(&s.Name, &s.Kind); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return append([]ReconTemplateStep(nil), defaultReconSteps...), nil
	}
	return out, nil
}

// StartReconPlan makes a car's plan from the shop's template. A car has one
// plan; asked again it is refused, and more steps are added to it.
func StartReconPlan(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(unitID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		var has bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM recon_steps WHERE stock_unit_id = $1)`, unitID).Scan(&has); err != nil {
			return err
		}
		if has {
			return errReconHasPlan
		}
		steps, err := reconTemplateTx(ctx, tx)
		if err != nil {
			return err
		}
		for i, s := range steps {
			if _, err := tx.Exec(ctx, `INSERT INTO recon_steps (shop_id, stock_unit_id, position, name, kind, created_by)
				VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid)`, scope.ShopID, unitID, i+1, s.Name, s.Kind, scope.UserID); err != nil {
				return err
			}
		}
		return nil
	})
}

// NewReconStep is a step added to a car's plan, last in order.
type NewReconStep struct {
	Name             string
	Kind             string
	Owner            string
	DueOn            *time.Time
	AgreedPriceMinor *int64
}

// AddReconStep adds a step to a car's plan. An agreed price is a cost, so
// only whoever may see what cars cost may state one.
func AddReconStep(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string, n NewReconStep) (string, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", err
	}
	if !looksLikeUUID(unitID) {
		return "", ErrNotFound
	}
	n.Name, n.Owner = strings.TrimSpace(n.Name), strings.TrimSpace(n.Owner)
	if n.Name == "" || utf8.RuneCountInString(n.Name) > 100 || utf8.RuneCountInString(n.Owner) > 200 ||
		(n.Kind != ReconInHouse && n.Kind != ReconOutside) ||
		(n.AgreedPriceMinor != nil && (*n.AgreedPriceMinor < 0 || *n.AgreedPriceMinor > 100_000_000_000)) {
		return "", errReconStep
	}
	if n.AgreedPriceMinor != nil && !scope.Role.SeesPurchasePrice() {
		return "", access.ErrForbidden
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO recon_steps (shop_id, stock_unit_id, position, name, kind, owner, due_on, agreed_price_minor, created_by)
			VALUES ($1, $2, coalesce((SELECT max(position) FROM recon_steps WHERE stock_unit_id = $2), 0) + 1, $3, $4, nullif($5, ''), $6::date, $7,
			        NULLIF($8, '')::uuid) RETURNING id`,
			scope.ShopID, unitID, n.Name, n.Kind, n.Owner, n.DueOn, n.AgreedPriceMinor, scope.UserID).Scan(&id)
	})
	return id, err
}

// reconNext is the state a move leads to from a state, empty when it is not a
// move that state allows.
func reconNext(from, move string) string {
	switch {
	case from == ReconWaiting && move == ReconStart, from == ReconDone && move == ReconReopen, from == ReconSkipped && move == ReconReopen:
		return ReconInProgress
	case from == ReconInProgress && move == ReconFinish:
		return ReconDone
	case (from == ReconWaiting || from == ReconInProgress) && move == ReconSkip:
		return ReconSkipped
	}
	return ""
}

// MoveReconStep records a move of a step: started, finished, skipped or
// reopened to be done again. Every move is a row; none is written over.
func MoveReconStep(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, stepID, move, note string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(stepID) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 300 {
		return errReconNote
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var unit string
		if err := tx.QueryRow(ctx, `SELECT stock_unit_id FROM recon_steps WHERE id = $1`, stepID).Scan(&unit); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		// Locks the car's cycle, so two moves of one step are told apart.
		if _, err := unitOpenTx(ctx, tx, unit); err != nil {
			return err
		}
		var last *string
		if err := tx.QueryRow(ctx, `SELECT kind FROM recon_step_events WHERE step_id = $1 ORDER BY seq DESC LIMIT 1`, stepID).Scan(&last); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if reconNext(reconStateOf(last), move) == "" {
			return errReconMove
		}
		_, err := tx.Exec(ctx, `INSERT INTO recon_step_events (shop_id, step_id, kind, note, by_user) VALUES ($1, $2, $3, nullif($4, ''), NULLIF($5, '')::uuid)`,
			scope.ShopID, stepID, move, note, scope.UserID)
		return err
	})
}

// reconStateOf is a step's state from its newest event's kind.
func reconStateOf(last *string) string {
	if last == nil {
		return ReconWaiting
	}
	switch *last {
	case ReconFinish:
		return ReconDone
	case ReconSkip:
		return ReconSkipped
	}
	return ReconInProgress
}

// reconStepColumns reads a step with its state and how long it has been in
// it, in the shop's days; $1 says whether the price may be read.
const reconStepColumns = `
	SELECT s.id, s.position, s.name, s.kind, coalesce(s.owner, ''), s.due_on,
	       CASE WHEN $1 THEN s.agreed_price_minor END,
	       CASE WHEN $1 THEN (SELECT sum(c.net_minor) FROM recon_step_costs l JOIN stock_cost_entries c ON c.id = l.cost_entry_id
	                          WHERE l.step_id = s.id AND NOT EXISTS (SELECT 1 FROM stock_cost_entries r WHERE r.reverses_id = c.id)) END,
	       e.kind, coalesce(e.at, s.created_at),
	       (clock_timestamp() AT TIME ZONE z.timezone)::date - (coalesce(e.at, s.created_at) AT TIME ZONE z.timezone)::date
	FROM recon_steps s CROSS JOIN shops z
	LEFT JOIN LATERAL (SELECT x.kind, x.at FROM recon_step_events x WHERE x.step_id = s.id ORDER BY x.seq DESC LIMIT 1) e ON true`

func scanReconStep(row pgx.Row) (ReconStep, error) {
	var s ReconStep
	var last *string
	if err := row.Scan(&s.ID, &s.Position, &s.Name, &s.Kind, &s.Owner, &s.DueOn, &s.AgreedPriceMinor, &s.CostedMinor, &last, &s.Since, &s.DaysInState); err != nil {
		return ReconStep{}, err
	}
	s.State = reconStateOf(last)
	return s, nil
}

// ReconPlanOf reads a car's plan in order, in one statement. For whoever buys
// or sells cars; reading is Core's, so a car sold keeps its history.
func ReconPlanOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) (ReconPlan, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return ReconPlan{}, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return ReconPlan{}, ErrNotFound
	}
	out := ReconPlan{UnitID: unitID}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, reconStepColumns+` WHERE s.stock_unit_id = $2 ORDER BY s.position`, scope.Role.SeesPurchasePrice(), unitID)
		if err != nil {
			return fmt.Errorf("read the plan: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanReconStep(rows)
			if err != nil {
				return err
			}
			out.Steps = append(out.Steps, s)
		}
		return rows.Err()
	})
	return out, err
}

// PipelineRow is one car on its way to the ad: the step it stands in and
// for how long.
type PipelineRow struct {
	UnitID       string
	Car          string
	Status       StockStatus
	StepName     string
	StepKind     string
	StepOwner    string
	StepState    string
	DaysInStep   int
	DaysSinceBuy int
	// The days a car may stand in one step in this shop.
	StallDays  int
	StepsDone  int
	StepsTotal int
}

// Stalled reports whether the car has stood in its step longer than the
// shop lets a car.
func (r PipelineRow) Stalled() bool { return r.DaysInStep > r.StallDays }

// ReconPipeline lists the cars being made ready, by the step each stands in,
// the ones that have stood longest first. A car with a finished plan is not
// on it, and nor is one with no plan. For whoever buys or sells cars.
func ReconPipeline(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]PipelineRow, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return nil, err
	}
	var out []PipelineRow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH steps AS (
			  SELECT s.stock_unit_id, s.position, s.name, s.kind, coalesce(s.owner, '') AS owner,
			         coalesce(e.kind, 'waiting') AS last, coalesce(e.at, s.created_at) AS since
			  FROM recon_steps s
			  LEFT JOIN LATERAL (SELECT x.kind, x.at FROM recon_step_events x WHERE x.step_id = s.id ORDER BY x.seq DESC LIMIT 1) e ON true
			), cur AS (
			  SELECT DISTINCT ON (stock_unit_id) * FROM steps WHERE last NOT IN ('done', 'skipped') ORDER BY stock_unit_id, position
			)
			SELECT u.id, `+carNameSQL+`, u.status, c.name, c.kind, c.owner, c.last,
			       (clock_timestamp() AT TIME ZONE z.timezone)::date - (c.since AT TIME ZONE z.timezone)::date,
			       (clock_timestamp() AT TIME ZONE z.timezone)::date - (u.opened_at AT TIME ZONE z.timezone)::date,
			       (SELECT count(*) FROM steps t WHERE t.stock_unit_id = u.id AND t.last IN ('done', 'skipped')),
			       (SELECT count(*) FROM steps t WHERE t.stock_unit_id = u.id),
			       `+reconStallSQL+`
			FROM cur c JOIN stock_units u ON u.id = c.stock_unit_id CROSS JOIN shops z
			JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
			WHERE u.closed_at IS NULL AND u.status IN ('in_preparation', 'for_sale', 'reserved')
			ORDER BY 8 DESC, u.id`)
		if err != nil {
			return fmt.Errorf("read the pipeline: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r PipelineRow
			var last, status string
			if err := rows.Scan(&r.UnitID, &r.Car, &status, &r.StepName, &r.StepKind, &r.StepOwner, &last, &r.DaysInStep, &r.DaysSinceBuy,
				&r.StepsDone, &r.StepsTotal, &r.StallDays); err != nil {
				return err
			}
			r.Status = StockStatus(status)
			r.StepState = reconStateOf(&last)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// reconStalledSQL counts the cars that have stood in one step longer than
// reconStallDays, as a scalar for a statement to carry beside another
// count: the queue reads both in one. A count, never a car's cost.
var reconStalledSQL = `(
	WITH steps AS (
	  SELECT s.stock_unit_id, s.position,
	         coalesce(e.kind, 'waiting') AS last, coalesce(e.at, s.created_at) AS since
	  FROM recon_steps s
	  LEFT JOIN LATERAL (SELECT x.kind, x.at FROM recon_step_events x WHERE x.step_id = s.id ORDER BY x.seq DESC LIMIT 1) e ON true
	), cur AS (
	  SELECT DISTINCT ON (stock_unit_id) * FROM steps WHERE last NOT IN ('done', 'skipped') ORDER BY stock_unit_id, position
	)
	SELECT count(*) FROM cur c JOIN stock_units u ON u.id = c.stock_unit_id
	WHERE u.closed_at IS NULL AND u.status IN ('in_preparation', 'for_sale', 'reserved')
	  AND (clock_timestamp() AT TIME ZONE (SELECT timezone FROM shops))::date - (c.since AT TIME ZONE (SELECT timezone FROM shops))::date > ` + reconStallSQL + `
)`

// PostReconStepCost enters what a finished step cost against the car, as a
// reconditioning cost like any other (#164) and tied to the step, so the plan
// can set it beside the price agreed. A step may have several. For whoever
// buys cars, in a shop that sells them.
func PostReconStepCost(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, stepID string, n NewStockCost) (StockCost, error) {
	if !scope.Role.BuysVehicles() {
		return StockCost{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return StockCost{}, err
	}
	if !looksLikeUUID(stepID) {
		return StockCost{}, ErrNotFound
	}
	if n.Kind == "" {
		n.Kind = CostReconditioning
	}
	var out StockCost
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var unit string
		if err := tx.QueryRow(ctx, `SELECT stock_unit_id FROM recon_steps WHERE id = $1`, stepID).Scan(&unit); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		p, err := prepareStockCost(unit, n)
		if err != nil {
			return err
		}
		// The car's cycle is locked, so a step finished and a cost entered
		// at the same moment are told apart.
		if _, err := unitOpenTx(ctx, tx, unit); err != nil {
			return err
		}
		var last *string
		if err := tx.QueryRow(ctx, `SELECT kind FROM recon_step_events WHERE step_id = $1 ORDER BY seq DESC LIMIT 1`, stepID).Scan(&last); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if reconStateOf(last) != ReconDone {
			return errReconNotDone
		}
		if out, err = insertStockCostTx(ctx, tx, scope, unit, p); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO recon_step_costs (shop_id, step_id, cost_entry_id) VALUES ($1, $2, $3)`, scope.ShopID, stepID, out.ID)
		return err
	})
	return out, err
}

// StepStat is what the finished steps of one name, by one owner, add up to:
// how many, how long each took from being started to being finished, and what
// was agreed for them against what they cost.
type StepStat struct {
	Name  string
	Kind  string
	Owner string
	Count int
	// The mean of whole and part days from started to finished.
	MeanDays float64
	// Nil for a role that may not see what a car cost, and when none of the
	// finished steps had a price agreed or a cost entered.
	AgreedMinor *int64
	CostedMinor *int64
}

// ReconStats sums up the steps that have been finished, by name and owner,
// the most often done first: where the days go and with which supplier. A step
// is timed from the move that last started it to the one that finished it, so
// a step done again counts its second time. For whoever buys or sells cars;
// the sums of money only for whoever may see what a car cost.
func ReconStats(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]StepStat, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return nil, err
	}
	var out []StepStat
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH finished AS (
			  SELECT s.id, s.name, s.kind, coalesce(s.owner, '') AS owner, s.agreed_price_minor,
			         e.at AS done_at, e.seq,
			         (SELECT x.at FROM recon_step_events x WHERE x.step_id = s.id AND x.kind IN ('started', 'reopened') AND x.seq < e.seq
			          ORDER BY x.seq DESC LIMIT 1) AS started_at
			  FROM recon_steps s
			  JOIN LATERAL (SELECT x.kind, x.at, x.seq FROM recon_step_events x WHERE x.step_id = s.id ORDER BY x.seq DESC LIMIT 1) e ON e.kind = 'done'
			)
			SELECT f.name, f.kind, f.owner, count(*),
			       coalesce(avg(extract(epoch FROM f.done_at - f.started_at) / 86400.0) FILTER (WHERE f.started_at IS NOT NULL), 0)::float8,
			       CASE WHEN $1 THEN sum(f.agreed_price_minor) END,
			       CASE WHEN $1 THEN sum((SELECT sum(c.net_minor) FROM recon_step_costs l JOIN stock_cost_entries c ON c.id = l.cost_entry_id
			                              WHERE l.step_id = f.id AND NOT EXISTS (SELECT 1 FROM stock_cost_entries r WHERE r.reverses_id = c.id))) END
			FROM finished f
			GROUP BY f.name, f.kind, f.owner
			ORDER BY count(*) DESC, f.name, f.owner`, scope.Role.SeesPurchasePrice())
		if err != nil {
			return fmt.Errorf("read the step statistics: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r StepStat
			if err := rows.Scan(&r.Name, &r.Kind, &r.Owner, &r.Count, &r.MeanDays, &r.AgreedMinor, &r.CostedMinor); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
