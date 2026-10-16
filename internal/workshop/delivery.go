package workshop

import (
	"bytes"
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
)

// What the customer is given back with the car.
//
// It is built from what was recorded as done -- lines marked performed, steps
// finished, parts taken out to the job, inspections completed -- and never from
// a quotation: an approved line nobody did is "agreed, not done", not "done".
// It says what was declined, what was left, and what the workshop noticed and
// did not deal with. It does not say the car is safe; it lists the work.
//
// When it is handed over it is frozen. A correction is the next version, and
// the one the customer was given stays as it was.

// SummaryItem is a line of work or a thing to know, with when.
type SummaryItem struct {
	Text string     `json:"text"`
	At   *time.Time `json:"at,omitempty"`
	// For work not done: what is in the way.
	Why string `json:"why,omitempty"`
}

// SummaryPart is a part fitted.
type SummaryPart struct {
	Name  string `json:"name"`
	Count string `json:"quantity"`
}

// SummaryInspection is a check that was made, with what it found.
type SummaryInspection struct {
	Name      string     `json:"name"`
	At        *time.Time `json:"at,omitempty"`
	Passed    int        `json:"passed"`
	Attention int        `json:"attention"`
	Failed    int        `json:"failed"`
	// What needs attention or failed, in the words the technician wrote.
	Findings []SummaryItem `json:"findings,omitempty"`
}

// SummaryContent is the summary itself.
type SummaryContent struct {
	Registration string `json:"registration"`
	Make         string `json:"make,omitempty"`
	Model        string `json:"model,omitempty"`
	// The odometer when the car came in, and the latest reading since.
	OdometerIn  int64 `json:"odometer_in,omitempty"`
	OdometerOut int64 `json:"odometer_out,omitempty"`

	Completed       []SummaryItem       `json:"completed,omitempty"`
	Parts           []SummaryPart       `json:"parts,omitempty"`
	Inspections     []SummaryInspection `json:"inspections,omitempty"`
	Declined        []SummaryItem       `json:"declined,omitempty"`
	Unfinished      []SummaryItem       `json:"unfinished,omitempty"`
	Unresolved      []SummaryItem       `json:"unresolved,omitempty"`
	Recommendations []SummaryItem       `json:"recommendations,omitempty"`
	// Set when the visit was a return for an earlier repair.
	Return *SummaryReturn `json:"return,omitempty"`
}

// SummaryReturn is what the customer is told about a return visit: what they
// reported and, only where the workshop has decided to cover it, that it did.
// Nothing here says whose fault it was; the reasons stay inside the workshop.
type SummaryReturn struct {
	OriginalNumber int64      `json:"original_number"`
	Reported       string     `json:"reported"`
	ReturnedOn     *time.Time `json:"returned_on,omitempty"`
	// "warranty" or "goodwill" when the repair is without charge for that
	// reason; empty otherwise.
	Covered string `json:"covered,omitempty"`
}

// Empty reports that nothing was recorded as done, left or noticed.
func (c SummaryContent) Empty() bool {
	return len(c.Completed) == 0 && len(c.Parts) == 0 && len(c.Inspections) == 0 && len(c.Declined) == 0 &&
		len(c.Unfinished) == 0 && len(c.Unresolved) == 0 && len(c.Recommendations) == 0
}

// DeliverySummary is an issued version.
type DeliverySummary struct {
	ID           string
	JobID        string
	Version      int
	Content      SummaryContent
	Instructions string
	DeliveredBy  string
	DeliveredTo  string
	Note         string
	DeliveredAt  time.Time
	// The version this one replaces, if any.
	Replaces int
	// Whether a later version has replaced this one.
	Superseded bool
}

// DeliveryDraft composes what a summary would say now. Nothing is stored.
func DeliveryDraft(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (SummaryContent, error) {
	if !looksLikeUUID(jobID) {
		return SummaryContent{}, ErrNotFound
	}
	var c SummaryContent
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = draftTx(ctx, tx, jobID)
		return err
	})
	return c, err
}

func draftTx(ctx context.Context, tx pgx.Tx, jobID string) (SummaryContent, error) {
	var c SummaryContent
	var vehicleID string
	var opened time.Time
	if err := tx.QueryRow(ctx, `
		SELECT w.vehicle_id::text, w.opened_at, coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, '')
		FROM work_orders w JOIN vehicles v ON v.id = w.vehicle_id
		LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
		WHERE w.id = $1`, jobID).Scan(&vehicleID, &opened, &c.Registration, &c.Make, &c.Model); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNotFound
		}
		return c, fmt.Errorf("read job: %w", err)
	}
	// The reading when it came in, and the latest since.
	var in, out *int64
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT km FROM odometer_readings WHERE vehicle_id = $1 AND read_at <= $2::timestamptz + interval '1 hour' ORDER BY read_at DESC LIMIT 1),
		       (SELECT km FROM odometer_readings WHERE vehicle_id = $1 AND read_at >= $2::timestamptz ORDER BY read_at DESC LIMIT 1)`,
		vehicleID, opened).Scan(&in, &out); err != nil {
		return c, fmt.Errorf("read odometer: %w", err)
	}
	if in != nil {
		c.OdometerIn = *in
	}
	if out != nil {
		c.OdometerOut = *out
	}

	collect := func(q string, each func(r pgx.Rows) error) error {
		rows, err := tx.Query(ctx, q, jobID)
		if err != nil {
			return fmt.Errorf("draft: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := each(rows); err != nil {
				return fmt.Errorf("draft: %w", err)
			}
		}
		return rows.Err()
	}

	// Done: lines marked performed, and steps finished that no line stands for.
	if err := collect(`
		SELECT l.description, l.performed_at FROM work_order_lines l
		WHERE l.work_order_id = $1 AND l.performed_at IS NOT NULL AND l.withdrawn_at IS NULL AND l.kind IN ('labour', 'sublet')
		ORDER BY l.performed_at, l.position`, func(r pgx.Rows) error {
		var it SummaryItem
		if err := r.Scan(&it.Text, &it.At); err != nil {
			return err
		}
		c.Completed = append(c.Completed, it)
		return nil
	}); err != nil {
		return c, err
	}
	if err := collect(`
		SELECT t.title, (SELECT max(e.at) FROM job_task_events e WHERE e.task_id = t.id AND e.to_status = 'done')
		FROM job_tasks t WHERE t.work_order_id = $1 AND t.line_id IS NULL AND t.status = 'done' ORDER BY t.position`, func(r pgx.Rows) error {
		var it SummaryItem
		if err := r.Scan(&it.Text, &it.At); err != nil {
			return err
		}
		c.Completed = append(c.Completed, it)
		return nil
	}); err != nil {
		return c, err
	}

	// Fitted: what came off the shelf to this job and was not put back.
	if err := collect(`
		SELECT p.name, (-sum(m.quantity))::text
		FROM stock_movements m JOIN parts p ON p.id = m.part_id
		WHERE m.work_order_id = $1 AND m.kind IN ('consumed', 'put_back')
		GROUP BY p.id, p.name HAVING sum(m.quantity) < 0 ORDER BY p.name`, func(r pgx.Rows) error {
		var p SummaryPart
		if err := r.Scan(&p.Name, &p.Count); err != nil {
			return err
		}
		p.Count = strings.TrimSuffix(strings.TrimRight(p.Count, "0"), ".")
		c.Parts = append(c.Parts, p)
		return nil
	}); err != nil {
		return c, err
	}

	// Checked.
	insp, err := tx.Query(ctx, `
		SELECT i.id::text, i.template_name, i.completed_at,
		       count(*) FILTER (WHERE it.status = 'pass'), count(*) FILTER (WHERE it.status = 'attention'), count(*) FILTER (WHERE it.status = 'fail')
		FROM inspections i LEFT JOIN inspection_items it ON it.inspection_id = i.id
		WHERE i.work_order_id = $1 AND i.completed_at IS NOT NULL
		GROUP BY i.id ORDER BY i.completed_at`, jobID)
	if err != nil {
		return c, fmt.Errorf("draft inspections: %w", err)
	}
	var ids []string
	for insp.Next() {
		var id string
		var s SummaryInspection
		if err := insp.Scan(&id, &s.Name, &s.At, &s.Passed, &s.Attention, &s.Failed); err != nil {
			insp.Close()
			return c, fmt.Errorf("scan inspection: %w", err)
		}
		c.Inspections = append(c.Inspections, s)
		ids = append(ids, id)
	}
	insp.Close()
	if err := insp.Err(); err != nil {
		return c, err
	}
	for i, id := range ids {
		items, err := tx.Query(ctx, `
			SELECT label || CASE WHEN coalesce(note, '') = '' THEN '' ELSE ': ' || note END, status
			FROM inspection_items WHERE inspection_id = $1 AND status IN ('attention', 'fail') ORDER BY position`, id)
		if err != nil {
			return c, fmt.Errorf("draft findings: %w", err)
		}
		for items.Next() {
			var it SummaryItem
			var status string
			if err := items.Scan(&it.Text, &status); err != nil {
				items.Close()
				return c, err
			}
			it.Why = status
			c.Inspections[i].Findings = append(c.Inspections[i].Findings, it)
		}
		items.Close()
		if err := items.Err(); err != nil {
			return c, err
		}
	}

	// Declined: what the customer said no to, and nobody did.
	if err := collect(`
		SELECT l.description, l.declined_at FROM work_order_lines l
		WHERE l.work_order_id = $1 AND l.declined_at IS NOT NULL AND l.performed_at IS NULL AND l.withdrawn_at IS NULL
		ORDER BY l.position`, func(r pgx.Rows) error {
		var it SummaryItem
		if err := r.Scan(&it.Text, &it.At); err != nil {
			return err
		}
		c.Declined = append(c.Declined, it)
		return nil
	}); err != nil {
		return c, err
	}

	// Not done: agreed and not performed, and steps still open.
	if err := collect(`
		SELECT l.description,
		       coalesce((SELECT CASE t.status WHEN 'blocked' THEN CASE t.blocker WHEN 'part' THEN 'waiting for a part' WHEN 'customer' THEN 'waiting for the customer' ELSE 'held up' END END
		                   FROM job_tasks t WHERE t.line_id = l.id AND t.status NOT IN ('omitted') ORDER BY t.position LIMIT 1), '')
		FROM work_order_lines l
		WHERE l.work_order_id = $1 AND l.approved_at IS NOT NULL AND l.performed_at IS NULL AND l.declined_at IS NULL
		  AND l.withdrawn_at IS NULL AND l.kind IN ('labour', 'sublet')
		ORDER BY l.position`, func(r pgx.Rows) error {
		var it SummaryItem
		if err := r.Scan(&it.Text, &it.Why); err != nil {
			return err
		}
		c.Unfinished = append(c.Unfinished, it)
		return nil
	}); err != nil {
		return c, err
	}
	if err := collect(`
		SELECT t.title, CASE t.status WHEN 'blocked' THEN CASE t.blocker WHEN 'part' THEN 'waiting for a part' WHEN 'customer' THEN 'waiting for the customer' ELSE 'held up' END ELSE '' END
		FROM job_tasks t WHERE t.work_order_id = $1 AND t.line_id IS NULL AND t.status IN ('pending', 'underway', 'blocked')
		ORDER BY t.position`, func(r pgx.Rows) error {
		var it SummaryItem
		if err := r.Scan(&it.Text, &it.Why); err != nil {
			return err
		}
		c.Unfinished = append(c.Unfinished, it)
		return nil
	}); err != nil {
		return c, err
	}

	// Noticed and not dealt with: disclosed, not buried.
	if err := collect(`
		SELECT f.note, f.reported_at FROM findings f WHERE f.work_order_id = $1 AND f.handled_at IS NULL ORDER BY f.reported_at`, func(r pgx.Rows) error {
		var it SummaryItem
		if err := r.Scan(&it.Text, &it.At); err != nil {
			return err
		}
		c.Unresolved = append(c.Unresolved, it)
		return nil
	}); err != nil {
		return c, err
	}

	if err := collect(`
		SELECT body, coalesce(when_hint, '') FROM job_recommendations WHERE work_order_id = $1 AND withdrawn_at IS NULL ORDER BY at`, func(r pgx.Rows) error {
		var it SummaryItem
		if err := r.Scan(&it.Text, &it.Why); err != nil {
			return err
		}
		c.Recommendations = append(c.Recommendations, it)
		return nil
	}); err != nil {
		return c, err
	}

	var ret SummaryReturn
	var kind string
	err = tx.QueryRow(ctx, `
		SELECT o.number, cb.symptoms, cb.returned_on::timestamptz,
		       coalesce((SELECT e.kind FROM comeback_events e
		         WHERE e.comeback_id = cb.id AND e.kind IN ('reported', 'investigating', 'warranty', 'goodwill', 'unrelated')
		         ORDER BY e.at DESC, e.id DESC LIMIT 1), '')
		FROM comebacks cb JOIN work_orders o ON o.id = cb.original_work_order_id
		WHERE cb.follow_up_work_order_id = $1`, jobID).Scan(&ret.OriginalNumber, &ret.Reported, &ret.ReturnedOn, &kind)
	switch {
	case err == nil:
		if kind == "warranty" || kind == "goodwill" {
			ret.Covered = kind
		}
		c.Return = &ret
	case !errors.Is(err, pgx.ErrNoRows):
		return c, fmt.Errorf("draft return visit: %w", err)
	}
	return c, nil
}

// SummaryInput is the handover.
type SummaryInput struct {
	// Words from the workshop to the customer.
	Instructions string
	// Who it was handed to: not always the customer.
	DeliveredTo string
	Note        string
	// The odometer at collection, if it was read.
	OdometerKm *int64
}

// IssueSummary hands the summary over: it records what it said, who it was
// given to and by whom, and freezes it as the next version. Asking again with
// nothing changed returns the version already issued.
//
// It does not say the car is collected or notify anybody; those are their own
// records, and a customer told their car is ready is not a customer who has it.
func IssueSummary(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, in SummaryInput) (DeliverySummary, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return DeliverySummary{}, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return DeliverySummary{}, ErrNotFound
	}
	in.Instructions, in.DeliveredTo, in.Note = strings.TrimSpace(in.Instructions), strings.TrimSpace(in.DeliveredTo), strings.TrimSpace(in.Note)
	if in.DeliveredTo == "" || len([]rune(in.DeliveredTo)) > 100 {
		return DeliverySummary{}, fmt.Errorf("%w: say who it is being handed to", ErrInvalid)
	}
	if len([]rune(in.Instructions)) > 1500 || len([]rune(in.Note)) > 300 {
		return DeliverySummary{}, fmt.Errorf("%w: the instructions or the note are too long", ErrInvalid)
	}
	if in.OdometerKm != nil && (*in.OdometerKm < 0 || *in.OdometerKm >= 10000000) {
		return DeliverySummary{}, fmt.Errorf("%w: that is not an odometer reading", ErrInvalid)
	}
	var out DeliverySummary
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var state State
		var vehicleID string
		if err := tx.QueryRow(ctx, `SELECT state, vehicle_id::text FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state, &vehicleID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock job: %w", err)
		}
		switch state {
		case StateInProgress, StateAwaitingParts, StateReady, StateInvoiced, StateClosed, StateDeclined:
		default:
			return fmt.Errorf("%w: nothing has been done on this job yet, so there is nothing to hand over", ErrInvalid)
		}
		if in.OdometerKm != nil {
			var decreasing bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM odometer_readings WHERE vehicle_id = $1 AND km > $2)`, vehicleID, *in.OdometerKm).Scan(&decreasing); err != nil {
				return fmt.Errorf("compare odometer: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, decreasing) VALUES ($1, $2, $3, 'collection', $4)`,
				scope.ShopID, vehicleID, *in.OdometerKm, decreasing); err != nil {
				return fmt.Errorf("record odometer: %w", err)
			}
		}
		content, err := draftTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(content)
		if err != nil {
			return fmt.Errorf("encode summary: %w", err)
		}
		// The same again is the same, whoever pressed it twice.
		latest, err := latestSummaryTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if latest != nil {
			prev, _ := json.Marshal(latest.Content)
			if bytes.Equal(prev, raw) && latest.Instructions == in.Instructions && latest.DeliveredTo == in.DeliveredTo && latest.Note == in.Note {
				out = *latest
				return nil
			}
		}
		version := 1
		if latest != nil {
			version = latest.Version + 1
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO delivery_summaries (shop_id, work_order_id, version, content, instructions, delivered_by, delivered_to, note)
			VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, nullif($8, '')) RETURNING id`,
			scope.ShopID, jobID, version, raw, in.Instructions, scope.UserID, in.DeliveredTo, in.Note).Scan(&id); err != nil {
			return fmt.Errorf("issue summary: %w", err)
		}
		got, err := summaryTx(ctx, tx, jobID, version)
		if err != nil {
			return err
		}
		out = *got
		return nil
	})
	return out, err
}

func scanSummary(r pgx.Row) (*DeliverySummary, error) {
	var s DeliverySummary
	var raw []byte
	if err := r.Scan(&s.ID, &s.JobID, &s.Version, &raw, &s.Instructions, &s.DeliveredBy, &s.DeliveredTo, &s.Note, &s.DeliveredAt, &s.Superseded); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.Content); err != nil {
		return nil, fmt.Errorf("decode summary: %w", err)
	}
	if s.Version > 1 {
		s.Replaces = s.Version - 1
	}
	return &s, nil
}

const summarySelect = `
	SELECT d.id::text, d.work_order_id::text, d.version, d.content, coalesce(d.instructions, ''),
	       coalesce(p.display_name, ''), d.delivered_to, coalesce(d.note, ''), d.delivered_at,
	       EXISTS (SELECT 1 FROM delivery_summaries n WHERE n.work_order_id = d.work_order_id AND n.version > d.version)
	FROM delivery_summaries d LEFT JOIN users u ON u.id = d.delivered_by LEFT JOIN people p ON p.id = u.person_id`

func latestSummaryTx(ctx context.Context, tx pgx.Tx, jobID string) (*DeliverySummary, error) {
	s, err := scanSummary(tx.QueryRow(ctx, summarySelect+` WHERE d.work_order_id = $1 ORDER BY d.version DESC LIMIT 1`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

func summaryTx(ctx context.Context, tx pgx.Tx, jobID string, version int) (*DeliverySummary, error) {
	s, err := scanSummary(tx.QueryRow(ctx, summarySelect+` WHERE d.work_order_id = $1 AND d.version = $2`, jobID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// SummariesFor lists the versions issued on a job, newest first. Only the
// counter reads them: they name who the car was handed to.
func SummariesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]DeliverySummary, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	var out []DeliverySummary
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, summarySelect+` WHERE d.work_order_id = $1 ORDER BY d.version DESC`, jobID)
		if err != nil {
			return fmt.Errorf("list summaries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanSummary(rows)
			if err != nil {
				return err
			}
			out = append(out, *s)
		}
		return rows.Err()
	})
	return out, err
}

// SummaryVersion reads one issued version.
func SummaryVersion(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, version int) (DeliverySummary, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return DeliverySummary{}, access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return DeliverySummary{}, ErrNotFound
	}
	var out DeliverySummary
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		s, err := summaryTx(ctx, tx, jobID, version)
		if err != nil {
			return err
		}
		out = *s
		return nil
	})
	return out, err
}

// CustomerSummary is the latest issued summary for the job a link opens. Wrong,
// expired and closed links are one answer, as for the rest of the customer's
// page.
func CustomerSummary(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (DeliverySummary, string, error) {
	if token == "" {
		return DeliverySummary{}, "", ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	var out DeliverySummary
	var shop string
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var jobID string
		if err := tx.QueryRow(ctx, `
			SELECT w.id::text, s.name FROM job_links l JOIN work_orders w ON w.id = l.work_order_id JOIN shops s ON s.id = w.shop_id
			WHERE l.token_sha256 = $1 AND l.revoked_at IS NULL AND l.expires_at > now()`, sum[:]).Scan(&jobID, &shop); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrShareNotUsable
			}
			return fmt.Errorf("read link: %w", err)
		}
		s, err := latestSummaryTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if s == nil {
			return ErrNotFound
		}
		out = *s
		return nil
	})
	return out, shop, err
}

// ---- Recommendations -----------------------------------------------------

// Recommendation is something the workshop suggests doing later.
type Recommendation struct {
	ID          string
	Body        string
	When        string
	By          string
	At          time.Time
	WithdrawnAt *time.Time
	EnquiryID   string
}

// AddRecommendation notes something worth doing later. The technician is the
// one who sees it; the counter follows it up.
func AddRecommendation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, body, when string) error {
	if scope.Role != access.RoleTechnician && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return ErrNotFound
	}
	body, when = strings.TrimSpace(body), strings.TrimSpace(when)
	if body == "" || len([]rune(body)) > 300 || len([]rune(when)) > 80 {
		return fmt.Errorf("%w: say what, in 300 characters or fewer, and when in 80", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO job_recommendations (shop_id, work_order_id, body, when_hint, by_user)
			SELECT $1, w.id, $2, nullif($3, ''), $4 FROM work_orders w WHERE w.id = $5`,
			scope.ShopID, body, when, scope.UserID, jobID)
		if err != nil {
			return fmt.Errorf("add recommendation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RecommendationsFor lists what has been suggested on a job.
func RecommendationsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]Recommendation, error) {
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	var out []Recommendation
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT r.id::text, r.body, coalesce(r.when_hint, ''), coalesce(p.display_name, ''), r.at, r.withdrawn_at, coalesce(r.enquiry_id::text, '')
			FROM job_recommendations r LEFT JOIN users u ON u.id = r.by_user LEFT JOIN people p ON p.id = u.person_id
			WHERE r.work_order_id = $1 ORDER BY r.at`, jobID)
		if err != nil {
			return fmt.Errorf("list recommendations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r Recommendation
			if err := rows.Scan(&r.ID, &r.Body, &r.When, &r.By, &r.At, &r.WithdrawnAt, &r.EnquiryID); err != nil {
				return fmt.Errorf("scan recommendation: %w", err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// WithdrawRecommendation takes a suggestion back. Whoever made it may, and the
// counter may.
func WithdrawRecommendation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, recID string) error {
	if scope.Role != access.RoleTechnician && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(recID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var author string
		var gone *time.Time
		if err := tx.QueryRow(ctx, `SELECT by_user::text, withdrawn_at FROM job_recommendations WHERE id = $1 FOR UPDATE`, recID).Scan(&author, &gone); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock recommendation: %w", err)
		}
		if !scope.Role.SeesCustomerPersonalData() && author != scope.UserID {
			return access.ErrForbidden
		}
		if gone != nil {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE job_recommendations SET withdrawn_at = clock_timestamp(), withdrawn_by = $2 WHERE id = $1`, recID, scope.UserID)
		return err
	})
}

// RecommendationToEnquiry starts a future enquiry from a suggestion, for the
// counter to follow up. It is not an approval and nobody is sent anything: the
// enquiry is a note that this customer may be worth ringing, and what is said
// to them is the counter's to decide. Doing it twice finds the same enquiry.
func RecommendationToEnquiry(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, recID string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(recID) {
		return "", ErrNotFound
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var body, jobID string
		var existing, gone *string
		if err := tx.QueryRow(ctx, `
			SELECT body, work_order_id::text, enquiry_id::text, withdrawn_at::text FROM job_recommendations WHERE id = $1 FOR UPDATE`,
			recID).Scan(&body, &jobID, &existing, &gone); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock recommendation: %w", err)
		}
		if existing != nil {
			id = *existing
			return nil
		}
		if gone != nil {
			return fmt.Errorf("%w: that suggestion was taken back", ErrInvalid)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO enquiries (shop_id, subject, registration, vehicle_text, customer_name, customer_phone, created_by)
			SELECT $1, $2, r.registration, btrim(concat_ws(' ', v.make, v.model)),
			       coalesce(p.display_name, c.company_name), p.phone, $4
			FROM work_orders w JOIN vehicles v ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			LEFT JOIN customers c ON c.id = w.customer_id LEFT JOIN people p ON p.id = c.person_id
			WHERE w.id = $3 RETURNING id::text`, scope.ShopID, body, jobID, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("make enquiry: %w", err)
		}
		_, err := tx.Exec(ctx, `UPDATE job_recommendations SET enquiry_id = $2 WHERE id = $1`, recID, id)
		return err
	})
	return id, err
}
