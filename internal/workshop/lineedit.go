package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// ErrLineChanged is an edit made against a version of a line that is no
// longer the current one: somebody else corrected it while this form was open,
// or sat in an offline queue. It never overwrites their correction.
var ErrLineChanged = errors.New("workshop: the line was changed by somebody else")

// errPreview rolls a transaction back once it has said what it would have
// done. A preview that ran the real code and threw it away cannot disagree
// with the save that follows it.
var errPreview = errors.New("workshop: preview")

// LineValues is what a line says, the part of it an edit can change.
type LineValues struct {
	Kind           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
	CostBearer     string
	PartID         string
}

// Quantity renders the quantity as the job page does.
func (v LineValues) Quantity() string { return Line{QuantityMilli: v.QuantityMilli}.Quantity() }

// GrossMinor is what the line comes to with VAT for the customer: a line
// somebody else pays for comes to nothing.
func (v LineValues) GrossMinor() int64 {
	if v.CostBearer != "customer" {
		return 0
	}
	t := money.Compute([]money.Line{{
		QuantityMilli: v.QuantityMilli, UnitPriceMinor: v.UnitPriceMinor,
		VATRateBasis: v.VATRateBasis, ChargedToCustomer: true,
	}})
	return t.GrossMinor
}

// LineEdit is a correction to a draft line. The kind is not among the things
// that can change: a labour line that turns out to be a part is a different
// line, because the kind decides how the rest of the system treats it.
type LineEdit struct {
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
	CostBearer     string
	PartID         string
	Reason         string
}

// StockEffect is what a change does to one part's place on the shelf.
type StockEffect struct {
	PartID, Number, Name string
	// Put aside for this job before and after, in thousandths.
	ReservedBeforeMilli, ReservedAfterMilli int64
	// Already taken out to the job, in thousandths. This is a fact about the
	// car and is not changed by anything here.
	TakenOutMilli int64
	// Priced on the job's lines after the change, in thousandths.
	PricedAfterMilli int64
}

// NotPriced is a part that is on the car and no longer on the bill.
func (e StockEffect) NotPriced() bool { return e.TakenOutMilli > e.PricedAfterMilli }

// Qty renders thousandths the way a quantity is.
func (e StockEffect) Qty(milli int64) string { return Line{QuantityMilli: milli}.Quantity() }

// LineChange is what a correction does, or did.
type LineChange struct {
	Position int
	// The version the line is at after this (or is at, if nothing changed).
	Revision int
	Before   LineValues
	After    LineValues
	// Nothing differed, so nothing was written. A retry of a save that went
	// through lands here.
	NoChange bool
	// Withdrawing rather than editing.
	Withdrawal bool

	WasApproved     bool
	ApprovalCleared bool

	LineGrossBefore, LineGrossAfter int64
	JobGrossBefore, JobGrossAfter   int64

	Stock []StockEffect

	// Where agreement stands on the job once this is done: what the customer
	// has said yes to and still has, and what they have yet to be asked.
	Agreed, Waiting []string
}

// Differs is whether anything the customer pays for moved.
func (c LineChange) Differs() bool { return c.LineGrossBefore != c.LineGrossAfter }

// KeepsApproval is an edit to an approved line that leaves the approval where
// it was.
func (c LineChange) KeepsApproval() bool { return c.WasApproved && !c.ApprovalCleared && !c.Withdrawal }

// EditLine corrects a line that is still a draft.
func EditLine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, position, revision int, edit LineEdit) (LineChange, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return LineChange{}, access.ErrForbidden
	}
	return changeLine(ctx, pool, scope, jobID, position, revision, lineOp{edit: &edit})
}

// PreviewLineEdit says what EditLine would do and does none of it.
func PreviewLineEdit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, position, revision int, edit LineEdit) (LineChange, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return LineChange{}, access.ErrForbidden
	}
	return changeLine(ctx, pool, scope, jobID, position, revision, lineOp{edit: &edit, preview: true})
}

// WithdrawLine takes back a line that should never have been there.
//
// The row stays, with who withdrew it and why, and nothing counts it: not the
// total, the quote, the invoice or the shelf. Work that was done is not
// withdrawn: a diagnosis that happened is on the car whatever the repair
// turned out to be, so a line marked performed is refused.
func WithdrawLine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, position, revision int, reason string) (LineChange, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return LineChange{}, access.ErrForbidden
	}
	return changeLine(ctx, pool, scope, jobID, position, revision, lineOp{withdraw: true, reason: reason})
}

// PreviewWithdrawal says what WithdrawLine would do and does none of it.
func PreviewWithdrawal(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, position, revision int, reason string) (LineChange, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return LineChange{}, access.ErrForbidden
	}
	return changeLine(ctx, pool, scope, jobID, position, revision, lineOp{withdraw: true, reason: reason, preview: true})
}

type lineOp struct {
	edit     *LineEdit
	withdraw bool
	reason   string
	preview  bool
}

func changeLine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, position, expected int, op lineOp) (LineChange, error) {
	var out LineChange
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		c, err := changeLineTx(ctx, tx, scope, jobID, position, expected, op)
		out = c
		if err != nil {
			return err
		}
		if op.preview {
			return errPreview
		}
		return nil
	})
	if errors.Is(err, errPreview) {
		err = nil
	}
	if err != nil {
		return LineChange{}, err
	}
	return out, nil
}

// current is a line as it is stored, with what the rules need to know.
type current struct {
	id         string
	values     LineValues
	approved   bool
	performed  bool
	declined   bool
	withdrawn  bool
	revision   int
	fromAnswer bool
}

func changeLineTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, position, expected int, op lineOp) (LineChange, error) {
	var state State
	if err := tx.QueryRow(ctx,
		`SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LineChange{}, ErrNotFound
		}
		return LineChange{}, fmt.Errorf("read state: %w", err)
	}
	switch state {
	case StateInvoiced, StateClosed, StateCancelled:
		// An issued invoice is frozen, and the lines under it with it. Putting
		// a number right now would leave the document saying one thing and the
		// job another, which is what the credit note exists to avoid.
		return LineChange{}, fmt.Errorf("%w: a %s order's lines are part of an issued document; correct it with a credit note", ErrInvalid, state)
	}

	var cur current
	err := tx.QueryRow(ctx, `
		SELECT id, kind, description, (quantity * 1000)::bigint, unit_price_minor, vat_rate_bp,
		       cost_bearer, coalesce(part_id::text, ''),
		       approved_at IS NOT NULL, performed_at IS NOT NULL, declined_at IS NOT NULL,
		       withdrawn_at IS NOT NULL, revision, customer_decision_id IS NOT NULL
		FROM work_order_lines
		WHERE work_order_id = $1 AND position = $2
		FOR UPDATE`, jobID, position).Scan(
		&cur.id, &cur.values.Kind, &cur.values.Description, &cur.values.QuantityMilli,
		&cur.values.UnitPriceMinor, &cur.values.VATRateBasis, &cur.values.CostBearer, &cur.values.PartID,
		&cur.approved, &cur.performed, &cur.declined, &cur.withdrawn, &cur.revision, &cur.fromAnswer)
	if errors.Is(err, pgx.ErrNoRows) {
		return LineChange{}, ErrNotFound
	}
	if err != nil {
		return LineChange{}, fmt.Errorf("read line: %w", err)
	}

	change := LineChange{
		Position: position, Revision: cur.revision, Before: cur.values, After: cur.values,
		Withdrawal: op.withdraw, WasApproved: cur.approved,
		LineGrossBefore: cur.values.GrossMinor(),
	}
	before, err := jobLinesTx(ctx, tx, jobID)
	if err != nil {
		return LineChange{}, err
	}
	change.JobGrossBefore = TotalsFor(before).GrossMinor

	// A line already withdrawn is already as it was asked to be. Saying so is
	// what lets a second press of the same button, or a queued form replayed
	// after the first got through, succeed instead of reporting a conflict.
	if cur.withdrawn {
		if op.withdraw {
			change.NoChange = true
			change.LineGrossBefore, change.LineGrossAfter = 0, 0
			change.JobGrossAfter = change.JobGrossBefore
			return change, nil
		}
		return LineChange{}, fmt.Errorf("%w: that line was withdrawn", ErrInvalid)
	}
	if cur.declined {
		return LineChange{}, fmt.Errorf("%w: the customer declined that line; it stays as what was offered", ErrInvalid)
	}

	if op.withdraw {
		return withdrawTx(ctx, tx, scope, jobID, cur, change, before, expected, op)
	}
	return editTx(ctx, tx, scope, jobID, state, cur, change, before, expected, *op.edit)
}

func editTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, state State, cur current,
	change LineChange, before []Line, expected int, e LineEdit) (LineChange, error) {
	after := LineValues{
		Kind: cur.values.Kind, Description: strings.TrimSpace(e.Description),
		QuantityMilli: e.QuantityMilli, UnitPriceMinor: e.UnitPriceMinor,
		VATRateBasis: e.VATRateBasis, CostBearer: e.CostBearer, PartID: strings.TrimSpace(e.PartID),
	}
	if after.CostBearer == "" {
		after.CostBearer = "customer"
	}
	nl := NewLine{
		Kind: after.Kind, Description: after.Description, QuantityMilli: after.QuantityMilli,
		UnitPriceMinor: after.UnitPriceMinor, VATRateBasis: after.VATRateBasis, CostBearer: after.CostBearer,
	}
	if err := checkLine(&nl); err != nil {
		return LineChange{}, err
	}
	if after.VATRateBasis < 0 || after.VATRateBasis > 10000 {
		return LineChange{}, fmt.Errorf("%w: the tax rate must be between 0 and 100 per cent", ErrInvalid)
	}
	if len(strings.TrimSpace(e.Reason)) > 300 {
		return LineChange{}, fmt.Errorf("%w: the reason is limited to 300 characters", ErrInvalid)
	}
	change.After = after
	change.LineGrossAfter = after.GrossMinor()

	// Saying the line is already as it was asked to be is a success whatever
	// version the form was drawn from: a retry of a save that went through
	// finds the line one version on and must not be told it conflicts.
	if after == cur.values {
		change.NoChange = true
		change.JobGrossAfter = change.JobGrossBefore
		return change, nil
	}
	if expected != cur.revision {
		return LineChange{}, ErrLineChanged
	}
	if after.PartID != "" {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM parts WHERE id = $1)`, after.PartID).Scan(&found); err != nil || !found {
			return LineChange{}, fmt.Errorf("%w: no such part", ErrInvalid)
		}
	}

	// What the customer agreed to is the line as it was. Anything that could
	// make it cost them more, or turns it into something else, is not covered
	// by that; making it cheaper, or fixing the wording, is.
	clears := cur.approved && (after.RaisesOver(cur.values) || after.PartID != cur.values.PartID)
	change.ApprovalCleared = clears

	preApproval := state == StateDraft || state == StateEstimated || state == StateAwaitingApproval
	if _, err := tx.Exec(ctx, `
		UPDATE work_order_lines
		   SET description = $2, quantity = ($3::bigint)::numeric / 1000, unit_price_minor = $4,
		       vat_rate_bp = $5, cost_bearer = $6, part_id = nullif($7, '')::uuid,
		       estimated_unit_price_minor = CASE WHEN $8 THEN $4 ELSE estimated_unit_price_minor END,
		       approved_at       = CASE WHEN $9 THEN NULL ELSE approved_at END,
		       quote_decision_id = CASE WHEN $9 THEN NULL ELSE quote_decision_id END,
		       revision = revision + 1, updated_at = now()
		 WHERE id = $1`,
		cur.id, after.Description, after.QuantityMilli, after.UnitPriceMinor, after.VATRateBasis,
		after.CostBearer, after.PartID, preApproval, clears); err != nil {
		return LineChange{}, fmt.Errorf("edit line: %w", err)
	}
	change.Revision = cur.revision + 1
	if err := writeRevisionTx(ctx, tx, scope, cur.id, change.Revision, "edited", after, strings.TrimSpace(e.Reason), clears); err != nil {
		return LineChange{}, err
	}

	stock, err := reconcileTx(ctx, tx, scope, jobID, cur.values.PartID, after.PartID)
	if err != nil {
		return LineChange{}, err
	}
	change.Stock = stock
	return finishTx(ctx, tx, jobID, change)
}

// RaisesOver is whether a line, replacing old, could cost the
// customer more than the one they agreed to.
func (v LineValues) RaisesOver(old LineValues) bool {
	switch {
	case v.GrossMinor() > old.GrossMinor():
		return true
	case v.CostBearer == "customer" && old.CostBearer != "customer":
		return true
	case v.QuantityMilli > old.QuantityMilli:
		// More of the same thing at the same price is more work than was
		// agreed even when a lower price has hidden it in the total.
		return true
	case v.UnitPriceMinor > old.UnitPriceMinor:
		return true
	case v.VATRateBasis > old.VATRateBasis:
		return true
	}
	return false
}

func withdrawTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, cur current,
	change LineChange, before []Line, expected int, op lineOp) (LineChange, error) {
	reason := strings.TrimSpace(op.reason)
	if reason == "" {
		return LineChange{}, fmt.Errorf("%w: say why the line is withdrawn", ErrInvalid)
	}
	if len(reason) > 300 {
		return LineChange{}, fmt.Errorf("%w: the reason is limited to 300 characters", ErrInvalid)
	}
	if cur.performed {
		return LineChange{}, fmt.Errorf("%w: that work was done, so the line stays; correct what it says instead", ErrInvalid)
	}
	if expected != cur.revision {
		return LineChange{}, ErrLineChanged
	}
	change.LineGrossAfter = 0
	if _, err := tx.Exec(ctx, `
		UPDATE work_order_lines
		   SET withdrawn_at = now(), withdrawn_by = $2, withdrawn_reason = $3,
		       revision = revision + 1, updated_at = now()
		 WHERE id = $1`, cur.id, scope.UserID, reason); err != nil {
		return LineChange{}, fmt.Errorf("withdraw line: %w", err)
	}
	change.Revision = cur.revision + 1
	if err := writeRevisionTx(ctx, tx, scope, cur.id, change.Revision, "withdrawn", cur.values, reason, false); err != nil {
		return LineChange{}, err
	}
	stock, err := reconcileTx(ctx, tx, scope, jobID, cur.values.PartID, "")
	if err != nil {
		return LineChange{}, err
	}
	change.Stock = stock
	return finishTx(ctx, tx, jobID, change)
}

func finishTx(ctx context.Context, tx pgx.Tx, jobID string, change LineChange) (LineChange, error) {
	after, err := jobLinesTx(ctx, tx, jobID)
	if err != nil {
		return LineChange{}, err
	}
	change.JobGrossAfter = TotalsFor(after).GrossMinor
	rows, err := tx.Query(ctx, `
		SELECT description, approved_at IS NOT NULL
		FROM work_order_lines
		WHERE work_order_id = $1 AND cost_bearer = 'customer'
		  AND declined_at IS NULL AND withdrawn_at IS NULL
		ORDER BY position`, jobID)
	if err != nil {
		return LineChange{}, fmt.Errorf("read where agreement stands: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		var agreed bool
		if err := rows.Scan(&d, &agreed); err != nil {
			return LineChange{}, fmt.Errorf("scan agreement: %w", err)
		}
		if agreed {
			change.Agreed = append(change.Agreed, d)
		} else {
			change.Waiting = append(change.Waiting, d)
		}
	}
	return change, rows.Err()
}

func writeRevisionTx(ctx context.Context, tx pgx.Tx, scope access.Scope, lineID string, revision int,
	change string, v LineValues, reason string, cleared bool) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO work_order_line_revisions
		  (shop_id, line_id, revision, change, kind, description, quantity, unit_price_minor,
		   vat_rate_bp, cost_bearer, part_id, reason, approval_cleared, by_user)
		VALUES ($1, $2, $3, $4, $5, $6, ($7::bigint)::numeric / 1000, $8, $9, $10,
		        nullif($11, '')::uuid, nullif($12, ''), $13, $14)`,
		scope.ShopID, lineID, revision, change, v.Kind, v.Description, v.QuantityMilli,
		v.UnitPriceMinor, v.VATRateBasis, v.CostBearer, v.PartID, reason, cleared, scope.UserID); err != nil {
		return fmt.Errorf("record the revision: %w", err)
	}
	return nil
}

// jobLinesTx reads a job's lines as far as pricing them goes.
func jobLinesTx(ctx context.Context, tx pgx.Tx, jobID string) ([]Line, error) {
	rows, err := tx.Query(ctx, `
		SELECT position, (quantity * 1000)::bigint, cost_bearer, approved_at, declined_at, withdrawn_at,
		       customer_decision_id IS NOT NULL, unit_price_minor, vat_rate_bp
		FROM work_order_lines WHERE work_order_id = $1 ORDER BY position`, jobID)
	if err != nil {
		return nil, fmt.Errorf("read the job's lines: %w", err)
	}
	defer rows.Close()
	var out []Line
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.Position, &l.QuantityMilli, &l.CostBearer, &l.ApprovedAt, &l.DeclinedAt,
			&l.WithdrawnAt, &l.FromAnswer, &l.UnitPriceMinor, &l.VATRateBasis); err != nil {
			return nil, fmt.Errorf("scan line: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// reconcileTx makes the shelf agree with the lines for each part named.
//
// It works out what ought to be put aside -- what the job's lines ask for,
// less what has already been taken out to the car -- and writes the
// difference from what is put aside now. Starting from the destination and
// not from the edit means doing it twice does nothing the second time, and
// the answer cannot go below nothing however the edits are ordered.
//
// What has been taken out is not touched. A part fitted to the car is on the
// car whatever happens to the line that priced it; saying it came back is
// the put-back's job, done by someone who has it in their hand.
func reconcileTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, partIDs ...string) ([]StockEffect, error) {
	seen := map[string]bool{}
	var parts []string
	for _, p := range partIDs {
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	// In one order, so two people correcting lines that share parts cannot
	// each hold one and wait for the other.
	sort.Strings(parts)

	var out []StockEffect
	for _, part := range parts {
		var e StockEffect
		e.PartID = part
		if err := tx.QueryRow(ctx,
			`SELECT number, name FROM parts WHERE id = $1 FOR UPDATE`, part).Scan(&e.Number, &e.Name); err != nil {
			return nil, fmt.Errorf("lock part: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT
			  coalesce((SELECT sum(quantity * 1000) FROM work_order_lines
			            WHERE work_order_id = $1 AND part_id = $2
			              AND withdrawn_at IS NULL AND declined_at IS NULL), 0)::bigint,
			  coalesce((SELECT -sum(quantity * 1000) FROM stock_movements
			            WHERE work_order_id = $1 AND part_id = $2 AND kind IN ('consumed', 'put_back')), 0)::bigint,
			  coalesce((SELECT sum(quantity * 1000) FROM stock_movements
			            WHERE work_order_id = $1 AND part_id = $2 AND kind IN ('reserved', 'unreserved')), 0)::bigint`,
			jobID, part).Scan(&e.PricedAfterMilli, &e.TakenOutMilli, &e.ReservedBeforeMilli); err != nil {
			return nil, fmt.Errorf("read the shelf: %w", err)
		}
		want := max(e.PricedAfterMilli-e.TakenOutMilli, 0)
		e.ReservedAfterMilli = want
		if delta := want - e.ReservedBeforeMilli; delta != 0 {
			kind := "reserved"
			if delta < 0 {
				kind = "unreserved"
			}
			if err := moveTx(ctx, tx, scope,
				Movement{Kind: kind, Quantity: float64(delta) / 1000, Note: "Line corrected"},
				part, jobID, ""); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// LineRevision is one version of a line, for the history under it.
type LineRevision struct {
	Revision        int
	Change          string
	Values          LineValues
	Reason          string
	ApprovalCleared bool
	By              string
	At              time.Time
}

// LineHistory lists the versions a line has been through, oldest first.
func LineHistory(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, position int) ([]LineRevision, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []LineRevision
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT r.revision, r.change, r.kind, r.description, (r.quantity * 1000)::bigint,
			       r.unit_price_minor, r.vat_rate_bp, r.cost_bearer, coalesce(r.part_id::text, ''),
			       coalesce(r.reason, ''), r.approval_cleared, coalesce(p.display_name, ''),
			       r.at
			FROM work_order_line_revisions r
			JOIN work_order_lines l ON l.id = r.line_id
			LEFT JOIN users u ON u.id = r.by_user
			LEFT JOIN people p ON p.id = u.person_id
			WHERE l.work_order_id = $1 AND l.position = $2
			ORDER BY r.revision`, jobID, position)
		if err != nil {
			return fmt.Errorf("line history: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r LineRevision
			if err := rows.Scan(&r.Revision, &r.Change, &r.Values.Kind, &r.Values.Description,
				&r.Values.QuantityMilli, &r.Values.UnitPriceMinor, &r.Values.VATRateBasis,
				&r.Values.CostBearer, &r.Values.PartID, &r.Reason, &r.ApprovalCleared, &r.By, &r.At); err != nil {
				return fmt.Errorf("scan revision: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// quoteStaleTx is whether the lines a quote was made from have since moved on:
// withdrawn, or at another quantity, price or rate than the customer was
// shown. A yes to such a quote would be a yes to a number the job no longer
// has.
func quoteStaleTx(ctx context.Context, tx pgx.Tx, quoteID string) (bool, error) {
	var stale bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM quote_lines q
		  JOIN work_order_lines l ON l.id = q.line_id
		  WHERE q.quote_id = $1
		    AND (l.withdrawn_at IS NOT NULL OR l.quantity <> q.quantity
		         OR l.unit_price_minor <> q.unit_price_minor OR l.vat_rate_bp <> q.vat_rate_bp))`,
		quoteID).Scan(&stale); err != nil {
		return false, fmt.Errorf("check the quote against its lines: %w", err)
	}
	return stale, nil
}
