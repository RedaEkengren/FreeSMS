package workshop

import (
	"context"
	"encoding/json"
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

// Audit events (#151): who did what, when, from what to what and why, for
// every transition that moves money, ownership or responsibility, read from
// one place -- the audit_trail view (0077). A transition that already has a
// home kept by the database -- a job's state change, a module switched, a
// payment, a signature -- is read from there and not written twice; the
// rest is written to audit_events, in the transition's own transaction, by
// the operation that makes it.

// AuditTransition is one transition that must leave an event: what it is
// called, what it is about, where it is kept, and whether it is made only
// with a reason.
type AuditTransition struct {
	Event       string
	Subject     string
	KeptIn      string
	NeedsReason bool
}

// AuditedTransitions is every transition that must leave an event. A test
// makes each happen and reads it back through AuditTrail, so one added here
// and not written, or written and not here, fails. Dealer and Rental add
// theirs as their issues do.
var AuditedTransitions = []AuditTransition{
	{"module.enabled", "shop", "shop_module_changes", false},
	{"module.disabled", "shop", "shop_module_changes", false},
	{"roles.changed", "user", "audit_events", false},
	{"block.overridden", "resource_block", "audit_events", true},
	{"document.signed", "document", "document_signatures", false},
	{"document.superseded", "document", "document_records", false},
	{"payment.recorded", "invoice", "invoice_payments", false},
	{"payment.reversed", "invoice", "invoice_payments", true},
	{"payment.refunded", "deposit", "invoice_payments", true},
	{"payment.applied", "deposit", "invoice_payments", false},
	{"payment.paid_out", "payout", "invoice_payments", false},
	{"payment.matched", "payment", "payment_matches", false},
	{"payment.match_undone", "payment", "payment_matches", false},
	{"payment.overpayment_confirmed", "payment", "audit_events", true},
	{"invoice.unagreed_lines_invoiced", "invoice", "audit_events", true},
	{"identity.read", "identity_record", "audit_events", false},
	{"job.state_changed", "work_order", "work_order_state_changes", false},
	{"record.corrected", "customer", "record_changes", false},
	// What a technician sees (#225): the shop's profile, and one person's
	// exceptions.
	{"technician_view.changed", "shop", "technician_view_changes", true},
	{"technician_view.granted", "user", "technician_view_exceptions", true},
	{"technician_view.ended", "user", "technician_view_exceptions", true},
	// Dealer
	// A car's cycle through stock (#160): opened, and every move of its
	// deal; ending one without a sale says why (MoveStockUnit).
	{"stock_unit.opened", "stock_unit", "audit_events", false},
	{"stock_unit.moved", "stock_unit", "audit_events", false},
	// A car bought (#161), and its VAT eligibility assessed again.
	{"purchase.recorded", "purchase", "audit_events", false},
	{"vat_eligibility.corrected", "purchase", "audit_events", true},
	// Money let leave for a car before its checks passed (#162).
	{"stock_checks.overridden", "stock_unit", "audit_events", true},
	// A stock car's job posting taken back because the job went back to
	// work after it was ready (#164).
	{"stock_cost.reversed", "stock_unit", "audit_events", true},
	// A test drive let start before its agreement was signed (#173).
	{"test_drive.started_unsigned", "test_drive", "audit_events", true},
	// Rental
	{"rental.price_overridden", "rental_booking", "audit_events", true},
	// Every read of the rental register, with its purpose (#200).
	{"rental_register.read", "shop", "audit_events", true},
}

func auditTransition(event string) (AuditTransition, bool) {
	for _, t := range AuditedTransitions {
		if t.Event == event {
			return t, true
		}
	}
	return AuditTransition{}, false
}

// auditTx writes an event to audit_events in the transition's transaction.
// The event must be one AuditedTransitions keeps there; detail holds ids,
// amounts and counts, never a person's details.
func auditTx(ctx context.Context, tx pgx.Tx, scope access.Scope, subjectID, event, from, to, reason string, detail map[string]any) error {
	t, ok := auditTransition(event)
	if !ok || t.KeptIn != "audit_events" {
		return fmt.Errorf("audit: %q is not a transition kept in audit_events", event)
	}
	reason = strings.TrimSpace(reason)
	if t.NeedsReason && reason == "" {
		return fmt.Errorf("audit: %q needs a reason", event)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("audit detail: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (shop_id, subject_kind, subject_id, event, from_state, to_state, actor, reason, detail)
		VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), NULLIF($7, '')::uuid, nullif($8, ''), $9)`,
		scope.ShopID, t.Subject, subjectID, event, from, to, scope.UserID, reason, b); err != nil {
		return fmt.Errorf("record the event: %w", err)
	}
	return nil
}

// checkReason is a reason a transition is made only with: required, and 300
// characters at most, or refused with the sentence given.
func checkReason(reason string, refused error) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 300 {
		return "", refused
	}
	return reason, nil
}

// AuditEvent is one event as a reader is shown it.
type AuditEvent struct {
	Event  string
	From   string
	To     string
	Actor  string // the person's name; "" for the system
	At     time.Time
	Reason string
	// The reason was taken away by an erasure: the event happened, why
	// can name somebody who asked to be forgotten.
	ReasonErased bool
	Detail       map[string]any
	KeptIn       string
}

// detailSeen is which detail keys need what: a role that may not see a
// purchase price is not handed the event detail that holds it.
var detailSeen = map[string]func(access.Roles) bool{
	"purchase_price_minor": access.Roles.SeesPurchasePrice,
	"margin_minor":         access.Roles.SeesMargin,
	// Dealer: the checks money was let leave over (#162).
	"outstanding": access.Roles.SeesPurchasePrice,
}

// reasonSeen is which events' reasons need what: why money was let leave
// for a car can say what it cost and what was owed on it (#162).
var reasonSeen = map[string]func(access.Roles) bool{
	"stock_checks.overridden": access.Roles.SeesPurchasePrice,
}

// AuditTrail is a subject's events, in the order they happened, for
// whoever may read that subject: the shop's and its people's for those who
// run it; a document's for those who may read its kind; a deposit's or a
// payout's for those who may make one; money's, a
// customer's, a vehicle's and a party's for those who see customers; a
// job's for those who work on or follow jobs; a booked resource's for those
// who book it, with why only for those who run the shop; a car's cycle
// through stock for whoever buys or sells cars, a purchase's for whoever
// may see what the car cost. What a role may not see of the detail is left
// out.
func AuditTrail(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, subjectKind, subjectID string) ([]AuditEvent, error) {
	if !looksLikeUUID(subjectID) {
		return nil, ErrNotFound
	}
	reasons := true
	switch subjectKind {
	case "shop":
		if !scope.Role.RunsTheShop() {
			return nil, access.ErrForbidden
		}
		if !strings.EqualFold(subjectID, scope.ShopID) {
			return nil, ErrNotFound
		}
	case "user":
		if !scope.Role.RunsTheShop() {
			return nil, access.ErrForbidden
		}
	case "resource_block":
		if !scope.Role.KeepsResources() && !scope.Role.SellsVehicles() && !scope.Role.RunsTheShop() {
			return nil, access.ErrForbidden
		}
		reasons = scope.Role.RunsTheShop()
	case "invoice", "payment", "customer", "vehicle", "party":
		if !scope.Role.SeesCustomerPersonalData() {
			return nil, access.ErrForbidden
		}
	case "identity_record":
		if !scope.Role.SeesIdentityRecords() {
			return nil, access.ErrForbidden
		}
	case "work_order":
		if !scope.Role.SeesCustomerPersonalData() && !scope.Role.WorksInTheWorkshop() && !scope.Role.FollowsJobs() {
			return nil, access.ErrForbidden
		}
	case "document", "deposit", "payout":
	case "stock_unit":
		if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
			return nil, access.ErrForbidden
		}
	case "test_drive":
		if !scope.Role.SellsVehicles() {
			return nil, access.ErrForbidden
		}
	case "purchase":
		if !scope.Role.SeesPurchasePrice() {
			return nil, access.ErrForbidden
		}
	case "rental_booking":
		if !scope.Role.HandsOverRentals() {
			return nil, access.ErrForbidden
		}
	default:
		return nil, fmt.Errorf("%w: that is not something with a history", ErrInvalid)
	}
	var out []AuditEvent
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if subjectKind == "deposit" || subjectKind == "payout" {
			var purpose string
			err := tx.QueryRow(ctx, `SELECT purpose FROM `+subjectKind+`s WHERE id = $1`, subjectID).Scan(&purpose)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", subjectKind, err)
			}
			if err := mayReadHolding(scope.Role, subjectKind, purpose); err != nil {
				return err
			}
		}
		if subjectKind == "document" {
			var kind string
			err := tx.QueryRow(ctx, `SELECT kind FROM document_records WHERE id = $1`, subjectID).Scan(&kind)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read document: %w", err)
			}
			if err := mayReadDocument(scope.Role, kind); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `
			SELECT t.event, coalesce(t.from_state, ''), coalesce(t.to_state, ''), coalesce(p.display_name, ''), t.at,
			       coalesce(t.reason, ''), t.reason_erased, t.detail, t.source
			FROM audit_trail t
			LEFT JOIN users u ON u.id = t.actor LEFT JOIN people p ON p.id = u.person_id
			WHERE t.subject_kind = $1 AND t.subject_id = $2
			ORDER BY t.at, t.seq`, subjectKind, subjectID)
		if err != nil {
			return fmt.Errorf("read the trail: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e AuditEvent
			if err := rows.Scan(&e.Event, &e.From, &e.To, &e.Actor, &e.At, &e.Reason, &e.ReasonErased, &e.Detail, &e.KeptIn); err != nil {
				return fmt.Errorf("scan event: %w", err)
			}
			if may, ok := reasonSeen[e.Event]; !reasons || (ok && !may(scope.Role)) {
				e.Reason = ""
			}
			for k, may := range detailSeen {
				if _, there := e.Detail[k]; there && !may(scope.Role) {
					delete(e.Detail, k)
				}
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// eraseAuditReasonsTx takes away, for a person being erased, the reasons
// given for events about them -- their customer, their party, their
// account -- which can say who they are. That the event happened, who made
// it, when and from what to what stays.
func eraseAuditReasonsTx(ctx context.Context, tx pgx.Tx, personID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE audit_events SET reason = NULL, reason_erased_at = clock_timestamp()
		WHERE reason IS NOT NULL AND subject_id IN (
			SELECT id FROM customers WHERE person_id = $1
			UNION ALL SELECT pa.id FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id
			          WHERE pa.person_id = $1 OR c.person_id = $1
			UNION ALL SELECT id FROM users WHERE person_id = $1)`, personID)
	if err != nil {
		return fmt.Errorf("clear the reasons of events about them: %w", err)
	}
	// And why a field was opened for them as a technician, or closed
	// again (0092): that it was, by whom and until when stays.
	if _, err := tx.Exec(ctx, `
		UPDATE technician_view_exceptions SET reason = NULL, reason_erased_at = clock_timestamp()
		WHERE reason IS NOT NULL AND user_id IN (SELECT id FROM users WHERE person_id = $1)`, personID); err != nil {
		return fmt.Errorf("clear the reasons of their technician exceptions: %w", err)
	}
	return nil
}
