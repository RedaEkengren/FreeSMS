package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"

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

// WaitingItem is one thing waiting for somebody.
type WaitingItem struct {
	Kind         string
	JobID        string
	Number       int64
	Registration string
	// How many, where it is several of the same thing on one job.
	Count int
	// The shop's or the technician's own words -- a part's description, the
	// line the customer said yes to. Never a customer's details.
	Text string
}

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
}

// Says is the item's catalogue key.
func (w WaitingItem) Says() string { return waitingSays[w.Kind] }

// Plural reports whether the key counts, for the page to pick N or T.
func (w WaitingItem) Plural() bool {
	switch w.Kind {
	case "finding", "answer", "uncollected":
		return true
	}
	return false
}

// Link is where dealing with it starts.
func (w WaitingItem) Link() string {
	if w.Kind == "part_requested" {
		return "/parts"
	}
	return "/jobs/" + w.JobID
}

// WaitingFor is what is waiting for the caller, by what their role does.
// Every role may ask, and each gets only its own kind of item: a
// technician's list says "the customer said yes", never who.
func WaitingFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]WaitingItem, error) {
	var out []WaitingItem
	add := func(items []WaitingItem, err error) error {
		out = append(out, items...)
		return err
	}
	switch scope.Role {
	case access.RoleTechnician:
		if err := add(technicianWaiting(ctx, pool, scope)); err != nil {
			return nil, err
		}
	case access.RoleParts:
		if err := add(partsWaiting(ctx, pool, scope)); err != nil {
			return nil, err
		}
	case access.RoleServiceAdvisor:
		if err := add(deskWaiting(ctx, pool, scope, false)); err != nil {
			return nil, err
		}
	case access.RoleOwner, access.RoleAdmin:
		// In a small workshop the owner is the counter and the parts desk
		// too, and the one who minds what is late.
		if err := add(deskWaiting(ctx, pool, scope, true)); err != nil {
			return nil, err
		}
		if err := add(partsWaiting(ctx, pool, scope)); err != nil {
			return nil, err
		}
	default:
		return nil, access.ErrForbidden
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// CountWaiting is how many things are waiting, for the header.
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
func technicianWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]WaitingItem, error) {
	var out []WaitingItem
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
			WHERE b.technician_id = $1 AND b.status = 'arrived' AND m.back_on IS NULL`, scope.UserID)
		if err != nil {
			return fmt.Errorf("technician's list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it WaitingItem
			if err := rows.Scan(&it.Kind, &it.JobID, &it.Number, &it.Registration, &it.Text); err != nil {
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
func partsWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]WaitingItem, error) {
	requests, err := OpenPartRequests(ctx, pool, scope)
	if err != nil {
		return nil, err
	}
	numbers := map[string]int64{}
	var out []WaitingItem
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
			                WHERE l.work_order_id = w.id AND l.part_id = p.id), 0)`)
		if err != nil {
			return fmt.Errorf("taken out, not priced: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			it := WaitingItem{Kind: "taken_out_unpriced"}
			if err := rows.Scan(&it.JobID, &it.Number, &it.Registration, &it.Text); err != nil {
				return err
			}
			out = append(out, it)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// The request list does not carry the job's number; one query for
		// all of them rather than one each.
		var ids []string
		for _, r := range requests {
			ids = append(ids, r.WorkOrderID)
		}
		if len(ids) == 0 {
			return nil
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
		out = append(out, WaitingItem{Kind: "part_requested", JobID: r.WorkOrderID, Number: numbers[r.WorkOrderID],
			Registration: r.Registration, Text: r.Description})
	}
	return out, nil
}

// deskWaiting reads the counter's board and says what on it is waiting for
// the counter. The owner also sees what is past its promise.
func deskWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, late bool) ([]WaitingItem, error) {
	board, err := Board(ctx, pool, scope)
	if err != nil {
		return nil, err
	}
	owed, err := Receivables(ctx, pool, scope)
	if err != nil {
		return nil, err
	}
	overdue := map[string]bool{}
	for _, r := range owed {
		if r.Overdue() {
			overdue[r.WorkOrderID] = true
		}
	}
	var out []WaitingItem
	for _, b := range board {
		item := func(kind string, count int) {
			out = append(out, WaitingItem{Kind: kind, JobID: b.ID, Number: b.Number, Registration: b.Registration, Count: count})
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
