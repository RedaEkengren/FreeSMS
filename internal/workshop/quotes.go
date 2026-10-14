package workshop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// A quote is what the customer was shown, frozen as a numbered version, and a
// decision points at that version. The lines of a job are the shop's working
// copy; the quote is the customer's copy, and "yes" is a yes to that copy --
// to its lines, its amounts and its total, not to whatever the job comes to
// by the time somebody invoices it.

// ErrQuoteSuperseded is a decision on a version that is no longer the latest:
// a link opened before the shop put a new quote forward, or a form that sat
// in an offline queue. It never approves the newer one by accident.
var ErrQuoteSuperseded = errors.New("workshop: that quote has been replaced by a newer one")

// ErrNeedsApproval is moving a job to approved with nothing recorded that
// the customer agreed to what they are being asked to pay for.
var ErrNeedsApproval = errors.New(
	"workshop: the customer's agreement is not recorded; put a quote to them, or record their yes with the amount they agreed to")

// QuoteLine is a line as the customer saw it.
type QuoteLine struct {
	Position       int
	Kind           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
	NetMinor       int64
	VATMinor       int64
}

// Quantity renders the quantity as the job page does.
func (l QuoteLine) Quantity() string { return Line{QuantityMilli: l.QuantityMilli}.Quantity() }

// QuoteDecision is one answer to one version.
type QuoteDecision struct {
	Decision         string // "approved" or "declined"
	Via              string // "link", "phone" or "counter"
	RecordedBy       string // who at the workshop wrote it down; empty on the link
	AgreedGrossMinor *int64
	Note             string
	DecidedAt        time.Time
}

// Quote is one numbered version of what a job was put to the customer as.
type Quote struct {
	ID          string
	Version     int
	NetMinor    int64
	VATMinor    int64
	GrossMinor  int64
	PresentedAt time.Time
	Lines       []QuoteLine
	// The latest answer to this version; nil while it is unanswered.
	Decision *QuoteDecision

	lineIDs []string
}

// AgreedMinor is the amount agreed to, or zero when none was.
func (d QuoteDecision) AgreedMinor() int64 {
	if d.AgreedGrossMinor == nil {
		return 0
	}
	return *d.AgreedGrossMinor
}

// QuoteDecisionRow is the record of an answer, for the job's history.
type QuoteDecisionRow struct {
	Version int
	QuoteDecision
}

// QuoteView is what the front desk sees of a job's quotes.
type QuoteView struct {
	// The newest version, or nil if none has been put to the customer.
	Latest *Quote
	// Lines the customer has not been asked about, or has not answered.
	Pending int
	// Whether the job may be moved to approved.
	CanApprove bool
	// Every answer to every version, oldest first.
	Decisions []QuoteDecisionRow
}

// quotable is a job line that has not been agreed or declined.
type quotable struct {
	id   string
	line QuoteLine
}

const pendingLines = `
	SELECT id, kind, description, (quantity * 1000)::bigint, unit_price_minor, vat_rate_bp
	FROM work_order_lines
	WHERE work_order_id = $1 AND cost_bearer = 'customer'
	  AND approved_at IS NULL AND declined_at IS NULL
	ORDER BY position`

func pendingTx(ctx context.Context, tx pgx.Tx, jobID string) ([]quotable, error) {
	rows, err := tx.Query(ctx, pendingLines, jobID)
	if err != nil {
		return nil, fmt.Errorf("read the lines to quote: %w", err)
	}
	defer rows.Close()
	var out []quotable
	for rows.Next() {
		var q quotable
		if err := rows.Scan(&q.id, &q.line.Kind, &q.line.Description, &q.line.QuantityMilli,
			&q.line.UnitPriceMinor, &q.line.VATRateBasis); err != nil {
			return nil, fmt.Errorf("scan a line to quote: %w", err)
		}
		q.line.NetMinor = money.LineNet(q.line.QuantityMilli, q.line.UnitPriceMinor)
		q.line.VATMinor = money.VAT(q.line.NetMinor, q.line.VATRateBasis)
		out = append(out, q)
	}
	return out, rows.Err()
}

// latestQuoteTx reads the newest version of a job's quote with its lines and
// its latest answer, or nil.
func latestQuoteTx(ctx context.Context, tx pgx.Tx, jobID string) (*Quote, error) {
	var q Quote
	err := tx.QueryRow(ctx, `
		SELECT id, version, net_minor, vat_minor, gross_minor, presented_at
		FROM quotes WHERE work_order_id = $1 ORDER BY version DESC LIMIT 1`, jobID).
		Scan(&q.ID, &q.Version, &q.NetMinor, &q.VATMinor, &q.GrossMinor, &q.PresentedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the quote: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT coalesce(line_id::text, ''), position, kind, description, (quantity * 1000)::bigint,
		       unit_price_minor, vat_rate_bp, net_minor, vat_minor
		FROM quote_lines WHERE quote_id = $1 ORDER BY position`, q.ID)
	if err != nil {
		return nil, fmt.Errorf("read the quote's lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l QuoteLine
		var id string
		if err := rows.Scan(&id, &l.Position, &l.Kind, &l.Description, &l.QuantityMilli,
			&l.UnitPriceMinor, &l.VATRateBasis, &l.NetMinor, &l.VATMinor); err != nil {
			return nil, fmt.Errorf("scan a quote line: %w", err)
		}
		q.Lines = append(q.Lines, l)
		q.lineIDs = append(q.lineIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	var d QuoteDecision
	var by *string
	var note *string
	err = tx.QueryRow(ctx, `
		SELECT d.decision, d.via, p.display_name, d.agreed_gross_minor, d.note, d.decided_at
		FROM quote_decisions d
		LEFT JOIN users u ON u.id = d.recorded_by
		LEFT JOIN people p ON p.id = u.person_id
		WHERE d.quote_id = $1 ORDER BY d.decided_at DESC, d.id DESC LIMIT 1`, q.ID).
		Scan(&d.Decision, &d.Via, &by, &d.AgreedGrossMinor, &note, &d.DecidedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("read the answer: %w", err)
	default:
		if by != nil {
			d.RecordedBy = *by
		}
		if note != nil {
			d.Note = *note
		}
		q.Decision = &d
	}
	return &q, nil
}

func sameIDs(a []string, b []quotable) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i].id {
			return false
		}
	}
	return true
}

func lockedState(ctx context.Context, tx pgx.Tx, jobID string) (State, error) {
	var state State
	if err := tx.QueryRow(ctx,
		`SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("lock order: %w", err)
	}
	return state, nil
}

func settled(s State) bool {
	switch s {
	case StateInvoiced, StateClosed, StateCancelled, StateDeclined:
		return true
	}
	return false
}

// PresentQuote puts to the customer the lines they have not yet answered, as
// the next numbered version, and returns it.
//
// A version still waiting for an answer, with exactly these lines, is
// returned as it is: a double click does not number twice. A new version
// replaces the old for the customer's link; the old stays, with its answer if
// it had one. Work added after a yes comes out as a version of its own
// holding only the new lines, so what was agreed is not asked again.
func PresentQuote(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (Quote, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Quote{}, access.ErrForbidden
	}
	var out Quote
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		state, err := lockedState(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if settled(state) {
			return fmt.Errorf("%w: a %s order is not quoted", ErrInvalid, state)
		}
		pending, err := pendingTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return fmt.Errorf("%w: there is nothing on this order that the customer has not answered", ErrInvalid)
		}
		latest, err := latestQuoteTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if latest != nil && latest.Decision == nil && sameIDs(latest.lineIDs, pending) {
			out = *latest
			return nil
		}
		version := 1
		if latest != nil {
			version = latest.Version + 1
		}
		mlines := make([]money.Line, 0, len(pending))
		for _, p := range pending {
			mlines = append(mlines, money.Line{QuantityMilli: p.line.QuantityMilli,
				UnitPriceMinor: p.line.UnitPriceMinor, VATRateBasis: p.line.VATRateBasis, ChargedToCustomer: true})
		}
		totals := money.Compute(mlines)
		var by *string
		if scope.UserID != "" {
			by = &scope.UserID
		}
		var id string
		var at time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO quotes (shop_id, work_order_id, version, net_minor, vat_minor, gross_minor, presented_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, presented_at`,
			scope.ShopID, jobID, version, totals.NetMinor, totals.VATMinor, totals.GrossMinor, by).Scan(&id, &at); err != nil {
			return fmt.Errorf("write the quote: %w", err)
		}
		for i, p := range pending {
			if _, err := tx.Exec(ctx, `
				INSERT INTO quote_lines (shop_id, quote_id, line_id, position, kind, description,
				       quantity, unit_price_minor, vat_rate_bp, net_minor, vat_minor)
				VALUES ($1, $2, $3, $4, $5, $6, ($7::bigint)::numeric / 1000, $8, $9, $10, $11)`,
				scope.ShopID, id, p.id, i+1, p.line.Kind, p.line.Description, p.line.QuantityMilli,
				p.line.UnitPriceMinor, p.line.VATRateBasis, p.line.NetMinor, p.line.VATMinor); err != nil {
				return fmt.Errorf("write a quote line: %w", err)
			}
		}
		q, err := latestQuoteTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		out = *q
		return nil
	})
	return out, err
}

// QuoteFor is the front desk's view of a job's quotes.
//
// Two statements, because the job page is read on every live refresh and
// each one is a round trip: the newest version with its lines and every
// answer to every version in one, and the agreement rule in the other.
func QuoteFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (QuoteView, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return QuoteView{}, access.ErrForbidden
	}
	var v QuoteView
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var q Quote
		var lines, answers []byte
		err := tx.QueryRow(ctx, `
			SELECT q.id, q.version, q.net_minor, q.vat_minor, q.gross_minor, q.presented_at,
			       coalesce((SELECT jsonb_agg(jsonb_build_object(
			           'position', l.position, 'kind', l.kind, 'description', l.description,
			           'quantity_milli', (l.quantity * 1000)::bigint, 'unit_price_minor', l.unit_price_minor,
			           'vat_rate_bp', l.vat_rate_bp, 'net_minor', l.net_minor, 'vat_minor', l.vat_minor)
			           ORDER BY l.position) FROM quote_lines l WHERE l.quote_id = q.id), '[]'),
			       coalesce((SELECT jsonb_agg(jsonb_build_object(
			           'version', v.version, 'quote_id', v.id, 'decision', d.decision, 'via', d.via,
			           'by', coalesce(p.display_name, ''), 'agreed', d.agreed_gross_minor,
			           'note', coalesce(d.note, ''), 'at', d.decided_at)
			           ORDER BY d.decided_at, d.id)
			         FROM quote_decisions d
			         JOIN quotes v ON v.id = d.quote_id
			         LEFT JOIN users u ON u.id = d.recorded_by
			         LEFT JOIN people p ON p.id = u.person_id
			         WHERE d.work_order_id = q.work_order_id), '[]')
			FROM quotes q WHERE q.work_order_id = $1 ORDER BY q.version DESC LIMIT 1`, jobID).
			Scan(&q.ID, &q.Version, &q.NetMinor, &q.VATMinor, &q.GrossMinor, &q.PresentedAt, &lines, &answers)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read the quote: %w", err)
		default:
			var rawLines []struct {
				Position    int    `json:"position"`
				Kind        string `json:"kind"`
				Description string `json:"description"`
				Qty         int64  `json:"quantity_milli"`
				Unit        int64  `json:"unit_price_minor"`
				VAT         int    `json:"vat_rate_bp"`
				Net         int64  `json:"net_minor"`
				VATMinor    int64  `json:"vat_minor"`
			}
			if err := json.Unmarshal(lines, &rawLines); err != nil {
				return fmt.Errorf("read the quote's lines: %w", err)
			}
			for _, l := range rawLines {
				q.Lines = append(q.Lines, QuoteLine{Position: l.Position, Kind: l.Kind, Description: l.Description,
					QuantityMilli: l.Qty, UnitPriceMinor: l.Unit, VATRateBasis: l.VAT, NetMinor: l.Net, VATMinor: l.VATMinor})
			}
			var rawAnswers []struct {
				Version  int       `json:"version"`
				QuoteID  string    `json:"quote_id"`
				Decision string    `json:"decision"`
				Via      string    `json:"via"`
				By       string    `json:"by"`
				Agreed   *int64    `json:"agreed"`
				Note     string    `json:"note"`
				At       time.Time `json:"at"`
			}
			if err := json.Unmarshal(answers, &rawAnswers); err != nil {
				return fmt.Errorf("read the answers: %w", err)
			}
			for _, a := range rawAnswers {
				d := QuoteDecision{Decision: a.Decision, Via: a.Via, RecordedBy: a.By,
					AgreedGrossMinor: a.Agreed, Note: a.Note, DecidedAt: a.At}
				v.Decisions = append(v.Decisions, QuoteDecisionRow{Version: a.Version, QuoteDecision: d})
				if a.QuoteID == q.ID {
					dd := d
					q.Decision = &dd
				}
			}
			v.Latest = &q
		}
		pending, err := agreementTx(ctx, tx, jobID)
		v.Pending = pending.lines
		v.CanApprove = pending.err == nil
		return err
	})
	return v, err
}

// QuoteAnswer is what the front desk writes down when the customer answered
// by telephone or at the counter.
type QuoteAnswer struct {
	Decision string // "approved" or "declined"
	Via      string // "phone" or "counter"
	// What the customer said they would pay, including VAT, in minor units.
	// Required for a yes and must be the quote's total.
	AgreedGrossMinor int64
	Note             string
}

// RecordQuoteAnswer writes down a yes or a no given to the front desk.
//
// The amount is typed, not defaulted, and has to match the version's total:
// an approval is of a number somebody said out loud, which is what the
// conversation about it later will be about.
func RecordQuoteAnswer(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, quoteID string, a QuoteAnswer) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if a.Decision != "approved" && a.Decision != "declined" {
		return fmt.Errorf("%w: %q is not approved or declined", ErrInvalid, a.Decision)
	}
	if a.Via != "phone" && a.Via != "counter" {
		return fmt.Errorf("%w: say whether it was by telephone or at the counter", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		state, err := lockedState(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if settled(state) {
			return fmt.Errorf("%w: a %s order is not quoted", ErrInvalid, state)
		}
		latest, err := latestQuoteTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if latest == nil || latest.ID != quoteID {
			return ErrQuoteSuperseded
		}
		var agreed *int64
		if a.Decision == "approved" {
			if a.AgreedGrossMinor != latest.GrossMinor {
				return fmt.Errorf("%w: the quote comes to %s including VAT, not %s",
					ErrInvalid, money.Format(latest.GrossMinor), money.Format(a.AgreedGrossMinor))
			}
			agreed = &a.AgreedGrossMinor
		}
		var by *string
		if scope.UserID != "" {
			by = &scope.UserID
		}
		return decideTx(ctx, tx, scope.ShopID, jobID, latest, a.Decision, a.Via, by, nil, agreed, strings.TrimSpace(a.Note))
	})
}

// QuoteAnswerFromLink is the customer's own yes or no on their link.
func QuoteAnswerFromLink(ctx context.Context, pool *pgxpool.Pool, shopID, token, quoteID, decision string) error {
	if decision != "approved" && decision != "declined" {
		return fmt.Errorf("%w: %q is not approved or declined", ErrInvalid, decision)
	}
	if token == "" {
		return ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var jobID, linkID string
		err := tx.QueryRow(ctx, `
			SELECT work_order_id, id FROM job_links
			WHERE token_sha256 = $1 AND revoked_at IS NULL AND expires_at > now()`, sum[:]).Scan(&jobID, &linkID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShareNotUsable
		}
		if err != nil {
			return fmt.Errorf("read link: %w", err)
		}
		state, err := lockedState(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if settled(state) {
			return ErrShareNotUsable
		}
		latest, err := latestQuoteTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if latest == nil || latest.ID != quoteID {
			return ErrQuoteSuperseded
		}
		// Pressing the same button twice is the same answer.
		if latest.Decision != nil && latest.Decision.Decision == decision {
			return nil
		}
		var agreed *int64
		if decision == "approved" {
			agreed = &latest.GrossMinor
		}
		return decideTx(ctx, tx, shopID, jobID, latest, decision, "link", nil, &linkID, agreed, "")
	})
}

// decideTx writes the answer and what it does to the job's lines: a yes makes
// the quote's lines approved as of now, under this answer; a no declines the
// ones nobody has done yet. Earlier answers stay.
func decideTx(ctx context.Context, tx pgx.Tx, shopID, jobID string, q *Quote, decision, via string,
	by, linkID *string, agreed *int64, note string) error {
	var id string
	var at time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO quote_decisions (shop_id, work_order_id, quote_id, decision, via, recorded_by,
		       job_link_id, agreed_gross_minor, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, '')) RETURNING id, decided_at`,
		shopID, jobID, q.ID, decision, via, by, linkID, agreed, note).Scan(&id, &at); err != nil {
		return fmt.Errorf("record the answer: %w", err)
	}
	if decision == "approved" {
		if _, err := tx.Exec(ctx, `
			UPDATE work_order_lines
			   SET approved_at = $2, declined_at = NULL, quote_decision_id = $3, updated_at = now()
			 WHERE id IN (SELECT line_id FROM quote_lines WHERE quote_id = $1 AND line_id IS NOT NULL)`,
			q.ID, at, id); err != nil {
			return fmt.Errorf("approve the quoted lines: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE work_order_lines
		   SET declined_at = $2, updated_at = now()
		 WHERE id IN (SELECT line_id FROM quote_lines WHERE quote_id = $1 AND line_id IS NOT NULL)
		   AND performed_at IS NULL`, q.ID, at); err != nil {
		return fmt.Errorf("decline the quoted lines: %w", err)
	}
	return nil
}

// agreement is what agreementTx found out about a job.
type agreement struct {
	lines int   // customer lines nobody has agreed to or declined
	err   error // ErrNeedsApproval, or nil if the job may be approved
}

// agreementTx reads the rule for moving a job to approved in one statement:
// whatever the customer is to pay for has been agreed to, on a recorded
// answer. A job with nothing on it for the customer to pay has nothing to
// agree to, and one whose every line was declined has nothing to go ahead
// with.
func agreementTx(ctx context.Context, tx pgx.Tx, jobID string) (agreement, error) {
	var pending, live, total int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE approved_at IS NULL AND declined_at IS NULL),
		       count(*) FILTER (WHERE declined_at IS NULL),
		       count(*)
		FROM work_order_lines WHERE work_order_id = $1 AND cost_bearer = 'customer'`, jobID).
		Scan(&pending, &live, &total); err != nil {
		return agreement{}, fmt.Errorf("check the agreement: %w", err)
	}
	a := agreement{lines: pending}
	if pending > 0 || (total > 0 && live == 0) {
		a.err = ErrNeedsApproval
	}
	return a, nil
}

// approvalGateTx refuses to approve a job without a recorded agreement.
func approvalGateTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	a, err := agreementTx(ctx, tx, jobID)
	if err != nil {
		return err
	}
	return a.err
}

// CustomerQuote is the quote as the customer's link shows it.
type CustomerQuote struct {
	ID         string
	Version    int
	Lines      []QuoteLine
	NetMinor   int64
	VATMinor   int64
	GrossMinor int64
	// "", "approved" or "declined": their latest answer to this version.
	Answer     string
	AnsweredAt time.Time
}
