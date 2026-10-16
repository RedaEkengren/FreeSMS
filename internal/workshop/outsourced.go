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

// Work sent to somebody else.
//
// A sublet line says what the customer is charged. It cannot say that the
// turbocharger left on Tuesday, was promised for Friday, came back with a
// report, and failed the shop's own check. That is a step between the shop's
// tasks, and it was lost there.
//
// Where it physically is -- a component, or the car itself, at the supplier --
// is its own fact, apart from the job's state and apart from the car being on
// a lift. Receipt is not acceptance: "returned" is what the supplier handed
// back and "checked" is the shop's own word that it is good, by whoever
// checked. A required piece of outsourced work that has not been checked does
// not let the job be called ready, whatever the final check says.
//
// This is not an accounts payable ledger. It keeps the supplier's invoice
// number and amount, and whether somebody handed it to the accounts; paying it
// is somewhere else.

// OutsourcedInput is a piece of work sent out.
type OutsourcedInput struct {
	JobID  string
	LineID string
	// The supplier's name, a contact there, what they are to do, and their
	// own reference for it.
	Supplier    string
	Contact     string
	Scope       string
	ExternalRef string
	// What physically goes, and whether that is a part or the car.
	Item          string
	ItemKind      string
	ExpectedBack  *time.Time
	EstimateMinor *int64
	// Not required: the job may be ready without it.
	Optional bool
}

// OutsourcedEventInput is something that happened to it.
type OutsourcedEventInput struct {
	Kind         string
	Note         string
	Outcome      string
	ExpectedBack *time.Time
	AmountMinor  *int64
	InvoiceRef   string
	Assignee     string
}

// Outcomes of work that came back.
var OutsourcedOutcomes = []string{"repaired", "replaced", "no_work", "diagnosis_only"}

// OutsourcedOutcomeLabel is an outcome, as a catalogue key.
func OutsourcedOutcomeLabel(o string) string {
	switch o {
	case "repaired":
		return "repaired"
	case "replaced":
		return "replaced with another unit"
	case "no_work":
		return "no work was done"
	case "diagnosis_only":
		return "diagnosis only"
	}
	return o
}

// outsourcedMoves are the statuses each status can go on to. The money and the
// notes are not statuses and can be recorded at any time.
var outsourcedMoves = map[string][]string{
	"requested":    {"sent", "cancelled"},
	"sent":         {"waiting", "returned", "cancelled"},
	"waiting":      {"waiting", "returned", "cancelled"},
	"returned":     {"checked", "check_failed"},
	"check_failed": {"sent", "cancelled"},
	// Accepted, and then found not good enough at the final check.
	"checked":   {"check_failed"},
	"cancelled": {},
}

func isStatusKind(k string) bool {
	_, ok := outsourcedMoves[k]
	return ok
}

// OutsourcedEvent is one entry in a piece of outsourced work's history.
type OutsourcedEvent struct {
	Kind         string
	Note         string
	Outcome      string
	ExpectedBack *time.Time
	AmountMinor  *int64
	InvoiceRef   string
	Assignee     string
	By           string
	At           time.Time
}

// Outsourced is a piece of work at, or on its way to or from, a supplier.
type Outsourced struct {
	ID          string
	JobID       string
	JobNumber   int64
	Plate       string
	LineID      string
	LineText    string
	Supplier    string
	Contact     string
	Scope       string
	ExternalRef string
	Item        string
	ItemKind    string
	Required    bool

	// Where it stands, as the latest status event says.
	Status       string
	ExpectedBack *time.Time
	Responsible  string
	Events       []OutsourcedEvent
	Outcome      string

	// Money, for the roles that may see it.
	MoneyVisible  bool
	EstimateMinor *int64
	CostMinor     *int64
	InvoiceRef    string
	InvoiceMinor  *int64
	Reconciled    bool
	// The customer's price on the linked line, for comparison. Nil without one.
	ChargeMinor *int64

	// Overdue is set by the reader against the shop's day.
	Overdue bool
}

// Away is whether the item is not in the shop: sent and not yet back.
func (o Outsourced) Away() bool { return o.Status == "sent" || o.Status == "waiting" }

// Unresolved is whether the shop still has something to do about it before the
// job can be called ready: required, and neither accepted nor cancelled.
func (o Outsourced) Unresolved() bool {
	return o.Required && o.Status != "checked" && o.Status != "cancelled"
}

// CostDiffers is whether what it cost is not what was estimated.
func (o Outsourced) CostDiffers() bool {
	return o.EstimateMinor != nil && o.CostMinor != nil && *o.EstimateMinor != *o.CostMinor
}

// Estimate, Cost, InvoiceAmount and Charge are the amounts, or zero, for a page
// that cannot print a pointer.
func (o Outsourced) Estimate() int64      { return deref64(o.EstimateMinor) }
func (o Outsourced) Cost() int64          { return deref64(o.CostMinor) }
func (o Outsourced) InvoiceAmount() int64 { return deref64(o.InvoiceMinor) }
func (o Outsourced) Charge() int64        { return deref64(o.ChargeMinor) }

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// NextSteps are the statuses it can be moved to now.
func (o Outsourced) NextSteps() []Option {
	var out []Option
	for _, k := range outsourcedMoves[o.Status] {
		out = append(out, Option{k, OutsourcedKindLabel(k)})
	}
	return out
}

// InvoiceWaiting is whether a supplier invoice has been noted and not handed
// to the accounts.
func (o Outsourced) InvoiceWaiting() bool { return o.InvoiceRef != "" && !o.Reconciled }

// OutsourcedKindLabel is an event or status, as a catalogue key.
func OutsourcedKindLabel(k string) string {
	switch k {
	case "requested":
		return "asked for"
	case "sent":
		return "sent"
	case "waiting":
		return "waiting for them"
	case "returned":
		return "back from them"
	case "checked":
		return "checked and accepted"
	case "check_failed":
		return "checked, not good enough"
	case "cancelled":
		return "cancelled"
	case "assigned":
		return "put on"
	case "cost":
		return "cost recorded"
	case "invoice":
		return "supplier invoice noted"
	case "reconciled":
		return "handed to the accounts"
	case "note":
		return "note"
	}
	return k
}

// Label is the event, as a catalogue key.
func (e OutsourcedEvent) Label() string { return OutsourcedKindLabel(e.Kind) }

// OutcomeLabel is what came back, as a catalogue key.
func (e OutsourcedEvent) OutcomeLabel() string { return OutsourcedOutcomeLabel(e.Outcome) }

// StatusLabel is where it stands, as a catalogue key.
func (o Outsourced) StatusLabel() string { return OutsourcedKindLabel(o.Status) }

// OutcomeLabel is what came back, as a catalogue key.
func (o Outsourced) OutcomeLabel() string { return OutsourcedOutcomeLabel(o.Outcome) }

// Amount is the event's amount, or zero.
func (e OutsourcedEvent) Amount() int64 { return deref64(e.AmountMinor) }

// RequestOutsourced records that work is to be sent out, on a job that is
// still open.
func RequestOutsourced(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, in OutsourcedInput) (string, error) {
	if !scope.Role.SeesParts() {
		return "", access.ErrForbidden
	}
	in.Supplier, in.Contact, in.Scope = strings.TrimSpace(in.Supplier), strings.TrimSpace(in.Contact), strings.TrimSpace(in.Scope)
	in.ExternalRef, in.Item = strings.TrimSpace(in.ExternalRef), strings.TrimSpace(in.Item)
	switch {
	case !looksLikeUUID(in.JobID):
		return "", ErrNotFound
	case in.LineID != "" && !looksLikeUUID(in.LineID):
		return "", ErrNotFound
	case in.Supplier == "" || len(in.Supplier) > 100:
		return "", fmt.Errorf("%w: name the supplier, up to 100 characters", ErrInvalid)
	case len(in.Contact) > 200 || len(in.ExternalRef) > 100:
		return "", fmt.Errorf("%w: the contact and reference are limited to 200 and 100 characters", ErrInvalid)
	case in.Scope == "" || len(in.Scope) > 500:
		return "", fmt.Errorf("%w: say what they are to do, up to 500 characters", ErrInvalid)
	case in.Item == "" || len(in.Item) > 100:
		return "", fmt.Errorf("%w: say what is sent, up to 100 characters", ErrInvalid)
	case in.ItemKind != "component" && in.ItemKind != "vehicle":
		return "", fmt.Errorf("%w: a part off the car or the car", ErrInvalid)
	case in.EstimateMinor != nil && *in.EstimateMinor < 0:
		return "", fmt.Errorf("%w: an estimate cannot be negative", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockJobTx(ctx, tx, in.JobID); err != nil {
			return err
		}
		if in.LineID != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM work_order_lines WHERE id = $1 AND work_order_id = $2)`,
				in.LineID, in.JobID).Scan(&ok); err != nil {
				return fmt.Errorf("check the line: %w", err)
			}
			if !ok {
				return fmt.Errorf("%w: that line is not on this job", ErrInvalid)
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO outsourced_work (shop_id, work_order_id, line_id, supplier, contact, scope, external_ref,
			                             item, item_kind, expected_back, estimate_minor, required, created_by)
			VALUES ($1, $2, nullif($3, '')::uuid, $4, nullif($5, ''), $6, nullif($7, ''), $8, $9, $10, $11, $12, $13)
			RETURNING id`,
			scope.ShopID, in.JobID, in.LineID, in.Supplier, in.Contact, in.Scope, in.ExternalRef,
			in.Item, in.ItemKind, in.ExpectedBack, in.EstimateMinor, !in.Optional, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record the request: %w", err)
		}
		return outsourcedEventTx(ctx, tx, scope, id, OutsourcedEventInput{Kind: "requested", ExpectedBack: in.ExpectedBack})
	})
	return id, err
}

func outsourcedEventTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string, e OutsourcedEventInput) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO outsourced_events (shop_id, outsourced_id, kind, note, expected_back, outcome, amount_minor, invoice_ref, assignee, by_user)
		VALUES ($1, $2, $3, nullif($4, ''), $5, nullif($6, ''), $7, nullif($8, ''), nullif($9, '')::uuid, $10)`,
		scope.ShopID, id, e.Kind, strings.TrimSpace(e.Note), e.ExpectedBack, e.Outcome, e.AmountMinor,
		strings.TrimSpace(e.InvoiceRef), e.Assignee, scope.UserID); err != nil {
		return fmt.Errorf("record the event: %w", err)
	}
	return nil
}

// RecordOutsourced records something that happened to outsourced work: it was
// sent, delayed, came back, was checked, cost something, was invoiced, or was
// handed to the accounts.
func RecordOutsourced(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, in OutsourcedEventInput) error {
	if !scope.Role.SeesParts() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	in.Note, in.InvoiceRef = strings.TrimSpace(in.Note), strings.TrimSpace(in.InvoiceRef)
	switch {
	case len(in.Note) > 500:
		return fmt.Errorf("%w: the note is limited to 500 characters", ErrInvalid)
	case len(in.InvoiceRef) > 100:
		return fmt.Errorf("%w: the reference is limited to 100 characters", ErrInvalid)
	case in.AmountMinor != nil && *in.AmountMinor < 0:
		return fmt.Errorf("%w: an amount cannot be negative", ErrInvalid)
	}
	switch in.Kind {
	case "sent", "checked":
	case "waiting":
		if in.Note == "" && in.ExpectedBack == nil {
			return fmt.Errorf("%w: say why, or when it is now expected", ErrInvalid)
		}
	case "returned":
		known := false
		for _, o := range OutsourcedOutcomes {
			known = known || o == in.Outcome
		}
		if !known {
			return fmt.Errorf("%w: say what came of it", ErrInvalid)
		}
		if in.Note == "" {
			return fmt.Errorf("%w: write down what they report", ErrInvalid)
		}
	case "check_failed", "cancelled", "note":
		if in.Note == "" {
			return fmt.Errorf("%w: say why", ErrInvalid)
		}
	case "cost":
		if in.AmountMinor == nil {
			return fmt.Errorf("%w: say what it cost", ErrInvalid)
		}
	case "invoice":
		if in.InvoiceRef == "" {
			return fmt.Errorf("%w: write down their invoice number", ErrInvalid)
		}
	case "reconciled", "assigned":
	default:
		return fmt.Errorf("%w: %q is not something that can be recorded", ErrInvalid, in.Kind)
	}
	if in.Kind != "returned" {
		in.Outcome = ""
	}
	if in.Kind != "invoice" && in.Kind != "cost" {
		in.AmountMinor = nil
	}
	if in.Kind != "invoice" {
		in.InvoiceRef = ""
	}
	if in.Kind != "sent" && in.Kind != "waiting" {
		in.ExpectedBack = nil
	}
	if in.Kind != "assigned" {
		in.Assignee = ""
	}

	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var jobID, lineID, status string
		var hasLine bool
		if err := tx.QueryRow(ctx, `
			SELECT o.work_order_id::text, coalesce(o.line_id::text, ''), o.line_id IS NOT NULL
			FROM outsourced_work o WHERE o.id = $1`, id).Scan(&jobID, &lineID, &hasLine); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read the work: %w", err)
		}
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT true FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&locked); err != nil {
			return fmt.Errorf("lock the job: %w", err)
		}
		status, err := outsourcedStatusTx(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case isStatusKind(in.Kind):
			ok := false
			for _, next := range outsourcedMoves[status] {
				ok = ok || next == in.Kind
			}
			if !ok {
				return fmt.Errorf("%w: it is %s, and cannot go to %s", ErrInvalid, OutsourcedKindLabel(status), OutsourcedKindLabel(in.Kind))
			}
		case in.Kind == "reconciled":
			var invoiced, done bool
			if err := tx.QueryRow(ctx, `
				SELECT exists(SELECT 1 FROM outsourced_events WHERE outsourced_id = $1 AND kind = 'invoice'),
				       coalesce((SELECT e.kind = 'reconciled' FROM outsourced_events e WHERE e.outsourced_id = $1 AND e.kind IN ('invoice', 'reconciled')
				                  ORDER BY e.at DESC, e.id DESC LIMIT 1), false)`, id).Scan(&invoiced, &done); err != nil {
				return fmt.Errorf("read the invoice: %w", err)
			}
			if !invoiced {
				return fmt.Errorf("%w: there is no supplier invoice to hand over", ErrInvalid)
			}
			if done {
				return fmt.Errorf("%w: it has been handed over already", ErrInvalid)
			}
		case in.Kind == "assigned":
			var ok bool
			if !looksLikeUUID(in.Assignee) {
				return fmt.Errorf("%w: say who", ErrInvalid)
			}
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM users WHERE id = $1 AND active)`, in.Assignee).Scan(&ok); err != nil {
				return fmt.Errorf("check the person: %w", err)
			}
			if !ok {
				return fmt.Errorf("%w: nobody by that name works here", ErrInvalid)
			}
		}
		if err := outsourcedEventTx(ctx, tx, scope, id, in); err != nil {
			return err
		}
		if !hasLine {
			return nil
		}
		return followOutsourcedTaskTx(ctx, tx, scope, jobID, lineID, in.Kind, status)
	})
}

// followOutsourcedTaskTx keeps the work list honest: a step whose work is at
// the supplier is blocked by that and nothing else, and is done when the shop
// has accepted what came back.
func followOutsourcedTaskTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, lineID, kind, before string) error {
	var taskID, status string
	err := tx.QueryRow(ctx, `
		SELECT id::text, status FROM job_tasks
		WHERE line_id = $1 AND status IN ('pending', 'underway', 'blocked') ORDER BY position LIMIT 1`, lineID).Scan(&taskID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the step: %w", err)
	}
	var m TaskMove
	switch kind {
	case "sent":
		m = TaskMove{To: TaskBlocked, Blocker: BlockerOther, Note: "At the supplier"}
	case "checked":
		m = TaskMove{To: TaskDone, Note: "Back from the supplier and checked"}
	case "cancelled":
		if status != "blocked" {
			return nil
		}
		m = TaskMove{To: TaskPending, Note: "Not going to the supplier after all"}
	default:
		return nil
	}
	return moveTaskTx(ctx, tx, scope, jobID, taskID, m)
}

func outsourcedStatusTx(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT kind FROM outsourced_events
		WHERE outsourced_id = $1 AND kind IN ('requested', 'sent', 'waiting', 'returned', 'checked', 'check_failed', 'cancelled')
		ORDER BY at DESC, id DESC LIMIT 1`, id).Scan(&status); err != nil {
		return "", fmt.Errorf("read the status: %w", err)
	}
	return status, nil
}

// OutsourcedFor lists the outsourced work on a job, oldest first.
func OutsourcedFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]Outsourced, error) {
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	var out []Outsourced
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = outsourcedTx(ctx, tx, scope, `o.work_order_id = $1`, jobID)
		return err
	})
	return out, err
}

// OutsourcedOpen lists what is out with suppliers or waiting to be accepted,
// and what is invoiced and not handed to the accounts: the counter's list.
func OutsourcedOpen(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Outsourced, error) {
	if !scope.Role.SeesParts() {
		return nil, access.ErrForbidden
	}
	var out []Outsourced
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = outsourcedTx(ctx, tx, scope, `
			(SELECT e.kind FROM outsourced_events e
			  WHERE e.outsourced_id = o.id AND e.kind IN ('requested', 'sent', 'waiting', 'returned', 'checked', 'check_failed', 'cancelled')
			  ORDER BY e.at DESC, e.id DESC LIMIT 1) NOT IN ('checked', 'cancelled')
			OR coalesce((SELECT e.kind = 'invoice' FROM outsourced_events e
			              WHERE e.outsourced_id = o.id AND e.kind IN ('invoice', 'reconciled')
			              ORDER BY e.at DESC, e.id DESC LIMIT 1), false)`, "")
		return err
	})
	return out, err
}

func outsourcedTx(ctx context.Context, tx pgx.Tx, scope access.Scope, where, arg string) ([]Outsourced, error) {
	money := scope.Role.SeesParts()
	args := []any{}
	if arg != "" {
		args = append(args, arg)
	}
	rows, err := tx.Query(ctx, `
		SELECT o.id::text, o.work_order_id::text, w.number, coalesce(r.registration, ''), coalesce(o.line_id::text, ''), coalesce(l.description, ''),
		       o.supplier, coalesce(o.contact, ''), o.scope, coalesce(o.external_ref, ''), o.item, o.item_kind, o.required,
		       o.estimate_minor, o.expected_back,
		       CASE WHEN l.id IS NULL THEN NULL ELSE round(l.quantity * l.unit_price_minor)::bigint END,
		       (now() AT TIME ZONE (SELECT s.timezone FROM shops s WHERE s.id = o.shop_id))::date
		FROM outsourced_work o
		JOIN work_orders w ON w.id = o.work_order_id
		LEFT JOIN vehicle_registrations r ON r.vehicle_id = w.vehicle_id AND r.valid_to IS NULL
		LEFT JOIN work_order_lines l ON l.id = o.line_id
		WHERE `+where+` ORDER BY o.created_at, o.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read outsourced work: %w", err)
	}
	var out []Outsourced
	var today []time.Time
	for rows.Next() {
		var o Outsourced
		var t time.Time
		var charge *int64
		if err := rows.Scan(&o.ID, &o.JobID, &o.JobNumber, &o.Plate, &o.LineID, &o.LineText, &o.Supplier, &o.Contact, &o.Scope,
			&o.ExternalRef, &o.Item, &o.ItemKind, &o.Required, &o.EstimateMinor, &o.ExpectedBack, &charge, &t); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan outsourced work: %w", err)
		}
		o.MoneyVisible = money
		if money {
			o.ChargeMinor = charge
		} else {
			o.EstimateMinor = nil
		}
		out = append(out, o)
		today = append(today, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]string, len(out))
	at := map[string]int{}
	for i, o := range out {
		ids[i], at[o.ID] = o.ID, i
	}
	ev, err := tx.Query(ctx, `
		SELECT e.outsourced_id::text, e.kind, coalesce(e.note, ''), coalesce(e.outcome, ''), e.expected_back, e.amount_minor, coalesce(e.invoice_ref, ''),
		       coalesce(ap.display_name, ''), coalesce(p.display_name, ''), e.at
		FROM outsourced_events e
		LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
		LEFT JOIN users au ON au.id = e.assignee LEFT JOIN people ap ON ap.id = au.person_id
		WHERE e.outsourced_id = ANY($1::uuid[]) ORDER BY e.at, e.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	for ev.Next() {
		var owner string
		var e OutsourcedEvent
		if err := ev.Scan(&owner, &e.Kind, &e.Note, &e.Outcome, &e.ExpectedBack, &e.AmountMinor, &e.InvoiceRef, &e.Assignee, &e.By, &e.At); err != nil {
			ev.Close()
			return nil, fmt.Errorf("scan event: %w", err)
		}
		o := &out[at[owner]]
		switch e.Kind {
		case "cost", "invoice", "reconciled":
			if !money {
				continue
			}
		}
		o.Events = append(o.Events, e)
		if isStatusKind(e.Kind) {
			o.Status = e.Kind
		}
		if e.ExpectedBack != nil {
			o.ExpectedBack = e.ExpectedBack
		}
		switch e.Kind {
		case "returned":
			o.Outcome = e.Outcome
		case "assigned":
			o.Responsible = e.Assignee
		case "cost":
			o.CostMinor = e.AmountMinor
		case "invoice":
			o.InvoiceRef, o.InvoiceMinor, o.Reconciled = e.InvoiceRef, e.AmountMinor, false
		case "reconciled":
			o.Reconciled = true
		}
	}
	ev.Close()
	if err := ev.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		o := &out[i]
		if o.Responsible == "" && len(o.Events) > 0 {
			o.Responsible = o.Events[0].By
		}
		if o.Status == "" {
			o.Status = "requested"
		}
		o.Overdue = o.Away() && o.ExpectedBack != nil && o.ExpectedBack.Before(today[i])
	}
	return out, nil
}

// unresolvedOutsourcedTx is the required outsourced work on a job that has
// not been checked and accepted, as "supplier: item".
func unresolvedOutsourcedTx(ctx context.Context, tx pgx.Tx, jobID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT o.supplier || ': ' || o.item
		FROM outsourced_work o
		WHERE o.work_order_id = $1 AND o.required
		  AND (SELECT e.kind FROM outsourced_events e
		        WHERE e.outsourced_id = o.id AND e.kind IN ('requested', 'sent', 'waiting', 'returned', 'checked', 'check_failed', 'cancelled')
		        ORDER BY e.at DESC, e.id DESC LIMIT 1) NOT IN ('checked', 'cancelled')
		ORDER BY o.created_at`, jobID)
	if err != nil {
		return nil, fmt.Errorf("read outsourced work: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// outsourcedWaiting is what the counter and the parts desk should chase: work
// that was due back and is not, and a supplier invoice nobody handed on.
func outsourcedWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]WaitingItem, error) {
	if !scope.Role.SeesParts() {
		return nil, access.ErrForbidden
	}
	all, err := OutsourcedOpen(ctx, pool, scope)
	if err != nil {
		return nil, err
	}
	var out []WaitingItem
	for _, o := range all {
		switch {
		case o.Overdue:
			out = append(out, WaitingItem{Kind: "outsourced_overdue", JobID: o.JobID, Number: o.JobNumber, Registration: o.Plate, Text: o.Supplier + ": " + o.Item})
		case o.InvoiceWaiting():
			out = append(out, WaitingItem{Kind: "outsourced_invoice", JobID: o.JobID, Number: o.JobNumber, Registration: o.Plate, Text: o.Supplier + " " + o.InvoiceRef})
		}
	}
	return out, nil
}
