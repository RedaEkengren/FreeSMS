package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// One job, in the order it happened.
//
// Nothing here is a second copy. Every event is read from the ledger that
// already holds it -- state changes, quotes and their decisions, line
// revisions, steps, parts, inspections, car movements, contacts, messages,
// invoices and payments, who was put on the job -- and put in one list. The
// only thing written for the timeline is what had no record: a note, and the
// record of a conversation.
//
// Who may see what is decided where it is read. A technician's list is built
// from queries that never touch a price, a quote, a message or an invoice, so
// there is nothing to hide afterwards.

// NoteVersion is an earlier wording of a note that was corrected.
type NoteVersion struct {
	Body string
	By   string
	At   time.Time
}

// Event is one thing that happened on a job.
type Event struct {
	// When it happened, and when it was written down; these differ for a
	// conversation recorded after the call.
	At         time.Time
	RecordedAt time.Time
	// The kind of event, for grouping and for the symbol on the page.
	Kind string
	// A catalogue key with at most one argument. ArgKey says the argument is
	// itself a catalogue key (a state, a way of getting in touch).
	Label  string
	Arg    string
	ArgKey bool
	Detail string
	Actor  string
	Link   string
	// An amount in minor units, for the roles that may see one.
	AmountMinor *int64
	// The customer was shown or told this.
	CustomerSees bool

	// For a note or a conversation.
	NoteID    string
	Internal  bool
	DeskOnly  bool
	Corrected bool
	Earlier   []NoteVersion
	// Whether the page should offer the viewer a correction.
	Mine bool

	rank int
	id   string
}

// ---- Notes ----------------------------------------------------------------

// NoteInput is a note or the record of a conversation.
type NoteInput struct {
	// "note" or "conversation".
	Kind string
	// For a conversation: "phone" or "in_person".
	Channel string
	Body    string
	// DeskOnly keeps it from the technician's timeline.
	DeskOnly bool
	// When it happened, if not just now.
	Occurred time.Time
}

func cleanNote(n *NoteInput) error {
	n.Body = strings.TrimSpace(n.Body)
	if n.Body == "" || len([]rune(n.Body)) > 1000 {
		return fmt.Errorf("%w: write something, in 1000 characters or fewer", ErrInvalid)
	}
	switch n.Kind {
	case "note":
		n.Channel = ""
	case "conversation":
		if n.Channel != "phone" && n.Channel != "in_person" {
			return fmt.Errorf("%w: say whether it was on the telephone or in person", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: a note or a conversation", ErrInvalid)
	}
	return nil
}

// AddNote writes a note on a job, or the record of a conversation with the
// customer. Any of the shop floor may leave a note for the next shift; the
// conversation with the customer is the counter's, and is kept from the
// technician's list unless the writer says otherwise.
//
// Neither is an approval. Agreement to a price is a quote decision, and a note
// that says the customer agreed changes nothing about the job.
func AddNote(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string, n NoteInput) (string, error) {
	if scope.Role != access.RoleTechnician && !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(jobID) {
		return "", ErrNotFound
	}
	if err := cleanNote(&n); err != nil {
		return "", err
	}
	if n.Kind == "conversation" && !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	// A technician's note is for whoever works on the job next. Only the
	// counter keeps one to itself.
	if !scope.Role.SeesCustomerPersonalData() {
		n.DeskOnly = false
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var opened time.Time
		if err := tx.QueryRow(ctx, `SELECT opened_at FROM work_orders WHERE id = $1`, jobID).Scan(&opened); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read job: %w", err)
		}
		occurred := n.Occurred
		if occurred.IsZero() {
			occurred = time.Now()
		}
		if occurred.After(time.Now().Add(time.Minute)) {
			return fmt.Errorf("%w: that has not happened yet", ErrInvalid)
		}
		// A call can come before the car does -- the booking, the enquiry --
		// but not a month before.
		if occurred.Before(opened.Add(-30 * 24 * time.Hour)) {
			return fmt.Errorf("%w: that is from long before the job was opened", ErrInvalid)
		}
		audience := "everyone"
		if n.DeskOnly {
			audience = "desk"
		}
		return tx.QueryRow(ctx, `
			INSERT INTO job_notes (shop_id, work_order_id, kind, channel, body, audience, occurred_at, author)
			VALUES ($1, $2, $3, nullif($4, ''), $5, $6, $7, $8) RETURNING id`,
			scope.ShopID, jobID, n.Kind, n.Channel, n.Body, audience, occurred, scope.UserID).Scan(&id)
	})
	return id, err
}

// CorrectNote replaces the wording of a note with a new one. The earlier
// wording stays and is shown as having been corrected. The writer may correct
// their own; the counter may correct anybody's. Two people correcting the same
// version is a conflict for the second.
func CorrectNote(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, noteID, body string) error {
	if scope.Role != access.RoleTechnician && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(noteID) {
		return ErrNotFound
	}
	body = strings.TrimSpace(body)
	if body == "" || len([]rune(body)) > 1000 {
		return fmt.Errorf("%w: write something, in 1000 characters or fewer", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var jobID, kind, audience, author string
		var channel *string
		var occurred time.Time
		if err := tx.QueryRow(ctx, `
			SELECT work_order_id, kind, audience, author::text, channel, occurred_at FROM job_notes WHERE id = $1`,
			noteID).Scan(&jobID, &kind, &audience, &author, &channel, &occurred); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read note: %w", err)
		}
		if !scope.Role.SeesCustomerPersonalData() && (author != scope.UserID || audience == "desk") {
			return access.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_notes (shop_id, work_order_id, kind, channel, body, audience, occurred_at, author, corrects_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			scope.ShopID, jobID, kind, channel, body, audience, occurred, scope.UserID, noteID); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" {
				return ErrRecordChanged
			}
			return fmt.Errorf("correct note: %w", err)
		}
		return nil
	})
}

// ---- Reading the timeline ------------------------------------------------

// TimelineFor lists what has happened on a job, oldest first. The same
// instant sorts the same way every time: by when it was written down, then by
// the kind of record, then by its identifier.
func TimelineFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]Event, error) {
	if !looksLikeUUID(jobID) {
		return nil, ErrNotFound
	}
	desk := scope.Role.SeesCustomerPersonalData()
	var out []Event
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM work_orders WHERE id = $1)`, jobID).Scan(&exists); err != nil {
			return fmt.Errorf("read job: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		var err error
		out, err = timelineTx(ctx, tx, scope, jobID, desk)
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if !a.RecordedAt.Equal(b.RecordedAt) {
			return a.RecordedAt.Before(b.RecordedAt)
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.id < b.id
	})
	return out, nil
}

func collect(ctx context.Context, tx pgx.Tx, q string, args []any, each func(r pgx.Rows) (Event, error)) ([]Event, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timeline: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := each(rows)
		if err != nil {
			return nil, fmt.Errorf("timeline: %w", err)
		}
		if e.RecordedAt.IsZero() {
			e.RecordedAt = e.At
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func timelineTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, desk bool) ([]Event, error) {
	var all []Event
	add := func(es []Event, err error) error {
		if err != nil {
			return err
		}
		all = append(all, es...)
		return nil
	}
	job := []any{jobID}

	// Intake.
	if err := add(collect(ctx, tx, `
		SELECT w.id::text, w.opened_at, coalesce(p.display_name, ''), coalesce(w.complaint, '')
		FROM work_orders w LEFT JOIN users u ON u.id = w.created_by LEFT JOIN people p ON p.id = u.person_id
		WHERE w.id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		e.Kind, e.Label, e.rank = "intake", "The job was opened", 0
		return e, r.Scan(&e.id, &e.At, &e.Actor, &e.Detail)
	})); err != nil {
		return nil, err
	}

	// A return visit: what was reported and the positions taken, on the job
	// that followed it up; and on the job it was about, that the car came back.
	// Claims are money matters and belong to the counter.
	if err := add(collect(ctx, tx, `
		SELECT e.id::text, e.at, e.kind, coalesce(e.note, ''), coalesce(p.display_name, ''), o.number, cb.symptoms
		FROM comeback_events e JOIN comebacks cb ON cb.id = e.comeback_id
		JOIN work_orders o ON o.id = cb.original_work_order_id
		LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE cb.follow_up_work_order_id = $1 AND (e.kind NOT LIKE 'claim\_%' OR $2)`, append(job, desk), func(r pgx.Rows) (Event, error) {
		var e Event
		var kind, note, symptoms string
		var original int64
		if err := r.Scan(&e.id, &e.At, &kind, &note, &e.Actor, &original, &symptoms); err != nil {
			return e, err
		}
		e.Kind, e.rank = "comeback", 1
		switch kind {
		case "reported":
			e.Label, e.Arg, e.Detail = "The car came back after job %s", fmt.Sprint(original), symptoms
		case "note":
			e.Label, e.Detail = "A note on the return visit", note
		default:
			e.Label, e.Arg, e.ArgKey, e.Detail = "Return visit: %s", ComebackKindLabel(kind), true, note
		}
		return e, nil
	})); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, tx, `
		SELECT cb.id::text, cb.opened_at, f.number, coalesce(p.display_name, '')
		FROM comebacks cb JOIN work_orders f ON f.id = cb.follow_up_work_order_id
		LEFT JOIN users u ON u.id = cb.opened_by LEFT JOIN people p ON p.id = u.person_id
		WHERE cb.original_work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var number int64
		if err := r.Scan(&e.id, &e.At, &number, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.Label, e.Arg, e.rank = "comeback", "The car came back; follow-up is job %s", fmt.Sprint(number), 1
		return e, nil
	})); err != nil {
		return nil, err
	}

	// State.
	if err := add(collect(ctx, tx, `
		SELECT c.id::text, c.at, c.to_state, coalesce(p.display_name, '')
		FROM work_order_state_changes c LEFT JOIN users u ON u.id = c.changed_by LEFT JOIN people p ON p.id = u.person_id
		WHERE c.work_order_id = $1 AND c.from_state IS NOT NULL`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var to string
		if err := r.Scan(&e.id, &e.At, &to, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.Label, e.Arg, e.ArgKey, e.rank = "state", "Moved to: %s", State(to).Label(), true, 1
		return e, nil
	})); err != nil {
		return nil, err
	}

	// Scope and price: the lines as they were written, with prices for the
	// counter only.
	if err := add(collect(ctx, tx, `
		SELECT r.id::text, r.at, r.change, r.description, coalesce(r.reason, ''), r.approval_cleared,
		       coalesce(p.display_name, ''),
		       CASE WHEN $2 THEN round(r.quantity * r.unit_price_minor)::bigint END
		FROM work_order_line_revisions r
		JOIN work_order_lines l ON l.id = r.line_id
		LEFT JOIN users u ON u.id = r.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE l.work_order_id = $1 AND r.revision > 0`, append(job, desk), func(r pgx.Rows) (Event, error) {
		var e Event
		var change string
		var cleared bool
		if err := r.Scan(&e.id, &e.At, &change, &e.Arg, &e.Detail, &cleared, &e.Actor, &e.AmountMinor); err != nil {
			return e, err
		}
		e.Kind, e.rank = "scope", 2
		switch change {
		case "added":
			e.Label = "Line added: %s"
		case "edited":
			e.Label = "Line corrected: %s"
		default:
			e.Label = "Line withdrawn: %s"
		}
		if cleared {
			e.Detail = strings.TrimSpace(e.Detail + " (agreement taken off the line)")
		}
		return e, nil
	})); err != nil {
		return nil, err
	}

	// Steps.
	if err := add(collect(ctx, tx, `
		SELECT v.id::text, v.at, v.to_status, coalesce(v.blocker, ''), coalesce(v.note, ''), t.title, coalesce(p.display_name, '')
		FROM job_task_events v JOIN job_tasks t ON t.id = v.task_id
		LEFT JOIN users u ON u.id = v.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE v.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var to, blocker string
		if err := r.Scan(&e.id, &e.At, &to, &blocker, &e.Detail, &e.Arg, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.rank = "work", 3
		switch to {
		case "pending":
			e.Label = "Step added: %s"
		case "underway":
			e.Label = "Step started: %s"
		case "blocked":
			e.Label = "Step held up: %s"
			if b := BlockerLabel(blocker); b != "" {
				e.Detail = strings.TrimSpace(b + ". " + e.Detail)
			}
		case "done":
			e.Label = "Step done: %s"
		case "omitted":
			e.Label = "Step left out: %s"
		default:
			e.Label = "Customer decision about a step: %s"
		}
		return e, nil
	})); err != nil {
		return nil, err
	}

	// Findings.
	if err := add(collect(ctx, tx, `
		SELECT f.id::text, f.reported_at, f.note, coalesce(p.display_name, '')
		FROM findings f LEFT JOIN users u ON u.id = f.reported_by LEFT JOIN people p ON p.id = u.person_id
		WHERE f.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		e.Kind, e.Label, e.rank = "work", "Something was noticed", 4
		return e, r.Scan(&e.id, &e.At, &e.Detail, &e.Actor)
	})); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, tx, `
		SELECT f.id::text, f.handled_at, f.note, coalesce(p.display_name, '')
		FROM findings f LEFT JOIN users u ON u.id = f.handled_by LEFT JOIN people p ON p.id = u.person_id
		WHERE f.work_order_id = $1 AND f.handled_at IS NOT NULL`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		e.Kind, e.Label, e.rank = "work", "What was noticed was dealt with", 5
		return e, r.Scan(&e.id, &e.At, &e.Detail, &e.Actor)
	})); err != nil {
		return nil, err
	}

	// Parts: asked for, arrived, taken out, put back. Names and quantities,
	// never a cost.
	if err := add(collect(ctx, tx, `
		SELECT q.id::text, q.requested_at, q.description, coalesce(p.display_name, '')
		FROM part_requests q LEFT JOIN users u ON u.id = q.requested_by LEFT JOIN people p ON p.id = u.person_id
		WHERE q.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		e.Kind, e.Label, e.rank = "parts", "A part was asked for: %s", 6
		return e, r.Scan(&e.id, &e.At, &e.Arg, &e.Actor)
	})); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, tx, `
		SELECT q.id::text, coalesce(q.arrived_at, q.cancelled_at), q.description, q.arrived_at IS NOT NULL, coalesce(p.display_name, '')
		FROM part_requests q LEFT JOIN users u ON u.id = q.arrived_by LEFT JOIN people p ON p.id = u.person_id
		WHERE q.work_order_id = $1 AND (q.arrived_at IS NOT NULL OR q.cancelled_at IS NOT NULL)`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var arrived bool
		if err := r.Scan(&e.id, &e.At, &e.Arg, &arrived, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.rank = "parts", 7
		e.Label = "A part arrived: %s"
		if !arrived {
			e.Label = "A part request was cancelled: %s"
		}
		return e, nil
	})); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, tx, `
		SELECT m.id::text, m.moved_at, m.kind, pt.name, m.quantity::text, coalesce(p.display_name, '')
		FROM stock_movements m JOIN parts pt ON pt.id = m.part_id
		LEFT JOIN users u ON u.id = m.moved_by LEFT JOIN people p ON p.id = u.person_id
		WHERE m.work_order_id = $1 AND m.kind IN ('consumed', 'put_back')`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var kind, name, qty string
		if err := r.Scan(&e.id, &e.At, &kind, &name, &qty, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.rank, e.Arg = "parts", 8, name
		e.Detail = strings.TrimSuffix(strings.TrimRight(qty, "0"), ".")
		e.Label = "Taken out to the job: %s"
		if kind == "put_back" {
			e.Label = "Put back on the shelf: %s"
		}
		return e, nil
	})); err != nil {
		return nil, err
	}

	// Inspections.
	if err := add(collect(ctx, tx, `
		SELECT i.id::text, i.started_at, i.completed_at, i.template_name, coalesce(p.display_name, '')
		FROM inspections i LEFT JOIN users u ON u.id = i.performed_by LEFT JOIN people p ON p.id = u.person_id
		WHERE i.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var done *time.Time
		if err := r.Scan(&e.id, &e.At, &done, &e.Arg, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.Label, e.rank = "inspection", "Inspection started: %s", 9
		e.Link = "/inspections/" + e.id
		return e, nil
	})); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, tx, `
		SELECT i.id::text, i.completed_at, i.template_name, coalesce(p.display_name, ''),
		       (SELECT count(*) FROM inspection_items it WHERE it.inspection_id = i.id AND it.status = 'fail')::text
		FROM inspections i LEFT JOIN users u ON u.id = i.performed_by LEFT JOIN people p ON p.id = u.person_id
		WHERE i.work_order_id = $1 AND i.completed_at IS NOT NULL`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var failed string
		if err := r.Scan(&e.id, &e.At, &e.Arg, &e.Actor, &failed); err != nil {
			return e, err
		}
		e.Kind, e.Label, e.rank = "inspection", "Inspection finished: %s", 10
		if failed != "0" {
			e.Detail = failed + " failed"
		}
		e.Link = "/inspections/" + e.id
		return e, nil
	})); err != nil {
		return nil, err
	}

	// The car.
	if err := add(collect(ctx, tx, `
		SELECT v.id::text, v.at, v.event, coalesce(to_char(v.expected_back, 'YYYY-MM-DD'), ''), coalesce(p.display_name, '')
		FROM vehicle_presence v LEFT JOIN users u ON u.id = v.recorded_by LEFT JOIN people p ON p.id = u.person_id
		WHERE v.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var ev string
		if err := r.Scan(&e.id, &e.At, &ev, &e.Detail, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.rank = "car", 11
		switch ev {
		case "left":
			e.Label = "The car left"
		case "collected":
			e.Label = "The car was collected"
		case "returned":
			e.Label = "The car came back"
		default:
			e.Label = "The car's return was moved"
		}
		return e, nil
	})); err != nil {
		return nil, err
	}

	// Who is on it, and when it is planned.
	if err := add(collect(ctx, tx, `
		SELECT a.id::text, a.assigned_at, coalesce(pp.display_name, ''), coalesce(a.reason, ''), coalesce(pb.display_name, '')
		FROM job_assignments a JOIN users uu ON uu.id = a.user_id JOIN people pp ON pp.id = uu.person_id
		LEFT JOIN users ub ON ub.id = a.assigned_by LEFT JOIN people pb ON pb.id = ub.person_id
		WHERE a.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		e.Kind, e.Label, e.rank = "plan", "%s was put on the job", 12
		return e, r.Scan(&e.id, &e.At, &e.Arg, &e.Detail, &e.Actor)
	})); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, tx, `
		SELECT a.id::text, a.released_at, coalesce(pp.display_name, ''), coalesce(a.reason, ''), coalesce(pb.display_name, '')
		FROM job_assignments a JOIN users uu ON uu.id = a.user_id JOIN people pp ON pp.id = uu.person_id
		LEFT JOIN users ub ON ub.id = a.released_by LEFT JOIN people pb ON pb.id = ub.person_id
		WHERE a.work_order_id = $1 AND a.released_at IS NOT NULL`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		e.Kind, e.Label, e.rank = "plan", "%s was taken off the job", 13
		return e, r.Scan(&e.id, &e.At, &e.Arg, &e.Detail, &e.Actor)
	})); err != nil {
		return nil, err
	}
	if err := add(collect(ctx, tx, `
		SELECT j.id::text, j.at, j.version::text, coalesce(j.note, ''), coalesce(j.override_reason, ''), coalesce(p.display_name, '')
		FROM job_plans j LEFT JOIN users u ON u.id = j.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE j.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
		var e Event
		var note, override string
		if err := r.Scan(&e.id, &e.At, &e.Arg, &note, &override, &e.Actor); err != nil {
			return e, err
		}
		e.Kind, e.Label, e.rank = "plan", "The plan was set (version %s)", 14
		e.Detail = strings.TrimSpace(note + " " + override)
		return e, nil
	})); err != nil {
		return nil, err
	}

	// The counter's side: what the customer was shown, said, was told and was
	// billed. None of it is read for anybody else.
	if desk {
		if err := add(collect(ctx, tx, `
			SELECT q.id::text, q.presented_at, q.version::text, q.gross_minor, coalesce(p.display_name, '')
			FROM quotes q LEFT JOIN users u ON u.id = q.presented_by LEFT JOIN people p ON p.id = u.person_id
			WHERE q.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
			var e Event
			var gross int64
			if err := r.Scan(&e.id, &e.At, &e.Arg, &gross, &e.Actor); err != nil {
				return e, err
			}
			e.Kind, e.Label, e.rank, e.CustomerSees, e.AmountMinor = "price", "Quote version %s put to the customer", 15, true, &gross
			return e, nil
		})); err != nil {
			return nil, err
		}
		if err := add(collect(ctx, tx, `
			SELECT d.id::text, d.decided_at, d.decision, d.via, d.agreed_gross_minor, coalesce(d.note, ''), coalesce(p.display_name, '')
			FROM quote_decisions d LEFT JOIN users u ON u.id = d.recorded_by LEFT JOIN people p ON p.id = u.person_id
			WHERE d.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
			var e Event
			var decision, via string
			if err := r.Scan(&e.id, &e.At, &decision, &via, &e.AmountMinor, &e.Detail, &e.Actor); err != nil {
				return e, err
			}
			e.Kind, e.rank, e.CustomerSees = "approval", 16, true
			e.Label = "The customer said yes (%s)"
			if decision != "approved" {
				e.Label = "The customer said no (%s)"
			}
			switch via {
			case "link":
				e.Arg, e.Actor = "on their link", firstNonEmpty(e.Actor, "the customer")
			case "phone":
				e.Arg = "by telephone"
			default:
				e.Arg = "at the counter"
			}
			e.ArgKey = true
			return e, nil
		})); err != nil {
			return nil, err
		}
		if err := add(collect(ctx, tx, `
			SELECT c.id::text, c.at, c.how, coalesce(c.note, ''), coalesce(p.display_name, '')
			FROM customer_contacts c LEFT JOIN users u ON u.id = c.recorded_by LEFT JOIN people p ON p.id = u.person_id
			WHERE c.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
			var e Event
			var how string
			if err := r.Scan(&e.id, &e.At, &how, &e.Detail, &e.Actor); err != nil {
				return e, err
			}
			e.Kind, e.Label, e.Arg, e.ArgKey, e.rank, e.CustomerSees = "contact", "The customer was contacted: %s", ContactLabel(how), true, 17, how != "no_answer"
			return e, nil
		})); err != nil {
			return nil, err
		}
		if err := add(collect(ctx, tx, `
			SELECT m.id::text, m.created_at, m.about, m.channel, coalesce(p.display_name, ''),
			       coalesce((SELECT e.event FROM message_events e WHERE e.message_id = m.id ORDER BY e.at DESC, e.id DESC LIMIT 1), 'queued')
			FROM outbound_messages m LEFT JOIN users u ON u.id = m.created_by LEFT JOIN people p ON p.id = u.person_id
			WHERE m.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
			var e Event
			var about, channel, last string
			if err := r.Scan(&e.id, &e.At, &about, &channel, &e.Actor, &last); err != nil {
				return e, err
			}
			e.Kind, e.Label, e.rank, e.CustomerSees = "message", "A message was sent: %s", 18, true
			e.Arg = channel
			e.Detail = about + ", " + last
			return e, nil
		})); err != nil {
			return nil, err
		}
		if err := add(collect(ctx, tx, `
			SELECT i.id::text, i.issued_at, i.series || '-' || i.number, i.gross_minor, i.credit_of_id IS NOT NULL, coalesce(p.display_name, '')
			FROM invoices i LEFT JOIN users u ON u.id = i.issued_by LEFT JOIN people p ON p.id = u.person_id
			WHERE i.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
			var e Event
			var gross int64
			var credit bool
			if err := r.Scan(&e.id, &e.At, &e.Arg, &gross, &credit, &e.Actor); err != nil {
				return e, err
			}
			e.Kind, e.Label, e.rank, e.AmountMinor, e.CustomerSees = "money", "Invoice %s was issued", 19, &gross, true
			if credit {
				e.Label = "Credit note %s was issued"
			}
			e.Link = "/invoices/" + e.id
			return e, nil
		})); err != nil {
			return nil, err
		}
		if err := add(collect(ctx, tx, `
			SELECT y.id::text, y.recorded_at, i.series || '-' || i.number, y.amount_minor, y.method, to_char(y.paid_on, 'YYYY-MM-DD'), y.reverses_id IS NOT NULL, coalesce(p.display_name, '')
			FROM invoice_payments y JOIN invoices i ON i.id = y.invoice_id
			LEFT JOIN users u ON u.id = y.recorded_by LEFT JOIN people p ON p.id = u.person_id
			WHERE i.work_order_id = $1`, job, func(r pgx.Rows) (Event, error) {
			var e Event
			var amount int64
			var method, paidOn string
			var reversal bool
			if err := r.Scan(&e.id, &e.At, &e.Arg, &amount, &method, &paidOn, &reversal, &e.Actor); err != nil {
				return e, err
			}
			e.Kind, e.Label, e.rank, e.AmountMinor = "money", "Payment recorded on %s", 20, &amount
			if reversal {
				e.Label = "A payment on %s was reversed"
			}
			e.Detail = method + ", paid " + paidOn
			e.Link = "/payments/" + e.id
			return e, nil
		})); err != nil {
			return nil, err
		}
	}

	// Notes, last: a chain of corrections is one event at the first wording's
	// time, carrying the latest words and the ones before.
	notes, err := notesTx(ctx, tx, scope, jobID, desk)
	if err != nil {
		return nil, err
	}
	return append(all, notes...), nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type noteRow struct {
	id, corrects, kind, channel, body, audience, author, authorName string
	occurred, recorded                                              time.Time
}

func notesTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, desk bool) ([]Event, error) {
	rows, err := tx.Query(ctx, `
		SELECT n.id::text, coalesce(n.corrects_id::text, ''), n.kind, coalesce(n.channel, ''), n.body, n.audience,
		       n.author::text, coalesce(p.display_name, ''), n.occurred_at, n.recorded_at
		FROM job_notes n LEFT JOIN users u ON u.id = n.author LEFT JOIN people p ON p.id = u.person_id
		WHERE n.work_order_id = $1 AND ($2 OR n.audience = 'everyone')
		ORDER BY n.recorded_at, n.id`, jobID, desk)
	if err != nil {
		return nil, fmt.Errorf("timeline notes: %w", err)
	}
	defer rows.Close()
	byID := map[string]*noteRow{}
	var order []*noteRow
	for rows.Next() {
		n := &noteRow{}
		if err := rows.Scan(&n.id, &n.corrects, &n.kind, &n.channel, &n.body, &n.audience, &n.author, &n.authorName, &n.occurred, &n.recorded); err != nil {
			return nil, fmt.Errorf("scan note: %w", err)
		}
		byID[n.id] = n
		order = append(order, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	next := map[string]*noteRow{} // the note that corrects this one
	for _, n := range order {
		if n.corrects != "" {
			next[n.corrects] = n
		}
	}
	var out []Event
	for _, n := range order {
		if n.corrects != "" {
			continue // not the first wording
		}
		head, versions := n, []NoteVersion{}
		for next[head.id] != nil {
			versions = append(versions, NoteVersion{Body: head.body, By: head.authorName, At: head.recorded})
			head = next[head.id]
		}
		e := Event{
			At: n.occurred, RecordedAt: head.recorded, Kind: "note", rank: 21, id: n.id,
			Detail: head.body, Actor: head.authorName, NoteID: head.id,
			Internal: true, DeskOnly: head.audience == "desk", Corrected: len(versions) > 0, Earlier: versions,
			Mine: desk || head.author == scope.UserID && head.audience == "everyone",
		}
		if n.kind == "conversation" {
			e.Kind, e.Label = "conversation", "A conversation with the customer"
			e.Arg, e.ArgKey = "by telephone", true
			if n.channel == "in_person" {
				e.Arg = "at the counter"
			}
		} else {
			e.Label = "A note"
		}
		out = append(out, e)
	}
	return out, nil
}

// ---- The handover --------------------------------------------------------

// Handover is where a job stands, for whoever picks it up.
type Handover struct {
	State    State
	Promised *time.Time
	// On the job now.
	People []string
	// What is left, and what is in the way.
	Unfinished []Task
	Blocked    int
	// Noticed and not dealt with; parts asked for and not arrived.
	OpenFindings int
	OpenParts    int
	// What happens next and whose it is.
	Step Step
	// The most recent note for the next shift, if there is one.
	LastNote *Event
}

// HandoverFor summarises a job for the next person to work on it. It holds
// what a technician already sees on the job page and nothing customer-shaped.
func HandoverFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (Handover, error) {
	if !looksLikeUUID(jobID) {
		return Handover{}, ErrNotFound
	}
	var h Handover
	desk := scope.Role.SeesCustomerPersonalData()
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var hasCustomer bool
		if err := tx.QueryRow(ctx, `SELECT state, promised_at, customer_id IS NOT NULL FROM work_orders WHERE id = $1`,
			jobID).Scan(&h.State, &h.Promised, &hasCustomer); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read job: %w", err)
		}
		plan, err := planTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		for _, a := range plan.On {
			h.People = append(h.People, a.Name)
		}
		tasks, err := tasksTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if t.Outstanding() {
				h.Unfinished = append(h.Unfinished, t)
				if t.Status == TaskBlocked {
					h.Blocked++
				}
			}
		}
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM findings WHERE work_order_id = $1 AND handled_at IS NULL),
			       (SELECT count(*) FROM part_requests WHERE work_order_id = $1 AND arrived_at IS NULL AND cancelled_at IS NULL)`,
			jobID).Scan(&h.OpenFindings, &h.OpenParts); err != nil {
			return fmt.Errorf("count open items: %w", err)
		}
		h.Step = NextStep(StepFacts{State: h.State, HasCustomer: hasCustomer, HasTechnician: len(h.People) > 0,
			HasPromise: h.Promised != nil, OpenRequests: h.OpenParts})
		notes, err := notesTx(ctx, tx, scope, jobID, desk)
		if err != nil {
			return err
		}
		for i := range notes {
			if notes[i].Kind != "note" {
				continue
			}
			if h.LastNote == nil || notes[i].RecordedAt.After(h.LastNote.RecordedAt) {
				n := notes[i]
				h.LastNote = &n
			}
		}
		return nil
	})
	return h, err
}

// RecordedLater says the event was written down well after it happened, which
// the page shows so a late entry is not mistaken for a late event.
func (e Event) RecordedLater() bool { return e.RecordedAt.Sub(e.At) > 5*time.Minute }
