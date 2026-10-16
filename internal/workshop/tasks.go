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

// A job's work list: the steps the car needs, apart from what is charged.
//
// A priced line says what the customer pays. It cannot say that the brakes
// are done and the exhaust waits for a part, and a job with three operations
// is one job in the state machine whichever of them is finished. A task is
// that finer grain. It may point at the line it is the work for, and at the
// finding it came from, and needs neither: a road test has no price.
//
// Time is not here. A clock entry belongs to a person and a job; when two
// operations share one there is no honest way to split it, so none is
// invented.

// TaskStatus is where a task stands.
type TaskStatus string

const (
	TaskPending  TaskStatus = "pending"
	TaskUnderway TaskStatus = "underway"
	TaskBlocked  TaskStatus = "blocked"
	TaskDone     TaskStatus = "done"
	// Left undone on purpose, with a reason. Not the same as pending: nobody
	// is going to do it.
	TaskOmitted TaskStatus = "omitted"
)

func (s TaskStatus) valid() bool {
	switch s {
	case TaskPending, TaskUnderway, TaskBlocked, TaskDone, TaskOmitted:
		return true
	}
	return false
}

// Label is the status as a catalogue key.
func (s TaskStatus) Label() string {
	switch s {
	case TaskPending:
		return "To do"
	case TaskUnderway:
		return "Underway"
	case TaskBlocked:
		return "Blocked"
	case TaskDone:
		return "Done"
	case TaskOmitted:
		return "Left out"
	}
	return string(s)
}

// What a blocked task is waiting for.
const (
	BlockerPart     = "part"
	BlockerCustomer = "customer"
	BlockerOther    = "other"
)

// BlockerLabel is a blocker as a catalogue key.
func BlockerLabel(b string) string {
	switch b {
	case BlockerPart:
		return "waiting for a part"
	case BlockerCustomer:
		return "waiting for the customer"
	case BlockerOther:
		return "blocked"
	}
	return ""
}

// TaskEvent is one change to a task.
type TaskEvent struct {
	To      string
	Blocker string
	Note    string
	By      string
	At      time.Time
}

// Task is one step on the work list.
type Task struct {
	ID       string
	Position int
	Title    string
	Status   TaskStatus
	Blocker  string
	// "needed" while an omission still has to be put to the customer.
	Decision string

	// The line it is the work for, 0 when it is not tied to one.
	LinePosition int
	// The line was declined or withdrawn, so the task is not owed.
	LineGone  bool
	FindingID string

	// The latest change: who and when.
	By   string
	At   time.Time
	Note string

	History []TaskEvent
}

// Outstanding is whether the task still stands between the car and "ready":
// it is not done, not left out on purpose, and the work it was for has not
// been declined or taken back.
func (t Task) Outstanding() bool {
	switch t.Status {
	case TaskPending, TaskUnderway, TaskBlocked:
		return !t.LineGone
	}
	return false
}

// NeedsCustomer is an omission still waiting to be put to the customer.
func (t Task) NeedsCustomer() bool { return t.Status == TaskOmitted && t.Decision == "needed" }

// Why is what a person is told about a task that stands in the way.
func (t Task) Why() string {
	if t.Status == TaskBlocked {
		return BlockerLabel(t.Blocker)
	}
	return t.Status.Label()
}

// TaskCounts is how a work list stands, for a line on a board.
type TaskCounts struct {
	Total, Done, Outstanding, Blocked, AwaitingCustomer int
}

// CountTasks summarises a work list.
func CountTasks(tasks []Task) TaskCounts {
	var c TaskCounts
	for _, t := range tasks {
		c.Total++
		if t.Status == TaskDone {
			c.Done++
		}
		if t.Outstanding() {
			c.Outstanding++
			if t.Status == TaskBlocked {
				c.Blocked++
			}
		}
		if t.NeedsCustomer() {
			c.AwaitingCustomer++
		}
	}
	return c
}

// TaskMove is a change of status.
type TaskMove struct {
	To      TaskStatus
	Blocker string // for TaskBlocked
	Note    string
	// For TaskOmitted: the customer has to be told or asked.
	NeedsDecision bool
}

// AddTask puts a step on the job's work list. linePosition ties it to a line
// of the job (0 for none); findingID to a finding (empty for none).
//
// The technician may add one -- they are the one who sees what the car needs
// -- and it carries no price. Adding work to a car that was ready puts it
// back in progress: it is on the lift again.
func AddTask(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, title string, linePosition int, findingID string) (string, error) {
	if scope.Role != access.RoleTechnician && !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	title = strings.TrimSpace(title)
	if title == "" || len([]rune(title)) > 120 {
		return "", fmt.Errorf("%w: say what is to be done, in 120 characters or fewer", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		var lineID, finding *string
		if linePosition > 0 {
			var l string
			var gone bool
			if err := tx.QueryRow(ctx, `
				SELECT id, withdrawn_at IS NOT NULL FROM work_order_lines
				WHERE work_order_id = $1 AND position = $2`, jobID, linePosition).Scan(&l, &gone); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: that line is not on this job", ErrInvalid)
				}
				return fmt.Errorf("read line: %w", err)
			}
			if gone {
				return fmt.Errorf("%w: that line was taken back", ErrInvalid)
			}
			lineID = &l
		}
		if findingID != "" {
			if !looksLikeUUID(findingID) {
				return fmt.Errorf("%w: no such finding", ErrInvalid)
			}
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM findings WHERE id = $1 AND work_order_id = $2)`,
				findingID, jobID).Scan(&ok); err != nil || !ok {
				return fmt.Errorf("%w: no such finding on this job", ErrInvalid)
			}
			finding = &findingID
		}
		var err error
		id, err = insertTaskTx(ctx, tx, scope, jobID, title, lineID, finding)
		if err != nil {
			return err
		}
		return backOnTheLiftTx(ctx, tx, jobID)
	})
	return id, err
}

func insertTaskTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, title string, lineID, findingID *string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO job_tasks (shop_id, work_order_id, position, title, line_id, finding_id, created_by)
		VALUES ($1, $2, (SELECT coalesce(max(position), 0) + 1 FROM job_tasks WHERE work_order_id = $2),
		        $3, $4, $5, $6)
		RETURNING id`, scope.ShopID, jobID, title, lineID, findingID, scope.UserID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("add task: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_task_events (shop_id, work_order_id, task_id, to_status, by_user)
		VALUES ($1, $2, $3, 'pending', $4)`, scope.ShopID, jobID, id, scope.UserID); err != nil {
		return "", fmt.Errorf("record task: %w", err)
	}
	return id, nil
}

// backOnTheLiftTx moves a ready job back to in progress. New or reopened work
// on a car that was handed back means it is not ready, and a job that says
// ready with work open is the lie this list exists to stop.
func backOnTheLiftTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, jobID).Scan(&state); err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if state != StateReady {
		return nil
	}
	return setStateTx(ctx, tx, jobID, StateInProgress)
}

// TasksFromLines makes a task of each agreed line of work that has none. The
// front desk's one click on a job that has just been approved; running it
// again adds nothing.
//
// Labour and sublet work only: a part is fitted as part of an operation, and
// a fee is not work on the car. Lines the customer declined or that were taken
// back are not work.
func TasksFromLines(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (int, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return 0, access.ErrForbidden
	}
	var made int
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT l.id, l.description FROM work_order_lines l
			WHERE l.work_order_id = $1 AND l.kind IN ('labour', 'sublet')
			  AND l.approved_at IS NOT NULL AND l.declined_at IS NULL AND l.withdrawn_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM job_tasks t WHERE t.line_id = l.id)
			ORDER BY l.position`, jobID)
		if err != nil {
			return fmt.Errorf("list lines: %w", err)
		}
		type todo struct{ id, title string }
		var lines []todo
		for rows.Next() {
			var t todo
			if err := rows.Scan(&t.id, &t.title); err != nil {
				rows.Close()
				return fmt.Errorf("scan line: %w", err)
			}
			lines = append(lines, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, l := range lines {
			title := []rune(strings.TrimSpace(l.title))
			if len(title) > 120 {
				title = title[:120]
			}
			id := l.id
			if _, err := insertTaskTx(ctx, tx, scope, jobID, string(title), &id, nil); err != nil {
				return err
			}
			made++
		}
		if made > 0 {
			return backOnTheLiftTx(ctx, tx, jobID)
		}
		return nil
	})
	return made, err
}

// MoveTask changes where a task stands.
//
// Done on a task tied to a line says the work on that line was done -- the
// same fact, written once -- which is what a declined job is still billed for.
// It never takes a part off the shelf and never records the customer's yes:
// stock is taken out by someone saying so, and agreement is the customer's.
//
// Two people reaching the same state is not an error, so a retried tap is
// harmless. Leaving a task out is the front desk's, since it may mean telling
// the customer something.
func MoveTask(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, taskID string, m TaskMove) error {
	if scope.Role != access.RoleTechnician && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(taskID) {
		return ErrNotFound
	}
	if !m.To.valid() {
		return fmt.Errorf("%w: not a status", ErrInvalid)
	}
	m.Note = strings.TrimSpace(m.Note)
	if len([]rune(m.Note)) > 300 {
		return fmt.Errorf("%w: keep the note to 300 characters", ErrInvalid)
	}
	switch m.To {
	case TaskBlocked:
		switch m.Blocker {
		case BlockerPart, BlockerCustomer:
		case BlockerOther:
			if m.Note == "" {
				return fmt.Errorf("%w: say what is in the way", ErrInvalid)
			}
		default:
			return fmt.Errorf("%w: say what it is waiting for", ErrInvalid)
		}
	case TaskOmitted:
		if !scope.Role.SeesCustomerPersonalData() {
			return access.ErrForbidden
		}
		if m.Note == "" {
			return fmt.Errorf("%w: say why it is being left out", ErrInvalid)
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var jobID string
		if err := tx.QueryRow(ctx, `SELECT work_order_id FROM job_tasks WHERE id = $1`, taskID).Scan(&jobID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read task: %w", err)
		}
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		return moveTaskTx(ctx, tx, scope, jobID, taskID, m)
	})
}

func moveTaskTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, taskID string, m TaskMove) error {
	var from TaskStatus
	var blocker *string
	var lineID *string
	if err := tx.QueryRow(ctx, `SELECT status, blocker, line_id FROM job_tasks WHERE id = $1 FOR UPDATE`,
		taskID).Scan(&from, &blocker, &lineID); err != nil {
		return fmt.Errorf("lock task: %w", err)
	}
	// Putting an omitted task back is the desk's, as leaving it out was.
	if from == TaskOmitted && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if from == m.To && (m.To != TaskBlocked || deref(blocker) == m.Blocker) {
		return nil
	}
	blk := ""
	if m.To == TaskBlocked {
		blk = m.Blocker
	}
	decision := ""
	if m.To == TaskOmitted && m.NeedsDecision {
		decision = "needed"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE job_tasks SET status = $2, blocker = nullif($3, ''), decision = nullif($4, '') WHERE id = $1`,
		taskID, string(m.To), blk, decision); err != nil {
		return fmt.Errorf("move task: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO job_task_events (shop_id, work_order_id, task_id, to_status, blocker, note, by_user)
		VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), $7)`,
		scope.ShopID, jobID, taskID, string(m.To), blk, m.Note, scope.UserID); err != nil {
		return fmt.Errorf("record task move: %w", err)
	}
	if lineID != nil {
		if m.To == TaskDone {
			if err := performedByTaskTx(ctx, tx, jobID, *lineID, true); err != nil {
				return err
			}
		} else if from == TaskDone && m.To != TaskOmitted {
			if err := performedByTaskTx(ctx, tx, jobID, *lineID, false); err != nil {
				return err
			}
		}
	}
	// A task opened again on a car that was handed back.
	if from == TaskDone && (m.To == TaskPending || m.To == TaskUnderway || m.To == TaskBlocked) {
		return backOnTheLiftTx(ctx, tx, jobID)
	}
	if from == TaskOmitted && m.To != TaskDone {
		return backOnTheLiftTx(ctx, tx, jobID)
	}
	return nil
}

// performedByTaskTx writes what MarkPerformed would for the task's line. A
// line the customer declined is only made billable again on a declined job,
// where the line's whole status is what was performed; on any other job a
// technician's tick must not bill what the customer turned down.
func performedByTaskTx(ctx context.Context, tx pgx.Tx, jobID, lineID string, performed bool) error {
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, jobID).Scan(&state); err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if performed {
		_, err := tx.Exec(ctx, `
			UPDATE work_order_lines
			   SET performed_at = coalesce(performed_at, now()), declined_at = NULL, updated_at = now()
			 WHERE id = $1 AND withdrawn_at IS NULL AND (declined_at IS NULL OR $2)`,
			lineID, state == StateDeclined)
		if err != nil {
			return fmt.Errorf("mark performed: %w", err)
		}
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE work_order_lines
		   SET performed_at = NULL,
		       declined_at = CASE WHEN $2 THEN coalesce(declined_at, now()) END,
		       updated_at = now()
		 WHERE id = $1 AND performed_at IS NOT NULL`, lineID, state == StateDeclined)
	if err != nil {
		return fmt.Errorf("unmark performed: %w", err)
	}
	return nil
}

// finishTasksOfLineTx completes the open tasks of a line that was marked
// performed some other way, so the two lists cannot disagree.
func finishTasksOfLineTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, positions []int, note string) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id FROM job_tasks t JOIN work_order_lines l ON l.id = t.line_id
		WHERE t.work_order_id = $1 AND l.position = ANY($2) AND t.status IN ('pending', 'underway', 'blocked')
		FOR UPDATE OF t`, jobID, positions)
	if err != nil {
		return fmt.Errorf("find tasks: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE job_tasks SET status = 'done', blocker = NULL WHERE id = $1`, id); err != nil {
			return fmt.Errorf("finish task: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_task_events (shop_id, work_order_id, task_id, to_status, note, by_user)
			VALUES ($1, $2, $3, 'done', $4, $5)`, scope.ShopID, jobID, id, note, scope.UserID); err != nil {
			return fmt.Errorf("record task: %w", err)
		}
	}
	return nil
}

// closeTasksOfDeclinedJobTx settles the work list when the customer says no:
// what was performed before it is done, everything else still open is left
// out. The customer's no is the decision, so none is owed.
func closeTasksOfDeclinedJobTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID string, performed []int) error {
	if len(performed) > 0 {
		if err := finishTasksOfLineTx(ctx, tx, scope, jobID, performed, "Performed before the customer declined."); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT id FROM job_tasks WHERE work_order_id = $1 AND status IN ('pending', 'underway', 'blocked')
		FOR UPDATE`, jobID)
	if err != nil {
		return fmt.Errorf("find tasks: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE job_tasks SET status = 'omitted', blocker = NULL WHERE id = $1`, id); err != nil {
			return fmt.Errorf("leave task out: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_task_events (shop_id, work_order_id, task_id, to_status, note, by_user)
			VALUES ($1, $2, $3, 'omitted', 'The customer declined.', $4)`, scope.ShopID, jobID, id, scope.UserID); err != nil {
			return fmt.Errorf("record task: %w", err)
		}
	}
	return nil
}

// RecordTaskDecision notes that the customer was told or asked about a task
// that was left out, and what they said.
func RecordTaskDecision(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, taskID, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(taskID) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if note == "" || len([]rune(note)) > 300 {
		return fmt.Errorf("%w: say what the customer decided, in 300 characters or fewer", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var jobID string
		if err := tx.QueryRow(ctx, `SELECT work_order_id FROM job_tasks WHERE id = $1`, taskID).Scan(&jobID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read task: %w", err)
		}
		if err := lockJobTx(ctx, tx, jobID); err != nil {
			return err
		}
		var decision *string
		if err := tx.QueryRow(ctx, `SELECT decision FROM job_tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&decision); err != nil {
			return fmt.Errorf("lock task: %w", err)
		}
		if deref(decision) != "needed" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE job_tasks SET decision = 'recorded' WHERE id = $1`, taskID); err != nil {
			return fmt.Errorf("record decision: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO job_task_events (shop_id, work_order_id, task_id, to_status, note, by_user)
			VALUES ($1, $2, $3, 'decision', $4, $5)`, scope.ShopID, jobID, taskID, note, scope.UserID)
		return err
	})
}

// TasksFor is a job's work list, with how each step got to where it is. No
// customer field exists on it, so a technician may read it.
func TasksFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]Task, error) {
	var out []Task
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = tasksTx(ctx, tx, jobID)
		return err
	})
	return out, err
}

func tasksTx(ctx context.Context, tx pgx.Tx, jobID string) ([]Task, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.position, t.title, t.status, coalesce(t.blocker, ''), coalesce(t.decision, ''),
		       coalesce(l.position, 0),
		       coalesce(l.withdrawn_at IS NOT NULL OR (l.declined_at IS NOT NULL AND l.performed_at IS NULL), false),
		       coalesce(t.finding_id::text, '')
		FROM job_tasks t
		LEFT JOIN work_order_lines l ON l.id = t.line_id
		WHERE t.work_order_id = $1
		ORDER BY t.position`, jobID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var out []Task
	index := map[string]int{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Position, &t.Title, &t.Status, &t.Blocker, &t.Decision,
			&t.LinePosition, &t.LineGone, &t.FindingID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan task: %w", err)
		}
		index[t.ID] = len(out)
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	ev, err := tx.Query(ctx, `
		SELECT e.task_id, e.to_status, coalesce(e.blocker, ''), coalesce(e.note, ''),
		       coalesce(p.display_name, ''), e.at
		FROM job_task_events e
		LEFT JOIN users u ON u.id = e.by_user
		LEFT JOIN people p ON p.id = u.person_id
		WHERE e.work_order_id = $1
		ORDER BY e.at, e.id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("list task history: %w", err)
	}
	defer ev.Close()
	for ev.Next() {
		var id string
		var e TaskEvent
		if err := ev.Scan(&id, &e.To, &e.Blocker, &e.Note, &e.By, &e.At); err != nil {
			return nil, fmt.Errorf("scan task event: %w", err)
		}
		i, ok := index[id]
		if !ok {
			continue
		}
		out[i].History = append(out[i].History, e)
		if e.To != "decision" {
			out[i].By, out[i].At, out[i].Note = e.By, e.At, e.Note
		}
	}
	return out, ev.Err()
}

// outstandingTasksTx is the open work that stands between a car and "ready".
func outstandingTasksTx(ctx context.Context, tx pgx.Tx, jobID string) ([]Task, error) {
	all, err := tasksTx(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, t := range all {
		if t.Outstanding() {
			out = append(out, t)
		}
	}
	return out, nil
}

// unfinishedTasksError is the refusal for a car that is not ready, naming
// what is in the way so it does not have to be looked for.
func unfinishedTasksError(tasks []Task) error {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		parts = append(parts, fmt.Sprintf("%s (%s)", t.Title, strings.ToLower(t.Why())))
	}
	return fmt.Errorf("%w: there is work left on the list: %s", ErrInvalid, strings.Join(parts, "; "))
}
