package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// SupplyLine is an order line, with what has come against it.
type SupplyLine struct {
	LineID       string
	OrderID      string
	Supplier     string
	Reference    string
	Description  string
	PlacedOn     time.Time
	Ordered      float64
	Received     float64
	ETA          *time.Time
	Slips        int // how many times the day was moved after the first
	ExpectedUnit *int64
	Cancelled    bool

	JobID        string
	JobNumber    int64
	Registration string
}

// Rest is what is still owed on the line.
func (l SupplyLine) Rest() float64 { return max(l.Ordered-l.Received, 0) }

// Late reports whether it was due before today and is still owed.
func (l SupplyLine) Late(today time.Time) bool {
	return !l.Cancelled && l.Rest() > 1e-9 && l.ETA != nil && dayOf(*l.ETA) < dayOf(today)
}

// ExpectedCost renders the expected unit cost.
func (l SupplyLine) ExpectedCost() string {
	if l.ExpectedUnit == nil {
		return ""
	}
	return money.Format(*l.ExpectedUnit)
}

// Receipt is one delivery.
type Receipt struct {
	ID          string
	Quantity    float64
	UnitCost    *int64
	DeliveryRef string
	Route       string
	Substitute  string
	// "pending", "accepted" or "rejected" for a substitute.
	SubstituteStatus string
	At               time.Time
	By               string
	Returns          []ReturnItem
}

// Returnable is how many of it can still be planned back.
func (r Receipt) Returnable() float64 {
	left := r.Quantity
	for _, x := range r.Returns {
		left -= x.Quantity
	}
	return max(left, 0)
}

// ReturnItem is something going back, or gone back.
type ReturnItem struct {
	ID            string
	ReceiptID     string
	Supplier      string
	Description   string
	Quantity      float64
	Reason        string
	Note          string
	ExpectedMinor *int64
	CreditedMinor *int64
	CreditRef     string
	PlannedAt     time.Time
	ReturnedAt    *time.Time
	CreditedAt    *time.Time
}

// Status is where a return stands, as a catalogue key.
func (r ReturnItem) Status() string {
	switch {
	case r.ReturnedAt == nil && r.CreditedAt == nil:
		return "to send back"
	case r.ReturnedAt != nil && r.CreditedAt == nil:
		return "sent back, waiting for the credit"
	case r.ReturnedAt == nil:
		return "credited, the part is still here"
	}
	return "credited"
}

// Open reports whether somebody still has something to do about it.
func (r ReturnItem) Open() bool { return r.ReturnedAt == nil || r.CreditedAt == nil }

// ReasonLabel is why, as a catalogue key.
func (r ReturnItem) ReasonLabel() string {
	switch r.Reason {
	case "wrong_part":
		return "the wrong part"
	case "damaged":
		return "damaged"
	case "not_needed":
		return "no longer needed"
	case "substitute_rejected":
		return "a substitute that would not do"
	}
	return "another reason"
}

// ETAEntry is a day a line was said to be expected.
type ETAEntry struct {
	ETA  time.Time
	Note string
	By   string
	At   time.Time
}

// OrderLineView is a line with its history.
type OrderLineView struct {
	SupplyLine
	Receipts []Receipt
	ETAs     []ETAEntry
}

// OrderView is an order with everything under it.
type OrderView struct {
	ID                string
	Supplier          string
	Reference         string
	PlacedOn          time.Time
	Note              string
	By                string
	CancelledAt       *time.Time
	CancelNote        string
	SupplierConfirmed *time.Time
	Lines             []OrderLineView
}

// CostDifference is a delivery that cost other than expected.
type CostDifference struct {
	Supplier    string
	Description string
	OrderID     string
	Quantity    float64
	Expected    int64
	Actual      int64
	At          time.Time
}

// PerUnit is how much more (or less) each cost.
func (c CostDifference) PerUnit() int64 { return c.Actual - c.Expected }

// Total is what the difference comes to over the quantity delivered.
func (c CostDifference) Total() int64 {
	return int64(float64(c.PerUnit())*c.Quantity + 0.5*sign(c.PerUnit()))
}

func sign(n int64) float64 {
	if n < 0 {
		return -1
	}
	return 1
}

// PendingSubstitute is something sent instead, waiting for a decision.
type PendingSubstitute struct {
	ReceiptID    string
	OrderID      string
	Supplier     string
	Ordered      string
	Substitute   string
	Quantity     float64
	At           time.Time
	Registration string
}

// UnconfirmedCancellation is an order cancelled here that the supplier has not
// confirmed stopping.
type UnconfirmedCancellation struct {
	OrderID     string
	Supplier    string
	Reference   string
	CancelledAt time.Time
	Note        string
	// What was still owed when it was cancelled.
	Open int
}

// SupplyDesk is what the parts desk has to act on.
type SupplyDesk struct {
	Unordered     []PartRequest
	Open          []SupplyLine
	Overdue       []SupplyLine
	Substitutes   []PendingSubstitute
	Returns       []ReturnItem
	CostDiffs     []CostDifference
	Cancellations []UnconfirmedCancellation
}

// Empty reports that there is nothing to act on.
func (d SupplyDesk) Empty() bool {
	return len(d.Unordered) == 0 && len(d.Open) == 0 && len(d.Substitutes) == 0 && len(d.Returns) == 0 &&
		len(d.CostDiffs) == 0 && len(d.Cancellations) == 0
}

const supplyLineSelect = `
	SELECT l.id::text, o.id::text, o.supplier, coalesce(o.reference, ''), l.description, o.placed_on::timestamptz,
	       l.quantity::float8,
	       coalesce((SELECT sum(r.quantity) FROM supplier_receipts r
	                  WHERE r.line_id = l.id AND (r.substitute IS NULL OR r.substitute_status = 'accepted')), 0)::float8,
	       (SELECT e.eta::timestamptz FROM supplier_line_etas e WHERE e.line_id = l.id ORDER BY e.at DESC, e.id DESC LIMIT 1),
	       greatest((SELECT count(*) FROM supplier_line_etas e WHERE e.line_id = l.id) - 1, 0)::int,
	       l.expected_unit_cost_minor, l.cancelled_at IS NOT NULL,
	       coalesce(w.id::text, ''), coalesce(w.number, 0), coalesce(vr.registration, '')
	FROM supplier_order_lines l
	JOIN supplier_orders o ON o.id = l.order_id
	LEFT JOIN part_requests pr ON pr.id = l.request_id
	LEFT JOIN work_orders w ON w.id = pr.work_order_id
	LEFT JOIN vehicle_registrations vr ON vr.vehicle_id = w.vehicle_id AND vr.valid_to IS NULL`

func scanSupplyLine(r pgx.Row, l *SupplyLine) error {
	return r.Scan(&l.LineID, &l.OrderID, &l.Supplier, &l.Reference, &l.Description, &l.PlacedOn,
		&l.Ordered, &l.Received, &l.ETA, &l.Slips, &l.ExpectedUnit, &l.Cancelled, &l.JobID, &l.JobNumber, &l.Registration)
}

// SupplyDeskFor reads what the parts desk has to act on as of today.
func SupplyDeskFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, today time.Time) (SupplyDesk, error) {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return SupplyDesk{}, access.ErrForbidden
	}
	var d SupplyDesk
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, supplyLineSelect+`
			WHERE l.cancelled_at IS NULL
			ORDER BY 10 NULLS LAST, o.placed_on`)
		if err != nil {
			return fmt.Errorf("list order lines: %w", err)
		}
		for rows.Next() {
			var l SupplyLine
			if err := scanSupplyLine(rows, &l); err != nil {
				rows.Close()
				return fmt.Errorf("scan order line: %w", err)
			}
			if l.Rest() > 1e-9 {
				d.Open = append(d.Open, l)
				if l.Late(today) {
					d.Overdue = append(d.Overdue, l)
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// Waiting for, and nobody has ordered it.
		reqs, err := tx.Query(ctx, `
			SELECT pr.id, pr.work_order_id, pr.description, coalesce(p.display_name, ''), pr.requested_at,
			       coalesce(vr.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''), pr.quantity::float8,
			       coalesce((SELECT sum(rc.quantity) FROM part_request_receipts rc WHERE rc.request_id = pr.id), 0)::float8
			FROM part_requests pr
			JOIN work_orders w ON w.id = pr.work_order_id AND w.state NOT IN ('closed', 'cancelled')
			JOIN vehicles v ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations vr ON vr.vehicle_id = v.id AND vr.valid_to IS NULL
			LEFT JOIN users u ON u.id = pr.requested_by LEFT JOIN people p ON p.id = u.person_id
			WHERE pr.arrived_at IS NULL AND pr.cancelled_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM supplier_order_lines l WHERE l.request_id = pr.id AND l.cancelled_at IS NULL)
			ORDER BY pr.requested_at`)
		if err != nil {
			return fmt.Errorf("list unordered: %w", err)
		}
		for reqs.Next() {
			var p PartRequest
			if err := reqs.Scan(&p.ID, &p.WorkOrderID, &p.Description, &p.RequestedBy, &p.RequestedAt,
				&p.Registration, &p.Make, &p.Model, &p.Quantity, &p.Received); err != nil {
				reqs.Close()
				return fmt.Errorf("scan unordered: %w", err)
			}
			d.Unordered = append(d.Unordered, p)
		}
		reqs.Close()
		if err := reqs.Err(); err != nil {
			return err
		}

		subs, err := tx.Query(ctx, `
			SELECT r.id::text, o.id::text, o.supplier, l.description, r.substitute, r.quantity::float8, r.received_at,
			       coalesce(vr.registration, '')
			FROM supplier_receipts r JOIN supplier_order_lines l ON l.id = r.line_id JOIN supplier_orders o ON o.id = l.order_id
			LEFT JOIN part_requests pr ON pr.id = l.request_id LEFT JOIN work_orders w ON w.id = pr.work_order_id
			LEFT JOIN vehicle_registrations vr ON vr.vehicle_id = w.vehicle_id AND vr.valid_to IS NULL
			WHERE r.substitute_status = 'pending' ORDER BY r.received_at`)
		if err != nil {
			return fmt.Errorf("list substitutes: %w", err)
		}
		for subs.Next() {
			var s PendingSubstitute
			if err := subs.Scan(&s.ReceiptID, &s.OrderID, &s.Supplier, &s.Ordered, &s.Substitute, &s.Quantity, &s.At, &s.Registration); err != nil {
				subs.Close()
				return fmt.Errorf("scan substitute: %w", err)
			}
			d.Substitutes = append(d.Substitutes, s)
		}
		subs.Close()
		if err := subs.Err(); err != nil {
			return err
		}

		if d.Returns, err = returnsTx(ctx, tx, "WHERE x.returned_at IS NULL OR x.credited_at IS NULL", nil); err != nil {
			return err
		}

		diffs, err := tx.Query(ctx, `
			SELECT o.supplier, l.description, o.id::text, r.quantity::float8, l.expected_unit_cost_minor, r.unit_cost_minor, r.received_at
			FROM supplier_receipts r JOIN supplier_order_lines l ON l.id = r.line_id JOIN supplier_orders o ON o.id = l.order_id
			WHERE l.expected_unit_cost_minor IS NOT NULL AND r.unit_cost_minor IS NOT NULL
			  AND l.expected_unit_cost_minor <> r.unit_cost_minor
			  AND (r.substitute IS NULL OR r.substitute_status = 'accepted')
			ORDER BY r.received_at DESC LIMIT 50`)
		if err != nil {
			return fmt.Errorf("list cost differences: %w", err)
		}
		for diffs.Next() {
			var c CostDifference
			if err := diffs.Scan(&c.Supplier, &c.Description, &c.OrderID, &c.Quantity, &c.Expected, &c.Actual, &c.At); err != nil {
				diffs.Close()
				return fmt.Errorf("scan cost difference: %w", err)
			}
			d.CostDiffs = append(d.CostDiffs, c)
		}
		diffs.Close()
		if err := diffs.Err(); err != nil {
			return err
		}

		cans, err := tx.Query(ctx, `
			SELECT o.id::text, o.supplier, coalesce(o.reference, ''), o.cancelled_at, coalesce(o.cancel_note, ''),
			       (SELECT count(*) FROM supplier_order_lines l WHERE l.order_id = o.id
			         AND l.quantity > coalesce((SELECT sum(r.quantity) FROM supplier_receipts r WHERE r.line_id = l.id
			                                     AND (r.substitute IS NULL OR r.substitute_status = 'accepted')), 0))::int
			FROM supplier_orders o WHERE o.cancelled_at IS NOT NULL AND o.supplier_cancel_confirmed_at IS NULL
			ORDER BY o.cancelled_at`)
		if err != nil {
			return fmt.Errorf("list cancellations: %w", err)
		}
		defer cans.Close()
		for cans.Next() {
			var c UnconfirmedCancellation
			if err := cans.Scan(&c.OrderID, &c.Supplier, &c.Reference, &c.CancelledAt, &c.Note, &c.Open); err != nil {
				return fmt.Errorf("scan cancellation: %w", err)
			}
			d.Cancellations = append(d.Cancellations, c)
		}
		return cans.Err()
	})
	return d, err
}

func returnsTx(ctx context.Context, tx pgx.Tx, where string, args []any) ([]ReturnItem, error) {
	rows, err := tx.Query(ctx, `
		SELECT x.id::text, x.receipt_id::text, o.supplier, l.description, x.quantity::float8, x.reason, coalesce(x.note, ''),
		       x.expected_credit_minor, x.credited_minor, coalesce(x.credit_ref, ''), x.planned_at, x.returned_at, x.credited_at
		FROM supplier_returns x JOIN supplier_receipts r ON r.id = x.receipt_id
		JOIN supplier_order_lines l ON l.id = r.line_id JOIN supplier_orders o ON o.id = l.order_id `+where+`
		ORDER BY x.planned_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("list returns: %w", err)
	}
	defer rows.Close()
	var out []ReturnItem
	for rows.Next() {
		var x ReturnItem
		if err := rows.Scan(&x.ID, &x.ReceiptID, &x.Supplier, &x.Description, &x.Quantity, &x.Reason, &x.Note,
			&x.ExpectedMinor, &x.CreditedMinor, &x.CreditRef, &x.PlannedAt, &x.ReturnedAt, &x.CreditedAt); err != nil {
			return nil, fmt.Errorf("scan return: %w", err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// OrderByID reads an order with its lines, deliveries and returns.
func OrderByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (OrderView, error) {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return OrderView{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return OrderView{}, ErrNotFound
	}
	var o OrderView
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT o.id::text, o.supplier, coalesce(o.reference, ''), o.placed_on::timestamptz, coalesce(o.note, ''), coalesce(p.display_name, ''),
			       o.cancelled_at, coalesce(o.cancel_note, ''), o.supplier_cancel_confirmed_at
			FROM supplier_orders o LEFT JOIN users u ON u.id = o.placed_by LEFT JOIN people p ON p.id = u.person_id
			WHERE o.id = $1`, id).Scan(&o.ID, &o.Supplier, &o.Reference, &o.PlacedOn, &o.Note, &o.By, &o.CancelledAt, &o.CancelNote, &o.SupplierConfirmed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read order: %w", err)
		}
		rows, err := tx.Query(ctx, supplyLineSelect+` WHERE o.id = $1 ORDER BY l.position`, id)
		if err != nil {
			return fmt.Errorf("read order lines: %w", err)
		}
		for rows.Next() {
			var v OrderLineView
			if err := scanSupplyLine(rows, &v.SupplyLine); err != nil {
				rows.Close()
				return fmt.Errorf("scan order line: %w", err)
			}
			o.Lines = append(o.Lines, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range o.Lines {
			l := &o.Lines[i]
			etas, err := tx.Query(ctx, `
				SELECT e.eta::timestamptz, coalesce(e.note, ''), coalesce(p.display_name, ''), e.at
				FROM supplier_line_etas e LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
				WHERE e.line_id = $1 ORDER BY e.at, e.id`, l.LineID)
			if err != nil {
				return fmt.Errorf("read etas: %w", err)
			}
			for etas.Next() {
				var e ETAEntry
				if err := etas.Scan(&e.ETA, &e.Note, &e.By, &e.At); err != nil {
					etas.Close()
					return fmt.Errorf("scan eta: %w", err)
				}
				l.ETAs = append(l.ETAs, e)
			}
			etas.Close()
			recs, err := tx.Query(ctx, `
				SELECT r.id::text, r.quantity::float8, r.unit_cost_minor, coalesce(r.delivery_ref, ''), r.route,
				       coalesce(r.substitute, ''), coalesce(r.substitute_status, ''), r.received_at, coalesce(p.display_name, '')
				FROM supplier_receipts r LEFT JOIN users u ON u.id = r.received_by LEFT JOIN people p ON p.id = u.person_id
				WHERE r.line_id = $1 ORDER BY r.received_at, r.id`, l.LineID)
			if err != nil {
				return fmt.Errorf("read receipts: %w", err)
			}
			for recs.Next() {
				var r Receipt
				if err := recs.Scan(&r.ID, &r.Quantity, &r.UnitCost, &r.DeliveryRef, &r.Route, &r.Substitute, &r.SubstituteStatus, &r.At, &r.By); err != nil {
					recs.Close()
					return fmt.Errorf("scan receipt: %w", err)
				}
				l.Receipts = append(l.Receipts, r)
			}
			recs.Close()
			for j := range l.Receipts {
				if l.Receipts[j].Returns, err = returnsTx(ctx, tx, "WHERE x.receipt_id = $1", []any{l.Receipts[j].ID}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return o, err
}
