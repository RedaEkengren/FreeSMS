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

// Parts bought outside FreeSMS, and what came of them.
//
// Nothing here places an order or talks to a supplier (that stays deferred,
// #25). It is the parts desk's note of what it did: what was ordered, from
// whom, for which jobs, when it is expected, what arrived and what went back.
// A request is a technician's need; an order line is the desk's answer to it;
// a receipt is what physically came. They stay three things.

// DuplicateOrder is returned when the same supplier reference is entered twice.
type DuplicateOrder struct{ ID string }

func (e *DuplicateOrder) Error() string {
	return "workshop: invalid: that order is already entered under the same reference"
}

// Is makes a duplicate an invalid request to anything that asks.
func (e *DuplicateOrder) Is(target error) bool { return target == ErrInvalid }

// OrderLineInput is one thing on an order.
type OrderLineInput struct {
	// The job's request it answers, if it answers one.
	RequestID string
	// The catalogue part, by its number, if it is one.
	PartNumber  string
	Description string
	Quantity    float64
	// What one is expected to cost, in minor units.
	ExpectedUnitCostMinor *int64
	// When this line is expected, if not as the order is.
	ETA *time.Time
}

// OrderInput is an order placed outside FreeSMS.
type OrderInput struct {
	Supplier  string
	Reference string
	Note      string
	// The day it was placed; today when empty.
	PlacedOn time.Time
	ETA      *time.Time
	Lines    []OrderLineInput
}

func dayOf(t time.Time) string { return t.Format("2006-01-02") }

// RecordOrder notes an order placed with a supplier, tying each line to the
// request it is for. One order can supply several jobs; a request can have
// several orders behind it.
func RecordOrder(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, in OrderInput) (string, error) {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	in.Supplier, in.Reference = strings.TrimSpace(in.Supplier), strings.TrimSpace(in.Reference)
	in.Note = strings.TrimSpace(in.Note)
	if in.Supplier == "" || len([]rune(in.Supplier)) > 100 {
		return "", fmt.Errorf("%w: say who it was ordered from", ErrInvalid)
	}
	if len([]rune(in.Reference)) > 100 || len([]rune(in.Note)) > 300 {
		return "", fmt.Errorf("%w: the reference or the note is too long", ErrInvalid)
	}
	if len(in.Lines) == 0 {
		return "", fmt.Errorf("%w: an order has at least one line", ErrInvalid)
	}
	if in.PlacedOn.IsZero() {
		in.PlacedOn = time.Now()
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if in.Reference != "" {
			var other string
			err := tx.QueryRow(ctx, `SELECT id FROM supplier_orders WHERE lower(supplier) = lower($1) AND lower(reference) = lower($2)`,
				in.Supplier, in.Reference).Scan(&other)
			if err == nil {
				return &DuplicateOrder{ID: other}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check reference: %w", err)
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO supplier_orders (shop_id, supplier, reference, placed_on, note, placed_by)
			VALUES ($1, $2, nullif($3, ''), $4, nullif($5, ''), $6) RETURNING id`,
			scope.ShopID, in.Supplier, in.Reference, dayOf(in.PlacedOn), in.Note, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record order: %w", err)
		}
		for i, l := range in.Lines {
			if l.Quantity <= 0 {
				return fmt.Errorf("%w: order at least one of each thing", ErrInvalid)
			}
			desc := strings.TrimSpace(l.Description)
			var req *string
			if l.RequestID != "" {
				if !looksLikeUUID(l.RequestID) {
					return ErrNotFound
				}
				var open bool
				var rdesc string
				if err := tx.QueryRow(ctx, `
					SELECT arrived_at IS NULL AND cancelled_at IS NULL, description FROM part_requests WHERE id = $1 FOR UPDATE`,
					l.RequestID).Scan(&open, &rdesc); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return ErrNotFound
					}
					return fmt.Errorf("read request: %w", err)
				}
				if !open {
					return fmt.Errorf("%w: %q is no longer being waited for", ErrInvalid, rdesc)
				}
				r := l.RequestID
				req = &r
				if desc == "" {
					desc = rdesc
				}
			}
			var partID *string
			if n := strings.TrimSpace(l.PartNumber); n != "" {
				var pid, name string
				if err := tx.QueryRow(ctx, `SELECT id, name FROM parts WHERE lower(number) = lower($1)`, n).Scan(&pid, &name); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return fmt.Errorf("%w: no part in the catalogue is numbered %q", ErrInvalid, n)
					}
					return fmt.Errorf("read part: %w", err)
				}
				partID = &pid
				if desc == "" {
					desc = name
				}
			}
			if desc == "" || len([]rune(desc)) > 200 {
				return fmt.Errorf("%w: say what is ordered", ErrInvalid)
			}
			var lineID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO supplier_order_lines (shop_id, order_id, position, request_id, part_id, description, quantity, expected_unit_cost_minor)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
				scope.ShopID, id, i+1, req, partID, desc, l.Quantity, l.ExpectedUnitCostMinor).Scan(&lineID); err != nil {
				return fmt.Errorf("record order line: %w", err)
			}
			eta := l.ETA
			if eta == nil {
				eta = in.ETA
			}
			if eta != nil {
				if err := addETATx(ctx, tx, scope, lineID, *eta, ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return id, err
}

func addETATx(ctx context.Context, tx pgx.Tx, scope access.Scope, lineID string, eta time.Time, note string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO supplier_line_etas (shop_id, line_id, eta, note, by_user) VALUES ($1, $2, $3, nullif($4, ''), $5)`,
		scope.ShopID, lineID, dayOf(eta), strings.TrimSpace(note), scope.UserID)
	if err != nil {
		return fmt.Errorf("record eta: %w", err)
	}
	return nil
}

// ChangeETA says a line is now expected on another day. The earlier days stay,
// so how often it has slipped can be read.
func ChangeETA(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, lineID string, eta time.Time, note string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(lineID) {
		return ErrNotFound
	}
	if eta.IsZero() {
		return fmt.Errorf("%w: say when it is expected", ErrInvalid)
	}
	if len([]rune(note)) > 300 {
		return fmt.Errorf("%w: keep the note to 300 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var last *string
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT to_char(eta, 'YYYY-MM-DD') FROM supplier_line_etas WHERE line_id = l.id ORDER BY at DESC, id DESC LIMIT 1)
			FROM supplier_order_lines l WHERE l.id = $1 FOR UPDATE`, lineID).Scan(&last); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read line: %w", err)
		}
		if last != nil && *last == dayOf(eta) {
			return nil
		}
		return addETATx(ctx, tx, scope, lineID, eta, note)
	})
}

// DeliveryInput is what physically arrived against an order line.
type DeliveryInput struct {
	LineID   string
	Quantity float64
	// What one cost, if the delivery note says.
	UnitCostMinor *int64
	// The delivery note or label. The same one for the same line is the same
	// delivery entered twice.
	DeliveryRef string
	// "stock": onto the shelf. "direct": straight onto the car without
	// passing the shelf. "customer": the customer brought it.
	Route string
	// What came instead, if it is not what was ordered.
	Substitute string
}

// ReceiveDelivery books a delivery in one step: the receipt, what it did to
// the request it answers, and, when it went onto the shelf, the stock
// movement. A part that goes straight onto the car never touches stock, which
// is how it is not counted twice. Entering the same delivery note again
// changes nothing and says so.
func ReceiveDelivery(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, in DeliveryInput) (receiptID string, already bool, err error) {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return "", false, access.ErrForbidden
	}
	if !looksLikeUUID(in.LineID) {
		return "", false, ErrNotFound
	}
	in.DeliveryRef, in.Substitute = strings.TrimSpace(in.DeliveryRef), strings.TrimSpace(in.Substitute)
	if in.Quantity <= 0 {
		return "", false, fmt.Errorf("%w: something has to have arrived", ErrInvalid)
	}
	switch in.Route {
	case "stock", "direct", "customer":
	default:
		return "", false, fmt.Errorf("%w: onto the shelf, straight to the car, or brought by the customer", ErrInvalid)
	}
	if in.Substitute != "" && in.Route == "stock" {
		return "", false, fmt.Errorf("%w: something else than was ordered cannot go onto the shelf under the ordered part's number; take it straight to the car once it is accepted", ErrInvalid)
	}
	if len([]rune(in.DeliveryRef)) > 100 || len([]rune(in.Substitute)) > 200 {
		return "", false, fmt.Errorf("%w: the reference or description is too long", ErrInvalid)
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var requestID, partID *string
		var expected *int64
		if err := tx.QueryRow(ctx, `
			SELECT request_id::text, part_id::text, expected_unit_cost_minor FROM supplier_order_lines WHERE id = $1 FOR UPDATE`,
			in.LineID).Scan(&requestID, &partID, &expected); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock line: %w", err)
		}
		if in.DeliveryRef != "" {
			err := tx.QueryRow(ctx, `SELECT id FROM supplier_receipts WHERE line_id = $1 AND lower(delivery_ref) = lower($2)`,
				in.LineID, in.DeliveryRef).Scan(&receiptID)
			if err == nil {
				already = true
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check delivery: %w", err)
			}
		}
		if in.Route == "stock" && partID == nil {
			return fmt.Errorf("%w: an uncatalogued part cannot go onto a shelf; take it straight to the car", ErrInvalid)
		}
		cost := in.UnitCostMinor
		if cost == nil {
			cost = expected
		}
		status := any(nil)
		if in.Substitute != "" {
			status = "pending"
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO supplier_receipts (shop_id, line_id, quantity, unit_cost_minor, delivery_ref, route, substitute, substitute_status, received_by)
			VALUES ($1, $2, $3, $4, nullif($5, ''), $6, nullif($7, ''), $8, $9) RETURNING id`,
			scope.ShopID, in.LineID, in.Quantity, cost, in.DeliveryRef, in.Route, in.Substitute, status, scope.UserID).Scan(&receiptID); err != nil {
			return fmt.Errorf("record delivery: %w", err)
		}
		if in.Substitute != "" {
			return nil // counts for nothing until somebody says it will do
		}
		return bookDeliveryTx(ctx, tx, scope, receiptID, in.Route, in.Quantity, cost, requestID, partID)
	})
	return receiptID, already, err
}

// bookDeliveryTx does what a delivery does once it counts: onto the shelf if
// that is where it went, and against the request it answers.
func bookDeliveryTx(ctx context.Context, tx pgx.Tx, scope access.Scope, receiptID, route string, qty float64, cost *int64, requestID, partID *string) error {
	var movementID *string
	if route == "stock" {
		id, err := moveTxID(ctx, tx, scope, Movement{Kind: "received", Quantity: qty, CostMinor: cost, Note: "Supplier delivery"}, *partID, "", "")
		if err != nil {
			return err
		}
		movementID = &id
	}
	var receiptRow *string
	if requestID != nil {
		var open bool
		if err := tx.QueryRow(ctx, `SELECT arrived_at IS NULL AND cancelled_at IS NULL FROM part_requests WHERE id = $1`, *requestID).Scan(&open); err != nil {
			return fmt.Errorf("read request: %w", err)
		}
		// A delivery for a request that is already done, or called off, is
		// still a delivery; it just has no request to finish.
		if open {
			source := "delivery"
			if route == "customer" {
				source = "customer"
			}
			id, _, err := receiveRequestTx(ctx, tx, scope, *requestID, qty, source)
			if err != nil {
				return err
			}
			receiptRow = &id
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE supplier_receipts SET movement_id = $2, request_receipt_id = $3 WHERE id = $1`,
		receiptID, movementID, receiptRow); err != nil {
		return fmt.Errorf("link delivery: %w", err)
	}
	return nil
}

// AcceptSubstitute says what the supplier sent instead will do. It goes
// straight to the car it was for, and counts against the request.
func AcceptSubstitute(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, receiptID string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(receiptID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var status *string
		var qty float64
		var cost *int64
		var requestID *string
		if err := tx.QueryRow(ctx, `
			SELECT r.substitute_status, r.quantity::float8, r.unit_cost_minor, l.request_id::text
			FROM supplier_receipts r JOIN supplier_order_lines l ON l.id = r.line_id
			WHERE r.id = $1 FOR UPDATE OF r`, receiptID).Scan(&status, &qty, &cost, &requestID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock receipt: %w", err)
		}
		if status == nil {
			return fmt.Errorf("%w: that was what was ordered", ErrInvalid)
		}
		if *status == "accepted" {
			return nil
		}
		if *status != "pending" {
			return fmt.Errorf("%w: it was already sent back", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE supplier_receipts SET substitute_status = 'accepted', decided_at = clock_timestamp(), decided_by = $2, route = 'direct' WHERE id = $1`,
			receiptID, scope.UserID); err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		return bookDeliveryTx(ctx, tx, scope, receiptID, "direct", qty, cost, requestID, nil)
	})
}

// RejectSubstitute says what was sent will not do, and plans it going back.
func RejectSubstitute(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, receiptID string) (string, error) {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(receiptID) {
		return "", ErrNotFound
	}
	var returnID string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var status *string
		var qty float64
		if err := tx.QueryRow(ctx, `SELECT substitute_status, quantity::float8 FROM supplier_receipts WHERE id = $1 FOR UPDATE`, receiptID).Scan(&status, &qty); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock receipt: %w", err)
		}
		if status == nil || *status != "pending" {
			return fmt.Errorf("%w: only something waiting for a decision can be turned down", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `UPDATE supplier_receipts SET substitute_status = 'rejected', decided_at = clock_timestamp(), decided_by = $2 WHERE id = $1`,
			receiptID, scope.UserID); err != nil {
			return fmt.Errorf("reject: %w", err)
		}
		return tx.QueryRow(ctx, `
			INSERT INTO supplier_returns (shop_id, receipt_id, quantity, reason, planned_by)
			VALUES ($1, $2, $3, 'substitute_rejected', $4) RETURNING id`, scope.ShopID, receiptID, qty, scope.UserID).Scan(&returnID)
	})
	return returnID, err
}

// CancelOrder calls off what has not come on an order. It is a note to
// ourselves: the supplier has to confirm they stopped it, and until somebody
// records that it stays on the desk's list.
func CancelOrder(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, orderID, note string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(orderID) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if note == "" || len([]rune(note)) > 300 {
		return fmt.Errorf("%w: say why, in 300 characters or fewer", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var cancelled *time.Time
		if err := tx.QueryRow(ctx, `SELECT cancelled_at FROM supplier_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&cancelled); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}
		if cancelled != nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE supplier_orders SET cancelled_at = clock_timestamp(), cancelled_by = $2, cancel_note = $3 WHERE id = $1`,
			orderID, scope.UserID, note); err != nil {
			return fmt.Errorf("cancel order: %w", err)
		}
		_, err := tx.Exec(ctx, `UPDATE supplier_order_lines SET cancelled_at = clock_timestamp() WHERE order_id = $1 AND cancelled_at IS NULL`, orderID)
		return err
	})
}

// ConfirmSupplierCancellation records that the supplier said they stopped it.
func ConfirmSupplierCancellation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, orderID string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(orderID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE supplier_orders SET supplier_cancel_confirmed_at = clock_timestamp(), supplier_cancel_confirmed_by = $2
			WHERE id = $1 AND cancelled_at IS NOT NULL AND supplier_cancel_confirmed_at IS NULL`, orderID, scope.UserID)
		if err != nil {
			return fmt.Errorf("confirm cancellation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var cancelled bool
			if err := tx.QueryRow(ctx, `SELECT cancelled_at IS NOT NULL FROM supplier_orders WHERE id = $1`, orderID).Scan(&cancelled); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("read order: %w", err)
			}
			if !cancelled {
				return fmt.Errorf("%w: it was not cancelled here", ErrInvalid)
			}
		}
		return nil
	})
}

// ReturnInput plans sending something back.
type ReturnInput struct {
	ReceiptID string
	Quantity  float64
	Reason    string
	Note      string
	// What the supplier is expected to credit, in minor units.
	ExpectedCreditMinor *int64
	// The job still needs the part: ask for it again.
	NeedReplacement bool
}

// PlanReturn decides that something that arrived goes back. Nothing leaves the
// shelf yet and no credit is expected as fact; both come later, in their own
// steps.
func PlanReturn(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, in ReturnInput) (string, error) {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(in.ReceiptID) {
		return "", ErrNotFound
	}
	switch in.Reason {
	case "wrong_part", "damaged", "not_needed", "other":
	default:
		return "", fmt.Errorf("%w: wrong part, damaged, not needed or something else", ErrInvalid)
	}
	in.Note = strings.TrimSpace(in.Note)
	if in.Quantity <= 0 || len([]rune(in.Note)) > 300 {
		return "", fmt.Errorf("%w: send back at least one, with a note of 300 characters or fewer", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var qty, planned float64
		var status *string
		var requestID *string
		var desc string
		var jobID *string
		if err := tx.QueryRow(ctx, `
			SELECT r.quantity::float8, r.substitute_status, l.request_id::text, l.description, pr.work_order_id::text,
			       coalesce((SELECT sum(x.quantity) FROM supplier_returns x WHERE x.receipt_id = r.id), 0)::float8
			FROM supplier_receipts r JOIN supplier_order_lines l ON l.id = r.line_id
			LEFT JOIN part_requests pr ON pr.id = l.request_id
			WHERE r.id = $1 FOR UPDATE OF r`, in.ReceiptID).Scan(&qty, &status, &requestID, &desc, &jobID, &planned); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock receipt: %w", err)
		}
		if status != nil && *status == "pending" {
			return fmt.Errorf("%w: decide about the substitute first; turning it down plans its return", ErrInvalid)
		}
		if planned+in.Quantity > qty+1e-9 {
			return fmt.Errorf("%w: only %s came, and %s is already going back", ErrInvalid, trimQty(qty), trimQty(planned))
		}
		var replacement *string
		if in.NeedReplacement {
			if jobID == nil {
				return fmt.Errorf("%w: this delivery was not for a job, so there is no job to ask again for", ErrInvalid)
			}
			rid, err := requestPartsTx(ctx, tx, scope, *jobID, "Replacement: "+desc, in.Quantity)
			if err != nil {
				return err
			}
			replacement = &rid
		}
		return tx.QueryRow(ctx, `
			INSERT INTO supplier_returns (shop_id, receipt_id, quantity, reason, note, expected_credit_minor, planned_by, replacement_request_id)
			VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8) RETURNING id`,
			scope.ShopID, in.ReceiptID, in.Quantity, in.Reason, in.Note, in.ExpectedCreditMinor, scope.UserID, replacement).Scan(&id)
	})
	return id, err
}

func trimQty(q float64) string {
	s := fmt.Sprintf("%.3f", q)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// MarkReturned says the part has left. If it had gone onto the shelf it comes
// off it, in the ledger, at what it cost when it arrived.
func MarkReturned(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, returnID string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(returnID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var returned *time.Time
		var qty float64
		var route string
		var cost *int64
		var partID *string
		var reason string
		if err := tx.QueryRow(ctx, `
			SELECT x.returned_at, x.quantity::float8, r.route, r.unit_cost_minor, l.part_id::text, x.reason
			FROM supplier_returns x JOIN supplier_receipts r ON r.id = x.receipt_id JOIN supplier_order_lines l ON l.id = r.line_id
			WHERE x.id = $1 FOR UPDATE OF x`, returnID).Scan(&returned, &qty, &route, &cost, &partID, &reason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock return: %w", err)
		}
		if returned != nil {
			return nil
		}
		var movement *string
		if route == "stock" && partID != nil {
			id, err := moveTxID(ctx, tx, scope, Movement{Kind: "returned", Quantity: -qty, CostMinor: cost, Note: "Sent back to the supplier"}, *partID, "", "")
			if err != nil {
				return err
			}
			movement = &id
		}
		_, err := tx.Exec(ctx, `UPDATE supplier_returns SET returned_at = clock_timestamp(), returned_by = $2, movement_id = $3 WHERE id = $1`,
			returnID, scope.UserID, movement)
		if err != nil {
			return fmt.Errorf("mark returned: %w", err)
		}
		return nil
	})
}

// ConfirmCredit records that the supplier will pay for a return. It can come
// before the part has left; then the part is credited and still here, which is
// on the desk's list until it goes.
func ConfirmCredit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, returnID string, creditedMinor int64, ref string) error {
	if scope.Role != access.RoleParts && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(returnID) {
		return ErrNotFound
	}
	ref = strings.TrimSpace(ref)
	if creditedMinor < 0 || len([]rune(ref)) > 100 {
		return fmt.Errorf("%w: an amount of zero or more, and a reference of 100 characters or fewer", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var at *time.Time
		var minor *int64
		if err := tx.QueryRow(ctx, `SELECT credited_at, credited_minor FROM supplier_returns WHERE id = $1 FOR UPDATE`, returnID).Scan(&at, &minor); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock return: %w", err)
		}
		if at != nil {
			if minor != nil && *minor == creditedMinor {
				return nil
			}
			return fmt.Errorf("%w: a credit was already recorded for it", ErrInvalid)
		}
		_, err := tx.Exec(ctx, `
			UPDATE supplier_returns SET credited_at = clock_timestamp(), credited_by = $2, credited_minor = $3, credit_ref = nullif($4, '') WHERE id = $1`,
			returnID, scope.UserID, creditedMinor, ref)
		if err != nil {
			return fmt.Errorf("confirm credit: %w", err)
		}
		return nil
	})
}
