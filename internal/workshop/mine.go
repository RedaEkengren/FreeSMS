package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// What is waiting for the person signed in: the few things that are theirs to
// act on, rather than everything that happened.
//
// Nothing here is stored. Each item is a fact about the data -- a part has
// arrived and the technician has not been back on the job since, a car is
// ready and nobody has reached the customer -- and it stops being true when
// somebody deals with it. A list of notifications would need emptying by
// hand, which is a second job, and would drift from the data it described;
// this cannot, and when one person at the counter rings the customer the item
// leaves both of their lists.

// ActionItem is one thing waiting for somebody (#158): the one queue,
// whatever modules the shop runs. Each module, and Core, supplies its
// items through an ActionProvider; nothing is stored, and an item leaves
// every list once somebody deals with what made it true.
type ActionItem struct {
	Kind string
	// The module whose business it is; empty for Core's.
	Module access.Module
	// What it is about: "job", "booking", "document", or a page of its own
	// ("outbox", "payment_flows", "schedule"), and its id where it has one.
	SubjectKind string
	SubjectID   string
	// A job's number and the car's plate, where it is about a job.
	Number       int64
	Registration string
	// How many, where it is several of the same thing.
	Count int
	// The shop's, the technician's or the system's own words -- a part's
	// description, the line the customer said yes to, a template's name.
	// Never a customer's details, and never a price.
	Text string
	// How soon: UrgencyNow before UrgencySoon. Due, where it has one, then
	// orders within the same urgency.
	Urgency Urgency
	Due     time.Time
}

// Urgency is how soon an item wants somebody.
type Urgency int

// The urgencies, most urgent first.
const (
	// UrgencyNow is something already gone wrong: a call to another service
	// given up.
	UrgencyNow Urgency = iota
	// UrgencySoon is everything else waiting for a person.
	UrgencySoon
)

// ActionProvider is how a module puts its items on the queue: its module
// (empty for Core), whose providers are called only while the shop runs it,
// and a function that asks only what the caller's role may ask -- each
// checks the role itself, where it reads.
type ActionProvider struct {
	Module access.Module
	Name   string
	Items  func(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error)
}

// actionProviders are every provider there is, in the order their items
// come when nothing else orders them. Dealer and Rental add theirs with the
// issues that build what they wait on.
var actionProviders = []ActionProvider{
	{Name: "schedule", Items: scheduleChanged},
	// Money taken in person (#156), for whoever runs the shop.
	{Name: "payment flows", Items: uncheckedFlowsWaiting},
	// Documents waiting for a signature (#150), for whoever makes them.
	{Name: "documents", Items: unsignedDocumentsWaiting},
	// Calls to the outside world a person has to look at (#154).
	{Name: "outbox", Items: outboxWaiting},
	{Module: access.Workshop, Name: "workshop", Items: workshopWaiting},
}

// IncompleteError is WaitingFor's when a provider failed: the items of the
// rest are still returned, and the page says some could not be loaded.
type IncompleteError struct {
	Failed []error
}

func (e *IncompleteError) Error() string {
	return fmt.Sprintf("workshop: %d of the queue's providers failed: %v", len(e.Failed), errors.Join(e.Failed...))
}

// Unwrap is the providers' errors.
func (e *IncompleteError) Unwrap() []error { return e.Failed }

// Waiting kinds, and what they say, as catalogue keys. A key with %d takes
// Count; with %s, Text.
var waitingSays = map[string]string{
	"part_arrived":       "The part you asked for has arrived: %s",
	"customer_yes":       "The customer said yes, and it is on the job: %s",
	"booked_on_you":      "Booked on you, and the car is in",
	"finding":            "%d thing noticed, not priced",
	"answer":             "%d thing approved by the customer, not priced",
	"not_told":           "Ready, and the customer has not been told",
	"uncollected":        "Uncollected for %d day",
	"overdue":            "unpaid, overdue",
	"late":               "Past its promised time",
	"part_requested":     "A part was asked for: %s",
	"taken_out_unpriced": "Taken out, not priced: %s",
	"schedule_changed":   "Your schedule has changed",
	"assigned_to_you":    "Put on you",
	"plan_changed":       "The plan for your job has moved",
	"unstaffed":          "Nobody is on it",
	"tech_away":          "%s is away; it needs somebody else",
	"plan_overdue":       "The plan ran out and the job is still open",
	"booking_not_told":   "The customer was not reached about a booking: %s",
	"outsourced_overdue": "Due back from a supplier and not here: %s",
	"outsourced_invoice": "A supplier invoice nobody has handed on: %s",
	"flow_unchecked":     "A way of taking money in person is in use and not checked against the cash register rules: %s",
	"unsigned":           "Waiting for a signature: %s",
	"outbox_given_up":    "%d call to another service was given up",
	"outbox_manual":      "%d call to another service is a task to do by hand",
}

// Says is the item's catalogue key.
func (w ActionItem) Says() string { return waitingSays[w.Kind] }

// Plural reports whether the key counts, for the page to pick N or T.
func (w ActionItem) Plural() bool {
	switch w.Kind {
	case "finding", "answer", "uncollected", "outbox_given_up", "outbox_manual":
		return true
	}
	return false
}

// Link is where dealing with it starts: its subject's page.
func (w ActionItem) Link() string {
	switch {
	case w.Kind == "part_requested":
		return "/parts"
	case w.SubjectKind == "booking":
		return "/bookings/" + w.SubjectID
	case w.SubjectKind == "document":
		return "/contracts/" + w.SubjectID
	case w.SubjectKind == "schedule":
		return "/schedule"
	case w.SubjectKind == "payment_flows":
		return "/payment-flows"
	case w.SubjectKind == "outbox":
		return "/outbox"
	}
	return "/jobs/" + w.SubjectID
}

// WaitingFor is what is waiting for the caller, by what their role does,
// from every provider of a module the shop runs and Core's. Every role may
// ask, and each gets only its own kind of item: a technician's list says
// "the customer said yes", never who.
//
// A provider that fails leaves its items out rather than the whole list:
// the rest are returned with an *IncompleteError naming what failed.
//
// One person with several roles gets the union, each item once. The order
// is by urgency, then by when it is due, then by module -- Core's first,
// then the workshop's, the dealer's and the rental desk's -- then by the
// job's number, and otherwise as the providers gave them.
func WaitingFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	var out []ActionItem
	var failed []error
	seen := map[ActionItem]bool{}
	for _, p := range actionProviders {
		// A module the shop has switched off puts nothing in it.
		if p.Module != "" && !scope.Uses(p.Module) {
			continue
		}
		items, err := p.Items(ctx, pool, scope)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", p.Name, err))
			continue
		}
		for _, it := range items {
			if !seen[it] {
				seen[it] = true
				out = append(out, it)
			}
		}
	}
	sortActions(out)
	if failed != nil {
		return out, &IncompleteError{Failed: failed}
	}
	return out, nil
}

// moduleRank orders the modules' items: Core's, then each module's.
var moduleRank = map[access.Module]int{"": 0, access.Workshop: 1, access.Dealer: 2, access.Rental: 3}

func sortActions(items []ActionItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case a.Urgency != b.Urgency:
			return a.Urgency < b.Urgency
		case !a.Due.Equal(b.Due):
			// Something with a due time before something without.
			return !a.Due.IsZero() && (b.Due.IsZero() || a.Due.Before(b.Due))
		case moduleRank[a.Module] != moduleRank[b.Module]:
			return moduleRank[a.Module] < moduleRank[b.Module]
		}
		return a.Number < b.Number
	})
}

// workshopWaiting is the workshop's items: what each role held is waiting
// for at the counter, the parts desk, the lift and with a supplier.
func workshopWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	var out []ActionItem
	add := func(items []ActionItem, err error) error {
		out = append(out, items...)
		return err
	}
	// What each role held is waiting for, once each: a parts person who also
	// turns spanners gets the parts desk's and the technician's. A role with
	// no part in the workshop (car sales, rental) gets none of it.
	roles := scope.Role
	runsIt := roles.RunsTheShop()
	switch {
	case runsIt:
		// In a small workshop the owner is the counter and the parts desk
		// too, and the one who minds what is late.
		if err := add(deskWaiting(ctx, pool, scope, true)); err != nil {
			return nil, err
		}
	case roles.Has(access.RoleServiceAdvisor):
		if err := add(deskWaiting(ctx, pool, scope, false)); err != nil {
			return nil, err
		}
	}
	if runsIt || roles.Has(access.RoleParts) {
		if err := add(partsWaiting(ctx, pool, scope)); err != nil {
			return nil, err
		}
	}
	if roles.Has(access.RoleTechnician) {
		if err := add(technicianWaiting(ctx, pool, scope)); err != nil {
			return nil, err
		}
	}
	// Everybody above the shop floor chases a supplier.
	if roles.SeesParts() {
		if err := add(outsourcedWaiting(ctx, pool, scope)); err != nil {
			return nil, err
		}
	}
	for i := range out {
		out[i].Module, out[i].Urgency = access.Workshop, UrgencySoon
		if out[i].SubjectKind == "" {
			out[i].SubjectKind = "job"
		}
	}
	return out, nil
}

// CountWaiting is how many things are waiting, for the header and its
// sound: the whole queue, every module's. With a provider failed it is
// what loaded, and the error says what did not.
func CountWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (int, error) {
	items, err := WaitingFor(ctx, pool, scope)
	return len(items), err
}

// AlertSound reports whether the caller asked to hear when something new is
// waiting.
func AlertSound(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (bool, error) {
	var on bool
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT alert_sound FROM users WHERE id = $1`, scope.UserID).Scan(&on)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return on, err
}

// SetAlertSound is the caller's own choice, and nobody else's.
func SetAlertSound(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, on bool) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET alert_sound = $2 WHERE id = $1`, scope.UserID, on)
		if err != nil {
			return fmt.Errorf("save alert sound: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// openJob is the states in which a job is still being worked on.
const openJob = `w.state IN ('draft', 'estimated', 'awaiting_approval', 'approved', 'in_progress', 'awaiting_parts')`

// technicianWaiting: a part the technician asked for has come, the customer
// said yes to something they found and it is now on the job, or a car booked
// on them has come in. Each stops once they clock on to the job: picking it
// up is dealing with it.
func technicianWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	var out []ActionItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH mine AS (
			  SELECT w.id, w.number, coalesce(r.registration, '') AS registration,
			         (SELECT max(t.started_at) FROM time_entries t
			          WHERE t.work_order_id = w.id AND t.user_id = $1) AS back_on
			  FROM work_orders w
			  JOIN vehicles v ON v.id = w.vehicle_id
			  LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			  WHERE `+openJob+`
			)
			SELECT 'part_arrived', m.id, m.number, m.registration, pr.description
			FROM mine m JOIN part_requests pr ON pr.work_order_id = m.id
			WHERE pr.requested_by = $1 AND pr.cancelled_at IS NULL
			  AND coalesce((SELECT max(rc.received_at) FROM part_request_receipts rc WHERE rc.request_id = pr.id), pr.arrived_at)
			      > coalesce(m.back_on, '-infinity')
			UNION ALL
			SELECT 'customer_yes', m.id, m.number, m.registration, l.description
			FROM mine m
			JOIN work_order_lines l      ON l.work_order_id = m.id AND l.customer_decision_id IS NOT NULL
			JOIN inspection_decisions d  ON d.id = l.customer_decision_id
			JOIN inspection_items it     ON it.id = d.item_id
			JOIN inspections i           ON i.id = it.inspection_id
			WHERE i.performed_by = $1 AND l.created_at > coalesce(m.back_on, '-infinity')
			UNION ALL
			SELECT 'booked_on_you', m.id, m.number, m.registration, ''
			FROM mine m JOIN bookings b ON b.work_order_id = m.id
			WHERE b.technician_id = $1 AND b.status = 'arrived' AND m.back_on IS NULL
			UNION ALL
			SELECT 'assigned_to_you', m.id, m.number, m.registration, ''
			FROM mine m JOIN job_assignments a ON a.work_order_id = m.id AND a.user_id = $1 AND a.released_at IS NULL
			WHERE a.assigned_by <> $1 AND a.assigned_at > coalesce(m.back_on, '-infinity')
			  AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.work_order_id = m.id AND b.technician_id = $1 AND b.status = 'arrived')
			UNION ALL
			SELECT 'plan_changed', m.id, m.number, m.registration, ''
			FROM mine m
			JOIN job_assignments a ON a.work_order_id = m.id AND a.user_id = $1 AND a.released_at IS NULL
			JOIN job_plans p ON p.work_order_id = m.id AND `+latestPlan+`
			WHERE p.version > 1 AND p.by_user <> $1 AND p.at > a.assigned_at AND p.at > coalesce(m.back_on, '-infinity')`, scope.UserID)
		if err != nil {
			return fmt.Errorf("technician's list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it ActionItem
			if err := rows.Scan(&it.Kind, &it.SubjectID, &it.Number, &it.Registration, &it.Text); err != nil {
				return fmt.Errorf("scan technician's item: %w", err)
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// partsWaiting: parts asked for and not yet here, and parts taken out to a
// job that nobody has priced -- the one a shop fits and never bills.
func partsWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	requests, err := OpenPartRequests(ctx, pool, scope)
	if err != nil {
		return nil, err
	}
	numbers := map[string]int64{}
	var out []ActionItem
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT w.id, w.number, coalesce(r.registration, ''), p.number || ' · ' || p.name
			FROM work_orders w
			JOIN vehicles v ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			JOIN parts p ON p.id IN (SELECT m.part_id FROM stock_movements m
			                         WHERE m.work_order_id = w.id AND m.kind IN ('consumed', 'put_back'))
			WHERE w.state NOT IN ('closed', 'cancelled')
			  AND coalesce((SELECT -sum(m.quantity) FROM stock_movements m
			                WHERE m.work_order_id = w.id AND m.part_id = p.id AND m.kind IN ('consumed', 'put_back')), 0)
			    > coalesce((SELECT sum(l.quantity) FROM work_order_lines l
			                WHERE l.work_order_id = w.id AND l.part_id = p.id AND l.withdrawn_at IS NULL), 0)`)
		if err != nil {
			return fmt.Errorf("taken out, not priced: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			it := ActionItem{Kind: "taken_out_unpriced"}
			if err := rows.Scan(&it.SubjectID, &it.Number, &it.Registration, &it.Text); err != nil {
				return err
			}
			out = append(out, it)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// The request list does not carry the job's number; one query for
		// all of them rather than one each -- asked with none as well, so
		// the list costs the same whether anything is on order or not.
		ids := []string{}
		for _, r := range requests {
			ids = append(ids, r.WorkOrderID)
		}
		nrows, err := tx.Query(ctx, `SELECT id, number FROM work_orders WHERE id = ANY($1::uuid[])`, ids)
		if err != nil {
			return err
		}
		defer nrows.Close()
		for nrows.Next() {
			var id string
			var n int64
			if err := nrows.Scan(&id, &n); err != nil {
				return err
			}
			numbers[id] = n
		}
		return nrows.Err()
	})
	if err != nil {
		return nil, err
	}
	for _, r := range requests {
		out = append(out, ActionItem{Kind: "part_requested", SubjectID: r.WorkOrderID, Number: numbers[r.WorkOrderID],
			Registration: r.Registration, Text: r.Description})
	}
	return out, nil
}

// deskWaiting reads the counter's board and says what on it is waiting for
// the counter. The owner also sees what is past its promise.
func deskWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, late bool) ([]ActionItem, error) {
	desk, err := deskFor(ctx, pool, scope)
	if err != nil {
		return nil, err
	}
	if desk.OwedErr != nil {
		return nil, desk.OwedErr
	}
	if desk.BacklogErr != nil {
		return nil, desk.BacklogErr
	}
	board, backlog := desk.Board, desk.Backlog
	overdue := map[string]bool{}
	for _, r := range desk.Owed {
		if r.Overdue() {
			overdue[r.WorkOrderID] = true
		}
	}
	var out []ActionItem
	untold, err := BookingsNeedingAttention(ctx, pool, scope)
	if err != nil {
		return nil, err
	}
	for _, b := range untold {
		out = append(out, ActionItem{Kind: "booking_not_told", SubjectKind: "booking", SubjectID: b.BookingID, Text: b.What})
	}
	for _, b := range backlog {
		kind := map[string]string{"unstaffed": "unstaffed", "away": "tech_away", "overdue": "plan_overdue"}[b.Kind]
		// A draft nobody is on yet is the intake's business, not a queue
		// item; work that has been agreed and has nobody is.
		if b.Kind == "unstaffed" && b.State != StateApproved && b.State != StateInProgress && b.State != StateAwaitingParts {
			continue
		}
		out = append(out, ActionItem{Kind: kind, SubjectID: b.JobID, Number: b.Number, Registration: b.Registration, Text: b.Who})
	}
	for _, b := range board {
		item := func(kind string, count int) {
			out = append(out, ActionItem{Kind: kind, SubjectID: b.ID, Number: b.Number, Registration: b.Registration, Count: count})
		}
		if b.OpenFindings > 0 {
			item("finding", b.OpenFindings)
		}
		if b.ApprovedUnpriced > 0 {
			item("answer", b.ApprovedUnpriced)
		}
		if b.State == "ready" && b.ToldAt == nil {
			item("not_told", 0)
		}
		if d := b.WaitingDays(); d > 0 {
			item("uncollected", d)
		}
		if overdue[b.ID] {
			item("overdue", 0)
		}
		if late && b.Late() {
			item("late", 0)
		}
	}
	return out, nil
}

// scheduleChanged: somebody else changed the caller's rota, a day of theirs
// still to come, or recorded them away, since they last looked at their own
// schedule. A rota that changes silently is how two people turn up for one
// shift, or none.
func scheduleChanged(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	var changed bool
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			WITH seen AS (SELECT coalesce(schedule_seen_at, '-infinity') AS at FROM users WHERE id = $1)
			SELECT EXISTS (SELECT 1 FROM rotas r, seen
			               WHERE r.user_id = $1 AND r.created_by <> $1 AND r.created_at > seen.at)
			    OR EXISTS (SELECT 1 FROM shift_changes c, seen
			               WHERE c.user_id = $1 AND c.created_by <> $1 AND c.day >= current_date AND c.created_at > seen.at)
			    OR EXISTS (SELECT 1 FROM staff_absences a, seen
			               WHERE a.user_id = $1 AND a.created_by <> $1 AND a.day >= current_date AND a.created_at > seen.at)`,
			scope.UserID).Scan(&changed)
	})
	if err != nil || !changed {
		return nil, err
	}
	return []ActionItem{{Kind: "schedule_changed", SubjectKind: "schedule", Urgency: UrgencySoon}}, nil
}
